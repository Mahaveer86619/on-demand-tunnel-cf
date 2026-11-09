// Package accesslog writes buffered, append-only log files that rotate by
// size or UTC day, gzip their archives in the background and prune old ones.
package accesslog

import (
	"bufio"
	"compress/gzip"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	activeName  = "access.log"
	archiveDir  = "archive"
	stampLayout = "20060102T150405.000"
	flushEvery  = time.Second
)

type Options struct {
	MaxSize     int64         // rotate once the active file would exceed this many bytes
	MaxAge      time.Duration // archives older than this are deleted; 0 keeps them forever
	MaxArchives int           // newest archives kept per directory; 0 means no limit
}

type Writer struct {
	dir  string
	opts Options

	mu     sync.Mutex
	f      *os.File
	bw     *bufio.Writer
	size   int64
	day    int
	dirty  bool
	closed bool

	stop chan struct{}
	wg   sync.WaitGroup
}

func Open(dir string, opts Options) (*Writer, error) {
	if err := os.MkdirAll(filepath.Join(dir, archiveDir), 0o755); err != nil {
		return nil, err
	}
	w := &Writer{dir: dir, opts: opts, stop: make(chan struct{})}
	if err := w.open(); err != nil {
		return nil, err
	}
	w.wg.Add(2)
	go func() {
		defer w.wg.Done()
		compressLeftovers(dir)
		Cleanup(dir, opts)
	}()
	go w.flushLoop()
	return w, nil
}

func (w *Writer) open() error {
	f, err := os.OpenFile(filepath.Join(w.dir, activeName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f, w.size = f, st.Size()
	// A non-empty file left from an earlier day is rotated on the next write.
	w.day = dayOf(time.Now())
	if w.size > 0 {
		w.day = dayOf(st.ModTime())
	}
	if w.bw == nil {
		w.bw = bufio.NewWriterSize(f, 32<<10)
	} else {
		w.bw.Reset(f)
	}
	return nil
}

// Write appends p, which should be one complete line.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	if w.size > 0 && (w.size+int64(len(p)) > w.opts.MaxSize || dayOf(time.Now()) != w.day) {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.bw.Write(p)
	w.size += int64(n)
	w.dirty = true
	return n, err
}

func (w *Writer) rotate() error {
	flushErr := w.bw.Flush()
	w.f.Close()
	dst := filepath.Join(w.dir, archiveDir, "access-"+time.Now().UTC().Format(stampLayout)+".log")
	renameErr := os.Rename(filepath.Join(w.dir, activeName), dst)
	if renameErr == nil {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			if err := compress(dst); err != nil {
				slog.Warn("compress access log", "file", dst, "err", err)
			}
			Cleanup(w.dir, w.opts)
		}()
	}
	if err := w.open(); err != nil {
		return err
	}
	if renameErr != nil {
		return renameErr
	}
	return flushErr
}

// RotateIfStale rotates a non-empty active file that belongs to a previous
// day, so idle tunnels still roll over daily.
func (w *Writer) RotateIfStale() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.size == 0 || dayOf(time.Now()) == w.day {
		return nil
	}
	return w.rotate()
}

func (w *Writer) flushLoop() {
	defer w.wg.Done()
	t := time.NewTicker(flushEvery)
	defer t.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-t.C:
		}
		w.mu.Lock()
		if w.dirty && !w.closed {
			w.bw.Flush()
			w.dirty = false
		}
		w.mu.Unlock()
	}
}

func (w *Writer) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	err := w.bw.Flush()
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	w.mu.Unlock()
	close(w.stop)
	w.wg.Wait()
	return err
}

// Cleanup applies the retention policy to the archives under dir.
func Cleanup(dir string, opts Options) {
	matches, _ := filepath.Glob(filepath.Join(dir, archiveDir, "access-*.log.gz"))
	sort.Strings(matches) // names embed the rotation time, so this is oldest first
	cutoff := time.Now().Add(-opts.MaxAge)
	kept := matches[:0]
	for _, p := range matches {
		if opts.MaxAge > 0 {
			if st, err := os.Stat(p); err == nil && st.ModTime().Before(cutoff) {
				os.Remove(p)
				continue
			}
		}
		kept = append(kept, p)
	}
	if opts.MaxArchives > 0 && len(kept) > opts.MaxArchives {
		for _, p := range kept[:len(kept)-opts.MaxArchives] {
			os.Remove(p)
		}
	}
}

// compressLeftovers finishes archives a crash or restart left uncompressed.
func compressLeftovers(dir string) {
	tmps, _ := filepath.Glob(filepath.Join(dir, archiveDir, "*.gz.tmp"))
	for _, p := range tmps {
		os.Remove(p)
	}
	logs, _ := filepath.Glob(filepath.Join(dir, archiveDir, "access-*.log"))
	for _, p := range logs {
		if err := compress(p); err != nil {
			slog.Warn("compress access log", "file", p, "err", err)
		}
	}
}

func compress(src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	tmp := src + ".gz.tmp"
	out, err := os.Create(tmp)
	if err != nil {
		in.Close()
		return err
	}
	zw, _ := gzip.NewWriterLevel(out, gzip.BestCompression)
	_, err = io.Copy(zw, in)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	in.Close()
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, src+".gz"); err != nil {
		return err
	}
	return os.Remove(src)
}

func dayOf(t time.Time) int {
	y, m, d := t.UTC().Date()
	return y*10000 + int(m)*100 + d
}

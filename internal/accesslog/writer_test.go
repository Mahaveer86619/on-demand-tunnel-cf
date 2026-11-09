package accesslog

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRotateCompressAndRetain(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, Options{MaxSize: 100, MaxArchives: 3})
	if err != nil {
		t.Fatal(err)
	}
	line := bytes.Repeat([]byte("x"), 39)
	line = append(line, '\n')
	for range 30 {
		if _, err := w.Write(line); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // keep archive names unique
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	gz, _ := filepath.Glob(filepath.Join(dir, archiveDir, "*.log.gz"))
	if len(gz) != 3 {
		t.Fatalf("want 3 archives after retention, got %d: %v", len(gz), gz)
	}
	plain, _ := filepath.Glob(filepath.Join(dir, archiveDir, "*.log"))
	if len(plain) != 0 {
		t.Fatalf("uncompressed archives left behind: %v", plain)
	}

	f, err := os.Open(gz[len(gz)-1])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(zr)
	if len(data) != 80 {
		t.Fatalf("archive holds %d bytes, want 80", len(data))
	}

	active, _ := os.ReadFile(filepath.Join(dir, activeName))
	if len(active) == 0 || len(active) > 100 {
		t.Fatalf("active file size %d out of range", len(active))
	}
}

func TestMaxAgeDeletesOldArchives(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, archiveDir), 0o755)
	old := filepath.Join(dir, archiveDir, "access-20200101T000000.000.log.gz")
	fresh := filepath.Join(dir, archiveDir, "access-20990101T000000.000.log.gz")
	os.WriteFile(old, nil, 0o644)
	os.WriteFile(fresh, nil, 0o644)
	past := time.Now().Add(-48 * time.Hour)
	os.Chtimes(old, past, past)

	Cleanup(dir, Options{MaxAge: 24 * time.Hour})

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old archive was not deleted")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh archive was deleted")
	}
}

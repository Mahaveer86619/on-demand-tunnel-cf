package tunnel

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const (
	minBackoff = 2 * time.Second
	maxBackoff = time.Minute
)

var quickURL = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

// runner keeps one cloudflared quick tunnel alive, restarting it with
// exponential backoff (trycloudflare.com rate-limits rapid re-creation).
type runner struct {
	bin  string
	args []string
	env  []string
	e    *entry
}

func (r *runner) run(ctx context.Context) {
	backoff := minBackoff
	for attempt := 0; ; attempt++ {
		r.e.launching(attempt > 0)
		began := time.Now()
		err := r.once(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(began) > time.Minute {
			backoff = minBackoff
		}
		r.e.exited(err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (r *runner) once(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, r.bin, r.args...)
	cmd.Cancel = func() error {
		// Windows has no SIGTERM for child processes, so fall back to Kill.
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = 10 * time.Second
	if len(r.env) > 0 {
		cmd.Env = append(os.Environ(), r.env...)
	}
	out, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 4096), 64<<10)
	for sc.Scan() {
		line := sc.Text()
		r.e.plog.add(line)
		switch {
		case strings.Contains(line, "Registered tunnel connection"):
			r.e.connected()
		case strings.Contains(line, " ERR "):
			r.e.setLastErr(strings.TrimSpace(line))
		}
		if u := quickURL.FindString(line); u != "" && u != "https://api.trycloudflare.com" {
			r.e.gotURL(u)
		}
	}
	if err := sc.Err(); err != nil {
		r.e.plog.add("tunneld: reading cloudflared output: " + err.Error())
	}
	io.Copy(io.Discard, out)
	return cmd.Wait()
}

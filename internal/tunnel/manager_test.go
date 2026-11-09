package tunnel

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pmaha/on-demand-tunnel-cf/internal/accesslog"
)

// TestMain doubles as a fake cloudflared: the manager re-executes this test
// binary, which prints the lines the real one would and then idles.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_CLOUDFLARED") == "1" {
		fmt.Fprintln(os.Stderr, "2026-11-09T00:00:00Z INF Requesting new quick Tunnel on trycloudflare.com...")
		fmt.Fprintln(os.Stderr, "2026-11-09T00:00:01Z INF |  https://fake-words-here.trycloudflare.com  |")
		fmt.Fprintln(os.Stderr, "2026-11-09T00:00:02Z INF Registered tunnel connection connIndex=0 location=fra01 protocol=quic")
		time.Sleep(time.Hour)
		return
	}
	os.Exit(m.Run())
}

func newTestManager(t *testing.T, dataDir string) *Manager {
	t.Helper()
	t.Setenv("FAKE_CLOUDFLARED", "1")
	m, err := New(Config{
		DataDir:         dataDir,
		Cloudflared:     os.Args[0],
		DefaultHost:     "127.0.0.1",
		AccessLog:       accesslog.Options{MaxSize: 1 << 20},
		RecentRequests:  10,
		ProcessLogLines: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestTunnelLifecycle(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello from %s host=%s xfp=%s", r.URL.Path, r.Host, r.Header.Get("X-Forwarded-Proto"))
	}))
	defer upstream.Close()
	port := upstream.Listener.Addr().(*net.TCPAddr).Port

	dir := t.TempDir()
	m := newTestManager(t, dir)

	st, err := m.Create(Input{Name: "My App", Port: port})
	if err != nil {
		t.Fatal(err)
	}
	if st.ID != "my-app" {
		t.Fatalf("id = %q", st.ID)
	}
	if _, err := m.Create(Input{Name: "my app", Port: port}); err != ErrExists {
		t.Fatalf("duplicate create: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	u, err := m.WaitURL(ctx, "my-app")
	if err != nil || u != "https://fake-words-here.trycloudflare.com" {
		t.Fatalf("WaitURL = %q, %v", u, err)
	}

	st, _ = m.Get("my-app")
	if st.State != StateRunning || st.LocalPort == 0 {
		t.Fatalf("status = %+v", st)
	}

	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/ping?x=\"1\"", st.LocalPort), nil)
	req.Header.Set("Cf-Connecting-Ip", "203.0.113.9")
	req.Header.Set("Cf-Ipcountry", "IN")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.HasPrefix(string(body), "hello from /ping") || !strings.Contains(string(body), "xfp=https") {
		t.Fatalf("body = %q", body)
	}

	acts, _ := m.Activity("my-app", 0)
	if len(acts) != 1 || acts[0].IP != "203.0.113.9" || acts[0].Status != 200 || acts[0].Country != "IN" {
		t.Fatalf("activity = %+v", acts)
	}

	// Retarget to a dead port: URL must survive, requests now fail with 502.
	dead := 1
	if _, err := m.Update("my-app", Patch{Port: &dead}); err != nil {
		t.Fatal(err)
	}
	st, _ = m.Get("my-app")
	if st.URL != u {
		t.Fatalf("url changed on retarget: %q", st.URL)
	}
	res, err = http.Get(fmt.Sprintf("http://127.0.0.1:%d/", st.LocalPort))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("status after retarget = %d", res.StatusCode)
	}

	off := false
	if st, _ = m.Update("my-app", Patch{Enabled: &off}); st.State != StateStopped || st.URL != "" {
		t.Fatalf("after disable: %+v", st)
	}

	logData, err := os.ReadFile(filepath.Join(dir, "logs", "my-app", "access.log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(logData)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `203.0.113.9 IN "GET /ping?x=\"1\" HTTP/1.1" 200`) {
		t.Fatalf("access log:\n%s", logData)
	}
	m.Close()

	// State persists across restarts.
	m2 := newTestManager(t, dir)
	defer m2.Close()
	st, err = m2.Get("my-app")
	if err != nil || st.Enabled || st.Target != "http://127.0.0.1:1" {
		t.Fatalf("reloaded = %+v, %v", st, err)
	}
	if err := m2.Delete("my-app", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "logs", "my-app")); !os.IsNotExist(err) {
		t.Fatal("logs not purged")
	}
}

func TestResolveTarget(t *testing.T) {
	m := &Manager{cfg: Config{DefaultHost: "host.docker.internal"}}
	cases := []struct {
		port   int
		target string
		want   string
		ok     bool
	}{
		{3000, "", "http://host.docker.internal:3000", true},
		{0, "8080", "http://host.docker.internal:8080", true},
		{0, "192.168.1.5:8123", "http://192.168.1.5:8123", true},
		{0, "https://nas.lan:5001/", "https://nas.lan:5001/", true},
		{0, "ftp://x", "", false},
		{70000, "", "", false},
		{80, "http://x", "", false},
		{0, "", "", false},
	}
	for _, c := range cases {
		got, err := m.resolveTarget(c.port, c.target)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("resolveTarget(%d, %q) = %q, %v", c.port, c.target, got, err)
		}
	}
}

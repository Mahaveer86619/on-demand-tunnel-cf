package tunnel

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pmaha/on-demand-tunnel-cf/internal/accesslog"
)

// Entry is one proxied request, as shown in the activity view.
type Entry struct {
	Time       time.Time `json:"time"`
	IP         string    `json:"ip"`
	Country    string    `json:"country,omitempty"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Status     int       `json:"status"`
	Bytes      int64     `json:"bytes"`
	DurationMs float64   `json:"duration_ms"`
	UserAgent  string    `json:"user_agent,omitempty"`
}

var transport = &http.Transport{
	DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	MaxIdleConns:          64,
	MaxIdleConnsPerHost:   8,
	IdleConnTimeout:       60 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: time.Second,
}

var (
	copyBuffers bufferPool
	lineBuffers = sync.Pool{New: func() any { b := make([]byte, 0, 512); return &b }}
)

type bufferPool struct{ p sync.Pool }

func (b *bufferPool) Get() []byte {
	if v := b.p.Get(); v != nil {
		return *v.(*[]byte)
	}
	return make([]byte, 32<<10)
}

func (b *bufferPool) Put(buf []byte) { b.p.Put(&buf) }

// proxy sits between cloudflared and the target so every request can be
// counted and logged; cloudflared itself only ever sees 127.0.0.1:<port>.
type proxy struct {
	target atomic.Pointer[url.URL]
	rp     *httputil.ReverseProxy
	srv    *http.Server
	ln     net.Listener
	alog   *accesslog.Writer
	stats  *Stats
	recent *ring[Entry]
}

func newProxy(target *url.URL, alog *accesslog.Writer, stats *Stats, recent *ring[Entry]) (*proxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &proxy{ln: ln, alog: alog, stats: stats, recent: recent}
	p.target.Store(target)
	errLog := slog.NewLogLogger(slog.Default().Handler(), slog.LevelDebug)
	p.rp = &httputil.ReverseProxy{
		Rewrite:      p.rewrite,
		Transport:    transport,
		BufferPool:   &copyBuffers,
		ErrorHandler: p.upstreamError,
		ErrorLog:     errLog,
	}
	p.srv = &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          errLog,
	}
	go p.srv.Serve(ln)
	return p, nil
}

func (p *proxy) port() int { return p.ln.Addr().(*net.TCPAddr).Port }

func (p *proxy) setTarget(u *url.URL) { p.target.Store(u) }

func (p *proxy) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if p.srv.Shutdown(ctx) != nil {
		p.srv.Close()
	}
}

func (p *proxy) rewrite(pr *httputil.ProxyRequest) {
	pr.SetURL(p.target.Load())
	// Rewrite strips inbound X-Forwarded-*; keep the client chain cloudflared built.
	if xff := pr.In.Header["X-Forwarded-For"]; len(xff) > 0 {
		pr.Out.Header["X-Forwarded-For"] = xff
	}
	pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
	pr.Out.Header.Set("X-Forwarded-Proto", "https")
}

func (p *proxy) upstreamError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		w.WriteHeader(499) // client went away; nginx's convention
		return
	}
	p.stats.upstreamErrors.Add(1)
	slog.Debug("upstream error", "target", p.target.Load().Host, "err", err)
	http.Error(w, "upstream unavailable", http.StatusBadGateway)
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &recorder{ResponseWriter: w}
	p.stats.active.Add(1)
	// Deferred so requests aborted mid-body (ErrAbortHandler panic) still get logged.
	defer func() {
		p.stats.active.Add(-1)
		p.record(r, rec.code(), rec.bytes, start, time.Since(start))
	}()
	p.rp.ServeHTTP(rec, r)
}

func (p *proxy) record(r *http.Request, status int, n int64, start time.Time, d time.Duration) {
	ip := clientIP(r)
	country := r.Header.Get("Cf-Ipcountry")
	ua := r.UserAgent()
	p.stats.observe(status, n, start)
	p.recent.add(Entry{
		Time:       start.UTC(),
		IP:         ip,
		Country:    country,
		Method:     r.Method,
		Path:       truncate(r.RequestURI, 512),
		Status:     status,
		Bytes:      n,
		DurationMs: float64(d.Microseconds()) / 1000,
		UserAgent:  truncate(ua, 256),
	})

	bp := lineBuffers.Get().(*[]byte)
	b := (*bp)[:0]
	b = start.UTC().AppendFormat(b, "2006-01-02T15:04:05.000Z")
	b = append(b, ' ')
	b = append(b, ip...)
	b = append(b, ' ')
	b = appendOr(b, country)
	b = append(b, ` "`...)
	b = appendEscaped(b, r.Method)
	b = append(b, ' ')
	b = appendEscaped(b, r.RequestURI)
	b = append(b, ' ')
	b = appendEscaped(b, r.Proto)
	b = append(b, `" `...)
	b = strconv.AppendInt(b, int64(status), 10)
	b = append(b, ' ')
	b = strconv.AppendInt(b, n, 10)
	b = append(b, ' ')
	b = strconv.AppendFloat(b, float64(d.Microseconds())/1000, 'f', 1, 64)
	b = append(b, `ms "`...)
	b = appendEscaped(b, r.Referer())
	b = append(b, `" "`...)
	b = appendEscaped(b, ua)
	b = append(b, "\"\n"...)
	if _, err := p.alog.Write(b); err != nil {
		slog.Warn("write access log", "err", err)
	}
	*bp = b
	lineBuffers.Put(bp)
}

func clientIP(r *http.Request) string {
	if ip := r.Header.Get("Cf-Connecting-Ip"); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func appendOr(b []byte, s string) []byte {
	if s == "" {
		return append(b, '-')
	}
	return appendEscaped(b, s)
}

const hexDigits = "0123456789abcdef"

func appendEscaped(b []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"' || c == '\\':
			b = append(b, '\\', c)
		case c < 0x20 || c == 0x7f:
			b = append(b, '\\', 'x', hexDigits[c>>4], hexDigits[c&0xf])
		default:
			b = append(b, c)
		}
	}
	return b
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *recorder) WriteHeader(code int) {
	// 1xx informational responses (e.g. 103) precede the real status.
	if r.status == 0 && (code >= 200 || code == http.StatusSwitchingProtocols) {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach Hijack for WebSocket upgrades.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *recorder) code() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

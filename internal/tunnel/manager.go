// Package tunnel manages registered forwards: one cloudflared quick tunnel
// plus one logging reverse proxy per registration.
package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pmaha/on-demand-tunnel-cf/internal/accesslog"
)

var (
	ErrNotFound = errors.New("tunnel not found")
	ErrExists   = errors.New("a tunnel with that name already exists")
)

type ValidationError struct{ msg string }

func (e *ValidationError) Error() string { return e.msg }

func invalid(format string, a ...any) error {
	return &ValidationError{fmt.Sprintf(format, a...)}
}

type Config struct {
	DataDir         string
	Cloudflared     string
	CloudflaredArgs []string
	DefaultHost     string
	AccessLog       accesslog.Options
	RecentRequests  int
	ProcessLogLines int
}

type Spec struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Target    string    `json:"target"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Input struct {
	Name    string `json:"name"`
	Port    int    `json:"port"`
	Target  string `json:"target"`
	Enabled *bool  `json:"enabled"`
}

type Patch struct {
	Name    *string `json:"name"`
	Port    *int    `json:"port"`
	Target  *string `json:"target"`
	Enabled *bool   `json:"enabled"`
}

type State string

const (
	StateStopped    State = "stopped"
	StateStarting   State = "starting"
	StateConnecting State = "connecting"
	StateRunning    State = "running"
	StateRestarting State = "restarting"
	StateError      State = "error"
)

type Status struct {
	Spec
	State     State         `json:"state"`
	URL       string        `json:"url,omitempty"`
	URLSince  *time.Time    `json:"url_since,omitempty"`
	LocalPort int           `json:"local_port,omitempty"`
	Restarts  int           `json:"restarts"`
	LastError string        `json:"last_error,omitempty"`
	Stats     StatsSnapshot `json:"stats"`
}

type entry struct {
	id     string
	spec   Spec // guarded by Manager.mu
	stats  *Stats
	recent *ring[Entry]
	plog   *ring[string]

	op      sync.Mutex // serializes start/stop/retarget
	deleted bool
	proxy   *proxy
	alog    *accesslog.Writer
	cancel  context.CancelFunc
	done    chan struct{}

	mu        sync.Mutex
	state     State
	url       string
	urlSince  time.Time
	localPort int
	restarts  int
	lastErr   string
	changed   chan struct{} // closed and replaced on every state change
}

type Manager struct {
	cfg    Config
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.RWMutex
	entries map[string]*entry
}

func New(cfg Config) (*Manager, error) {
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "logs"), 0o755); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{cfg: cfg, ctx: ctx, cancel: cancel, entries: map[string]*entry{}}
	specs, err := m.load()
	if err != nil {
		cancel()
		return nil, err
	}
	for _, s := range specs {
		m.entries[s.ID] = m.newEntry(s)
	}
	for _, s := range specs {
		if s.Enabled {
			e := m.entries[s.ID]
			e.op.Lock()
			m.start(e, s.Target)
			e.op.Unlock()
		}
	}
	go m.janitor()
	return m, nil
}

func (m *Manager) newEntry(s Spec) *entry {
	return &entry{
		id:      s.ID,
		spec:    s,
		stats:   newStats(),
		recent:  newRing[Entry](m.cfg.RecentRequests),
		plog:    newRing[string](m.cfg.ProcessLogLines),
		state:   StateStopped,
		changed: make(chan struct{}),
	}
}

func (m *Manager) Close() {
	m.cancel()
	m.mu.RLock()
	all := make([]*entry, 0, len(m.entries))
	for _, e := range m.entries {
		all = append(all, e)
	}
	m.mu.RUnlock()
	var wg sync.WaitGroup
	for _, e := range all {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.op.Lock()
			m.stop(e)
			e.op.Unlock()
		}()
	}
	wg.Wait()
}

func (m *Manager) List() []Status {
	m.mu.RLock()
	all := make([]*entry, 0, len(m.entries))
	for _, e := range m.entries {
		all = append(all, e)
	}
	m.mu.RUnlock()
	out := make([]Status, len(all))
	for i, e := range all {
		out[i] = m.status(e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (m *Manager) Get(id string) (Status, error) {
	e, err := m.lookup(id)
	if err != nil {
		return Status{}, err
	}
	return m.status(e), nil
}

func (m *Manager) Create(in Input) (Status, error) {
	target, err := m.resolveTarget(in.Port, in.Target)
	if err != nil {
		return Status{}, err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = defaultName(in.Port, target)
	}
	id := slugify(name)
	if id == "" {
		return Status{}, invalid("name must contain letters or digits")
	}
	now := time.Now().UTC()
	e := m.newEntry(Spec{
		ID:        id,
		Name:      name,
		Target:    target,
		Enabled:   in.Enabled == nil || *in.Enabled,
		CreatedAt: now,
		UpdatedAt: now,
	})

	m.mu.Lock()
	if _, ok := m.entries[id]; ok {
		m.mu.Unlock()
		return Status{}, ErrExists
	}
	m.entries[id] = e
	if err := m.saveLocked(); err != nil {
		delete(m.entries, id)
		m.mu.Unlock()
		return Status{}, err
	}
	m.mu.Unlock()

	if e.spec.Enabled {
		e.op.Lock()
		if !e.deleted {
			m.start(e, target)
		}
		e.op.Unlock()
	}
	slog.Info("tunnel registered", "id", id, "target", target)
	return m.status(e), nil
}

func (m *Manager) Update(id string, p Patch) (Status, error) {
	e, err := m.lookup(id)
	if err != nil {
		return Status{}, err
	}
	e.op.Lock()
	defer e.op.Unlock()
	if e.deleted {
		return Status{}, ErrNotFound
	}

	m.mu.Lock()
	old := e.spec
	next := old
	if p.Name != nil {
		if next.Name = strings.TrimSpace(*p.Name); next.Name == "" {
			m.mu.Unlock()
			return Status{}, invalid("name cannot be empty")
		}
	}
	if p.Port != nil || p.Target != nil {
		var port int
		var target string
		if p.Port != nil {
			port = *p.Port
		}
		if p.Target != nil {
			target = *p.Target
		}
		if next.Target, err = m.resolveTarget(port, target); err != nil {
			m.mu.Unlock()
			return Status{}, err
		}
	}
	if p.Enabled != nil {
		next.Enabled = *p.Enabled
	}
	next.UpdatedAt = time.Now().UTC()
	e.spec = next
	if err := m.saveLocked(); err != nil {
		e.spec = old
		m.mu.Unlock()
		return Status{}, err
	}
	m.mu.Unlock()

	switch {
	case !next.Enabled:
		m.stop(e)
	case e.cancel == nil:
		m.start(e, next.Target)
	case next.Target != old.Target:
		// Retargeting only swaps the proxy upstream, so the public URL survives.
		u, _ := url.Parse(next.Target)
		e.proxy.setTarget(u)
	}
	return m.status(e), nil
}

func (m *Manager) Delete(id string, purgeLogs bool) error {
	m.mu.Lock()
	e, ok := m.entries[id]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	delete(m.entries, id)
	if err := m.saveLocked(); err != nil {
		m.entries[id] = e
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()

	e.op.Lock()
	e.deleted = true
	m.stop(e)
	e.op.Unlock()
	slog.Info("tunnel deleted", "id", id, "purge_logs", purgeLogs)
	if purgeLogs {
		return os.RemoveAll(m.logDir(id))
	}
	return nil
}

// Restart replaces the cloudflared process, which yields a fresh URL.
func (m *Manager) Restart(id string) (Status, error) {
	e, err := m.lookup(id)
	if err != nil {
		return Status{}, err
	}
	e.op.Lock()
	defer e.op.Unlock()
	m.mu.RLock()
	spec := e.spec
	m.mu.RUnlock()
	if e.deleted {
		return Status{}, ErrNotFound
	}
	if !spec.Enabled {
		return Status{}, invalid("tunnel is disabled")
	}
	m.stop(e)
	m.start(e, spec.Target)
	return m.status(e), nil
}

// WaitURL blocks until the tunnel has a connected URL or ctx ends; on
// timeout it still returns an assigned-but-not-yet-connected URL if any.
func (m *Manager) WaitURL(ctx context.Context, id string) (string, error) {
	e, err := m.lookup(id)
	if err != nil {
		return "", err
	}
	for {
		e.mu.Lock()
		u, state, ch := e.url, e.state, e.changed
		e.mu.Unlock()
		if state == StateStopped {
			return "", invalid("tunnel is disabled")
		}
		if u != "" && state == StateRunning {
			return u, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return u, nil
		}
	}
}

func (m *Manager) Activity(id string, limit int) ([]Entry, error) {
	e, err := m.lookup(id)
	if err != nil {
		return nil, err
	}
	return e.recent.last(limit), nil
}

func (m *Manager) ProcessLog(id string, limit int) ([]string, error) {
	e, err := m.lookup(id)
	if err != nil {
		return nil, err
	}
	lines := e.plog.last(limit)
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return lines, nil
}

func (m *Manager) lookup(id string) (*entry, error) {
	m.mu.RLock()
	e, ok := m.entries[id]
	m.mu.RUnlock()
	if !ok {
		return nil, ErrNotFound
	}
	return e, nil
}

func (m *Manager) status(e *entry) Status {
	m.mu.RLock()
	st := Status{Spec: e.spec}
	m.mu.RUnlock()
	e.mu.Lock()
	st.State, st.URL, st.LocalPort, st.Restarts, st.LastError = e.state, e.url, e.localPort, e.restarts, e.lastErr
	if e.url != "" {
		t := e.urlSince
		st.URLSince = &t
	}
	e.mu.Unlock()
	st.Stats = e.stats.snapshot()
	return st
}

// start and stop must be called with e.op held.
func (m *Manager) start(e *entry, target string) {
	if e.cancel != nil {
		return
	}
	u, _ := url.Parse(target)
	alog, err := accesslog.Open(m.logDir(e.id), m.cfg.AccessLog)
	if err != nil {
		e.failed(err)
		return
	}
	px, err := newProxy(u, alog, e.stats, e.recent)
	if err != nil {
		alog.Close()
		e.failed(err)
		return
	}
	ctx, cancel := context.WithCancel(m.ctx)
	done := make(chan struct{})
	e.proxy, e.alog, e.cancel, e.done = px, alog, cancel, done
	e.update(func() { e.localPort = px.port() })

	args := []string{"tunnel", "--no-autoupdate", "--url", "http://127.0.0.1:" + strconv.Itoa(px.port())}
	r := &runner{bin: m.cfg.Cloudflared, args: append(args, m.cfg.CloudflaredArgs...), e: e}
	go func() {
		defer close(done)
		r.run(ctx)
	}()
}

func (m *Manager) stop(e *entry) {
	if e.cancel == nil {
		return
	}
	e.cancel()
	<-e.done
	e.proxy.close()
	e.alog.Close()
	e.proxy, e.alog, e.cancel, e.done = nil, nil, nil, nil
	e.update(func() {
		e.state, e.url, e.localPort = StateStopped, "", 0
	})
}

func (m *Manager) janitor() {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
		}
		m.mu.RLock()
		all := make([]*entry, 0, len(m.entries))
		for _, e := range m.entries {
			all = append(all, e)
		}
		m.mu.RUnlock()
		for _, e := range all {
			e.op.Lock()
			if e.alog != nil {
				if err := e.alog.RotateIfStale(); err != nil {
					slog.Warn("rotate access log", "id", e.id, "err", err)
				}
			}
			e.op.Unlock()
		}
		// Also covers logs of deleted tunnels, so their archives age out too.
		dirs, _ := os.ReadDir(filepath.Join(m.cfg.DataDir, "logs"))
		for _, d := range dirs {
			if d.IsDir() {
				accesslog.Cleanup(filepath.Join(m.cfg.DataDir, "logs", d.Name()), m.cfg.AccessLog)
			}
		}
	}
}

func (m *Manager) logDir(id string) string { return filepath.Join(m.cfg.DataDir, "logs", id) }

func (m *Manager) stateFile() string { return filepath.Join(m.cfg.DataDir, "tunnels.json") }

type stateFile struct {
	Tunnels []Spec `json:"tunnels"`
}

func (m *Manager) load() ([]Spec, error) {
	data, err := os.ReadFile(m.stateFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sf stateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", m.stateFile(), err)
	}
	return sf.Tunnels, nil
}

func (m *Manager) saveLocked() error {
	sf := stateFile{Tunnels: make([]Spec, 0, len(m.entries))}
	for _, e := range m.entries {
		sf.Tunnels = append(sf.Tunnels, e.spec)
	}
	sort.Slice(sf.Tunnels, func(i, j int) bool { return sf.Tunnels[i].CreatedAt.Before(sf.Tunnels[j].CreatedAt) })
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.stateFile() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, m.stateFile())
}

func (m *Manager) resolveTarget(port int, target string) (string, error) {
	target = strings.TrimSpace(target)
	if n, err := strconv.Atoi(target); err == nil && port == 0 {
		port, target = n, ""
	}
	switch {
	case port != 0 && target != "":
		return "", invalid("set either port or target, not both")
	case port != 0:
		if port < 1 || port > 65535 {
			return "", invalid("port must be between 1 and 65535")
		}
		return "http://" + net.JoinHostPort(m.cfg.DefaultHost, strconv.Itoa(port)), nil
	case target != "":
		if !strings.Contains(target, "://") {
			target = "http://" + target
		}
		u, err := url.Parse(target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "", invalid("target must look like http://host:port")
		}
		return u.String(), nil
	default:
		return "", invalid("port or target is required")
	}
}

func defaultName(port int, target string) string {
	if port != 0 {
		return "port-" + strconv.Itoa(port)
	}
	if u, err := url.Parse(target); err == nil {
		return u.Host
	}
	return target
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	return s
}

func (e *entry) update(f func()) {
	e.mu.Lock()
	f()
	close(e.changed)
	e.changed = make(chan struct{})
	e.mu.Unlock()
}

func (e *entry) launching(retry bool) {
	e.update(func() {
		e.state, e.url = StateStarting, ""
		if retry {
			e.restarts++
		}
	})
}

func (e *entry) gotURL(u string) {
	e.update(func() {
		if e.url != u {
			e.url, e.urlSince = u, time.Now().UTC()
			slog.Info("tunnel url", "id", e.id, "url", u)
		}
		if e.state == StateStarting {
			e.state = StateConnecting
		}
	})
}

func (e *entry) connected() {
	e.mu.Lock()
	already := e.state == StateRunning
	e.mu.Unlock()
	if !already {
		e.update(func() { e.state, e.lastErr = StateRunning, "" })
	}
}

func (e *entry) setLastErr(msg string) {
	e.mu.Lock()
	e.lastErr = msg
	e.mu.Unlock()
}

func (e *entry) exited(err error) {
	e.update(func() {
		e.state, e.url = StateRestarting, ""
		if err != nil {
			e.lastErr = "cloudflared exited: " + err.Error()
		}
	})
	slog.Warn("cloudflared exited, restarting", "id", e.id, "err", err)
}

func (e *entry) failed(err error) {
	e.update(func() { e.state, e.lastErr = StateError, err.Error() })
	slog.Error("start tunnel", "id", e.id, "err", err)
}

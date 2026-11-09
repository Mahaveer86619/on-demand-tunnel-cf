package tunnel

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pmaha/on-demand-tunnel-cf/internal/cloudflare"
)

var (
	ErrNoAccount       = errors.New("hostname tunnels need account mode: set CF_API_TOKEN and CF_DOMAIN")
	ErrAccountNotReady = errors.New("cloudflare account is not ready yet")
	ErrDNSConflict     = cloudflare.ErrRecordConflict
)

type AccountConfig struct {
	APIToken   string
	Domain     string
	TunnelName string
	APIBase    string
}

// account runs one remotely-managed named tunnel that carries every
// hostname tunnel; tunneld owns its ingress rules and the DNS CNAMEs.
type account struct {
	cfg  AccountConfig
	cf   *cloudflare.Client
	conn *entry // pseudo entry: connector process state and output

	mu        sync.RWMutex
	ready     bool
	zoneID    string
	accountID string
	tunnelID  string

	syncMu sync.Mutex
}

type AccountStatus struct {
	Enabled    bool   `json:"enabled"`
	Domain     string `json:"domain,omitempty"`
	TunnelName string `json:"tunnel_name,omitempty"`
	TunnelID   string `json:"tunnel_id,omitempty"`
	Ready      bool   `json:"ready"`
	State      State  `json:"state,omitempty"`
	Restarts   int    `json:"restarts"`
	LastError  string `json:"last_error,omitempty"`
}

func (a *account) cnameTarget() string { return a.tunnelID + ".cfargotunnel.com" }

func (a *account) ids() (ready bool, zoneID, accountID, tunnelID string) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ready, a.zoneID, a.accountID, a.tunnelID
}

func (m *Manager) runAccount() {
	a := m.acct
	backoff := 5 * time.Second
	for {
		err := m.setupAccount()
		if err == nil {
			break
		}
		a.conn.update(func() { a.conn.state, a.conn.lastErr = StateError, err.Error() })
		slog.Error("cloudflare account setup", "err", err)
		select {
		case <-m.ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Minute)
	}

	ready, _, accountID, tunnelID := a.ids()
	if !ready {
		return
	}
	ctx, cancel := context.WithTimeout(m.ctx, 20*time.Second)
	token, err := a.cf.TunnelToken(ctx, accountID, tunnelID)
	cancel()
	if err != nil {
		a.conn.failed(err)
		return
	}
	m.ensureAllDNS()
	if err := m.syncIngress(); err != nil {
		slog.Error("sync ingress", "err", err)
	}
	args := append([]string{"tunnel", "--no-autoupdate"}, m.cfg.CloudflaredArgs...)
	r := &runner{bin: m.cfg.Cloudflared, args: append(args, "run"), env: []string{"TUNNEL_TOKEN=" + token}, e: a.conn}
	r.run(m.ctx)
}

func (m *Manager) setupAccount() error {
	a := m.acct
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()
	zoneID, accountID, err := a.cf.Zone(ctx, a.cfg.Domain)
	if err != nil {
		return err
	}
	tunnelID, err := a.cf.EnsureTunnel(ctx, accountID, a.cfg.TunnelName)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.ready, a.zoneID, a.accountID, a.tunnelID = true, zoneID, accountID, tunnelID
	a.mu.Unlock()
	a.conn.update(func() { a.conn.lastErr = "" })
	slog.Info("cloudflare account ready", "domain", a.cfg.Domain, "tunnel", a.cfg.TunnelName, "tunnel_id", tunnelID)
	return nil
}

// syncIngress pushes one ingress rule per running hostname tunnel.
func (m *Manager) syncIngress() error {
	a := m.acct
	if a == nil {
		return nil
	}
	ready, _, accountID, tunnelID := a.ids()
	if !ready {
		return nil
	}
	a.syncMu.Lock()
	defer a.syncMu.Unlock()

	var rules []cloudflare.IngressRule
	m.mu.RLock()
	for _, e := range m.entries {
		if e.spec.Hostname == "" {
			continue
		}
		e.mu.Lock()
		port := e.localPort
		e.mu.Unlock()
		if port != 0 {
			rules = append(rules, cloudflare.IngressRule{Hostname: e.spec.Hostname, Service: "http://127.0.0.1:" + strconv.Itoa(port)})
		}
	}
	m.mu.RUnlock()
	sort.Slice(rules, func(i, j int) bool { return rules[i].Hostname < rules[j].Hostname })

	ctx, cancel := context.WithTimeout(m.ctx, 20*time.Second)
	defer cancel()
	if err := a.cf.PutIngress(ctx, accountID, tunnelID, rules); err != nil {
		a.conn.setLastErr("sync ingress: " + err.Error())
		return err
	}
	return nil
}

func (m *Manager) ensureAllDNS() {
	m.mu.RLock()
	var hosts []string
	for _, e := range m.entries {
		if e.spec.Hostname != "" {
			hosts = append(hosts, e.spec.Hostname)
		}
	}
	m.mu.RUnlock()
	for _, h := range hosts {
		if err := m.ensureDNS(h); err != nil {
			slog.Warn("ensure dns", "host", h, "err", err)
		}
	}
}

func (m *Manager) ensureDNS(host string) error {
	a := m.acct
	ready, zoneID, _, _ := a.ids()
	if !ready {
		return ErrAccountNotReady
	}
	ctx, cancel := context.WithTimeout(m.ctx, 20*time.Second)
	defer cancel()
	return a.cf.EnsureCNAME(ctx, zoneID, host, a.cnameTarget())
}

func (m *Manager) removeDNS(host string) {
	a := m.acct
	ready, zoneID, _, _ := a.ids()
	if !ready {
		return
	}
	ctx, cancel := context.WithTimeout(m.ctx, 20*time.Second)
	defer cancel()
	if err := a.cf.DeleteCNAME(ctx, zoneID, host, a.cnameTarget()); err != nil {
		slog.Warn("remove dns record", "host", host, "err", err)
	}
}

var hostLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// normalizeHostname accepts "app" or "app.example.com" and returns the FQDN
// inside the configured zone.
func (m *Manager) normalizeHostname(h string) (string, error) {
	if m.acct == nil {
		return "", ErrNoAccount
	}
	domain := m.acct.cfg.Domain
	h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
	switch {
	case h == "":
		return "", invalid("hostname is empty")
	case h == domain || strings.HasSuffix(h, "."+domain):
	case !strings.Contains(h, "."):
		h += "." + domain
	default:
		return "", invalid("hostname must be a single label or end in .%s", domain)
	}
	for _, l := range strings.Split(h, ".") {
		if !hostLabel.MatchString(l) {
			return "", invalid("invalid hostname %q", h)
		}
	}
	return h, nil
}

func (m *Manager) Account() AccountStatus {
	a := m.acct
	if a == nil {
		return AccountStatus{}
	}
	ready, _, _, tunnelID := a.ids()
	a.conn.mu.Lock()
	defer a.conn.mu.Unlock()
	return AccountStatus{
		Enabled:    true,
		Domain:     a.cfg.Domain,
		TunnelName: a.cfg.TunnelName,
		TunnelID:   tunnelID,
		Ready:      ready,
		State:      a.conn.state,
		Restarts:   a.conn.restarts,
		LastError:  a.conn.lastErr,
	}
}

func (m *Manager) AccountLog(limit int) ([]string, error) {
	if m.acct == nil {
		return nil, ErrNoAccount
	}
	return reverse(m.acct.conn.plog.last(limit)), nil
}

func reverse[T any](s []T) []T {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
	return s
}

package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pmaha/on-demand-tunnel-cf/internal/accesslog"
)

// fakeCF implements just enough of the Cloudflare API for account mode.
type fakeCF struct {
	mu      sync.Mutex
	tunnels map[string]string // name -> id
	records map[string]map[string]string
	ingress []map[string]string
	nextID  int
}

func newFakeCF() *fakeCF {
	return &fakeCF{tunnels: map[string]string{}, records: map[string]map[string]string{}}
}

func (f *fakeCF) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ok := func(v any) {
		json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "result": v})
	}
	p := r.URL.Path
	switch {
	case p == "/zones":
		ok([]map[string]any{{"id": "zone1", "account": map[string]string{"id": "acc1"}}})
	case p == "/accounts/acc1/cfd_tunnel" && r.Method == http.MethodGet:
		var out []map[string]string
		if id, found := f.tunnels[r.URL.Query().Get("name")]; found {
			out = append(out, map[string]string{"id": id})
		}
		ok(out)
	case p == "/accounts/acc1/cfd_tunnel" && r.Method == http.MethodPost:
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		f.tunnels[body["name"]] = "tun1"
		ok(map[string]string{"id": "tun1"})
	case p == "/accounts/acc1/cfd_tunnel/tun1/token":
		ok("fake-token")
	case p == "/accounts/acc1/cfd_tunnel/tun1/configurations":
		var body struct {
			Config struct {
				Ingress []map[string]string `json:"ingress"`
			} `json:"config"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.ingress = body.Config.Ingress
		ok(map[string]any{})
	case p == "/zones/zone1/dns_records" && r.Method == http.MethodGet:
		name := r.URL.Query().Get("name")
		var out []map[string]string
		for id, rec := range f.records {
			if rec["name"] == name {
				out = append(out, map[string]string{"id": id, "type": rec["type"], "content": rec["content"]})
			}
		}
		ok(out)
	case p == "/zones/zone1/dns_records" && r.Method == http.MethodPost:
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.nextID++
		id := "rec" + string(rune('0'+f.nextID))
		f.records[id] = map[string]string{"name": body["name"].(string), "type": body["type"].(string), "content": body["content"].(string)}
		ok(map[string]string{"id": id})
	case strings.HasPrefix(p, "/zones/zone1/dns_records/") && r.Method == http.MethodDelete:
		delete(f.records, strings.TrimPrefix(p, "/zones/zone1/dns_records/"))
		ok(map[string]string{})
	default:
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]any{{"code": 7003, "message": "no route " + p}}})
	}
}

func (f *fakeCF) snapshot() (records map[string]map[string]string, ingress []map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	records = map[string]map[string]string{}
	for k, v := range f.records {
		records[k] = v
	}
	return records, append([]map[string]string(nil), f.ingress...)
}

func TestHostnameTunnel(t *testing.T) {
	cf := newFakeCF()
	cf.records["mail"] = map[string]string{"name": "mail.example.com", "type": "A", "content": "1.2.3.4"}
	api := httptest.NewServer(cf)
	defer api.Close()

	t.Setenv("FAKE_CLOUDFLARED", "1")
	m, err := New(Config{
		DataDir:         t.TempDir(),
		Cloudflared:     testBinary(),
		DefaultHost:     "127.0.0.1",
		AccessLog:       accesslog.Options{MaxSize: 1 << 20},
		RecentRequests:  10,
		ProcessLogLines: 10,
		Account:         &AccountConfig{APIToken: "x", Domain: "example.com", TunnelName: "tunneld", APIBase: api.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	deadline := time.Now().Add(10 * time.Second)
	for !m.Account().Ready || m.Account().State != StateRunning {
		if time.Now().After(deadline) {
			t.Fatalf("account never ready: %+v", m.Account())
		}
		time.Sleep(50 * time.Millisecond)
	}

	if _, err := m.Create(Input{Port: 3000, Hostname: "mail"}); !errors.Is(err, ErrDNSConflict) {
		t.Fatalf("expected DNS conflict, got %v", err)
	}
	if _, err := m.Create(Input{Port: 3000, Hostname: "app.other.org"}); err == nil {
		t.Fatal("hostname outside the zone was accepted")
	}

	st, err := m.Create(Input{Port: 3000, Hostname: "Grafana"})
	if err != nil {
		t.Fatal(err)
	}
	if st.ID != "grafana" || st.Hostname != "grafana.example.com" || st.Mode != "hostname" {
		t.Fatalf("status = %+v", st)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if u, _ := m.WaitURL(ctx, "grafana"); u != "https://grafana.example.com" {
		t.Fatalf("url = %q", u)
	}

	records, ingress := cf.snapshot()
	var cname bool
	for _, r := range records {
		cname = cname || (r["name"] == "grafana.example.com" && r["content"] == "tun1.cfargotunnel.com")
	}
	if !cname {
		t.Fatalf("CNAME not created: %v", records)
	}
	if len(ingress) != 2 || ingress[0]["hostname"] != "grafana.example.com" || ingress[1]["service"] != "http_status:404" {
		t.Fatalf("ingress = %v", ingress)
	}

	off := false
	m.Update("grafana", Patch{Enabled: &off})
	if _, ingress = cf.snapshot(); len(ingress) != 1 {
		t.Fatalf("ingress after disable = %v", ingress)
	}

	if err := m.Delete("grafana", true); err != nil {
		t.Fatal(err)
	}
	records, _ = cf.snapshot()
	if len(records) != 1 {
		t.Fatalf("records after delete = %v (unrelated record must survive)", records)
	}
}

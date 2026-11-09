package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pmaha/on-demand-tunnel-cf/internal/cloudflare"
)

type report struct {
	fails, warns int
}

func (r *report) section(name string) { fmt.Printf("\n%s\n", name) }
func (r *report) ok(format string, a ...any) {
	fmt.Printf("  [ OK ] %s\n", fmt.Sprintf(format, a...))
}
func (r *report) info(format string, a ...any) {
	fmt.Printf("  [INFO] %s\n", fmt.Sprintf(format, a...))
}
func (r *report) warn(format string, a ...any) {
	r.warns++
	fmt.Printf("  [WARN] %s\n", fmt.Sprintf(format, a...))
}
func (r *report) fail(format string, a ...any) {
	r.fails++
	fmt.Printf("  [FAIL] %s\n", fmt.Sprintf(format, a...))
}
func (r *report) fix(lines ...string) {
	for i, l := range lines {
		prefix := "         "
		if i == 0 {
			prefix = "         fix: "
		}
		fmt.Println(prefix + l)
	}
}

func doctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	dir := fs.String("dir", ".", "folder that holds compose.yaml and .env")
	noApply := fs.Bool("no-apply", false, "only check; don't run docker compose up")
	build := fs.Bool("build", false, "rebuild the image while applying (after updating the code)")
	wait := fs.Duration("wait", 60*time.Second, "how long to wait for tunneld and the account connector")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	abs, _ := filepath.Abs(*dir)
	fmt.Printf("odt doctor  (%s)\n", abs)
	r := &report{}

	r.section(".env")
	env := checkEnv(r, abs)
	if env == nil {
		return summary(r)
	}

	r.section("Docker")
	if !*noApply {
		if !applyCompose(r, abs, *build) {
			return summary(r)
		}
	} else {
		r.info("skipped applying .env (--no-apply)")
	}

	port := env.get("PORT", "7171")
	base := "http://127.0.0.1:" + port
	token := env.get("ADMIN_TOKEN", "")
	r.section("tunneld")
	if !checkServer(r, base, token, *wait) {
		return summary(r)
	}

	r.section("Network (outbound to Cloudflare)")
	checkNetwork(r, env)

	if env.get("CF_API_TOKEN", "") == "" {
		r.section("Cloudflare account")
		r.info("off: tunnels use random *.trycloudflare.com URLs (no account needed)")
		r.info("to use your own domain run: odt help account")
		return summary(r)
	}
	r.section("Cloudflare account (" + env.get("CF_DOMAIN", "?") + ")")
	if checkAccount(r, env) {
		checkConnector(r, base, token, *wait)
	}
	return summary(r)
}

func summary(r *report) int {
	fmt.Println()
	switch {
	case r.fails > 0:
		fmt.Printf("%d problem(s), %d warning(s). Fix the [FAIL] items, edit .env if needed, then run `odt doctor` again.\n", r.fails, r.warns)
		return 1
	case r.warns > 0:
		fmt.Printf("Working, with %d warning(s).\n", r.warns)
	default:
		fmt.Println("All good.")
	}
	return 0
}

func checkEnv(r *report, dir string) *envFile {
	path := filepath.Join(dir, ".env")
	env, err := readEnv(path)
	if errors.Is(err, os.ErrNotExist) {
		r.fail("no .env in %s", dir)
		r.fix("copy .env.example to .env and edit it", "(quick tunnels work with the defaults as-is)")
		return nil
	}
	if err != nil {
		r.fail("read .env: %v", err)
		return nil
	}
	r.ok("read %s (%d settings)", path, len(env.values))
	for _, issue := range env.issues {
		r.warn("%s", issue)
	}
	for _, k := range env.order {
		if !knownKeys[k] {
			r.warn("unknown setting %s (typo?). See: odt help env", k)
		}
	}

	if p, err := strconv.Atoi(env.get("PORT", "7171")); err != nil || p < 1 || p > 65535 {
		r.fail("PORT=%q is not a valid port", env.values["PORT"])
	}
	bind := env.get("BIND_ADDR", "127.0.0.1")
	if net.ParseIP(bind) == nil {
		r.fail("BIND_ADDR=%q is not an IP address", bind)
	}
	switch {
	case bind != "127.0.0.1" && bind != "::1" && env.get("ADMIN_TOKEN", "") == "":
		r.fail("the admin API is reachable from your network (BIND_ADDR=%s) but ADMIN_TOKEN is empty", bind)
		r.fix("set ADMIN_TOKEN to a long random string, or BIND_ADDR=127.0.0.1")
	case bind == "127.0.0.1":
		r.ok("admin UI only reachable from this machine (BIND_ADDR=127.0.0.1)")
	default:
		r.ok("admin UI reachable on %s, protected by ADMIN_TOKEN", bind)
	}
	switch p := env.get("CF_PROTOCOL", ""); p {
	case "", "auto", "quic", "http2":
	default:
		r.fail("CF_PROTOCOL=%q must be empty, auto, quic or http2", p)
	}

	token, domain := env.get("CF_API_TOKEN", ""), env.get("CF_DOMAIN", "")
	switch {
	case token == "" && domain == "":
	case token == "":
		r.fail("CF_DOMAIN is set but CF_API_TOKEN is empty")
		r.fix("create a token: odt help account")
	case domain == "":
		r.fail("CF_API_TOKEN is set but CF_DOMAIN is empty")
		r.fix("set CF_DOMAIN to your root domain, e.g. CF_DOMAIN=example.com")
	case strings.Contains(domain, "://") || strings.Contains(domain, "/"):
		r.fail("CF_DOMAIN=%q should be a bare domain like example.com", domain)
	case strings.HasPrefix(domain, "www."):
		r.warn("CF_DOMAIN=%q starts with www.; use the root domain the zone was added as", domain)
	}
	return env
}

func applyCompose(r *report, dir string, build bool) bool {
	out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").CombinedOutput()
	if err != nil {
		r.fail("docker is not reachable: %s", firstLine(string(out), err))
		r.fix("start Docker (Docker Desktop on Windows/macOS, `sudo systemctl start docker` on Linux)")
		return false
	}
	r.ok("docker %s", strings.TrimSpace(string(out)))

	if _, err := os.Stat(filepath.Join(dir, "compose.yaml")); err != nil {
		r.warn("no compose.yaml in %s; skipping apply", dir)
		r.fix("run odt from the project folder or pass --dir, or restart tunneld yourself")
		return true
	}
	cmdArgs := []string{"compose", "up", "-d"}
	if build {
		cmdArgs = append(cmdArgs, "--build")
	}
	cmd := exec.Command("docker", cmdArgs...)
	cmd.Dir = dir
	out, err = cmd.CombinedOutput()
	if err != nil {
		r.fail("docker %s failed:\n%s", strings.Join(cmdArgs, " "), indent(string(out)))
		if strings.Contains(string(out), "ports are not available") {
			r.fix("another program already uses PORT; pick another PORT in .env")
		}
		return false
	}
	r.ok("applied .env (docker %s); the container is recreated only if something changed", strings.Join(cmdArgs, " "))
	return true
}

func checkServer(r *report, base, token string, wait time.Duration) bool {
	c := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(wait)
	for {
		res, err := c.Get(base + "/healthz")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			r.fail("tunneld is not answering at %s/healthz", base)
			r.fix("check `docker compose logs tunneld`")
			return false
		}
		time.Sleep(time.Second)
	}
	r.ok("healthy at %s", base)

	var list []json.RawMessage
	code, err := getJSON(base+"/api/tunnels", token, &list)
	switch {
	case err != nil:
		r.fail("GET /api/tunnels: %v", err)
		return false
	case code == http.StatusUnauthorized:
		r.fail("the server rejected ADMIN_TOKEN from .env")
		r.fix("the container may still run with an old token: run `odt doctor` without --no-apply")
		return false
	case code != http.StatusOK:
		r.fail("GET /api/tunnels returned HTTP %d", code)
		return false
	}
	r.ok("API reachable, %d tunnel(s) registered", len(list))
	return true
}

func checkNetwork(r *report, env *envFile) {
	targets := []string{"api.trycloudflare.com:443", "region1.v2.argotunnel.com:7844"}
	if env.get("CF_API_TOKEN", "") != "" {
		targets = append(targets, "api.cloudflare.com:443")
	}
	for _, t := range targets {
		conn, err := net.DialTimeout("tcp", t, 5*time.Second)
		if err != nil {
			r.warn("cannot reach %s: %v", t, err)
			if strings.HasSuffix(t, ":7844") {
				r.fix("a firewall blocks Cloudflare's tunnel port; outbound TCP/UDP 7844 must be allowed")
			}
			continue
		}
		conn.Close()
		r.ok("reachable: %s", t)
	}
}

func checkAccount(r *report, env *envFile) bool {
	cf := cloudflare.New(env.get("CF_API_TOKEN", ""), "")
	domain := strings.ToLower(env.get("CF_DOMAIN", ""))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if status, err := cf.VerifyToken(ctx); err != nil {
		r.fail("Cloudflare rejected CF_API_TOKEN: %v", err)
		r.fix("copy the token again (it's shown only once) or create a new one: odt help account")
		return false
	} else if status != "active" {
		r.fail("CF_API_TOKEN is %s", status)
		r.fix("create a new token: odt help account")
		return false
	}
	r.ok("API token is active")

	zone, err := cf.ZoneInfo(ctx, domain)
	if err != nil {
		r.fail("look up zone %s: %v", domain, err)
		r.fix("give the token  Zone > Zone > Read  for this zone")
		return false
	}
	if zone == nil {
		r.fail("%s is not visible to this token", domain)
		r.fix("1. add the domain in the Cloudflare dashboard (Add a domain, Free plan)",
			"2. make sure the token's Zone Resources include it",
			"details: odt help account")
		return false
	}
	switch zone.Status {
	case "active":
		r.ok("zone %s is active on Cloudflare (account %s)", zone.Name, zone.Account.Name)
	case "pending":
		r.fail("zone %s is still pending: its nameservers do not point to Cloudflare yet", zone.Name)
		lines := []string{"at your registrar (e.g. Hostinger: Domains > " + zone.Name + " > DNS / Nameservers > Change nameservers)", "replace the nameservers with:"}
		for _, ns := range zone.NameServers {
			lines = append(lines, "    "+ns)
		}
		if len(zone.OriginalNameServers) > 0 {
			lines = append(lines, "currently: "+strings.Join(zone.OriginalNameServers, ", "))
		}
		lines = append(lines, "turn off DNSSEC at the registrar first; propagation can take a few hours. See: odt help dns")
		r.fix(lines...)
	default:
		r.warn("zone %s status is %q", zone.Name, zone.Status)
	}

	tunnelErr := cf.Probe(ctx, "/accounts/"+zone.Account.ID+"/cfd_tunnel?per_page=1")
	if tunnelErr != nil {
		r.fail("token cannot access Cloudflare Tunnel: %v", tunnelErr)
		r.fix("add the permission  Account > Cloudflare Tunnel > Edit  to the token")
	} else {
		r.ok("token can access Cloudflare Tunnel")
	}
	dnsErr := cf.Probe(ctx, "/zones/"+zone.ID+"/dns_records?per_page=1")
	if dnsErr != nil {
		r.fail("token cannot access DNS records: %v", dnsErr)
		r.fix("add the permission  Zone > DNS > Edit  for " + zone.Name)
	} else {
		r.ok("token can access DNS records (Edit can only be confirmed by creating a hostname tunnel)")
	}
	return tunnelErr == nil && dnsErr == nil
}

func checkConnector(r *report, base, token string, wait time.Duration) {
	type account struct {
		Enabled    bool   `json:"enabled"`
		Ready      bool   `json:"ready"`
		State      string `json:"state"`
		TunnelName string `json:"tunnel_name"`
		TunnelID   string `json:"tunnel_id"`
		LastError  string `json:"last_error"`
	}
	var a account
	deadline := time.Now().Add(wait)
	for {
		code, err := getJSON(base+"/api/account", token, &a)
		if err != nil || code != http.StatusOK {
			r.fail("GET /api/account: HTTP %d %v", code, err)
			return
		}
		if !a.Enabled {
			r.fail("tunneld is running without account mode")
			r.fix("it was started with an older .env: run `odt doctor` without --no-apply")
			return
		}
		if a.Ready && a.State == "running" || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if a.Ready && a.State == "running" {
		r.ok("named tunnel %q (%s) is connected", a.TunnelName, a.TunnelID)
		r.info("add one with: POST /api/tunnels {\"name\":\"grafana\",\"port\":3000,\"hostname\":\"grafana\"}")
		return
	}
	r.fail("named tunnel is not connected (ready=%v, state=%s)", a.Ready, a.State)
	if a.LastError != "" {
		r.fix(a.LastError)
	}
	var lines []string
	if code, err := getJSON(base+"/api/account/cloudflared-log?limit=5", token, &lines); err == nil && code == http.StatusOK && len(lines) > 0 {
		fmt.Println("         last cloudflared output:")
		for _, l := range lines {
			fmt.Println("           " + l)
		}
	}
}

func getJSON(url, token string, out any) (int, error) {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK {
		return res.StatusCode, json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode, nil
}

func firstLine(s string, err error) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return err.Error()
	}
	l, _, _ := strings.Cut(s, "\n")
	return l
}

func indent(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > 12 {
		lines = lines[len(lines)-12:]
	}
	return "           " + strings.Join(lines, "\n           ")
}

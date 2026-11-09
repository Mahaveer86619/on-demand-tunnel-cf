// Package cloudflare is a minimal client for the few Cloudflare API calls
// needed to run a remotely-managed named tunnel on the user's own zone.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const apiBase = "https://api.cloudflare.com/client/v4"

// ErrRecordConflict means a DNS record we did not create already owns the hostname.
var ErrRecordConflict = errors.New("a different DNS record already exists for this hostname")

type Client struct {
	token string
	base  string
	http  *http.Client
}

// New returns a client for the public API; base overrides it (tests).
func New(token, base string) *Client {
	if base == "" {
		base = apiBase
	}
	return &Client{token: token, base: base, http: &http.Client{Timeout: 20 * time.Second}}
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type envelope struct {
	Success bool            `json:"success"`
	Errors  []apiError      `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var env envelope
	if err := json.NewDecoder(res.Body).Decode(&env); err != nil {
		return fmt.Errorf("cloudflare %s %s: HTTP %d", method, path, res.StatusCode)
	}
	if !env.Success {
		msgs := make([]string, 0, len(env.Errors))
		for _, e := range env.Errors {
			msgs = append(msgs, fmt.Sprintf("%d %s", e.Code, e.Message))
		}
		return fmt.Errorf("cloudflare %s %s: HTTP %d: %s", method, path, res.StatusCode, strings.Join(msgs, "; "))
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// Zone resolves a zone name (example.com) to its zone and account IDs.
func (c *Client) Zone(ctx context.Context, name string) (zoneID, accountID string, err error) {
	var zones []struct {
		ID      string `json:"id"`
		Account struct {
			ID string `json:"id"`
		} `json:"account"`
	}
	if err := c.do(ctx, http.MethodGet, "/zones?name="+url.QueryEscape(name), nil, &zones); err != nil {
		return "", "", err
	}
	if len(zones) == 0 {
		return "", "", fmt.Errorf("zone %q not found: add the domain to Cloudflare and give the token Zone:Read on it", name)
	}
	return zones[0].ID, zones[0].Account.ID, nil
}

// EnsureTunnel returns the ID of the named tunnel, creating it (with
// dashboard/API-managed config) if it does not exist yet.
func (c *Client) EnsureTunnel(ctx context.Context, accountID, name string) (string, error) {
	var found []struct {
		ID string `json:"id"`
	}
	q := "?is_deleted=false&name=" + url.QueryEscape(name)
	if err := c.do(ctx, http.MethodGet, "/accounts/"+accountID+"/cfd_tunnel"+q, nil, &found); err != nil {
		return "", err
	}
	if len(found) > 0 {
		return found[0].ID, nil
	}
	var created struct {
		ID string `json:"id"`
	}
	body := map[string]string{"name": name, "config_src": "cloudflare"}
	if err := c.do(ctx, http.MethodPost, "/accounts/"+accountID+"/cfd_tunnel", body, &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

func (c *Client) TunnelToken(ctx context.Context, accountID, tunnelID string) (string, error) {
	var token string
	err := c.do(ctx, http.MethodGet, "/accounts/"+accountID+"/cfd_tunnel/"+tunnelID+"/token", nil, &token)
	return token, err
}

type IngressRule struct {
	Hostname string `json:"hostname,omitempty"`
	Service  string `json:"service"`
}

// PutIngress replaces the tunnel's ingress rules; a 404 catch-all is appended.
func (c *Client) PutIngress(ctx context.Context, accountID, tunnelID string, rules []IngressRule) error {
	ingress := append(append([]IngressRule{}, rules...), IngressRule{Service: "http_status:404"})
	body := map[string]any{"config": map[string]any{"ingress": ingress}}
	return c.do(ctx, http.MethodPut, "/accounts/"+accountID+"/cfd_tunnel/"+tunnelID+"/configurations", body, nil)
}

type dnsRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Content string `json:"content"`
}

func (c *Client) records(ctx context.Context, zoneID, host string) ([]dnsRecord, error) {
	var recs []dnsRecord
	err := c.do(ctx, http.MethodGet, "/zones/"+zoneID+"/dns_records?name="+url.QueryEscape(host), nil, &recs)
	return recs, err
}

// EnsureCNAME points host at target (proxied). It refuses to touch any
// existing record that is not already that exact CNAME.
func (c *Client) EnsureCNAME(ctx context.Context, zoneID, host, target string) error {
	recs, err := c.records(ctx, zoneID, host)
	if err != nil {
		return err
	}
	for _, r := range recs {
		if r.Type == "CNAME" && strings.EqualFold(r.Content, target) {
			return nil
		}
	}
	if len(recs) > 0 {
		return fmt.Errorf("%w: %s (%s %s)", ErrRecordConflict, host, recs[0].Type, recs[0].Content)
	}
	body := map[string]any{
		"type":    "CNAME",
		"name":    host,
		"content": target,
		"proxied": true,
		"comment": "managed by tunneld",
	}
	return c.do(ctx, http.MethodPost, "/zones/"+zoneID+"/dns_records", body, nil)
}

// DeleteCNAME removes host's CNAME only if it still points at target.
func (c *Client) DeleteCNAME(ctx context.Context, zoneID, host, target string) error {
	recs, err := c.records(ctx, zoneID, host)
	if err != nil {
		return err
	}
	for _, r := range recs {
		if r.Type == "CNAME" && strings.EqualFold(r.Content, target) {
			return c.do(ctx, http.MethodDelete, "/zones/"+zoneID+"/dns_records/"+r.ID, nil, nil)
		}
	}
	return nil
}

type ZoneInfo struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	Status              string   `json:"status"`
	NameServers         []string `json:"name_servers"`
	OriginalNameServers []string `json:"original_name_servers"`
	OriginalRegistrar   string   `json:"original_registrar"`
	Account             struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"account"`
}

// ZoneInfo returns the zone with its activation status and assigned nameservers.
func (c *Client) ZoneInfo(ctx context.Context, name string) (*ZoneInfo, error) {
	var zones []ZoneInfo
	if err := c.do(ctx, http.MethodGet, "/zones?name="+url.QueryEscape(name), nil, &zones); err != nil {
		return nil, err
	}
	if len(zones) == 0 {
		return nil, nil
	}
	return &zones[0], nil
}

// VerifyToken reports the token's status ("active", "disabled", "expired").
func (c *Client) VerifyToken(ctx context.Context) (string, error) {
	var res struct {
		Status string `json:"status"`
	}
	err := c.do(ctx, http.MethodGet, "/user/tokens/verify", nil, &res)
	return res.Status, err
}

// Probe performs a read-only GET, used to check token permissions.
func (c *Client) Probe(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodGet, path, nil, nil)
}

// Package api exposes the tunnel manager over a small JSON HTTP API and
// serves the embedded single-page UI.
package api

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pmaha/on-demand-tunnel-cf/internal/tunnel"
)

//go:embed ui.html
var ui []byte

const maxWait = 60 * time.Second

type server struct {
	m     *tunnel.Manager
	token string
}

func Handler(m *tunnel.Manager, token string) http.Handler {
	s := &server{m: m, token: token}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.ui)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.Handle("GET /api/tunnels", s.auth(s.list))
	mux.Handle("POST /api/tunnels", s.auth(s.create))
	mux.Handle("GET /api/tunnels/{id}", s.auth(s.get))
	mux.Handle("PATCH /api/tunnels/{id}", s.auth(s.update))
	mux.Handle("DELETE /api/tunnels/{id}", s.auth(s.delete))
	mux.Handle("POST /api/tunnels/{id}/restart", s.auth(s.restart))
	mux.Handle("GET /api/tunnels/{id}/url", s.auth(s.url))
	mux.Handle("GET /api/tunnels/{id}/activity", s.auth(s.activity))
	mux.Handle("GET /api/tunnels/{id}/cloudflared-log", s.auth(s.processLog))
	mux.Handle("GET /api/account", s.auth(s.account))
	mux.Handle("GET /api/account/cloudflared-log", s.auth(s.accountLog))
	return mux
}

func (s *server) account(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.m.Account())
}

func (s *server) accountLog(w http.ResponseWriter, r *http.Request) {
	lines, err := s.m.AccountLog(limit(r, 200))
	reply(w, http.StatusOK, lines, err)
}

func (s *server) auth(h http.HandlerFunc) http.Handler {
	if s.token == "" {
		return h
	}
	want := []byte("Bearer " + s.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			writeJSON(w, http.StatusUnauthorized, errBody("unauthorized"))
			return
		}
		h(w, r)
	})
}

func (s *server) ui(w http.ResponseWriter, _ *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'")
	w.Write(ui)
}

func (s *server) list(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.m.List())
}

func (s *server) get(w http.ResponseWriter, r *http.Request) {
	st, err := s.m.Get(r.PathValue("id"))
	reply(w, http.StatusOK, st, err)
}

func (s *server) create(w http.ResponseWriter, r *http.Request) {
	var in tunnel.Input
	if !decode(w, r, &in) {
		return
	}
	st, err := s.m.Create(in)
	reply(w, http.StatusCreated, st, err)
}

func (s *server) update(w http.ResponseWriter, r *http.Request) {
	var p tunnel.Patch
	if !decode(w, r, &p) {
		return
	}
	st, err := s.m.Update(r.PathValue("id"), p)
	reply(w, http.StatusOK, st, err)
}

func (s *server) delete(w http.ResponseWriter, r *http.Request) {
	purge, _ := strconv.ParseBool(r.URL.Query().Get("purge_logs"))
	if err := s.m.Delete(r.PathValue("id"), purge); err != nil {
		reply(w, 0, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) restart(w http.ResponseWriter, r *http.Request) {
	st, err := s.m.Restart(r.PathValue("id"))
	reply(w, http.StatusOK, st, err)
}

// url returns the current public URL; ?wait=30s blocks until the tunnel is
// connected and ?format=text replies with the bare URL for shell scripts.
func (s *server) url(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	q := r.URL.Query()
	var u string
	if wait := parseWait(q.Get("wait")); wait > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), wait)
		defer cancel()
		var err error
		if u, err = s.m.WaitURL(ctx, id); err != nil {
			reply(w, 0, nil, err)
			return
		}
	} else {
		st, err := s.m.Get(id)
		if err != nil {
			reply(w, 0, nil, err)
			return
		}
		u = st.URL
	}
	if u == "" {
		writeJSON(w, http.StatusServiceUnavailable, errBody("url not assigned yet"))
		return
	}
	if q.Get("format") == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(u + "\n"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": u})
}

func (s *server) activity(w http.ResponseWriter, r *http.Request) {
	entries, err := s.m.Activity(r.PathValue("id"), limit(r, 100))
	reply(w, http.StatusOK, entries, err)
}

func (s *server) processLog(w http.ResponseWriter, r *http.Request) {
	lines, err := s.m.ProcessLog(r.PathValue("id"), limit(r, 200))
	reply(w, http.StatusOK, lines, err)
}

func parseWait(v string) time.Duration {
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0
		}
		d = time.Duration(n) * time.Second
	}
	return min(d, maxWait)
}

func limit(r *http.Request, def int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		return n
	}
	return def
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON: "+strings.TrimPrefix(err.Error(), "json: ")))
		return false
	}
	return true
}

func reply(w http.ResponseWriter, code int, v any, err error) {
	var verr *tunnel.ValidationError
	switch {
	case err == nil:
		writeJSON(w, code, v)
	case errors.As(err, &verr):
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
	case errors.Is(err, tunnel.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errBody(err.Error()))
	case errors.Is(err, tunnel.ErrExists), errors.Is(err, tunnel.ErrDNSConflict):
		writeJSON(w, http.StatusConflict, errBody(err.Error()))
	case errors.Is(err, tunnel.ErrNoAccount):
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
	case errors.Is(err, tunnel.ErrAccountNotReady):
		writeJSON(w, http.StatusServiceUnavailable, errBody(err.Error()))
	default:
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
	}
}

func errBody(msg string) map[string]string { return map[string]string{"error": msg} }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

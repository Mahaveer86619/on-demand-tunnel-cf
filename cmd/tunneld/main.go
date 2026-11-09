package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pmaha/on-demand-tunnel-cf/internal/accesslog"
	"github.com/pmaha/on-demand-tunnel-cf/internal/api"
	"github.com/pmaha/on-demand-tunnel-cf/internal/tunnel"
)

func main() {
	addr := env("LISTEN_ADDR", ":7171")
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(addr))
	}

	var level slog.Level
	level.UnmarshalText([]byte(env("LOG_LEVEL", "info")))
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	cfg := tunnel.Config{
		DataDir:         env("DATA_DIR", "./data"),
		Cloudflared:     env("CLOUDFLARED_BIN", "cloudflared"),
		CloudflaredArgs: strings.Fields(env("CF_EXTRA_ARGS", "")),
		DefaultHost:     env("DEFAULT_TARGET_HOST", "127.0.0.1"),
		AccessLog: accesslog.Options{
			MaxSize:     int64(max(envInt("LOG_MAX_SIZE_MB", 10), 1)) << 20,
			MaxAge:      time.Duration(envInt("LOG_RETENTION_DAYS", 14)) * 24 * time.Hour,
			MaxArchives: envInt("LOG_MAX_ARCHIVES", 50),
		},
		RecentRequests:  max(envInt("RECENT_REQUESTS", 200), 1),
		ProcessLogLines: 200,
	}
	if p := env("CF_PROTOCOL", ""); p != "" {
		cfg.CloudflaredArgs = append([]string{"--protocol", p}, cfg.CloudflaredArgs...)
	}
	if token := env("CF_API_TOKEN", ""); token != "" {
		domain := strings.ToLower(env("CF_DOMAIN", ""))
		if domain == "" {
			slog.Error("CF_API_TOKEN is set but CF_DOMAIN is empty")
			os.Exit(1)
		}
		cfg.Account = &tunnel.AccountConfig{APIToken: token, Domain: domain, TunnelName: env("CF_TUNNEL_NAME", "tunneld")}
	}
	if _, err := exec.LookPath(cfg.Cloudflared); err != nil {
		slog.Warn("cloudflared not found; tunnels will keep retrying", "bin", cfg.Cloudflared)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	m, err := tunnel.New(cfg)
	if err != nil {
		slog.Error("load tunnels", "err", err)
		os.Exit(1)
	}
	token := os.Getenv("ADMIN_TOKEN")
	srv := &http.Server{
		Addr:              addr,
		Handler:           api.Handler(m, token),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server", "err", err)
			stop()
		}
	}()
	slog.Info("tunneld ready", "addr", addr, "data", cfg.DataDir, "auth", token != "")

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	m.Close()
}

// healthcheck exists because the distroless image has no curl or wget.
func healthcheck(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		port = "7171"
	}
	c := &http.Client{Timeout: 3 * time.Second}
	res, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(env(key, "")); err == nil && n >= 0 {
		return n
	}
	return def
}

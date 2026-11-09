# Architecture

```
internet ──► Cloudflare edge ──(outbound QUIC/HTTP2)──► cloudflared ──► 127.0.0.1:<ephemeral>
                                                                         tunneld proxy (per tunnel)
                                                                           │  counts, logs
                                                                           ▼
                                                                         target (host port / LAN service)
```

## Per-tunnel pieces

Each registered tunnel gets:

1. **A logging reverse proxy** (`internal/tunnel/proxy.go`) on `127.0.0.1:0`. cloudflared only knows this
   address, so every request is measured before it reaches the target. The proxy uses `httputil.ReverseProxy`
   with a shared transport and pooled 32 KB copy buffers. WebSocket upgrades pass through.
   The target is held in an atomic pointer, so `PATCH`ing the target never restarts cloudflared and the public URL stays the same.
2. **A cloudflared process** (`internal/tunnel/runner.go`) running `cloudflared tunnel --no-autoupdate --url http://127.0.0.1:<port>`.
   Its stderr is scanned for the `*.trycloudflare.com` URL and for `Registered tunnel connection`.
   The state moves `starting → connecting → running`. If cloudflared exits, it restarts with exponential backoff (2 s doubling to 60 s, reset after a minute of uptime).
   Cloudflare rate-limits quick-tunnel creation, so it never restarts in a tight loop. A restart gives a new URL.
3. **An access log writer** (`internal/accesslog`).

State (`tunnels.json`) is written atomically (temp file + rename) on every change. Enabled tunnels are restarted when the server boots. Their URLs change, because that's how quick tunnels work.

## Access logs

`DATA_DIR/logs/<id>/access.log`, one line per request:

```
2026-11-09T06:26:00.123Z 203.0.113.9 IN "GET /ping?x=1 HTTP/1.1" 200 1234 12.3ms "referer" "user-agent"
```

The client IP comes from `Cf-Connecting-Ip` and the country from `Cf-Ipcountry`. Lines are built into pooled
buffers without allocating and written through a 32 KB `bufio.Writer` that flushes once per second.

- **Rotation:** when the file would exceed `LOG_MAX_SIZE_MB`, or on the first write of a new UTC day. An hourly janitor also rotates idle files that are left over from an earlier day.
- **Archiving:** the rotated file moves to `archive/access-<UTC stamp>.log` and is gzipped in the background. Files left uncompressed after a crash are compressed on the next start.
- **Retention:** archives older than `LOG_RETENTION_DAYS` are deleted, and only the newest `LOG_MAX_ARCHIVES` are kept. Retention runs after each rotation and every hour, and it also covers logs of deleted tunnels. `DELETE ...?purge_logs=true` removes a tunnel's logs right away.

Live stats (counters by status class, bytes, active connections, last request) and the last
`RECENT_REQUESTS` entries are kept in memory and reset when the server restarts. The log files are the durable record.

## Network and host footprint

The container is designed to leave the host's network untouched:

- **No new Docker network.** `network_mode: bridge` uses the existing docker0 bridge. Compose doesn't create a project network, which could otherwise take a subnet that clashes with your LAN.
- **Outbound only.** cloudflared dials Cloudflare on 7844 (QUIC/UDP, or TCP with `CF_PROTOCOL=http2`). Nothing on the host is opened to the internet. The only published port is the admin UI, and it's bound to `127.0.0.1` by default.
- **No host changes.** There's no `privileged`, no `NET_ADMIN`, no sysctls, and no host networking. cloudflared may log a warning that it couldn't raise UDP buffer sizes. That warning is harmless, and the setting is deliberately left alone.
- **Bounded resources.** The compose file caps the container at 0.5 CPU, 256 MB RAM and 128 PIDs, sets `GOMEMLIMIT=48MiB` for tunneld, and rotates Docker's own json-file logs (2 × 5 MB).
- **Hardened.** The root filesystem is read-only, all capabilities are dropped, `no-new-privileges` is set, and the process runs as the distroless `nonroot` user (65532). There's no shell in the image, so `tunneld healthcheck` is the healthcheck.

Each cloudflared process uses about 20–40 MB RSS. tunneld itself idles at a few MB.

## Limits of quick tunnels

These come from Cloudflare, not from this project:
- The URL is random and changes on every cloudflared restart.
- No uptime guarantee.
- 200 concurrent in-flight requests per tunnel.
- Server-Sent Events are not supported.
- Intended for testing and development.

For stable hostnames, see [cloudflare-free-options.md](cloudflare-free-options.md).

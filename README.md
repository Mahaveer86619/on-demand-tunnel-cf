# on-demand-tunnel-cf

A small Go server (`tunneld`) that publishes local ports to the internet using free
[Cloudflare quick tunnels](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/do-more-with-tunnels/trycloudflare/).
You don't need a Cloudflare account. You register a port, get a `https://<random>.trycloudflare.com` URL,
and every request through it lands in a rotated, gzip-archived access log.

- Register, edit, disable, delete, and restart forwards through the JSON API or the built-in web UI
- Fetch the current URL, or wait for a new one: `GET /api/tunnels/{id}/url?wait=30s&format=text`
- See live activity per tunnel: request counts, status classes, bytes, active connections, and the last 200 requests
- Access logs rotate by size or UTC day, archives are gzipped, and old archives are deleted automatically
- Changing a tunnel's target keeps its public URL
- The image holds only the static `tunneld` binary and `cloudflared` on distroless (no shell). It runs as a non-root user with a read-only root filesystem and no capabilities.

## Quick start (Docker)

```sh
docker compose up -d --build
# UI: http://127.0.0.1:7171
curl -s -XPOST localhost:7171/api/tunnels -d '{"name":"grafana","port":3000}'
curl -s "localhost:7171/api/tunnels/grafana/url?wait=30s&format=text"
```

`port` means "this port on the Docker host" (`host.docker.internal`). Any LAN service works through `target`:
`{"name":"ha","target":"192.168.1.20:8123"}`.

> On Linux the host service must listen on `0.0.0.0` or the docker0 address, not only `127.0.0.1`.
> If you use ufw/firewalld, allow traffic from the docker0 bridge to that port.

To manage the server from other machines on your LAN, set `BIND_ADDR=0.0.0.0` **and** `ADMIN_TOKEN` in `.env`
(see `.env.example`). Without a token, anyone who can reach the admin port can publish any service on your network.

## Run without Docker

```sh
go build -o tunneld ./cmd/tunneld   # needs cloudflared on PATH
./tunneld
```

## Configuration

| Env var | Default | Meaning |
|---|---|---|
| `LISTEN_ADDR` | `:7171` | Admin API/UI address |
| `DATA_DIR` | `./data` (`/data` in image) | `tunnels.json` + `logs/` |
| `ADMIN_TOKEN` | empty | If set, API requires `Authorization: Bearer <token>` |
| `DEFAULT_TARGET_HOST` | `127.0.0.1` (`host.docker.internal` in image) | Host used when you register a bare port |
| `CLOUDFLARED_BIN` | `cloudflared` | Path to cloudflared |
| `CF_PROTOCOL` | auto | `http2` forces TCP/443 if UDP (QUIC) is blocked or flaky |
| `CF_EXTRA_ARGS` | empty | Extra `cloudflared tunnel` flags |
| `LOG_MAX_SIZE_MB` | `10` | Rotate the active access log at this size |
| `LOG_RETENTION_DAYS` | `14` | Delete archives older than this (0 = keep) |
| `LOG_MAX_ARCHIVES` | `50` | Max archives kept per tunnel (0 = unlimited) |
| `RECENT_REQUESTS` | `200` | In-memory activity entries per tunnel |
| `LOG_LEVEL` | `info` | `debug` shows upstream errors |

More detail: [docs/](docs/README.md).

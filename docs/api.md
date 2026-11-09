# API

All endpoints are JSON. If `ADMIN_TOKEN` is set, send `Authorization: Bearer <token>`.
`/` (UI) and `/healthz` are always open.

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/tunnels` | List with state, URL and stats |
| `POST` | `/api/tunnels` | `{"name":"grafana","port":3000}` or `{"name":"ha","target":"192.168.1.20:8123"}`; optional `"enabled":false`. Returns 201, 400, or 409 if the name is taken |
| `GET` | `/api/tunnels/{id}` | One tunnel |
| `PATCH` | `/api/tunnels/{id}` | Any of `name`, `port`/`target`, `enabled`. Retargeting keeps the URL |
| `DELETE` | `/api/tunnels/{id}?purge_logs=true` | 204. Logs are kept unless you purge them |
| `POST` | `/api/tunnels/{id}/restart` | Restart cloudflared to get a new URL |
| `GET` | `/api/tunnels/{id}/url` | `{"url":...}`. `?wait=30s` (max 60 s) blocks until connected. `?format=text` returns the bare URL. 503 if no URL yet |
| `GET` | `/api/tunnels/{id}/activity?limit=100` | Recent requests, newest first |
| `GET` | `/api/tunnels/{id}/cloudflared-log?limit=200` | Recent cloudflared output, oldest first |

The `id` is the slugified name (`"My App"` → `my-app`) and never changes. `name` can be edited.

Tunnel object:

```json
{
  "id": "grafana", "name": "grafana", "target": "http://host.docker.internal:3000",
  "enabled": true, "created_at": "...", "updated_at": "...",
  "state": "running",
  "url": "https://words-words-words.trycloudflare.com", "url_since": "...",
  "local_port": 41234, "restarts": 0, "last_error": "",
  "stats": {"since": "...", "requests": 42, "bytes_out": 123456,
            "status": {"2xx": 40, "4xx": 2}, "upstream_errors": 0, "active": 1,
            "last_request": "..."}
}
```

`state` is one of `stopped`, `starting`, `connecting` (URL assigned, edge not registered yet), `running`, `restarting`, or `error`.

Shell example:

```sh
URL=$(curl -sf "localhost:7070/api/tunnels/grafana/url?wait=30s&format=text")
```

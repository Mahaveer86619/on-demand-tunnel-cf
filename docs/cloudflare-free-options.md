# Free Cloudflare options

Status as of late 2026. Check current limits on Cloudflare's pricing pages.

## No account

| Service | What you get | Catch |
|---|---|---|
| Quick tunnels (`trycloudflare.com`) | What this project uses: instant public HTTPS URL | Random URL that changes on restart, 200 concurrent requests, no SSE, no SLA, meant for testing |

## Free account, no domain

| Service | Useful for |
|---|---|
| **Workers** (free: 100k requests/day) on `*.workers.dev` | A **stable URL** that redirects or proxies to the current trycloudflare URL. tunneld would push each new URL to the Worker |
| **Workers KV** (free tier) | Where that Worker stores the `name → current URL` map |
| **R2** (10 GB storage free, no egress fees) | Off-box copies of gzipped access-log archives |
| **Zero Trust** (free up to 50 users) | WARP client + private networks: reach home services from your devices **without** making them public |

## Free account + a domain on Cloudflare

The domain itself costs money (about the registry price through Cloudflare Registrar). Everything below is free.

| Service | Why it beats quick tunnels |
|---|---|
| **Named Cloudflare Tunnels** | Stable hostnames such as `grafana.example.com`. No request cap like quick tunnels, SSE works, unlimited tunnels, managed with a token. Ingress rules are created via API, so this project could switch from `--url` to named tunnels with almost the same UX |
| **Cloudflare Access** (Zero Trust free) | Put an email-OTP / GitHub / Google login in front of any hostname with no code changes. The best fit for exposing admin UIs |
| **DNS, CDN cache, WAF basics, DDoS protection** | Included on the free plan |

## Recommended path

1. Keep quick tunnels for throwaway sharing. That's this project as it is today.
2. For a stable URL with no domain, add an optional Worker + KV "stable link" that tunneld updates whenever a URL changes.
3. With a domain, add a "named tunnel" mode (Cloudflare API token + zone) and protect sensitive hostnames with Access. It stays free.

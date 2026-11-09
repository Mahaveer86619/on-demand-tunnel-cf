package main

import "strings"

func helpText(topic string) string {
	switch strings.ToLower(topic) {
	case "account":
		return helpAccount
	case "dns", "hostinger", "nameservers":
		return helpDNS
	case "env":
		return helpEnv
	case "":
		return helpMain
	default:
		return "odt: no help topic " + topic + "\n\n" + helpMain
	}
}

const helpMain = `odt - helper for tunneld (on-demand Cloudflare tunnels)

Usage:
  odt doctor [flags]   re-read .env, apply it (docker compose up -d) and check everything
  odt help [topic]     show this help or a topic

Doctor flags:
  --dir PATH      folder with compose.yaml and .env (default: current folder)
  --no-apply      only check, don't touch the container
  --build         rebuild the image while applying (after updating the code)
  --wait 60s      how long to wait for tunneld and the Cloudflare connector

Help topics:
  odt help account   use your own domain (stable https://app.yourdomain.com URLs)
  odt help dns       move a domain from another registrar's DNS (e.g. Hostinger) to Cloudflare
  odt help env       every .env setting

The workflow is always: edit .env, run "odt doctor", follow any [FAIL] fix lines, repeat.

Two modes:
  quick     default, no account. Each tunnel gets a random https://<words>.trycloudflare.com URL
            that changes when the tunnel restarts.
  account   set CF_API_TOKEN + CF_DOMAIN. Tunnels created with "hostname" get a stable
            https://<name>.<your domain>. Quick tunnels keep working alongside.
`

const helpAccount = `Using your own domain (account mode)

Everything here is free on Cloudflare; you only pay your registrar for the domain.

1. Put the domain's DNS on Cloudflare
   Cloudflare can only route app.yourdomain.com if it runs the domain's DNS.
   Add the domain on dash.cloudflare.com (Add a domain, Free plan), then point the
   nameservers at Cloudflare. If the domain is registered elsewhere (Hostinger,
   GoDaddy...), see:  odt help dns
   The domain stays registered where it is; only DNS moves.

2. Create an API token
   dash.cloudflare.com > My Profile > API Tokens > Create Token > Create Custom Token
     Permissions:
       Account | Cloudflare Tunnel | Edit
       Zone    | DNS               | Edit
       Zone    | Zone              | Read
     Account Resources:  Include > your account
     Zone Resources:     Include > Specific zone > yourdomain.com
   Create it and copy the token (Cloudflare shows it only once).

3. Edit .env
     CF_API_TOKEN=<the token>
     CF_DOMAIN=yourdomain.com
     CF_TUNNEL_NAME=tunneld        (optional; one named tunnel carries all hostnames)

4. Run:  odt doctor
   It restarts tunneld with the new settings and checks the token, the zone and the
   connector. tunneld then creates the named tunnel by itself.

5. Create a hostname tunnel
     POST /api/tunnels  {"name":"grafana","port":3000,"hostname":"grafana"}
   tunneld creates the DNS record grafana.yourdomain.com and routes it.
   It never overwrites an existing DNS record; you get HTTP 409 instead.
   Deleting the tunnel removes only the record tunneld created.

Optional: protect a hostname with a login page using Cloudflare Zero Trust > Access >
Applications (free up to 50 users). No changes in tunneld are needed.
`

const helpDNS = `Moving DNS to Cloudflare when the domain is registered elsewhere (e.g. Hostinger)

Cloudflare tunnels with your own hostname need Cloudflare to answer DNS for the
domain ("full setup"). The free plan doesn't support the CNAME-only partial setup, and
subdomain-only zones aren't free either, so the nameservers have to move. The registration,
renewal and billing stay at your registrar.

1. dash.cloudflare.com > Add a domain > enter yourdomain.com > Free plan.
   Cloudflare scans and imports your existing DNS records. Check that everything you use
   is there before continuing: the website's A/CNAME records, MX + TXT records for email
   (Hostinger mail, Google Workspace...), and any verification TXT records.
2. Cloudflare shows two nameservers, e.g.  ada.ns.cloudflare.com  bob.ns.cloudflare.com
3. Hostinger (hPanel):
     Domains > yourdomain.com > DNS / Nameservers
     If DNSSEC is on, turn it off first (you can re-enable it in Cloudflare later).
     Change nameservers > "Use custom nameservers" > enter the two Cloudflare ones > Save.
4. Wait until Cloudflare marks the domain Active (minutes to a few hours, max ~24h).
   "odt doctor" shows the zone status and the exact nameservers to use while it's pending.

Website hosted on Hostinger? It keeps working: the imported A records still point to
Hostinger. Turn the orange cloud (proxy) off on a record if the host's SSL complains.
`

const helpEnv = `.env settings (edit the file, then run "odt doctor")

  ADMIN_TOKEN         API/UI password (Bearer token). Required if BIND_ADDR isn't 127.0.0.1
  BIND_ADDR           127.0.0.1 = admin UI only on this machine; 0.0.0.0 = whole LAN
  PORT                admin UI/API port on the host (default 7171)

  CF_API_TOKEN        Cloudflare API token, enables account mode (odt help account)
  CF_DOMAIN           your domain on Cloudflare, e.g. example.com
  CF_TUNNEL_NAME      named tunnel to create/use (default tunneld)

  CF_PROTOCOL         empty/auto, quic or http2 (use http2 if UDP is blocked)
  CF_EXTRA_ARGS       extra flags passed to "cloudflared tunnel"

  LOG_MAX_SIZE_MB     rotate access logs at this size (default 10)
  LOG_RETENTION_DAYS  delete archived logs older than this (default 14, 0 = keep)
  LOG_MAX_ARCHIVES    archived logs kept per tunnel (default 50, 0 = unlimited)
  LOG_LEVEL           debug, info, warn, error
  RECENT_REQUESTS     requests kept in memory per tunnel for the activity view (default 200)
`

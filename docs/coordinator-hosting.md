# Host the browser coordinator

The browser app in `bonjou-web` needs this separate, continuously reachable
coordinator. A static website deployment alone cannot create rooms or discover
devices. The coordinator serves only `GET /healthz` and the `GET /ws` WebSocket:
it groups candidates by their source address and forwards encrypted WebRTC
signaling. Files, messages, profiles, and encryption keys remain on the browsers.

Use one instance. Candidate and room state lives in memory, so two independent
instances split the discovery group and rooms. A restart ends those sessions;
clients reconnect and create or join a new session. No database or persistent
payload disk is required.

## Build and run the portable image

From the `bonjou-cli` repository root:

```sh
docker build -f packaging/relay/Dockerfile -t bonjou-coordinator .
docker run --rm --name bonjou-coordinator \
  -p 127.0.0.1:46330:10000 \
  -e BONJOU_RELAY_ORIGINS=http://127.0.0.1:4173 \
  bonjou-coordinator
```

In another terminal:

```sh
curl --fail http://127.0.0.1:46330/healthz
```

The image builds only the existing `cmd/bonjou-relay` application, runs it as an
unprivileged user, and supplies a health check. Its allowlisted Docker build
context excludes local configuration, credentials, Git history, and artifacts.
It uses the Go version declared in `go.mod`; review that version when updating
the image. Runtime logs are ephemeral and may contain source IP addresses. Keep
them out of public artifacts.

| Variable | Default | Purpose |
| --- | --- | --- |
| `PORT` | `10000` | HTTP and WebSocket listener on `0.0.0.0`. |
| `BONJOU_RELAY_ORIGINS` | `https://bonjou.vercel.app` | Exact comma-separated browser origins permitted to connect. |
| `BONJOU_RELAY_TRUST_PROXY` | `false` | Enable only behind an ingress that supplies a trusted client address. |
| `BONJOU_RELAY_CLIENT_IP_HEADER` | Unset | One ingress-overwritten IP header, such as `CF-Connecting-IP` on public Render services. |

Do not expose the image directly to the internet with proxy trust enabled. An
arbitrary client can supply forwarding headers. In explicit-header mode, the
coordinator requires proxy trust and exactly one valid IP address in that
header. Missing or invalid addresses receive HTTP 503 before a WebSocket is
accepted, instead of combining unrelated networks under a proxy's address.

## Render web service

[Render supports Docker services](https://render.com/docs/docker) and
[public WebSockets](https://render.com/docs/websocket). Create a **web service**
from this repository using these settings, after choosing and authorizing the
hosting plan:

| Setting | Value |
| --- | --- |
| Runtime | Docker |
| Root directory / Docker context | Repository root |
| Dockerfile path | `packaging/relay/Dockerfile` |
| Docker command | Leave empty; use the image entrypoint. |
| Health check path | `/healthz` |
| Instances | One; do not enable autoscaling. |
| `BONJOU_RELAY_ORIGINS` | `https://bonjou.vercel.app`, plus each actual custom site origin. |
| `BONJOU_RELAY_TRUST_PROXY` | `true` |
| `BONJOU_RELAY_CLIENT_IP_HEADER` | `CF-Connecting-IP` |

[Render requires listening on `0.0.0.0:$PORT`](https://render.com/docs/web-services#port-binding)
and terminates TLS at its load balancer. The image reads its `PORT` automatically;
use the assigned `https://…onrender.com` URL for the browser's coordinator base
URL. The browser converts it to `wss://…/ws`.

Render's [client-IP guidance](https://render.com/articles/host-pocketbase-on-render)
states that its public ingress overwrites `CF-Connecting-IP`. Select that
explicit header: the nginx-oriented default `X-Real-IP` and rightmost
`X-Forwarded-For` policy is not a substitute for Render's ingress policy. Confirm
this contract still holds if adding another proxy or custom ingress. Do not use
this setting for a private Render service or an externally reachable container
that bypasses Render's public edge.

The [Free plan](https://render.com/docs/free) sleeps after 15 idle minutes and
can take about a minute to wake. It is suitable for a trial, with visible cold
starts; use an approved always-on plan for production availability. Render may
replace the process during deployments or maintenance, so active sessions still
need to reconnect. Creating a paid plan requires the owner's authorization.

## Owned Linux server

The existing `packaging/relay/bonjou-relay.service` and
`packaging/relay/nginx-bonjou-relay.conf` provide a loopback-only service behind
nginx. nginx overwrites `X-Real-IP` with the address it sees, so leave
`-client-ip-header` unset and use the service's existing `-trust-proxy` setting.
Only nginx should reach port 46330. If another load balancer fronts nginx,
review the whole trusted ingress chain first.

Supply the verified SSH host, your coordinator hostname, target architecture,
and permitted website origins when using `scripts/deploy-relay.sh`:

```sh
BONJOU_RELAY_SSH_HOST=your-verified-ssh-alias \
BONJOU_RELAY_HOSTNAME=coordinator.example.com \
BONJOU_RELAY_GOARCH=amd64 \
BONJOU_RELAY_ORIGINS=https://bonjou.vercel.app \
  ./scripts/deploy-relay.sh
```

The target needs nginx and Certbot installed, a verified SSH host key, working
DNS, and inbound ports 80/443. Do not reuse the deleted former host or accept an
unverified replacement SSH key.

## Connect the website and verify the release

Set `VITE_COORDINATOR_URL=https://your-coordinator.example.com` in the
`bonjou-web` deployment's build environment and rebuild the website. This value
is public routing configuration, not a secret. Keep origins exact; an unrelated
preview origin is not automatically permitted.

Before directing production browsers at a replacement, verify its HTTPS health
response, actual WebSocket hello/roster, same-network discovery, room creation,
QR code, same-network room join, and a direct browser-to-browser transfer with
metadata approval and byte verification. Also confirm a different source
network cannot join the room and `/t/*` returns 404. A green website build or
health check alone does not establish that sharing works.

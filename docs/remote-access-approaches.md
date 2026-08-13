# Remote Access Approaches — Research

Research note: how to make remote access to the Ferngeist Gateway dramatically
easier to set up and use. Every factual claim cites a primary source (vendor
docs, IETF RFCs, source repos). Assessments are explicitly labeled.

## The problem

Two kinds of end users carry the setup burden today:

- **Gateway operators** — must get a process that by default listens on
  `127.0.0.1:5788` reachable from the internet. Today that means installing a
  tunnel client (ngrok with an account and token, Cloudflare Tunnel with a
  domain, Tailscale, or a reverse proxy), running it, and re-registering the
  service with `daemon install --public-url <url>` so the daemon knows its
  public address (the service manager writes `FERNGEIST_GATEWAY_PUBLIC_BASE_URL`
  into the service env — see `internal/service/manager_*.go`).
- **App users** — must pair a device with the gateway. The public pairing flow
  (code + challenge) already exists and the admin pairing response already
  carries a `ferngeist-gateway://pair?...` deep-link payload plus a QR code
  (`internal/api/pairing_handlers.go`, `internal/pairing/service.go`). The
  blocker: a remote device cannot pair until the gateway is already publicly
  reachable — the tunnel step is a hard prerequisite.

The gateway already classifies its remote configuration for status reporting
(`internal/api/server.go`: `tailscale`, `cloudflare_tunnel`, `lan_direct`,
`manual_reverse_proxy`), so it can already *tell* operators how they're
exposed. What it cannot do is *become* exposed on its own.

Five approach families below, then a comparison and a phased recommendation.

---

## 1. Embedded / one-shot tunnels (the daemon owns the tunnel)

The daemon provisions and manages the tunnel itself at install/startup, so the
operator never touches a second CLI. This is the highest-leverage change
because it also auto-solves the pairing chicken-and-egg (see §4).

### 1.1 Cloudflare Quick Tunnels (TryCloudflare)

- One command, no account, no domain: `cloudflared tunnel --url http://localhost:8080` prints a random `https://<random>.trycloudflare.com` URL that proxies to localhost over HTTPS [cloudflare docs](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/do-more-with-tunnels/trycloudflare/).
- **Explicitly for testing and development**: "no SLA or uptime", subject to a
  hard cap of **200 in-flight requests**, and **no Server-Sent Events**
  (WebSockets are fine — the tunnel is a TCP/HTTP proxy; SSE is the documented
  exclusion) [cloudflare docs](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/do-more-with-tunnels/trycloudflare/).
- **Setup effort:** zero (install `cloudflared`, run it). **Account:** none.
  **URL stability:** random per invocation — changes every restart. **HTTPS:**
  yes, automatic. **Cost:** $0. **Ops burden:** none.
- **Go-embeddability:** `cloudflared` is a Go binary distributed for all
  platforms [cloudflared downloads](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/), but there is no documented
  stable embedding API — the practical approach is bundling/shipping the binary
  and managing it as a child process (which also matches how the gateway already
  launches agent binaries). *[assessment: no official library API]*

**Best fit:** free, account-less, zero-config default path; not a production
SLA. URL churn is the main UX cost — on restart the URL changes, so paired
clients need a way to learn the new URL (see §5).

### 1.2 ngrok Go SDK (ngrok-go)

- `ngrok.Listen(ctx)` returns a `net.Listener`; the ngrok agent is packaged as
  a Go library (MIT license). Requires an authtoken from the dashboard [ngrok-go repo](https://github.com/ngrok/ngrok-go).
- Free plan: $5 one-time credit, 3 online endpoints, 20k HTTP requests, 1 GB
  transfer, **no custom domains** (random URLs only); paid from $10/mo
  (Hobbyist) or $20/mo base (pay-as-you-go) [ngrok pricing](https://ngrok.com/pricing).
- Traffic Policy can enforce auth/rate-limiting at ngrok's edge — useful belt
  and suspenders in front of the gateway [ngrok-go repo](https://github.com/ngrok/ngrok-go).
- **Setup effort:** account + authtoken, then one flag. **Account:** yes
  (free tier). **URL stability:** free tier random; custom domains require
  paid plans. **HTTPS:** yes. **Cost:** free tier with hard usage caps,
  paid tiers for custom domains. **Go-embeddability:** first-class, in-process.

### 1.3 Tailscale `tsnet` + Funnel

- `tsnet` embeds a Tailscale node directly in a Go program (`go get
  tailscale.com/tsnet`). Device auth is either an interactive auth URL or a
  pre-provisioned auth key [tsnet docs](https://tailscale.com/docs/features/tsnet).
- **Tailscale Funnel** makes a local service reachable from the public internet
  at a stable `https://<machine>.<tailnet>.ts.net` URL. Traffic is proxied
  through Funnel relay servers via an encrypted tunnel that **relays cannot
  decrypt** (end-to-end encryption) [funnel docs](https://tailscale.com/docs/features/tailscale-funnel).
- Constraints: Funnel is in beta; HTTPS only; ports 443/8443/10000;
  non-configurable bandwidth limits; requires MagicDNS + a `funnel` node
  attribute in the tailnet ACL policy; macOS has variant restrictions [funnel docs](https://tailscale.com/docs/features/tailscale-funnel).
- Personal plan is free (unlimited user devices, up to 6 users) [tailscale pricing](https://tailscale.com/pricing).
- **Setup effort:** one-time tailnet sign-in (auth URL or auth key).
  **Account:** yes. **URL stability:** yes — tied to the device/tailnet name.
  **HTTPS:** yes, automatic. **Cost:** free personal plan. **Ops burden:**
  tailnet policy (`funnel` attr) needs a one-time ACL edit.

### 1.4 zrok

- Open-source, self-hostable sharing platform; hosted `myzrok.io` (free
  account) or self-hosted instance; public shares via `zrok share public`,
  persistent shares via the `zrok` agent [zrok docs](https://docs.zrok.io/).
- **Setup effort:** account token, `zrok enable`, then share.
  **Account:** yes (hosted) or none (self-host, but then you run the server).
  **URL stability:** ephemeral by default; reserved names on self-host.
  **Go-embeddability:** Go codebase; no documented stable embedding API
  *[assessment]*.

### 1.5 frp / bore / rathole (self-hosted reverse tunnels)

- frp is a "fast reverse proxy to help you expose a local server behind a NAT
  or firewall to the internet" — you run **both** `frps` (public server) and
  `frpc` (the tunnel client) [frp repo](https://github.com/fatedier/frp).
- **Setup effort:** requires a public VPS. This *shifts* the burden to hosting
  a relay server — the opposite of convenience for end users. Only sensible as
  a fully self-hosted, privacy-maximalist option.

---

## 2. Managed relay / rendezvous (zero inbound ports)

The architectural pattern that removes port forwarding, public IPs, and
tunnel CLIs entirely: **the daemon makes one outbound connection to a relay;
clients also connect to the relay, which forwards between them.**

### 2.1 VS Code Remote Tunnels — the gold-standard UX

- `code tunnel` is the entire setup: it starts the VS Code Server, opens an
  outbound connection to the Azure-hosted dev-tunnels relay, and prints a
  `https://vscode.dev/tunnel/<machine>/<folder>` URL [VS Code tunnels docs](https://code.visualstudio.com/docs/remote/tunnels).
- Both ends authenticate with the same GitHub or Microsoft account; **no
  firewall changes, no network listeners** are set up; an SSH connection is
  created over the tunnel for end-to-end encryption [VS Code tunnels docs](https://code.visualstudio.com/docs/remote/tunnels).
- The underlying service (Microsoft dev tunnels) is a "tunnel relay service"
  that "facilitates secure connections between a dev tunnel host and clients
  via a cloud service, even when the host may be behind a firewall and unable
  to accept incoming connections directly"; tunnels are **secure by default**
  (only the owner's account can connect), support persistent URLs and multiple
  ports — but are **public preview, not for production workloads** [dev tunnels overview](https://learn.microsoft.com/en-us/azure/developer/dev-tunnels/overview).
- `code tunnel service install` runs the tunnel as a background service;
  usage limits exist (e.g. 10 tunnels per account) [VS Code tunnels docs](https://code.visualstudio.com/docs/remote/tunnels).

### 2.2 Teleport

- Teleport agents join the cluster by establishing an **SSH reverse tunnel to
  the Proxy Service**; "as long as an agent can dial back to the cluster's
  Proxy Service, it can be located behind a firewall" — the proxy identifies
  the agent by certificate and relays client connections through the reverse
  tunnel [Teleport agent architecture](https://goteleport.com/docs/reference/architecture/agents/).
- Enterprise-grade (audit, SSO, RBAC) and self-hostable, but it is an entire
  identity platform — heavy for this product, though it proves the pattern.

### 2.3 localtunnel

- `npx localtunnel --port 8000` exposes localhost through a community-hosted
  relay with an ephemeral random URL [localtunnel repo](https://github.com/localtunnel/localtunnel).

### What this means for Ferngeist

The relay pattern is the only approach where **neither** end needs inbound
reachability. The gateway already does outbound networking (FCM push,
update checks); the app already has outbound internet. A relay is
infrastructure you either run yourself (new ops burden — frp/Teleport), use
someone else's (cloudflared quick tunnel and ngrok are effectively hosted
relays; dev tunnels is one for VS Code), or buy. The product implication:
a relay decouples "gateway reachability" from "operator networking" entirely,
at the price of running/trusting a relay.

---

## 3. NAT traversal without tunnels

### 3.1 Automatic UPnP / NAT-PMP port mapping

- Syncthing's docs call UPnP "the easiest way to get a working port forward" —
  Syncthing creates the mapping itself and logs `Created UPnP port mapping for
  external port XXXXX`; when port forwarding is impossible it falls back to
  relaying (with worse performance than direct) [Syncthing firewall docs](https://docs.syncthing.net/users/firewall).
- The same pattern (auto port-map, no third-party service, no account, no
  tunnel CLI) is what a self-hosted gateway would want.
- **Limits:**
  - Carrier-grade NAT: RFC 6888 (CGN requirements) requires CGNs to implement
    a port-mapping control protocol (PCP) [RFC 6888](https://www.rfc-editor.org/rfc/rfc6888), but carriers rarely enable it — so on CGNAT networks (common for mobile and many residential ISPs) inbound mapping silently fails. *[assessment: practical prevalence of PCP-enabled CGNs is low]*
  - Router firmware support/trust for UPnP varies; corporate networks block it.
  - Security: opening a raw port means the gateway itself must serve TLS +
    authentication — today the gateway is plain HTTP on localhost and relies on
    the tunnel's HTTPS edge (see `docs/security.md`). Exposing raw port 5788
    would require adding in-process TLS first. *[assessment]*

### 3.2 IPv6

- IPv6 gives clients routable addresses without NAT when ISP and router
  support it. Not universal; adds a second address family to support; the
  gateway's WebSocket endpoint would need an IPv6 listener. Marginal today,
  worth revisiting as ISPs push IPv6. *[assessment — no primary source
  verified for this section]*

### 3.3 WebRTC / libp2p P2P

- P2P (WebRTC data channels, libp2p circuits) is technically possible but a
  poor fit here: clients need a stable URL and standard WebSocket; TURN relays
  become necessary anyway when hole punching fails, reintroducing relay infra;
  and it adds substantial protocol complexity for zero UX gain over a plain
  relay. *[assessment — fits poorly, not recommended]*

---

## 4. Auth & pairing UX (transport-agnostic)

These apply no matter which tunnel/relay is chosen, and compound with §5.

### 4.1 Device-flow pairing (RFC 8628)

- The OAuth 2.0 Device Authorization Grant is *the* standard for
  input-constrained/second-device pairing: the device gets a `user_code` and a
  verification URI, the user approves on another device, the device polls
  until approved [RFC 8628](https://www.rfc-editor.org/rfc/rfc8628). This is
  the GitHub/VS Code "enter a code" pattern, and it is exactly the shape the
  gateway's existing code-based pairing already approximates.
- RFC 8628 §3.3.1 explicitly optimizes **non-textual verification URI
  transmission — QR codes** [RFC 8628](https://www.rfc-editor.org/rfc/rfc8628);
  §6 covers user-code usability guidance (short, unambiguous codes).

### 4.2 Deep links and QR — already half built

- The admin pairing response already returns a `ferngeist-gateway://pair?...`
  deep-link `payload` for mobile clients and pairing supports "a short code
  separately from the QR payload" (`internal/api/pairing_handlers.go`,
  `internal/pairing/service.go`). The remaining work is *remote-first*: encode
  the public URL + pairing payload into one QR the app scans, so pairing and
  gateway-addressing happen in a single step. A magic-link
  (`https://<public-url>/pair?code=...`) is the same idea over HTTP.
- The gateway already dispatches FCM pushes with deep-link keys
  (`internal/push/fcm.go`), so **push-assisted pairing** (operator approves a
  remote pairing request on their phone) is available infrastructure.

### 4.3 Ordering fix

- Today remote pairing is impossible until the operator completes the manual
  tunnel step. Any of §1's one-shot tunnels removes this ordering constraint
  automatically: install → tunnel up → URL registered → print QR → scan.

---

## 5. Operator one-shot convenience

- `daemon install --remote` (default: Cloudflare Quick Tunnel, account-less):
  spawn `cloudflared`, wait for the printed `trycloudflare.com` URL, write it
  into the service env as `FERNGEIST_GATEWAY_PUBLIC_BASE_URL` (the service
  managers already persist that variable — `internal/service/manager_*.go`),
  and print a QR deep link for first pairing.
- URL re-discovery on restart: the tunnel URL changes; paired clients need a
  way to learn it. Options: push notification with the new URL (FCM already
  carries deep-link data), or a stable-URL upgrade path (§Phase 3 below).
- The status surface already exists: `remoteStatus` classifies mode/scope and
  health (`internal/api/server.go`); surfacing it in the client app turns
  "gateway offline" into actionable feedback.

---

## Comparison

| Approach | End-user setup effort | Account needed | Stable URL | HTTPS | Cost | Go-embeddable | Ops burden |
|---|---|---|---|---|---|---|---|
| Cloudflare Quick Tunnel (embedded/subprocess) | none (one flag) | **no** | no (random per restart) | yes | $0 | binary subprocess | none |
| ngrok Go SDK | account + token | yes | no (free); paid for custom | yes | free tier caps; $10+/mo | **in-process** | none |
| Tailscale tsnet + Funnel | tailnet sign-in | yes | **yes** (`*.ts.net`) | yes | free personal | **in-process** | ACL edit once |
| zrok | account token | yes (hosted) | no (ephemeral) | yes | free | no stable API | none |
| frp (self-host relay) | public VPS | n/a | yes | manual | VPS cost | no | **high** |
| Managed relay (dev-tunnels-style) | none (one command) | yes (account auth) | yes | yes | n/a (preview) | protocol to implement | relay infra |
| Teleport | join token | n/a | yes | yes | open source; heavy | no | **very high** |
| UPnP/NAT-PMP auto port-map | none | no | **yes** (IP:port) | **no** (raw port) | $0 | in-process (library) | router/CGNAT roulette |
| Manual reverse proxy (status quo) | install+configure proxy | domain | yes | yes | domain/hosting | n/a | high |

---

## Recommendation (phased, tailored to this repo)

The gateway's constraints: single Go binary, one WebSocket API, existing
proof-of-possession pairing, existing FCM push, status classification code
already present, and service managers that already persist a public-URL env
var. The binding constraint is the operator's manual tunnel step — that is the
entire problem.

**Phase 1 — one-shot remote install (biggest UX win, zero new infra, $0).**
Add `daemon install --remote` that provisions a Cloudflare Quick Tunnel as a
managed child process, captures the generated `trycloudflare.com` URL,
persists it as `FERNGEIST_GATEWAY_PUBLIC_BASE_URL`, and prints a QR deep link
for first pairing. No account, no domain, no second CLI. Accept the caveats
(documented): URL churns on restart, dev-grade SLA, 200-in-flight cap. This is
the closest thing to VS Code's `code tunnel` that exists without building a
relay, and it unblocks remote pairing on day one. (Sources: §1.1, §4.2, §5.)

**Phase 2 — pairing UX.** Reuse RFC 8628's device-flow shape and the existing
deep-link/QR plumbing: a single scannable `ferngeist-gateway://pair?...` (or
magic HTTP link) that carries the public URL, challenge, and code; optionally
push-assisted approval over the existing FCM channel. Remove the ordering
constraint: pairing works the moment Phase 1 finishes. (Sources: §4.)

**Phase 3 — stable URL / production path.** Two upgrade options, offered as a
flag, both already classified by `remoteStatus`:
- **Tailscale tsnet + Funnel** for operators who want a *stable* public URL:
  auth key in, `https://<machine>.<tailnet>.ts.net` out, E2E-encrypted, free
  personal plan (β caveats apply).
- **ngrok Go SDK** for operators who already have an account or want edge
  auth/rate limiting via Traffic Policy.
- Keep the **manual reverse proxy** path for production/self-hosted
  deployments, documented as today. (Sources: §1.2, §1.3, §5.)

**Explicitly not recommended:** raw port exposure via UPnP/NAT-PMP until the
gateway can serve TLS in-process (security model in `docs/security.md` assumes
a TLS-terminating edge); WebRTC/libp2p (relay infra returns anyway, no UX
gain); building your own relay as the first step (new ops burden) — revisit
only if the hosted quick-tunnel path proves too unstable for the target users.
(Sources: §3.)

---

## Sources

- Cloudflare Quick Tunnels / TryCloudflare — https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/do-more-with-tunnels/trycloudflare/
- cloudflared downloads — https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/
- ngrok Go SDK — https://github.com/ngrok/ngrok-go
- ngrok pricing — https://ngrok.com/pricing
- Tailscale tsnet — https://tailscale.com/docs/features/tsnet
- Tailscale Funnel — https://tailscale.com/docs/features/tailscale-funnel
- Tailscale pricing — https://tailscale.com/pricing
- zrok docs — https://docs.zrok.io/ (NetFoundry)
- frp — https://github.com/fatedier/frp
- VS Code Remote Tunnels — https://code.visualstudio.com/docs/remote/tunnels
- Microsoft dev tunnels overview — https://learn.microsoft.com/en-us/azure/developer/dev-tunnels/overview
- Teleport agent architecture — https://goteleport.com/docs/reference/architecture/agents/
- localtunnel — https://github.com/localtunnel/localtunnel
- Syncthing firewall/NAT docs — https://docs.syncthing.net/users/firewall
- RFC 6888 (CGN requirements) — https://www.rfc-editor.org/rfc/rfc6888
- RFC 8628 (OAuth device grant) — https://www.rfc-editor.org/rfc/rfc8628
- Repo: `docs/remote-access.md`, `docs/security.md`, `internal/api/pairing_handlers.go`, `internal/pairing/service.go`, `internal/api/server.go`, `internal/push/fcm.go`, `internal/service/manager_*.go`

# Remote Access

Ferngeist Gateway listens on `127.0.0.1:5788` by default.

## Modes

### Local only

Use this when the client and gateway are on the same machine or network and you do not need public access.

### LAN access

Use `daemon run --lan` when you want devices on the local network to reach the gateway during development.

```powershell
ferngeist-gateway daemon run --lan
ferngeist-gateway pair
```

### Tailscale (recommended)

Tailscale is the recommended way to reach the gateway from outside your LAN:
it gives a **stable HTTPS URL**, works behind any router or mobile network, and
needs no router configuration. The gateway can set it up for you.

Enable it with a single flag:

```powershell
ferngeist-gateway daemon install --remote   # installs + starts the service with remote access
ferngeist-gateway daemon run --remote       # or run in the foreground
```

The gateway then:

1. Uses the `tailscale` app if it is already installed and signed in on this
   machine. It runs `tailscale funnel` for public access (`tailscale serve`
   when `FERNGEIST_GATEWAY_TAILSCALE_PRIVATE=1`), then reads your stable
   `https://<machine>.<tailnet>.ts.net` address.
2. Otherwise it embeds a small Tailscale node inside the gateway. Sign in at
   the printed link once in a browser, or pre-authenticate with
   `FERNGEIST_GATEWAY_TAILSCALE_AUTH_KEY` to skip the click.

Either way the public URL never changes between restarts. The gateway saves it,
prints `remote access ready` with the URL, and pushes the URL to paired
devices — the app re-addresses itself, no re-entry needed.

#### One-time Tailscale settings (embedded mode only)

The first time you use the built-in node, enable two things in your Tailscale
admin console. Both are account-wide — once done, they stay done for every
future install.

**1. HTTPS (DNS settings)**

1. Open https://login.tailscale.com/admin/dns
2. Scroll to **HTTPS Certificates** and turn it **on**, confirm the prompt.
   (If the page shows a "Disable HTTPS" button, it is already on — skip.)

**2. Funnel permission (access controls)**

> The **"Add Funnel to policy"** button only exists on the **JSON editor**
> page. The console opens a visual "Policies" page by default that does not
> show it — this is the step people get stuck on.

1. Open https://login.tailscale.com/admin/acls/file (the JSON editor)
2. Find the **Funnel** section and click **Add Funnel to policy** — it adds
   the permission and saves automatically. No JSON to type.

If the button is missing, paste this as the first thing inside the opening
`{` (keep the trailing comma), then click **Save**:

```json
"nodeAttrs": [
  {
    "target": ["autogroup:member"],
    "attr": ["funnel"]
  }
],
```

If the file already contains a `"nodeAttrs"` line, add just the
`{"target": ["autogroup:member"], "attr": ["funnel"]}` object to that
existing list instead of a second copy.

If you already run the `tailscale` app, these steps are usually already done.
Funnel is in beta and limited to HTTPS traffic; see the
[Tailscale Funnel docs](https://tailscale.com/docs/features/tailscale-funnel).

> The gateway retries automatically: if a setting above is missing, it logs
> exactly which one to change and keeps trying in the background. Fix it in
> the browser and the gateway picks up on its own — no restart needed.

#### First boot: the Tailscale login

The first time a node runs, Tailscale asks for a one-time login. No need to
hunt through logs for the link:

- `ferngeist-gateway daemon install --remote` prints the login link right
  after installing (waits up to 30 seconds for the daemon to report it).
- Running `ferngeist-gateway daemon status` anytime shows it as an
  `AUTH REQUIRED` line until you log in.

Open the link once; provisioning then finishes on its own (it retries in the
background), and the URL appears in `daemon status` as `PUBLIC URL` — no
restart needed. If the URL line is still missing a minute after logging in,
check the daemon log for a tailnet setting hint (`docs/remote-access.md`
above) or run `ferngeist-gateway daemon restart`.

#### Finding your public address

Once provisioning succeeds the log shows `remote access ready` with the URL.
You can also:

- Run `ferngeist-gateway daemon status` — it prints a `PUBLIC URL` line
  as soon as provisioning finishes (it is never blank after a successful
  login; no restart required).
- Know it in advance: it is always
  `https://ferngeist-gateway.<your-tailnet>.ts.net`, and `<your-tailnet>`
  is shown next to your account name in the admin console. If you set
  `FERNGEIST_GATEWAY_TAILSCALE_HOSTNAME`, that name replaces
  `ferngeist-gateway`.

#### Public vs private

- **Funnel** (`--remote` default): anyone with the URL can reach the gateway.
  Pairing still protects it — proof of possession is on by default.
- **Private** (`FERNGEIST_GATEWAY_TAILSCALE_PRIVATE=1`): only devices on your
  tailnet can reach it. The phone needs the Tailscale app too.

### Manual alternatives (archived)

Manual tunnel and proxy setups still work — set the public URL the clients
will use:

#### Reverse proxy

If you already have a reverse proxy, point it at `127.0.0.1:5788` and set the
public URL to the address clients will use.

For a reverse proxy example, see [Caddy reverse proxy docs](https://caddyserver.com/docs/quick-starts/reverse-proxy#https-from-client-to-proxy).

```powershell
ferngeist-gateway daemon install --public-url https://your.domain.example
```

#### Cloudflare Tunnel

> NOTE: This requires you to have your own domain and use Cloudflare as its authoritative DNS provider.

Follow the official Cloudflare guide for creating a remote tunnel: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/get-started/create-remote-tunnel/

Point the tunnel at `http://localhost:5788` and set `--public-url` to the hostname clients will use.

#### ngrok

```powershell
ngrok http 5788
ferngeist-gateway daemon install --public-url https://xxxx.ngrok.io
```

> Note: ngrok and temporary Cloudflare URLs change on every restart, which
> means re-entering the URL in the app. Tailscale addresses do not change.

## Notes

- The public URL should match the address the client will use.
- For local development, LAN mode is usually enough.
- The admin API stays bound to localhost — it is never exposed through the
  remote access path.

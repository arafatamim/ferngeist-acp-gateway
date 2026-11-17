# Ferngeist Gateway

[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/arafatamim/ferngeist-acp-gateway)

The desktop half of [Ferngeist](https://github.com/arafatamim/Ferngeist). Install
it on the computer where your coding agents run, pair your phone, and drive those
agents from the Ferngeist Android app.

It speaks the [Agent Client Protocol (ACP)](https://agentclientprotocol.com), so
it works with any ACP-compatible agent. The [client API](#client-api) is
documented and open to other clients.

## Quickstart

**1. Install**

```sh
# macOS / Linux
curl -fsSL https://arafatamim.github.io/ferngeist-acp-gateway/install.sh | sh
```

```powershell
# Windows
irm https://arafatamim.github.io/ferngeist-acp-gateway/install.ps1 | iex
```

Homebrew, apt/dnf, pacman and manual installs are covered in
[docs/install.md](docs/install.md).

**2. Start it as a background service**

Pick how your phone will reach the computer:

```sh
# From anywhere: a stable HTTPS URL via Tailscale (free account)
ferngeist-gateway daemon install --remote

# Only at home: phone and computer on the same Wi-Fi
ferngeist-gateway daemon install --lan
```

On first `--remote` setup the gateway prints a Tailscale login link and tells
you which tailnet settings to turn on. See [docs/remote-access.md](docs/remote-access.md).

**3. Pair your phone**

```sh
ferngeist-gateway pair
```

Scan the QR code with the Ferngeist app. That's it.

## What you get

- **Sessions that survive your phone sleeping.** Agents keep working when the
  connection drops, and the app reconnects where it left off.
- **Push notifications** when a turn finishes, the agent asks for permission or
  input, or something fails. End-to-end encrypted Web Push via UnifiedPush or
  the browser; no Google/Firebase account needed.
- **See what the agent changed.** Browse project files and git diffs from the app.
- **Self-hosted.** Your code and agents stay on your machine. Revoke any paired
  device at any time.

## Day-to-day commands

```sh
ferngeist-gateway daemon status   # is it running, and on which URL?
ferngeist-gateway devices list    # paired phones
ferngeist-gateway agents list     # agents the gateway can launch
ferngeist-gateway logs -f --tail 50  # recent daemon log, then follow
```

Prefer not to install a service? Run it in the foreground with
`ferngeist-gateway daemon run --remote` (or `--lan`).

## Other ways to expose the gateway

If you'd rather not use Tailscale, put any tunnel or reverse proxy in front of
port `5788` and tell the gateway its public URL:

<details>
<summary>ngrok, Cloudflare Tunnel, reverse proxy</summary>

```sh
# ngrok
ngrok http 5788
ferngeist-gateway daemon install --public-url https://xxxx.ngrok.io

# Cloudflare Tunnel
cloudflared tunnel --url http://localhost:5788
ferngeist-gateway daemon install --public-url https://xxxx.trycloudflare.com

# Your own reverse proxy
ferngeist-gateway daemon install --public-url https://your.domain.example
```

Then pair as usual and add the public URL in the app.

</details>

## Client API

The Android app is one client of a documented HTTP + WebSocket API:

- **Standard ACP over WebSocket.** Each chat is a WebSocket carrying plain ACP
  JSON-RPC, with a few `_ferngeist/` extension notifications.
- **One endpoint, many agents.** `GET /v1/agents` lists every agent the host can
  run (from the ACP registry, plus registered custom agents).
  `POST /v1/agents/{id}/connect` launches one and returns a WebSocket URL.
  Connecting again opens a parallel chat.
- **Resilient sessions.** Agent processes outlive the socket.
  `POST /v1/sessions/{id}/resume` reattaches, and the gateway replays buffered
  updates.
- **Pairing and auth built in.** Short-code/QR pairing, per-device
  credentials with proof-of-possession, and revocation.
- **Push notifications.** End-to-end encrypted Web Push for turn completion,
  permission requests and errors.
- **Workspace access.** Read project files, `git status` and `git diff` for the
  directory an agent is working in, plus git worktrees for isolated runs.
- **Discovery.** mDNS advertisement on the LAN, with a protocol version for
  compatibility checks.

Minimal flow: arm pairing locally with `ferngeist-gateway pair`, complete the
pairing challenge, connect to an agent, open `websocketUrl`, and speak ACP. The
full contract is in
[docs/api.md](docs/api.md), and contract changes are tracked in
[docs/changelog.md](docs/changelog.md).

## Contributing

See [docs/architecture.md](docs/architecture.md) for how the pieces fit together.

**Build from source** (requires Go):

```sh
go build -ldflags "-X main.buildVersion=$(git describe --tags --always --dirty 2>/dev/null || echo dev) -X main.buildCommit=$(git rev-parse --short HEAD 2>/dev/null || echo none)" -o ferngeist-gateway ./cmd/ferngeist
```

The `buildVersion` ldflag is required; the CLI refuses to start without it. To
run without building: `go run -ldflags "-X main.buildVersion=dev" ./cmd/ferngeist daemon run`.

## Documentation

- [Install](docs/install.md): per-OS installation
- [Usage](docs/usage.md): running the daemon, pairing, common commands
- [Remote access](docs/remote-access.md): Tailscale, tunnels, reverse proxies
- [Security](docs/security.md): security model
- [Pairing](docs/pairing.md): pairing flow and device credentials
- [Configuration](docs/configuration.md): environment variables and defaults
- [API](docs/api.md): public and admin API
- [Architecture](docs/architecture.md): components overview
- [Development](docs/development.md): build, test, local development

## License

[AGPL-3.0](LICENSE)

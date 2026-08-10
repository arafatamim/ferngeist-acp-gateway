# Ferngeist Gateway

`Ferngeist Gateway` is a self-hosted backend service for ACP-compatible agents. Its main purpose is to expose ACP agents through a unified WebSocket API with **resilient sessions** that survive WebSocket disconnection — keeping agents alive, dispatching FCM push notifications (`end_turn`, permission requests, agent errors, agent crashes), and allowing seamless reconnection. It discovers and launches supported agents, provides one authenticated endpoint, and manages pairing and paired devices. It also powers the [Ferngeist](https://github.com/arafatamim/Ferngeist) Android app.

## Install

See [docs/install.md](docs/install.md) for per-OS instructions:

- **macOS + Linux (one command):** `curl -fsSL https://arafatamim.github.io/ferngeist-acp-gateway/install.sh | sh`
- **Windows:** `irm https://arafatamim.github.io/ferngeist-acp-gateway/install.ps1 | iex`
- **Linux packages:** apt/dnf, pacman, or tarball (the one-liner picks the right one)

## Building from source

```bash
go build -ldflags "-X main.buildVersion=$(git describe --tags --always --dirty 2>/dev/null || echo dev) -X main.buildCommit=$(git rev-parse --short HEAD 2>/dev/null || echo none)" -o ferngeist-gateway ./cmd/ferngeist
```

This builds the `ferngeist-gateway` binary into the current directory (the
`buildVersion` ldflag is required — the CLI refuses to start without it).
Requires Go.

To run the daemon from source without building:

```bash
go run -ldflags "-X main.buildVersion=dev" ./cmd/ferngeist daemon run
```

## Usage

### Local use

Run the daemon on the machine hosting the agents:

```powershell
ferngeist-gateway daemon run --lan
```

### Pair a device

Pair from the machine running the daemon:

```powershell
ferngeist-gateway pair
```

### Run as a service

Register the extracted binary as a background service.

> NOTE: On Windows, creating the scheduled task shows one UAC prompt
> (the installer handles it via `Start-Process -Verb RunAs`).

```powershell
ferngeist-gateway daemon install
```

Check status:
```powershell
ferngeist-gateway daemon status
```

### Expose remotely

Use a tunnel or reverse proxy if the client is not on the same network.

**ngrok**

```powershell
ngrok http 5788
ferngeist-gateway daemon install --public-url https://xxxx.ngrok.io
```

**Cloudflare Tunnel**

```powershell
cloudflared tunnel --url http://localhost:5788
ferngeist-gateway daemon install --public-url https://xxxx.trycloudflare.com
```

**Reverse proxy**

```powershell
ferngeist-gateway daemon install --public-url https://your.domain.example
```

Then pair the device and add the public URL in the Ferngeist app.

## What It Does

- exposes ACP agents through one unified WebSocket API
- discovers supported agents and launches them on demand
- handles pairing and paired device credentials
- supports **resilient sessions** that survive WebSocket disconnection with push notification wake-up (FCM, with a pluggable provider seam for other platforms) and seamless reconnection
- supports local and LAN-based access
- **workspace browsing:** read-only file view (`GET /v1/runtimes/{id}/files`) and git inspection (`git/status`, `git/diff`) so a paired client can see what the agent is working on

## Documentation

- [docs/install.md](docs/install.md) - per-OS installation
- [docs/architecture.md](docs/architecture.md) - system overview and components
- [docs/usage.md](docs/usage.md) - setup, pairing, and running the daemon
- [docs/api.md](docs/api.md) - public and admin API surface
- [docs/security.md](docs/security.md) - security model and remote access notes
- [docs/pairing.md](docs/pairing.md) - pairing flow and device credentials
- [docs/remote-access.md](docs/remote-access.md) - tunnel and reverse proxy setup
- [docs/configuration.md](docs/configuration.md) - environment variables and defaults
- [docs/development.md](docs/development.md) - build, test, and local development notes

# Ferngeist Gateway

`Ferngeist Gateway` is a self-hosted backend service for ACP-compatible agents. Its main purpose is to expose ACP agents through a unified WebSocket API. It discovers and launches supported agents, provides one authenticated endpoint, and manages pairing and paired devices. It also powers the [Ferngeist](https://github.com/arafatamim/Ferngeist) Android app.

## Quick Start

Run the daemon:

```powershell
go run ./cmd/ferngeist daemon run
```

For LAN access during development:

```powershell
go run ./cmd/ferngeist daemon run --lan
go run ./cmd/ferngeist pair
```

Default listen address: `127.0.0.1:5788`

## Usage

### Local use

Run the daemon on the machine hosting the agents:

```powershell
go run ./cmd/ferngeist daemon run
```

### Pair a device

Pair from the machine running the daemon:

```powershell
go run ./cmd/ferngeist pair
```

### Run as a service

Register the extracted binary as a background service.

> NOTE: Installing the service on Windows requires administrator privileges.

```powershell
go run ./cmd/ferngeist daemon install
```

Check status:
```powershell
go run ./cmd/ferngeist daemon status
```

### Expose remotely

Use a tunnel or reverse proxy if the client is not on the same network.

**ngrok**

```powershell
ngrok http 5788
.\ferngeist-gateway.exe daemon install --public-url https://xxxx.ngrok.io
```

**Cloudflare Tunnel**

```powershell
cloudflared tunnel --url http://localhost:5788
.\ferngeist-gateway.exe daemon install --public-url https://xxxx.trycloudflare.com
```

**Reverse proxy**

```powershell
.\ferngeist-gateway.exe daemon install --public-url https://your.domain.example
```

Then pair the device and add the public URL in the Ferngeist app.

## What It Does

- exposes ACP agents through one unified WebSocket API
- discovers supported agents and launches them on demand
- handles pairing and paired device credentials
- supports local and LAN-based access
- stores gateway state in SQLite

## Documentation

- [docs/architecture.md](docs/architecture.md) - system overview and components
- [docs/usage.md](docs/usage.md) - setup, pairing, and running the daemon
- [docs/api.md](docs/api.md) - public and admin API surface
- [docs/security.md](docs/security.md) - security model and remote access notes
- [docs/pairing.md](docs/pairing.md) - pairing flow and device credentials
- [docs/remote-access.md](docs/remote-access.md) - tunnel and reverse proxy setup
- [docs/configuration.md](docs/configuration.md) - environment variables and defaults
- [docs/development.md](docs/development.md) - build, test, and local development notes

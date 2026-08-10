# Usage

> Install the gateway first — see [docs/install.md](install.md) for per-OS
> instructions (PowerShell installer, Homebrew, apt/dnf, pacman, or manual).

## Capabilities

The gateway is a self-hosted backend for ACP-compatible agents:

- **One unified WebSocket API** for all supported agents, with resilient
  sessions that survive disconnects (push-notification wake-up + seamless
  reconnect).
- **Agent discovery + launch** — finds supported agents and starts them on
  demand.
- **Pairing + device credentials** — pair a phone/client once; revoke anytime.
- **Workspace browsing** — a paired client can read files and inspect git
  state inside the project the agent is working on
  (`GET /v1/runtimes/{id}/files`, `/git/status`, `/git/diff`; see
  [docs/api.md](api.md)).

## Run the daemon

```powershell
ferngeist-gateway daemon run --lan
```

This starts the gateway on `0.0.0.0:5788` by default.

## Pairing

To pair a device during local development:

```powershell
ferngeist-gateway pair
```

To expose the gateway on your local network:

```powershell
ferngeist-gateway daemon run --lan
```

Then pair the device from the client app.

## Common commands

```powershell
ferngeist-gateway daemon status
ferngeist-gateway devices list
```

## Notes

- The gateway is a local backend service for ACP-compatible agents.
- It is used as the backend for the Ferngeist Android app.

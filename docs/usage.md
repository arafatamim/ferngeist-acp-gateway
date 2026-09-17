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
- **Parallel chats per agent** — start several processes of the same agent at
  once (`POST /v1/agents/{id}/start` with `{"new": true}`); each chat runs in
  its own resilient session, so a background agent can keep working while you
  use another (see [docs/api.md](api.md)).
- **Pairing + device credentials** — pair a phone/client once; revoke anytime.
- **Workspace browsing** — a paired client can read files and inspect git
  state inside the project the agent is working on
  (`GET /v1/runtimes/{id}/files`, `/git/status`, `/git/diff`; see
  [docs/api.md](api.md)).
- **Custom agents** — register your own ACP agent (command + args) from a
  paired client or from the host with `ferngeist-gateway agents add`; it then
  lists and starts like any catalog agent.

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
ferngeist-gateway agents list
```

Commands that read or change gateway state — `pair`, `devices …`, `agents …` —
drive the running daemon over its local admin API. Run `daemon run` in another
terminal, or install the background service with `daemon install`. When nothing
answers, every one of them prints the same hint and exits `2`, so scripts can
tell "start the daemon" apart from an ordinary failure (`1`). `daemon status`
still prints its report and exits `2` when the API is unreachable.

First run: `ferngeist-gateway daemon install` → `ferngeist-gateway pair`.

## Notes

- The gateway is a local backend service for ACP-compatible agents.
- It is used as the backend for the Ferngeist Android app.

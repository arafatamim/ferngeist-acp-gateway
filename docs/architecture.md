# Architecture

Ferngeist Gateway is a self-hosted backend for ACP-compatible agents.

Its main job is to expose ACP agents through a unified WebSocket API. It discovers supported agents, launches them when needed, and manages pairing so clients can talk to them through one authenticated endpoint.

## Main parts

- `cmd/ferngeist` - CLI entrypoint for daemon, pairing, and device management
- `internal/api` - public and admin HTTP APIs
- `internal/gateway` - runtime token issuance and validation
- `internal/pairing` - pairing flow and device credentials
- `internal/runtime` - process supervision and transport bridging
- `internal/catalog` - supported agent discovery and validation
- `internal/registry` - ACP registry fetch and cache
- `internal/storage` - SQLite persistence
- `internal/discovery` - LAN advertising via mDNS
- `internal/config` - configuration and persisted settings

## Data flow

1. A client pairs with the gateway.
2. The gateway stores device credentials.
3. The client requests an ACP agent through the API.
4. The gateway launches or connects to the target agent.
5. ACP traffic is bridged over WebSocket through a single authenticated endpoint.

## Notes

- Agent support comes from the ACP registry plus local helper adapters.
- The gateway can auto-acquire managed binaries when supported.
- The Ferngeist Android app uses this service as its backend.
# API Surface

Ferngeist Gateway exposes two HTTP surfaces:

- **Public API** for paired clients and ACP agent control
- **Admin API** for local management and diagnostics

The public API is the one clients use most of the time. It exposes a single WebSocket bridge for ACP traffic plus the endpoints needed for pairing, status, and runtime control.

The admin API is bound to localhost and is intended for local setup, recovery, and management.

## Public API

Base path: `/v1`

### Health

- `GET /healthz`
  - Returns a simple health check response.

### Status and discovery

- `GET /v1/status`
  - Returns gateway status, build info, discovery state, remote access state, registry status, and runtime counts.

- `GET /v1/agents`
  - Returns the supported agent catalog merged with live runtime state.
  - Requires a paired client.

### Pairing

- `POST /v1/pair/start`
  - Starts a new pairing challenge.
  - Returns the challenge ID and expiration time.

- `POST /v1/pair/complete`
  - Completes pairing with a challenge ID, code, and device name.
  - Returns the issued device credential.

- `GET /v1/pair/status/{challengeId}`
  - Returns the current pairing state for a challenge.
  - Does not expose the pairing code.

### Authentication

- `POST /v1/auth/refresh`
  - Refreshes a paired device token.
  - Invalidates the old token immediately.

### Diagnostics

- `GET /v1/diagnostics/summary`
  - Returns a compact runtime health summary.
  - Requires `read` scope.

- `GET /v1/diagnostics/export`
  - Returns a full diagnostic bundle with runtime state and logs.
  - Disabled unless remote diagnostics export is allowed.
  - Requires `read` scope.

### Agent control

- `POST /v1/agents/{agentId}/start`
  - Starts the selected agent runtime.
  - Requires `control` scope.

- `POST /v1/agents/{agentId}/stop`
  - Stops the selected agent runtime.
  - Requires `control` scope.

### Runtime control

- `GET /v1/runtimes`
  - Lists managed ACP runtimes.
  - Requires a paired client.

- `GET /v1/runtimes/{runtimeId}/logs`
  - Returns buffered logs for a runtime.
  - Requires `read` scope.

- `POST /v1/runtimes/{runtimeId}/connect`
  - Returns WebSocket connection details and a short-lived bearer token for ACP traffic.
  - Requires a paired client.

- `POST /v1/runtimes/{runtimeId}/restart`
  - Restarts a runtime, optionally with environment overrides.
  - Requires `control` scope.
  - Runtime restart with environment overrides may be disabled by configuration.

### ACP bridge

- `GET /v1/acp/{runtimeId}`
  - WebSocket endpoint for ACP traffic.
  - Clients should send the bearer token in the `Authorization` header.

## Admin API

Base path: `/admin/v1`

The admin API is bound to localhost and is meant for local management only.

### Status

- `GET /admin/v1/status`
  - Returns detailed daemon status, including pairing target info and active pairing state.

### Pairing management

- `POST /admin/v1/pairings/start`
  - Starts a pairing challenge locally.
  - Returns the pairing code and deep-link payload.

- `GET /admin/v1/pairings/{challengeId}`
  - Returns pairing state for a challenge.

- `DELETE /admin/v1/pairings/{challengeId}`
  - Cancels an active pairing challenge.

### Device management

- `GET /admin/v1/devices`
  - Lists paired devices.

- `DELETE /admin/v1/devices/{deviceId}`
  - Revokes a paired device.

## Common response patterns

### Success

Most endpoints return JSON.

### Errors

Errors use a simple JSON envelope:

```json
{
  "error": "message"
}
```

### Authentication and scopes

- Some public endpoints require a paired device token.
- Some endpoints also require a scope such as `read` or `control`.
- Public-mode deployments may require proof-of-possession headers.

See [docs/security.md](docs/security.md) for the security model and remote access notes.

### WebSocket usage

The ACP WebSocket endpoint is the primary transport for agent traffic.

Typical flow:

1. Pair a device.
2. Request runtime connection details.
3. Open the returned WebSocket URL.
4. Send the bearer token in the `Authorization` header.
5. Exchange ACP messages over the socket.

## Notes

This document is intentionally high-level. For implementation details, see the code in `internal/api`.
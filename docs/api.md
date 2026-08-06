# API Surface

Ferngeist Gateway exposes two HTTP surfaces:

- **Public API** for paired clients and ACP agent control
- **Admin API** for local management and diagnostics

The public API is the one clients use most of the time. It exposes a WebSocket bridge for ACP traffic plus the endpoints needed for pairing, status, runtime control, resilient sessions, and push notification registration.

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
  - Returns the issued device credential, plus `gatewayId` — this gateway's
    stable instance identifier. It is user-independent and never changes for the
    lifetime of the state database (unlike the gateway *name*, which can be
    renamed). Clients should store it as the server identity and use it to address
    this gateway and to resolve incoming pushes back to the right server entry for
    deep-linking (see [Push notification registration](#push-notification-registration)).

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
  - Creates a runtime token and starts a gateway session for ACP traffic.
  - Requires `control` scope.
  - Response:
    ```json
    {
      "runtimeId": "string",
      "protocol": "string",
      "scheme": "string",
      "host": "string",
      "websocketUrl": "string",
      "websocketPath": "string",
      "bearerToken": "string",
      "tokenExpiresAt": "2026-05-22T10:00:00Z",
      "sessionId": "string",
      "attachToken": "string"
    }
    ```
  - Creates a persistent gateway session with push notification support. The `sessionId` and `attachToken` fields are populated. The client should store `sessionId` for reconnection.
  - The `sessionId` is a **gateway session identifier** (an opaque random string that identifies the gateway's internal session object). It is NOT an ACP agent session ID — that is negotiated between the client and agent during ACP initialization and is unrelated to this field.
  - Session creation is best-effort. If it fails, the response still contains a valid connection descriptor (the `sessionId`/`attachToken` fields are simply empty).

- `POST /v1/runtimes/{runtimeId}/restart`
  - Restarts a runtime, optionally with environment overrides.
  - Requires `control` scope.
  - Runtime restart with environment overrides may be disabled by configuration.

### Workspace browsing

The workspace endpoints are **read-only** views into the code the agent is
currently working on. They are anchored to the **project directory the client
opened in the agent** — captured from the ACP `session/new` request's
`params.cwd` as it flows through the gateway (and re-captured from
`session/load`, which Ferngeist sends when resuming a session) — not to the
gateway's own launch working directory. All file and git access is confined to
that directory: any path that escapes it (absolute path, `..` traversal, or
symlink escape) is rejected with `400`. "Not found / not known yet" cases return
`404`, and git failures (no `git` on PATH, or the cwd is not a git repository)
return `422`.

- `GET /v1/runtimes/{runtimeId}/files?path=<rel>`
  - Reads a file inside the runtime's project directory.
  - Requires `read` scope.
  - `path` is a repository-relative path (required). The filesystem root is the
    ACP project directory from `session/new` / `session/load`.
  - The response reuses the ACP v1 schema's resource types (see the [ACP v1
    schema](https://agentclientprotocol.com/protocol/v1/schema)). Text files
    return a [`TextResourceContents`](https://agentclientprotocol.com/protocol/v1/schema#textresourcecontents)
    (`text`/`uri`/`mimeType`); binary files return a
    [`BlobResourceContents`](https://agentclientprotocol.com/protocol/v1/schema#blobresourcecontents)
    (`blob` base64/`uri`/`mimeType`). `size` and `truncated` are **gateway
    extensions** (not ACP fields); `uri` carries the absolute `file://` URI.
    The Ferngeist client can decode these with its existing SDK types
    (`acp.TextResourceContents` / `acp.BlobResourceContents`).
  - Text response:
    ```json
    {
      "text": "…file contents…",
      "uri": "file:///path/to/notes.txt",
      "mimeType": "text/plain; charset=utf-8",
      "size": 8767,
      "truncated": false
    }
    ```
  - Binary response:
    ```json
    {
      "blob": "AAECAw==",
      "uri": "file:///path/to/clip.png",
      "mimeType": "image/png",
      "size": 4,
      "truncated": false
    }
    ```
  - Files larger than 1 MiB are truncated; `truncated` is `true` and `size` is
    the full on-disk file size (the payload `text`/`blob` is capped at 1 MiB).
    Binary detection uses the same NUL-byte heuristic
    as git. `mimeType` is a best-effort guess from the file extension and is
    omitted when the extension is not recognized (e.g. `.go` on platforms
    without a system mime.types database).
  - Error responses:
    - `400` — missing `path`, or path escapes the project directory (absolute,
      traversal, or symlink escape)
    - `400` — path is a directory
    - `404` — file not found
    - `404` — runtime has no session, or the client has not issued
      `session/new`/`session/load` yet (so the working directory is unknown)

- `GET /v1/runtimes/{runtimeId}/git/status`
  - Returns branch, ahead/behind counts, and changed files (with per-file line
    counts) for the runtime's project directory.
  - Requires `read` scope.
  - Response:
    ```json
    {
      "branch": "main",
      "ahead": 2,
      "behind": 0,
      "changed": [
        { "path": "wip.txt", "status": "?", "added": 1, "removed": 0, "binary": false },
        { "path": "edited.txt", "status": "M", "added": 1, "removed": 1, "binary": false }
      ]
    }
    ```
  - `status` is a single porcelain letter: `M` modified, `A` added, `D` deleted,
    `R` renamed, `?` untracked, etc.
  - `added` / `removed` are per-file line counts from `git diff HEAD --numstat`:
    lines added / removed over the whole `HEAD` → working-tree delta (the same
    comparison `/git/diff` reports), so partially staged files count their
    staged and unstaged edits together. For untracked files (`?`) — which
    `git diff` does not cover — `added` is the file's raw line count and
    `removed` is `0`.
  - `binary` is `true` when git reports no counts for a binary file (`added` /
    `removed` are then `0`), and for untracked files that contain a NUL byte.
  - **No ACP schema equivalent:** git status has no counterpart in the ACP v1
    schema, so this shape is **gateway-defined**. `status` uses git porcelain
    letters (`M`, `A`, `D`, `R`, `?`, ...); `added`/`removed`/`binary` are
    line-count extensions. The Ferngeist client defines this type itself rather
    than reusing an SDK type.
  - Error responses:
    - `404` — runtime has no session, or working directory unknown (see above)
    - `422` — `git` is missing from PATH, or the directory is not a git repository

- `GET /v1/runtimes/{runtimeId}/git/diff?path=<rel>`
  - Returns the diff for a single file (`?path=`), or for the whole working tree
    when `path` is omitted.
  - Requires `read` scope.
  - The response always uses the ACP v1 schema's
    [`ToolCallContentDiff`](https://agentclientprotocol.com/protocol/v1/schema)
    shape (`path`/`oldText`/`newText`/`type:"diff"`), so the Ferngeist client
    can decode it with its existing `acp.ToolCallContentDiff` SDK type. With
    `?path=` the body is a **single object**; without it, the body is a **JSON
    array** with one object per changed file (the same file set `/git/status`
    reports).
  - `oldText` is the committed version of the file (`git show HEAD:<path>`);
    it is omitted (absent from the JSON, not `null`) for new/untracked files
    or when there is no HEAD commit. `newText` is the working-tree file content
    (empty for deleted files). `type` is always `"diff"`.
  - Single-file response:
    ```json
    {
      "path": "edited.txt",
      "oldText": "old\n",
      "newText": "new\n",
      "type": "diff"
    }
    ```
  - Whole-tree response:
    ```json
    [
      { "path": "edited.txt", "oldText": "old\n", "newText": "new\n", "type": "diff" },
      { "path": "wip.txt", "newText": "wip\n", "type": "diff" }
    ]
    ```
  - **Breaking change:** this endpoint previously returned a raw unified patch
    (`{path?, diff}`). It now returns `ToolCallContentDiff` objects, so the
    client must render from `oldText`/`newText` instead of parsing a patch.
    The whole-tree array covers the same file set as `/git/status`: staged,
    unstaged, and untracked files, each compared against `HEAD`. `oldText` is
    the committed version and is present only when the file exists in `HEAD`
    (so it is omitted for new and untracked files, and for files renamed
    within the working tree); committed history beyond `HEAD` is not included.
  - Both `oldText` and `newText` are truncated to 1 MiB.
  - Error responses:
    - `400` — `path` escapes the project directory (absolute, traversal, or
      symlink escape)
    - `404` — runtime has no session, or working directory unknown (see above)
    - `422` — `git` is missing from PATH, or the directory is not a git repository

### Gateway sessions

> **Terminology note:** A "gateway session" is a gateway-internal object that keeps a runtime alive across WebSocket disconnections. It manages a stdio pump (for agent stdout), an exclusive pipe lease, and push notification dispatch on notable events. It is not an ACP agent session — ACP agent sessions are negotiated between the client and agent during protocol initialization and are not tracked by this API.

- `POST /v1/sessions/{sessionId}/resume`
  - Prepares a disconnected gateway session for WebSocket reconnection.
  - Authenticated with the device credential (bearer token).
  - Returns a new single-use attach token.
  - Response:
    ```json
    {
      "attachToken": "string"
    }
    ```
  - Error responses:
    - `400` — session not found, device mismatch, or session is in a non-resumable status
    - `401` — invalid or missing device credential
    - `503` — session service not available

- `GET /v1/sessions`
  - Lists all gateway sessions belonging to the authenticated device.
  - Authenticated with the device credential (bearer token).
  - Results ordered by `created_at DESC` (newest first).
  - Response:
    ```json
    {
      "sessions": [
        {
          "sessionId": "string",
          "runtimeId": "string",
          "agentId": "string",
          "status": "active",
          "createdAt": "2026-05-22T10:00:00Z"
        }
      ]
    }
    ```
  - Status values: `active`, `disconnected`, `closing`, `failed`

- `DELETE /v1/sessions/{sessionId}`
  - Closes a gateway session. Stops the backing runtime, releases the pipe lease, and deletes all session data.
  - Authenticated with the device credential (bearer token).
  - Response: `204 No Content`
  - Error responses:
    - `400` — session not found or device mismatch
    - `401` — invalid or missing device credential
    - `503` — session service not available

### Push notification registration

- `POST /v1/devices/push-token`
  - Registers or updates the calling device's push token. The device identity is
    taken from the authenticated credential, **never from the body** — the body
    carries only the token and the platform it was issued for.
  - Authenticated with the device credential (bearer token + proof-of-possession
    headers, the same scheme as every other `/v1` route).
  - Request body:
    ```json
    {
      "token": "string",
      "platform": "android"
    }
    ```
    - `platform` is the routing key the gateway uses to pick a delivery provider
      (`android` today; `ios`/`web` are reserved for future clients). It is
      optional and defaults to `android` if omitted.
  - **Idempotent.** The client re-POSTs on every app start and whenever the token
    rotates, once per paired gateway; the gateway upserts one token per device,
    replacing any prior token.
  - Response: `204 No Content`
  - Error responses:
    - `400` — missing or empty `token`
    - `401` — invalid or missing device credential

**Client obligation.** After pairing, the client must register its push token
here, and re-register whenever the platform rotates the token. A device with no
registered token simply receives no pushes — delivery is best-effort and never
blocks a session.

**Delivery payload (hybrid notification + data).** Pushes are sent as **hybrid**
messages carrying both an FCM `notification` block and a `data` block. The
`notification` block (title, body) is what lets Android display the alert when the
app is **killed** — the system renders it with no app process running. The `data`
block duplicates the title/body and adds the deep-link keys; the **foreground**
client reads `data` to suppress the duplicate and route a tap into the right chat.
For FCM the data keys are:

| key         | meaning                                                              |
|-------------|---------------------------------------------------------------------|
| `title`     | notification title                                                  |
| `body`      | notification body                                                   |
| `category`  | event kind — `turn_complete`, `permission_request`, `agent_error`, `agent_crash`, or `progress` |
| `serverId`  | the gateway's `gatewayId` (from pairing); deep-links with `sessionId` |
| `sessionId` | target gateway session/chat                                         |
| `cwd`       | working directory for the chat route, when known                    |

All `data` values are strings. A push deep-links into a chat only when it carries
**both** `serverId` and `sessionId`; otherwise a tap just opens the app. Empty
optional fields are omitted from `data`.

The message also carries an `android` block with `priority: high` (to wake the
device promptly) and a per-category `channel_id`: alert-worthy events
(`permission_request`, `agent_error`, `agent_crash`) route to the heads-up
`ferngeist_push` channel, and `turn_complete` and `progress` route to the quiet
`ferngeist_push_updates` channel. `progress` pushes are throttled to one per
`FERNGEIST_GATEWAY_PROGRESS_INTERVAL_SECONDS` (default 15s) while the agent is
mid-turn, and always fire on each tool call that completes or fails.

> A force-stopped app (Settings → Force Stop, or some OEM task-killers) cannot
> receive any push until the user reopens it — an Android platform rule. Normal
> "killed" (swiped from recents, reaped for memory) delivers fine.

### ACP bridge

- `GET /v1/acp/{runtimeId}`
  - WebSocket endpoint for ACP traffic.
  - Pass `?sessionId=<id>&attachToken=<token>` as query params. The session ID and initial attach token are obtained from `POST /v1/runtimes/{id}/connect`. For reconnects, a fresh attach token is obtained from `POST /v1/sessions/{id}/resume`.
  - On reconnect, the client is responsible for calling the ACP `session/load` method on the agent for context restoration. The gateway does not replay old frames.

### Attach tokens

Attach tokens are single-use, short-lived (5-minute TTL) nonces used to prove ownership of a gateway session at WebSocket connect time. They are:

- Minted on gateway session creation (`POST /v1/runtimes/{id}/connect`)
- Minted on gateway session resume (`POST /v1/sessions/{id}/resume`)
- Consumed on first WebSocket connect (`GET /v1/acp/{runtimeId}?sessionId=&attachToken=`)
- 64 hex characters (32 random bytes)
- Stored in memory only (not persisted to SQLite)

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

**Resilient gateway session flow (disconnect-tolerant):**

1. Pair a device.
2. `POST /v1/runtimes/{id}/connect` — get `sessionId`, `attachToken`, and connection details.
3. `GET /v1/acp/{runtimeId}?sessionId=<id>&attachToken=<token>` — WebSocket connect.
4. Exchange ACP messages. Disconnect does NOT kill the runtime; the gateway session stays `disconnected`. The gateway sends hybrid notification+data push notifications on notable events (turn complete, permission request, agent error, agent crash) regardless of whether a client is attached — the client suppresses them in the foreground and the system displays them when backgrounded or killed (when `FERNGEIST_GATEWAY_FCM_CREDENTIALS_FILE` is configured; otherwise these are logged only).
5. Register the push token: `POST /v1/devices/push-token` (authenticated with device credential).
6. To reconnect: `POST /v1/sessions/{id}/resume` (authenticated with device credential) — get new `attachToken`.
7. `GET /v1/acp/{runtimeId}?sessionId=<id>&attachToken=<token>` — live proxying resumes. The client calls `session/load` on the agent for context restoration.
8. `DELETE /v1/sessions/{id}` — close the gateway session and stop the runtime.

## Notes

This document is intentionally high-level. For implementation details, see the code in `internal/api` and `internal/session`.

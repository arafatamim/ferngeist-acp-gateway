# Architecture

Ferngeist Gateway is a self-hosted backend for ACP-compatible agents.

Its main job is to expose ACP agents through a unified WebSocket API. It discovers supported agents, launches them when needed, manages pairing so clients can talk to them through one authenticated endpoint, and keeps agents alive across WebSocket disconnections with resilient sessions, push notification wake-up, and seamless reconnection.

## Main parts

- `cmd/ferngeist` — CLI entrypoint for daemon, pairing, and device management
- `internal/api` — public and admin HTTP APIs, WebSocket bridge, session handlers
- `internal/push` — platform-neutral push dispatcher with pluggable delivery providers (FCM, log-only)
- `internal/gateway` — runtime token issuance, attach token and validation
- `internal/pairing` — pairing flow and device credentials
- `internal/runtime` — process supervision, lease-based transport bridging
- `internal/session` — resilient session domain: session lifecycle, stdio pump, inbound diagnostics
- `internal/token` — attach token hashing and validation
- `internal/catalog` — supported agent discovery and validation
- `internal/registry` — ACP registry fetch and cache
- `internal/storage` — SQLite persistence (sessions, pairings, runtimes, push tokens, gateway identity, inbound diagnostics)
- `internal/discovery` — LAN advertising via mDNS
- `internal/config` — configuration and persisted settings
- `internal/daemon` — wiring and startup reconciliation

## Resilient gateway sessions

Every connection creates a persistent gateway session with:

- **StdioPump** — a long-lived goroutine that drains agent stdout and forwards frames to the WebSocket when a client is attached. When no client is connected, output is discarded after end-turn detection and log append. The pump owns the pipe lifecycle and runs independently of WebSocket connectivity.
- **Lease** — the session holds an exclusive lease on the runtime's stdio pipes via `AcquireLease`/`ReleaseLease`. The runtime is not stopped on WebSocket disconnect — only the leaseholder string is cleared. Sessions always stop the runtime on close.
- **Push notifications** — when the pump detects a notable event (turn complete, permission request, agent error, or live progress) — or the runtime crashes — it emits a platform-neutral notification through the push dispatcher, which routes it to the device's provider (FCM when credentials are configured, otherwise log-only). Pushes fire **regardless of whether a client is attached**: the gateway can't tell whether the app is foregrounded or backgrounded (only whether a socket is attached, a poor proxy), so it always emits a hybrid notification+data message — the foreground client suppresses the duplicate, while the system displays the `notification` block when the app is backgrounded or killed. The client reconnects and calls `session/load` on the agent for context restoration.
- **Inbound diagnostics** — client-to-agent frames are logged asynchronously to SQLite via a buffered channel (non-blocking, dropped on overflow with counter).
- **ACP session/close** — before stopping the runtime on session close, the gateway sends a `session/close` JSON-RPC request to the agent if it advertised `sessionCapabilities.close` during initialize. The mock agent supports this for testing.

There is no ring buffer or catchup replay. On WebSocket disconnect:
1. The pump continues running, discarding agent output.
2. On notable events (turn complete, permission request, agent error, agent crash, or live progress), a push notification is dispatched (if a push service is configured). Pushes also fire while a client is attached — the client suppresses foreground notifications — so disconnection is not what gates them.
3. The client reconnects, calls `session/load` on the agent, and resumes live proxying.

## Session lifecycle

1. `POST /v1/runtimes/{id}/connect` → session created, connect descriptor with bearer token, sessionId, and attachToken returned
2. `GET /v1/acp/{runtimeId}?sessionId=<id>&attachToken=<token>` → initial WebSocket connect
3. ACP messages flow bidirectionally via pump + proxy
4. WebSocket disconnects → pump keeps running, session → `disconnected`
5. `POST /v1/sessions/{id}/resume` → new attach token minted
6. `GET /v1/acp/{runtimeId}?sessionId=&attachToken=` → sets pump client, live proxying resumes (client calls `session/load` on agent for context)
7. `DELETE /v1/sessions/{id}` → mark `closing`, stop pump, send ACP `session/close`, stop runtime (2s graceful timeout), release lease, delete from storage

### Session close ordering

1. Mark session status `closing` in SQLite
2. Stop the stdio pump (context cancellation)
3. If the agent advertised `sessionCapabilities.close`, send `{"jsonrpc":"2.0","method":"session/close",...}` to stdin
4. `StopByRuntimeID` with 2-second graceful timeout
5. Release the pipe lease
6. Delete session record from SQLite

## Data flow

1. A client pairs with the gateway.
2. The gateway stores device credentials.
3. The client requests an ACP agent through the API.
4. The gateway launches or connects to the target agent.
5. ACP traffic is bridged over WebSocket through a single authenticated endpoint.
6. The bridge survives WebSocket disconnection via the stdio pump + push notification wake-up.

## Push notifications

The push subsystem is split into a **platform-neutral core** and **pluggable
delivery providers** so the gateway can back any Ferngeist client, not just
Android. The session layer never speaks a wire format — it emits a neutral
`push.Notification` and the dispatcher routes it to the right provider by the
device's registered platform.

- `internal/push/service.go` defines:
  - `Notification` — the semantic event (title, body, category, and optional
    deep-link fields `serverId`/`sessionId`/`cwd`), free of any transport detail.
  - `PushService` with `Notify(ctx, deviceID, Notification)` — the gateway-facing
    entry point.
  - `Provider` with `Send(ctx, token, Notification)` — a per-platform transport,
    and the `ErrTokenUnregistered` sentinel a provider returns for dead tokens.
- `internal/push/dispatcher.go` provides `Dispatcher` (the `PushService`):
  - Resolves the device's `(token, platform)` from the `device_push_tokens` table.
  - Skips silently when the device has no token, or no provider is registered for
    its platform.
  - Routes delivery to the platform's `Provider`.
  - **Owns dead-token eviction** (platform-neutral): on `ErrTokenUnregistered` it
    deletes the token so it is not retried. Only genuine retryable failures are
    returned to the caller.
- `internal/push/fcm.go` provides `FCMProvider`, the Android transport (FCM HTTP v1):
  - Reads a Firebase service-account JSON file (path from `FERNGEIST_GATEWAY_FCM_CREDENTIALS_FILE`)
    and authenticates via OAuth2 with the `firebase.messaging` scope.
  - Sends **hybrid notification+data** messages: the FCM `notification` block lets
    a killed app's system display the alert, while the `data` block duplicates
    title/body and carries the deep-link keys for the foreground client. Adds an
    `android` block with high priority and a per-category channel (`ferngeist_push`
    for alerts, `ferngeist_push_updates` for quiet updates — `turn_complete` and
    live `progress` use the quiet channel). Reports
    `ErrTokenUnregistered` on `UNREGISTERED`/404.
  - Is store-free — token lookup and eviction live in the dispatcher.
- `internal/push/log.go` provides `LogProvider`, which logs instead of delivering.
  The daemon registers it for the `android` platform when no Firebase credentials
  are configured or they fail to load, so a bad push config never blocks boot.
- The session calls `PushSvc.Notify` (10-second context timeout) on each notable
  event, **regardless of client attachment** — the client suppresses foreground
  notifications, a foreground/background distinction the gateway can't make.
  Delivery is dispatched on its own goroutine so a slow provider never stalls the
  stdout drain loop (and the attached client's live stream). The crash push fires
  only on a genuine, unexpected exit — an intentional Close or reaper expiry marks
  the session `closing` first, so it is not reported as a crash. `Config.PushSvc`
  is nil-able (when nil, push is disabled). The neutral `Notification` carries the
  gateway's `gatewayId` as `serverId` for deep-linking.
- **Live progress** — the pump parses each agent stdout line for ACP
  `session/update` frames with a `tool_call` or `tool_call_update` discriminator
  and emits a `progress` push so a phone user sees the agent working mid-turn.
  The summary resolves from the frame's `title`, then the first `content` text
  block, then a verb derived from `kind` (e.g. "Running a command"). Non-terminal
  updates (`in_progress`) are deduplicated by `(toolCallId, summary)` and
  throttled to one push per `FERNGEIST_GATEWAY_PROGRESS_INTERVAL_SECONDS`
  (default 15s); terminal updates (`completed`/`failed`) always push immediately
  so the user sees the tool boundary. Progress pushes route to the quiet
  `ferngeist_push_updates` channel.
- `POST /v1/devices/push-token` registers/updates a device's `(token, platform)`,
  stored in SQLite. The interface only sends; registration is via the API endpoint.

Adding a platform (iOS/Web) is additive: implement another `Provider`, register it
under its platform key in the daemon. Neither the dispatcher nor the session layer
changes. (FCM itself can also relay to APNs/Web Push via override blocks, so a
single `FCMProvider` may cover multiple platforms.)

## Workspace browsing

The gateway exposes **read-only** views into the code the agent is working on
(files, git status, git diff) so a phone client can inspect the project without
an editor. The anchor is the **project directory the client opened in the
agent**: the pump snoops the ACP `session/new` request's `params.cwd` inbound
(and re-captures it from `session/load`, which Ferngeist sends when resuming a
session) in `internal/session/pump.go`, holds it on the session under the same
lock as the ACP session id, and surfaces it via
`RuntimeSession.WorkingDir(runtimeID)` (`internal/session/lifecycle.go`). This is
deliberately **not** the launch working directory — PATH-resolved agents run in
the gateway's own cwd, which is not the project.

The three handlers live in `internal/api/workspace.go` and are gated behind the
same `ScopeRead` auth used by `handleRuntimeLogs`. Their payloads reuse the ACP
**v1** schema types vendored in `github.com/coder/acp-go-sdk` (package `acp`)
so the Ferngeist client can decode them with SDK types it already has:

- `GET /v1/runtimes/{id}/files?path=<rel>` — bounded file read confined to the
  project directory (streams through `io.LimitReader` at the 1 MiB cap, so a
  huge file is never buffered in full; `truncated` flag). Text files return an
  `acp.TextResourceContents` (`text`/`uri`/`mimeType`); binary files (NUL-byte
  detection) return an `acp.BlobResourceContents` (`blob` base64/`uri`/`mimeType`).
  `size`/`truncated` are gateway extensions (`size` is the full on-disk file
  size); `uri` is the absolute `file://` URI.
- `GET /v1/runtimes/{id}/git/status` — `git -C <cwd> status --porcelain=v2 --branch`,
  enriched with per-file line counts (`git diff HEAD --numstat`, covering the
  whole HEAD → working-tree delta so staged + unstaged edits count together;
  a `--cached` fallback covers repos with no HEAD commit) and raw line counts
  for untracked files. Has **no ACP equivalent**; the shape is gateway-defined
  and documented as such.
- `GET /v1/runtimes/{id}/git/diff?path=<rel>` — always returns
  `acp.ToolCallContentDiff` (`path`/`oldText`/`newText`/`type:"diff"`): a single
  object with `?path=`, otherwise a JSON array with one entry per changed file.
  `oldText` comes from `git show HEAD:<path>`, `newText` from the working copy.
  This replaced the old raw unified-patch shape (`{path?,diff}`) — a breaking
  change coordinated with the client. The whole-tree array covers the same file
  set as `/git/status` (staged, unstaged, and untracked files), each compared
  against `HEAD`; `oldText` is present only when the file exists in `HEAD`, so
  new/untracked files and files renamed within the working tree have it
  omitted. Committed history beyond `HEAD` is not included.

**Path confinement.** `resolveWithinRoot` rejects any path that escapes the
project directory: absolute paths, `..` traversal, and symlink escapes (both the
root and the resolved target are `EvalSymlinks`-checked). Rejections are `400`.
Because runtimes are gateway-global resources (the same model as
`handleRuntimeLogs`), there is no per-device runtime ownership on these routes.

**Git.** Every invocation is `git -C <cwd> <subcommand>` under a 15-second
context timeout (the server `WriteTimeout` is 20s), so a locked or hanging repo
cannot stall a handler. Missing `git` or a non-repository cwd returns `422`. No
Go git library is used; read-only only — no `add`/`commit`, no file writes.

## Inbound diagnostics

Client-to-agent messages are recorded for debugging/audit. Direction is always client→agent. Never replayed. Written to SQLite `session_inbound_log` table asynchronously via a buffered channel (256 entries). Non-blocking send — if the channel is full, the frame is dropped and a counter is incremented. The hot path never blocks on I/O.

## Attach tokens

Single-use, short-lived (5-minute TTL) nonces used to prove ownership of a session at WebSocket connect time. Minted on session creation and resume. Consumed on first WebSocket connect. 64 hex characters (32 random bytes). Stored in memory only (not persisted to SQLite). Hashed via SHA-256 for storage; token service in `internal/token/`.

## Startup reconciliation

On daemon restart, all sessions in `active` or `disconnected` status are transitioned to `failed` in SQLite, since their backing processes are gone.

## Notes

- Agent support comes from the ACP registry plus local helper adapters.
- The gateway can auto-acquire managed binaries when supported.
- The Ferngeist Android app uses this service as its backend.
- See [docs/api.md](api.md) for the full API surface, including session endpoints and push token registration.

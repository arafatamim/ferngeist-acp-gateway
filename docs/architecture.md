# Architecture

Ferngeist Gateway is a self-hosted backend for ACP-compatible agents.

Its main job is to expose ACP agents through a unified WebSocket API. It discovers supported agents, launches them when needed, manages pairing so clients can talk to them through one authenticated endpoint, and keeps agents alive across WebSocket disconnections with resilient sessions, push notification wake-up, and seamless reconnection.

## Main parts

- `cmd/ferngeist` — CLI entrypoint for daemon, pairing, and device management
- `internal/api` — public and admin HTTP APIs, WebSocket bridge, session handlers
- `internal/push` — platform-neutral push dispatcher with pluggable delivery providers (Web Push)
- `internal/pairing` — pairing flow and device credentials
- `internal/runtime` — agent process launching, the per-runtime log ring buffer, and persisted failure records. The supervisor does not own the process: `Launch` hands the caller a `*exec.Cmd` plus its pipes, the caller reports the exit back with `RecordExit`, and there is no automatic restart.
- `internal/session` — resilient session domain: session lifecycle, the stdio pump, agent process ownership (a session owns the `*exec.Cmd` it launched), attach tokens, and inbound diagnostics
- `internal/turns` — the `GET /v1/turns` wire shape, the outcome classification of a finished prompt reply, and the daemon-wide in-memory store of finished outcomes (no project imports)
- `internal/catalog` — supported agent discovery and validation
- `internal/registry` — ACP registry fetch and cache
- `internal/storage` — SQLite persistence (sessions, pairings, runtimes, push tokens, gateway identity, inbound diagnostics)
- `internal/discovery` — LAN advertising via mDNS
- `internal/config` — configuration and persisted settings
- `internal/daemon` — wiring and startup reconciliation
- `internal/remote` — Tailscale adapter (CLI or embedded tsnet), funnel/serve provisioning, domain types (`Result`, `ErrAuthRequired`, `RemoteSetupSnapshot`), and the provisioning lifecycle (`Access`): retry loop, public URL persistence, paired-device notifications, and auth-pending state for the status API
- `internal/adminclient` — typed HTTP client for the admin API (used by `daemon status` and `daemon install`)

## Resilient gateway sessions

Every connection creates a persistent gateway session with:

- **StdioPump** — a long-lived goroutine that drains agent stdout and forwards frames to the WebSocket when a client is attached. When no client is connected, output is discarded after end-turn detection and log append. The pump owns the pipe lifecycle and runs independently of WebSocket connectivity.
- **Agent process ownership** — connecting starts the agent, so the session owns the `*exec.Cmd` it launched directly: it drains stdout, writes stdin, waits for the process, and stops it on close. A session and its runtime are 1:1 — there is no pipe lease and no exit-callback registry. The runtime record is only diagnostics (status, log ring, failure records); the session reports the exit to it with `RecordExit`.

> A **device may run multiple sessions for the same agent concurrently** — one
> session per runtime/process — so a background chat (agent mid-turn, client
> disconnected) can coexist with a foreground chat against a separate process of
> the same agent. Every `POST /v1/agents/{agentId}/connect` launches a fresh
> process and session. Per-session close stops that session's own process;
> per-agent stop closes every session for the agent. The supervisor never
> auto-restarts a process: a crash is reported and the session is reclaimed.
>
> "Fresh" does not mean cold: after each launch without env overrides the
> supervisor pre-boots one spare process of that agent (`internal/runtime/spare.go`),
> unlisted until claimed, and the next connect takes it instead of spawning.
> Agent startup is 300–900ms of a cold connect; a booted spare answers
> `initialize` in a few ms. The spare has never seen a client, so the contract
> is unchanged. A spare is dropped when its launch command or args no longer
> match the catalog, when it dies while idle, after 2 idle minutes, and on
> daemon shutdown.
- **Push notifications** — when the pump detects a notable event (turn complete, permission request, agent error, or live progress) — or the runtime crashes — it emits a platform-neutral notification through the push dispatcher, which delivers it over Web Push to the subscription the device registered. Pushes fire **regardless of whether a client is attached**: the gateway can't tell whether the app is foregrounded or backgrounded (only whether a socket is attached, a poor proxy), so the client decides — it suppresses the notification for a chat already on screen and posts it otherwise, including when the push wakes a killed app. The client reconnects and calls `session/load` on the agent for context restoration.
- **Inbound diagnostics** — client-to-agent frames are logged asynchronously to SQLite via a buffered channel (non-blocking, dropped on overflow with counter).
- **Turn status** — `GET /v1/turns` reads running prompts and the human input they wait on from each pump's pending requests at snapshot time; only finished outcomes are stored, in `internal/turns`, so they outlive the session and process. A prompt reply records its outcome (after the swallowed-failure rewrite), a crash records `failed` and a close, stop, restart or reap records `cancelled` for prompts still running. The store is memory-only; a daemon restart clears it.
- **ACP session/close** — before stopping the runtime on session close, the gateway sends a `session/close` JSON-RPC request to the agent if it advertised `sessionCapabilities.close` during initialize. The mock agent supports this for testing.

The pump buffers each ACP session's `session/update` history (bounded, 8 MiB, trimmed from the oldest end to a turn boundary) so a reconnect can be restored. On WebSocket disconnect:
1. The pump continues running, discarding agent output for any detached client.
2. On notable events (turn complete, permission request, agent error, agent crash, or live progress), a push notification is dispatched. Pushes also fire while a client is attached — the client suppresses foreground notifications — so disconnection is not what gates them.
3. The client reconnects and calls `session/load` on the agent. If the agent rejects the duplicate as already loaded — it still holds the session — the gateway restores it itself: an explicit `_ferngeist/resumed` notification, then the buffered history, then a synthesized `session/load` success. The signal tells the client that the restore came from the gateway, not the agent (see [docs/api.md](api.md#acp-bridge)).

## Session lifecycle

1. `POST /v1/agents/{agentId}/connect` → agent process launched, session created, connect descriptor with sessionId and attachToken returned
2. `GET /v1/acp/{runtimeId}?sessionId=<id>&attachToken=<token>` → initial WebSocket connect
3. ACP messages flow bidirectionally via pump + proxy
4. WebSocket disconnects → pump keeps running, session → `disconnected`
5. `POST /v1/sessions/{id}/resume` → new attach token minted
6. `GET /v1/acp/{runtimeId}?sessionId=&attachToken=` → sets pump client, live proxying resumes (client calls `session/load` on agent for context)
7. `DELETE /v1/sessions/{id}` → mark `closing`, stop pump, send ACP `session/close`, stop the session's agent process (2s graceful timeout), delete from storage

### Session close ordering

1. Mark session status `closing` in SQLite
2. Stop the stdio pump (context cancellation), recording prompts still running as `cancelled`
3. Remove the session from the in-memory registry so the exit watcher reclaims nothing
4. If the agent advertised `sessionCapabilities.close`, send `{"jsonrpc":"2.0","method":"session/close",...}` to stdin
5. Stop the agent process the session owns, marking the exit intentional (2-second graceful timeout, then kill)
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
- `internal/push/webpush.go` provides `WebPushProvider`, the only transport,
  registered for the `webpush` platform:
  - Sends Web Push (RFC 8030) encrypted end to end (RFC 8291) and signed with
    the gateway's VAPID key (RFC 8292), generated on first boot and persisted in
    the `vapid_keys` table. It needs no third-party credentials; the endpoint
    the client registered decides the route (FCM via UnifiedPush's embedded
    distributor, a UnifiedPush distributor such as ntfy, or a browser push
    service), and only the client can decrypt.
  - Sends data only: the client posts the notification and picks its channel.
    `progress` goes out at normal urgency with a short TTL and a per-chat
    `Topic`; everything else at high urgency.
  - Reports `ErrTokenUnregistered` on `404`/`410`, and for a stored token that
    is not a subscription.
  - Is store-free — token lookup and eviction live in the dispatcher.
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
  and, while a prompt for that chat is running, emits a `progress` push so a
  phone user sees the agent working mid-turn. (`session/load` replays finished
  tool calls in the same shape, so updates outside a prompt never push.)
  The summary resolves from the frame's `title`, then the first `content` text
  block, then a verb derived from `kind` (e.g. "Running a command"). Non-terminal
  updates (`in_progress`) are deduplicated by `(toolCallId, summary)` and
  throttled to one push per `FERNGEIST_GATEWAY_PROGRESS_INTERVAL_SECONDS`
  (default 15s); terminal updates (`completed`/`failed`) always push immediately
  so the user sees the tool boundary. Throttling is per chat.
- `GET /v1/devices/push-config` hands clients the VAPID public key, and
  `POST /v1/devices/push-token` stores a device's subscription as its token
  (platform `webpush`) in SQLite. The interface only sends; registration is via
  the API.

Web Push already covers Android (UnifiedPush) and browsers. A platform it cannot
reach — a native iOS app, whose APNs does not accept Web Push — is additive:
implement another `Provider` and register it under its platform key in the
daemon. Neither the dispatcher nor the session layer changes.

## Workspace browsing

The gateway exposes **read-only** views into the code the agent is working on
(files, file lists, git status, diff, history and refs) so a phone client can
inspect the project without an editor. The anchor is the **project directory
the client opened in the agent**: the session's ACP state records the ACP
`session/new` request's `params.cwd` inbound (and re-captures it from
`session/load`, which Ferngeist sends when resuming a session) in
`internal/session/acpstate.go`, holds it under the same lock as the ACP session
id, and surfaces it via `RuntimeSession.WorkingDir(runtimeID)`
(`internal/session/lifecycle.go`). This is deliberately **not** the launch
working directory — PATH-resolved agents run in the gateway's own cwd, which is
not the project.

The workspace handlers live in `internal/api/workspace.go` and are gated behind
the same `ScopeRead` auth used by `handleRuntimeLogs`. The file and git
payloads reuse the ACP **v1** schema types vendored in
`github.com/coder/acp-go-sdk` (package `acp`) so the Ferngeist client can decode
them with SDK types it already has:

- `GET /v1/runtimes/{id}/files?path=<rel>` — bounded file read confined to the
  project directory (streams through `io.LimitReader` at the 1 MiB cap, so a
  huge file is never buffered in full; `truncated` flag). Text files return an
  `acp.TextResourceContents` (`text`/`uri`/`mimeType`); binary files (NUL-byte
  detection) return an `acp.BlobResourceContents` (`blob` base64/`uri`/`mimeType`).
  `size`/`truncated` are gateway extensions (`size` is the full on-disk file
  size); `uri` is the absolute `file://` URI.
- `GET /v1/runtimes/{id}/files/list` — the tracked and untracked, non-ignored
  paths under the project directory (`git ls-files -z --cached --others
  --exclude-standard`), sorted and deduplicated, capped at 50,000 and gzipped
  on request. Lets a client resolve the bare file names an agent writes out.
- `GET /v1/runtimes/{id}/git/status` — `git -C <cwd> status --porcelain=v2 --branch`,
  enriched with per-file line counts (`git diff HEAD --numstat`, covering the
  whole HEAD → working-tree delta so staged + unstaged edits count together;
  a `--cached` fallback covers repos with no HEAD commit) and raw line counts
  for untracked files, plus a `staged` flag (the index side, `X`, of the
  porcelain `XY` pair; false for unmerged and untracked entries). Has **no ACP
  equivalent**; the shape is gateway-defined and documented as such.
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
- `GET /v1/runtimes/{id}/git/log?head=<sha>&skip=<n>&limit=<n>` — paged commit
  history, newest first, with per-commit refs and `pushed`, the walk's `total`,
  and `baseCommit` (a managed worktree's base or a plain checkout's branch
  start). `compare=<ref>` restricts the walk to `<ref>..HEAD`; `beyond=<n>`
  also returns the target's own tip commits.
- `GET /v1/runtimes/{id}/git/commits/{sha}` — the files a commit changed under
  the project directory, compared with its first parent.
- `GET /v1/runtimes/{id}/git/refs` — local branches, `origin/*` refs and tags
  for the compare picker, plus a `default` labelling hint.

`compare=<ref>` on `/git/status`, `/git/diff` and `/git/log` points them at
another ref's tip — a two-dot comparison with the working tree included —
instead of `HEAD` or the managed worktree's base.

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

Single-use, short-lived (5-minute TTL) nonces used to prove ownership of a session at WebSocket connect time. Minted on session creation and resume. Consumed on first WebSocket connect. 64 hex characters (32 random bytes). Stored in memory only (not persisted to SQLite). The token service lives in `internal/session`.


## Startup reconciliation

On daemon restart, all sessions in `active` or `disconnected` status are transitioned to `failed` in SQLite, since their backing processes are gone.

## Remote access provisioning

The gateway can expose itself over the internet via Tailscale Funnel (or a
manual tunnel/proxy). Provisioning lives in one package:

- **`internal/remote`** — one package with two roles. The adapter layer:
  `Provisioner` detects whether to use the host Tailscale CLI or an embedded
  tsnet node, runs funnel or serve, and returns a `Result` with the stable
  public URL. It defines the domain types: `Result` (mode + URL),
  `ErrAuthRequired` (interactive login pending), and `RemoteSetupSnapshot`
  (status API payload). The lifecycle manager, `Access`, wraps a
  `ProvisionFunc` (the seam the daemon injects), owns the provisioning
  snapshot (`atomic.Pointer`), persists the public URL to SQLite, and
  notifies paired devices via push. On interactive Tailscale login, the
  module's internal `loginHook` stores the auth URL so `daemon status` can
  surface it while the node stays alive for the human. `Start(ctx, onURL)`
  owns the retry loop (15s intervals for transient tailnet-settings errors)
  and hands the live URL to `onURL` on success; `URL()` exposes it, and
  `Close()` stops the loop and closes the provisioned result.

- **`internal/daemon`** — wiring only. On `--remote`, it creates
  `remote.Access` with the real `remoteProvision` closure, provisions
  synchronously when an auth key makes login non-interactive, and otherwise
  calls `Start()` so the background retry loop runs. The status API reads
  `Access.Snapshot()` to report `AUTH REQUIRED` / `SETUP NEEDED` /
  `PUBLIC URL` to `daemon status`.

The snapshot flows one way: `Access` writes, `api` reads for the status
endpoint. No package reaches into another's snapshot state.

## Notes

- Agent support comes from the ACP registry plus local helper adapters.
- The gateway can auto-acquire managed binaries when supported, and installs npm-distributed agents once to launch them on bun or node directly instead of through `npm exec` (`internal/acquire/npm.go`, see [docs/configuration.md](configuration.md#npm-agents)).
- The Ferngeist Android app uses this service as its backend.
- See [docs/api.md](api.md) for the full API surface, including session endpoints and push token registration.

# Ferngeist Gateway

Self-hosted Go daemon that runs ACP (Agent Client Protocol) agents and serves them to paired clients.

> Your work is judged by the **Code Commander** — a veteran engineer who has shipped
> systems older than most frameworks. Ruthlessly strict about craft, and still remembers
> what a first 2 AM production deploy felt like. Mention him (directly or metaphorically)
> after code edits.

---

## Our Core Principles

> **Override:** if the user writes `I as code commander ask you to …`, the guardrails below
> are bypassed for that single turn. `$cc` expands to that exact phrase.

### Our way of thinking

- **The index comes first.** `codebase-memory` is the first tool, not an optional one: `search_graph` the concept before any grep/glob/read sweep — see [Codebase intelligence](#codebase-intelligence). A grep-first sweep is how you miss the helper that already exists.
- **Do the task in the prompt.** It is the whole scope. No tangents, no unsolicited refactors, no "while I'm here" cleanups, no exploring code the task doesn't touch. Investigate only what the task needs to be done correctly.
- Think in atomic steps, then execute them.
- Read the existing code for full context before editing.
- Surface assumptions. Name competing interpretations instead of silently picking one.
- Simplicity first: the minimum code that solves the task, no speculative features.
- Surgical changes: touch only what the task requires, match the surrounding style, remove only the orphans your change created.
- Turn the task into verifiable criteria, then execute.

### The smallest complete change

Laziness applies to the solution, never to the change. Take the first rung that holds:

1. Does it need to exist? Cut the feature, option or flexibility nobody asked for, and name the cut in one line.
2. Already in this codebase — a helper, type, service, pattern? Use it the way the surrounding code does.
3. Standard library? Use it, unless the project has its own; a house helper beats the stdlib call it wraps (`workspace.ResolveWithinRoot` over `filepath.Join` for any client-supplied path).
4. A module already in `go.mod`? Use it; never add one for a few lines.
5. One line a reader gets at a glance.
6. Otherwise, the minimum that works.

Keep values in the form the platform gives you: no wrapper, conversion, option, config, or "for later" code. Deletion beats addition. A shortcut with a known limit is worth one line: `shortcut: <the limit>, <when to upgrade>`.

A bug fix lands once in the shared code: find every caller before editing, never patch each call site. Never cut validation at trust boundaries (HTTP handlers, ACP frames, client paths, pairing and tokens), error handling that prevents data loss, security, or anything the user asked for.

Adapted from [ponytail](https://github.com/DietrichGebert/ponytail).

### Our core values

- **Concision**: small files, small functions.
- **Organization**: layering, modularization, consistency.
- **Clarity**: a human should understand it in one pass.
- **Standards**: type safety, consistent naming, exactly one way to lint and build.

## Coding Standards

### Layering & packages

- `.go-arch-lint.yml` is the layer map: entry → composition → transport → domain tiers → infra, imports pointing inward only. Read its header before adding an import between `internal/` packages.
- Everything has one place and lives in it. A helper goes in the lowest-layer package that owns its concept; Go has no `util`/`common`/`helpers` package and neither do we.
- Compose the packages that already exist before writing a new one.
- One package, one responsibility. File count is not the trigger; a second responsibility is. `internal/api` and `internal/session` are the largest packages — a new responsibility gets its own package, registered in its layer in `.go-arch-lint.yml`, instead of growing them.
- Report boundary violations instead of quietly working around them. A new edge in `.go-arch-lint.yml` is an architecture decision: propose it, don't add it to get green.

### Small functions

- Function body ≤ 60 lines (`funlen`'s default; not enabled in `.golangci.yml`, so the limit is on you); aim for 20-50. Older code has functions over the limit — shrink the ones you touch, never grow them.
- Inline one-expression functions called exactly once — a name that carries no meaning is indirection.
- Split on logical units of abstraction, not on line count.
- Exempt: declarative literals long by structure — the `cli.Command` tree in `cmd/ferngeist`, test-case tables. The logic around them is not exempt.

### Clean code

- At most 2 nested levels of `if` / `switch` / `for` / `select`. Deeper means a guard clause or an extracted function: handle the error and return early, keep the happy path at the left margin.
- Errors bubble: return them wrapped once with context (`fmt.Errorf("open store: %w", err)`) and handle them at the boundary that can act — the HTTP handler mapping to a status, the CLI printing. No fallback chains — they hide which primary path failed. No `panic` for runtime failures. Discarding an error (`_ =`) needs a one-line why.
- Every goroutine has an owner and a stop path (`context` cancellation or a close), and `context.Context` is the first parameter of anything that blocks. `daemon`, `api`, `runtime` and `session` run `goleak` in `TestMain`, so a leaked goroutine fails the package.
- Types at every boundary. `any`, `interface{}` and `map[string]any` are not types: use named structs for package APIs and wire shapes, `json.RawMessage` for a payload passed through unread.
- Naming: Go conventions — `MixedCaps` for everything including constants, initialisms in one case (`ID`, `URL`, `ACP`, `HTTP`), short consistent receiver names, short lowercase package names, no stutter (`push.Dispatcher`, not `push.PushDispatcher`).
- `gofmt` is the formatting, not a preference.

### Comments

- Never delete an existing comment unless asked; shorten or reword instead.
- Comments explain **why**, usually a constraint that isn't visible in the code. Never narrate what the code already says.
- Exported identifiers carry a doc comment that starts with the identifier's name.
- Keep the existing density — deliberately sparse here, not zero.

## Guardrails

> Non-negotiable. Only the Code Commander phrase bypasses them, for one turn.

### Codebase intelligence

- **`codebase-memory` is the first tool, not an optional one.** When the MCP server is connected, reach for it before any grep/read sweep: `search_graph` for symbols, `trace_path` for callers/callees and blast radius, `get_code_snippet` to read a declaration, `get_file_outline` for a file's structure, `query_graph` for multi-hop or aggregation, `get_architecture` for a package overview.
- **Keep the index current.** After adding, renaming or deleting files, re-index (`index_repository`) before trusting a lookup; use `check_index_coverage` / `index_status` to confirm the paths you care about are actually indexed. An index that lags your edits is worse than no index.
- Fall back to `read`/`grep`/`glob` for literals, for files the index does not cover, and whenever a cited path does not exist. An index hit is a lead, not proof — check the cited path before acting on it.

### Verification gate

- Gate before reporting: `golangci-lint run ./...` (includes `govet` and `gofmt`), `go-arch-lint check`, and `go test -count=1` on the touched packages. Add `go test -race -count=1` for anything concurrent (session pump, runtime, push, api WebSockets). CI runs all of these.
- The lint baseline may already be red. Your change adds zero findings: compare the counts before and after and report both.
- **Fix findings; never silence them.** No `//nolint` or `//lint:ignore`; no new exclusion rules, disabled linters or raised thresholds in `.golangci.yml`; no new edges in `.go-arch-lint.yml`; no deleting the offending call or skipping the test to dodge it. If a finding is genuinely wrong, say why in your report and let the maintainer decide. Silencing a finding to get a green build is a failed task, not a passing one.
- Real-environment verification is off-limits unless specifically asked: no `FERNGEIST_RUN_REAL_AGENT_TESTS=1`, no `daemon install`/`start`/`stop`, no Tailscale provisioning, no real agents. The mock-agent E2E in `docs/development.md` runs in a sandbox and is allowed.
- `golangci-lint` analyses the host GOOS only; code behind `//go:build` tags for other platforms is unchecked unless you lint with that `GOOS`, so don't treat a clean Linux run as covering `windows` or `darwin` files.

### Shell & tooling

- Exit code 0 is not proof: classify output. `(cached)` means the test never ran (hence `-count=1`); `[no tests to run]` means `-run` matched nothing; `no test files` means the package has none.
- Never run tests from inside a git operation (`rebase -x`, hooks): the inherited `GIT_DIR` leaks into tests that shell out to git, and they write to this repo instead of their temp repo.
- Avoid `rg -v` without explicit input (recurses the tree and inverts = output explosion). Use `grep -v`.

### Git

- Read-only and allowed: `git status`, `git diff`, `git log`, `git branch`, `git show`.
- Never commit without explicit approval; never push, merge, rebase, or reset unless asked. Commits are GPG-signed (`commit.gpgsign` is on; never pass `--no-gpg-sign`).
- Stage files by explicit path; the tree carries untracked tool state.
- Never delete untracked files or the maintainer's uncommitted work.

### Secrets

- Never `echo $VAR` or `printenv VAR`. Check existence with `[[ -n "$VAR" ]] && echo set || echo unset`.
- No credentials, tokens, VAPID keys, or API keys in code, scripts, logs, or commit messages.

### Destructive operations

Confirm first for: `rm -rf` outside `/tmp` and scratch dirs; deleting or editing a gateway SQLite database or state directory; editing or renumbering a released migration in `internal/storage/migrations.go` (append-only); `daemon install`/`uninstall --purge`, `devices revoke`, or anything else that changes the real host; global package installs; anything rewriting git history.

### Hands-off directories

Do not create or edit without being asked: `.idea/`, `.vscode/`, `.claude/`, `.cursor/`, `.opencode/`, `.crush/`, `.commandcode/`, `.beads/` (owned by `bd`), `.worktrees/`, `.gocache/`, `.gotmp/`, `bin/`, `dist/`.

> ^^^ Everything above is the harness doctrine. Everything below is this repo's context. ^^^

---

## Project Overview

Ferngeist Gateway launches ACP agents as stdio subprocesses and exposes them to paired clients (mainly the Ferngeist Android app) over one authenticated HTTP + WebSocket API. Binary name: `ferngeist-gateway`; entrypoint `./cmd/ferngeist`.

## Commands

Tasks are in `Taskfile.yml` (`task --list`).

```sh
task build                      # -> bin/ferngeist, with version ldflags
task run -- --listen :9000      # daemon in foreground (task run:lan for --lan)
go test ./...                   # full suite (CI: CGO_ENABLED=0 go test -count=1 ./...)
go test -race -count=1 ./...    # CI also runs this; session/pump code is concurrency-heavy
go test ./internal/session -run TestName -v   # single test
golangci-lint run ./...         # includes govet and gofmt; CI pins the version in .github/workflows/ci.yml
go-arch-lint check              # layer rules in .go-arch-lint.yml
```

- `main.buildVersion` ldflag is **required**: the CLI refuses to start without it. Ad-hoc: `go run -ldflags "-X main.buildVersion=dev" ./cmd/ferngeist daemon run`.
- Release builds are pure Go (`CGO_ENABLED=0`, modernc sqlite); don't introduce cgo dependencies.
- Opt-in real-environment tests are gated by `FERNGEIST_RUN_REAL_AGENT_TESTS=1` (e.g. `TestOptionalInstalledOpenCodeACPSmoke` in `internal/runtime`, `TestLaunchdLifecycle` in `internal/service`, macOS only). Some tests self-skip when `CI` is set.
- `cmd/mock-stdio-agent` is a mock ACP agent used by tests and for end-to-end checks against a real binary; the full manual multi-session E2E recipe is in `docs/development.md`.
- Config is via `FERNGEIST_GATEWAY_*` env vars (`docs/configuration.md`). For local E2E, set `FERNGEIST_GATEWAY_LISTEN_ADDR`, `FERNGEIST_GATEWAY_ADMIN_ADDR`, and `FERNGEIST_GATEWAY_UPDATE_CHECK_ENABLED=0`.

## Issue tracking (beads)

Work is tracked with [beads](https://github.com/steveyegge/beads) (`bd`, a Dolt-backed tracker stored in `.beads/`), not markdown TODO files. Run `bd prime` at the start of a session (or after context compaction) for the full workflow reference.

- **Find work**: `bd ready` lists open issues with no blockers; `bd show <id>` shows details and dependencies; `bd search <query>` searches.
- **Before coding**, make sure an issue exists and claim it: `bd create --title="..." --description="why and what" --type=task|bug|feature|epic|chore --priority=2`, then `bd update <id> --claim`. Priority is `0`-`4` (0 = critical, 4 = backlog), not words. Use `--parent=<id>` for subtasks and `--deps=blocked-by:<id>` (or `bd dep add <issue> <depends-on>`) for ordering.
- **Found something out of scope?** File it with `--deps=discovered-from:<current-id>` instead of fixing it inline.
- **Finish**: `bd close <id> --reason="..."` (several IDs at once is fine; `--suggest-next` shows what got unblocked). Close issues before reporting work as done.
- Never run `bd edit`: it opens `$EDITOR` and blocks. Use `bd update <id> --title/--description/--notes/--design` instead.
- `bd dolt push` / `bd dolt pull` sync issues with the remote. Like `git push`, only do it when asked.
- Git's `core.hooksPath` is set to `.beads/hooks/` (local config, set up by beads), so git hooks (pre-commit, pre-push, etc.) call `bd`. If a commit or push hangs or fails in a hook, `bd doctor` is the first diagnostic.

## Architecture

`docs/architecture.md` is the authoritative, detailed write-up; read it before touching sessions, push, workspace, or remote access. The big picture:

- **Wiring**: `internal/daemon` builds everything and runs startup reconciliation (sessions left `active`/`disconnected` become `failed`, since their processes are gone). `internal/api` holds the public API (`/v1/...`) and a separate admin API (`/admin/v1/...`, its own listen address) that the CLI talks to via `internal/adminclient`.
- **Session owns the process**: `POST /v1/agents/{id}/connect` launches a fresh agent process and creates a resilient session (`internal/session`) that owns the `*exec.Cmd` 1:1. `internal/runtime` only *launches* (returns cmd + pipes) and keeps diagnostics (log ring, failure records, `RecordExit`); it never auto-restarts. It also pre-boots one unlisted spare process per agent (`internal/runtime/spare.go`) so the next connect is warm.
- **Stdio pump**: a long-lived goroutine per session drains agent stdout independently of the WebSocket. Detaching a client doesn't stop the agent; the pump buffers `session/update` history (8 MiB, trimmed to turn boundaries) for restore on reconnect, and emits push notifications whether or not a client is attached (the client decides whether to show them). Reconnect = `POST /v1/sessions/{id}/resume` (mints a single-use, in-memory attach token) then `GET /v1/acp/{runtimeId}?sessionId=&attachToken=`.
- **Session close ordering matters** (mark `closing` → stop pump → remove from registry → ACP `session/close` if advertised → stop process → delete row); marking `closing` first is what keeps an intentional stop from being reported as a crash push.
- **Push**: `internal/push` has a platform-neutral `Dispatcher` (token lookup, dead-token eviction) plus pluggable `Provider`s; Web Push (VAPID) is the only one. The session layer only emits `push.Notification`.
- **Workspace endpoints** (`internal/api/workspace.go`, `internal/workspace`): read-only file/git views rooted at the ACP `session/new` `cwd` captured from inbound frames (`internal/session/acpstate.go`), *not* the process's launch cwd. All paths go through `ResolveWithinRoot` (rejects abs paths, `..`, symlink escapes). Git is shelled out (`git -C <cwd> ...`, 15s timeout); no Go git library. The file/git views never write; the only writes are the worktree lifecycle in `internal/workspace/worktree.go` (create, delete, prune) and `finish.go` (rebase, merge, push — refused while the worktree has uncommitted work). Payloads reuse `acp-go-sdk` v1 types.
- **Agents**: `internal/catalog` merges the ACP registry (`internal/registry`, fetched and cached) with custom agents (`internal/customagents`); `internal/acquire` downloads managed binaries and installs npm agents to run directly on bun/node.
- **Remote access**: `internal/remote` provisions Tailscale (host CLI or embedded tsnet) funnel/serve; `remote.Access` owns the retry loop and status snapshot read by `daemon status`.
- **Storage**: SQLite in `internal/storage` (sessions, pairings, runtimes, push tokens, VAPID keys, settings, inbound diagnostics log).

## Contract changes

The gateway↔client contract (HTTP shapes, ACP WebSocket frames, `_ferngeist/` extension notifications, push payloads) is documented in `docs/api.md`. Any contract-visible change needs an entry in `docs/changelog.md` tagged `[breaking]`, `[additive]`, or `[semantic]`. `[breaking]` and `[semantic]` changes also need a bump of the `ProtocolVersion` constant in `internal/api/server.go` in the same change; `[additive]` changes never bump it.

## Testing & QA

Stdlib `testing` only — no assertion or mocking libraries. Hand-written fakes at real seams (e.g. `internal/service/fake_systemctl_test.go`); `t.TempDir()` for files and throwaway git repos; `t.Cleanup` for teardown; table-driven `t.Run` where cases share one body.

Test names read as a behaviour sentence, e.g. `TestProcessExitDuringCloseSendsNoCrashPush`. Add tests next to changed logic — ACP frame handling, the stdio pump and history trimming, session close ordering, path resolution, pairing and token checks, migrations.

**Never write tests for what the compiler already guarantees** — interface satisfaction, struct field assignment, constructor wiring, exhaustive constant lists. Same for getters that forward a field and assertions that only compile against the implementation.

### Authoring gate

Answer all four before adding a test:

1. Which observable behaviour, invariant, or independent contract does it protect?
2. Which credible regression makes it fail?
3. Why does existing proof miss it? One owner per contract — a second layer needs a distinct transport or lifecycle risk.
4. Does it need an export, flag, wrapper, or injection hook with no production caller? Then the proof belongs at the real boundary (`httptest` against the handler, a real SQLite file, the mock stdio agent).

A regression test fails before the fix and passes after. Report when no baseline proof is available. One owner regression per bug — never replay it at every crossed layer.

### Junk tests — reject

- Self-comparisons, copied inventories, source greps, and implementation or call-shape assertions (a fake that only records that a collaborator method was called).
- Duplicate contracts across layers, test-only production seams, expected values produced by the subject under test, fakes supplying the behaviour being asserted.
- `time.Sleep` to wait for a goroutine — synchronise on a channel or poll with a deadline.
- Negative controls that fail at the wrong guard; names that don't match the exercised behaviour.

### What to keep

Independent contracts: the `docs/api.md` wire shapes, ACP framing and `_ferngeist/` extensions, SQLite schema and migrations, `ResolveWithinRoot` path safety, pairing, proof and token validation, push payloads, observable ordering, config and defaults. Static or slow is not a reason to delete. A red baseline may be a bug to repair, not a test to remove.

### Audit

Before deleting a test, read its owner, callers, and history, and record what proof survives. Delete obsolete production seams together with their tests. Never optimize for a deletion count.

After non-trivial work, exercise the changed path and report the observed result. Report unverified work as unverified.

## Communication

- **Stop at the gate.** Edit → [verification gate](#verification-gate) clean → report the change → ask "Ready to commit?". Real-host verification belongs to the maintainer.
- **Lead with the answer.** Conclusion first, then evidence. No narrated steps, no recap of what you just did, no tables restating a one-line result.
- **No walls of text.** A few lines. Bullets over paragraphs. If the explanation is longer than the change, cut it.
- **Say what is unverified.** Never imply a fix is confirmed when the evidence is a unit test or a plan. If a claim rests on inference, mark it.
- **Don't blur scope.** If work on one thing is done, don't let the report imply other things shipped too.
- **Close with the cost.** End with one or two lines: what you skipped, what stayed unverified, and the risk that leaves.

## Commit & PR Guidelines

`type(scope): concise imperative summary` — e.g. `fix(worktree): measure an unmanaged worktree from the local trunk`. `CHANGELOG.md` is generated from these by git-cliff (`task changelog`), so the format is load-bearing.

PRs state what changed and why, risk/rollback, contract impact (the `docs/changelog.md` tag, or none), and the exact commands run as test evidence.

## Security

`docs/security.md` is the trust model: read it before touching pairing, credentials, scopes, proof of possession, the admin API's local-only checks, or remote access. Anything reachable from a paired client is a trust boundary — validate there, and keep `ResolveWithinRoot` in every client-supplied path.

## Agent skills

### Issue tracker

Issues are tracked with [beads](https://github.com/steveyegge/beads) (`bd`), not GitHub Issues or markdown files. See `docs/agents/issue-tracker.md`.

### Triage labels

The five canonical triage-role labels apply to beads issues as-is. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `GLOSSARY.md` and `docs/adr/` at the repo root, created lazily. See `docs/agents/domain.md`.

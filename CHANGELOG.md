## [1.4.0] - 2026-11-07

### 🚀 Features

- *(caffeine)* Keep the host awake while an agent turn runs (#8)
- *(quota)* Report CodexBar plan quotas
- *(workspace)* Support bounded image file reads
- *(session)* Resume chats seamlessly with catch-up and one-call attach
- *(quota)* Cache snapshots and enrich provider metadata (#10)
- *(api)* Stream turn status over WebSocket

### 🐛 Bug Fixes

- *(quota)* Expose provider billing balances
- *(quota)* Identify codexbar collection errors
- *(session)* Complete seamless chat resume
- *(session)* Restore catch-up after rejected prompts
- *(acquire)* Extract complete registry archives
- *(acquire)* Launch native npm bins directly

### 📚 Documentation

- *(changelog)* Point turn-stream entries at squashed commit

### 🧪 Testing

- *(turns)* Verify elapsed status timestamp contract
## [1.3.0] - 2026-10-30

### 🚀 Features

- *(packaging)* Ship man pages in packages and archives
- *(worktree)* Finish a worktree — rebase, merge and push
- *(worktree)* Read API — chats, one worktree, repo refs
- *(worktree)* Create from a remote branch or PR, adopt existing ones
- *(push)* Include the worktree branch in the push payload
- *(storage)* Record each worktree folder once
- *(api)* Report which chat folders no longer exist on disk
- *(worktree)* Flag worktrees whose work has landed on their base
- *(worktree)* List and operate on every git worktree
- *(api)* Stop an agent for the calling device only
- *(runtime)* Spawn processes outside the runtime registry
- *(session)* Report each session's ACP session ids
- *(projects)* List agent chats for the project-first home
- *(projects)* Report running turns across paired devices
- *(projects)* Evict idle listing agents under memory pressure
- *(catalog)* Bundle agent logo fallbacks
- *(turns)* Report turn status and outcomes via GET /v1/turns

### 🐛 Bug Fixes

- *(api)* Refuse browser requests on the admin listener
- *(acquire)* Extract agent archives and refresh changed binaries
- *(workspace)* Keep a worktree when its dirty check fails
- *(session)* Close two stale-read races in session lifecycle
- *(worktree)* Count list ahead from the merge-base, not the fixed base commit
- *(worktree)* Refuse to delete a worktree a running chat uses as its cwd
- *(worktree)* Give git worktree add its own two minute limit and clean up after it
- *(worktree)* Harden adopt, create and push against data loss and races
- *(worktree)* Prefer a local branch over PR shorthand for a base
- *(worktree)* Extend the push write deadline beyond the server default
- *(worktree)* Measure an unmanaged worktree from the local trunk
- *(lint)* Resolve the five CI lint findings
- *(architecture)* Compose persistence-backed services in the daemon
- *(workspace)* Return an empty changed array for clean status
- *(projects)* Cancel abandoned queued listings
- *(projects)* Reclaim expired listing payloads
- *(pairing)* Use the live tunnel URL for pairing
- *(remote)* Protect host tunnel provisioning and honor private mode
- *(remote)* Isolate embedded node state and migrate existing identities
- *(release)* Install apt repository tooling

### 🚜 Refactor

- *(worktree)* Move UnmanagedWorktree json tags to the api response type
- *(worktree)* Split base resolution out of the worktree service

### 📚 Documentation

- Add AGENTS.md and the agent-facing docs
- Strengthen repository architecture and cutover guidance

### ⚡ Performance

- *(worktree)* List worktrees concurrently, and report behind
- *(worktree)* Reuse the service branch lookup for push notifications

### 🧪 Testing

- *(projects)* Measure listing latency and memory

### ⚙️ Miscellaneous Tasks

- *(lint)* Set up golangci-lint, go-arch-lint, CI and AGENTS.md
## [1.2.0] - 2026-10-19

### 🚀 Features

- *(workspace)* Report staged state in git status
- *(api)* Compare the workspace against a branch, remote or tag
- *(api)* Return the compare target's commits with the log
- *(workspace)* Report the walk's total size in the git log page
- *(api)* List the workspace's files
- *(api)* Let clients recover from proof clock skew
- *(workspace)* Report a plain checkout's branch start as the git log base
- *(acquire)* Launch npm agents on bun or node instead of npm exec

### 🐛 Bug Fixes

- *(workspace)* Send an empty commits array, not null, for an empty range
- *(registry)* Serve the cached snapshot while refreshing in the background

### 📚 Documentation

- *(changelog)* Record staged in the git status contract
- *(changelog)* Point branch compare entry at its commit on main
- *(changelog)* Record total in the git log page contract
- *(api)* Default is a client preselection hint, not gateway-applied
- *(changelog)* Point file list entry at its commit on main
- Add DeepWiki badge
- *(changelog)* Point clock skew entry at its commit on main
- *(changelog)* Record the git log base for plain checkouts
- *(changelog)* Release v1.2.0
- *(changelog)* Point git diff gzip entry at its commit on main
- Cover the expanded workspace surface
- *(configuration)* Document NAME and LOG_LEVEL, drop the retired reaper interval
- *(changelog)* Release v1.2.0

### ⚡ Performance

- *(catalog)* Resolve npm bin names once per package
- *(api)* Parallelize and gzip the whole-tree git diff
- *(runtime)* Pre-boot one spare process per launched agent
- *(session)* Stop unmarshalling update content and titles
- *(runtime)* Idle a spare agent for 2 minutes, not 10
## [1.1.0] - 2026-10-15

### 🚀 Features

- *(workspace)* Commit history endpoints for the git sheet (#6)
- *(session)* Report each prompt's turn duration in its result _meta
- *(session)* Show turn duration in Turn Complete push
- *(workspace)* Diff worktrees against merge-base so rebases stop leaking upstream commits
- *(session)* Keep detached prompt results for the next attach
- *(session)* Flag truncated history in resume notice

### 🐛 Bug Fixes

- *(packaging)* Installer, upgrade and uninstall fixes with end-to-end CI (#7)

### 💼 Other

- Update ferngeist-gateway formula to v1.0.0

### 📚 Documentation

- *(changelog)* Rebuild contract history per release with commit links
- *(changelog)* Link turn_result and truncated entries to their commits
- *(changelog)* Point entry links at the commits on main
- Rewrite README around app setup and the client API
- *(changelog)* Release v1.1.0

### 🧪 Testing

- *(api)* Expect detached turn_result ahead of load reply after reconnect

### ⚙️ Miscellaneous Tasks

- Add .gitattributes forcing LF line endings
- Gofmt-align two test files so gofmt -l is clean
## [1.0.0] - 2026-10-11

### 🚀 Features

- *(session)* [**breaking**] Single-parse pump pipeline; signal resume with _ferngeist/resumed

### 🐛 Bug Fixes

- Address rearchitecture review findings

### 💼 Other

- Update ferngeist-gateway formula to v0.13.0

### 🚜 Refactor

- Delete dead code (runtime tokens, legacy lease, one-value settings)
- *(remote)* Merge remoteaccess into remote, move retry loop into Access
- *(api)* Move workspace/worktree logic to internal/workspace, share custom-agent handlers, add Deps
- *(storage)* Split sqlite.go by area; numbered user_version migrations
- *(runtime,session)* [**breaking**] Connecting starts the agent; drop lease, exit callbacks, ProcessManager

### 📚 Documentation

- *(changelog)* Release v1.0.0
## [0.13.0] - 2026-10-10

### 🚀 Features

- *(workspace)* Scope workspace endpoints to an ACP session
- *(worktree)* Gateway-managed git worktrees for parallel chats
- *(push)* Name the agent and its running tool kind in progress pushes
- *(push)* Name the agent in every push and send the latest throttled progress

### 🐛 Bug Fixes

- *(pairing)* Keep rotated tokens exchangeable for 48h after refresh
- *(workspace)* Scope git status to the agent's cwd and report conflicts
- *(mock)* Answer session/load before replaying history
- *(worktree)* Own the dirty check and retry the folder delete
- *(push)* Carry session cwd in push deep-links

### 💼 Other

- Update ferngeist-gateway formula to v0.12.0

### 📚 Documentation

- *(changelog)* Release v0.13.0
## [0.12.0] - 2026-10-07

### 🚀 Features

- *(session)* Support ACP elicitations
- *(push)* Deliver notifications over Web Push instead of FCM

### 🐛 Bug Fixes

- *(service)* Treat systemd "unit not loaded" as a fresh install
- *(update)* Make self-update safe across platforms
- *(daemon)* Skip update notifications for package-manager builds
- *(service)* Fix install, stop and reinstall on Linux, macOS and Windows
- *(packaging)* Keep user settings across package upgrades
- *(service)* Detect Task Scheduler access denial on any Windows language
- *(session)* Deliver agent requests missed while the client was away
- *(session)* Reap unattached sessions and stop false crash pushes
- *(session)* Stop spurious and duplicate agent notifications
- *(push)* Give the update notice its own category

### 💼 Other

- Update ferngeist-gateway formula to v0.11.3

### 📚 Documentation

- *(changelog)* Name v0.12.0 as the latest tagged release

### 🧪 Testing

- *(service)* Seed the service binary in the self-copy install test
## [0.11.3] - 2026-10-05

### 🐛 Bug Fixes

- *(session)* Bound the client queue by bytes and disconnect instead of dropping
- *(api)* Don't close a websocket while its client is still draining
- *(session)* Keep reattached clients' transcripts and turns intact

### 💼 Other

- Update ferngeist-gateway formula to v0.11.2

### 📚 Documentation

- Record the reconnect, backlog and turn_ended contract changes
- *(changelog)* Name v0.11.3 as the latest tagged release
## [0.11.2] - 2026-09-30

### 💼 Other

- Update ferngeist-gateway formula to v0.11.1
- Forward registry agent icon through /v1/agents

### 📚 Documentation

- *(changelog)* Name v0.11.2 as the latest tagged release
## [0.11.1] - 2026-09-23

### 💼 Other

- Update ferngeist-gateway formula to v0.11.0
- Evict expired attach tokens with amortized sweep and cap
- Bound runtime_failures, index failed_at, enable WAL
- Bound pairing rate-limiter buckets with sweep and cap
- O(1) credential lookup, amortized nonce sweep, cached proof keys
- Cache refresh results with TTL and explicit invalidation
- Prune terminal state, cap logs, split Summary lock
- Single-parse pump, async frame log, close-safe inbound
- Shut down cleanly on all exit paths; guard suites with goleak
## [0.11.0] - 2026-09-17

### 🚀 Features

- *(custom-agents)* Client-manageable custom agents with CLI management

### 🐛 Bug Fixes

- *(api)* Return 409 runtime_lease_held on resilient connect conflict

### 💼 Other

- Update ferngeist-gateway formula to v0.10.0

### 📚 Documentation

- *(changelog)* Name v0.10.0 as the latest tagged release
## [0.10.0] - 2026-09-12

### 🚀 Features

- *(runtime)* Track a set of runtimes per agent
- *(session)* Allow parallel sessions for the same agent
- *(api)* Add new:true, runtimes list, and stop-all for agents

### 🐛 Bug Fixes

- *(session)* Surface swallowed LLM failures as JSON-RPC errors

### 💼 Other

- Update ferngeist-gateway formula to v0.9.3

### 📚 Documentation

- Document multi-session per agent
## [0.9.3] - 2026-08-19

### 🐛 Bug Fixes

- *(update)* Extract windows binary by its .exe name
- *(remote)* Report provisioned public URL in status without restart
- *(linux)* Register per-user daemon service from package install; avoid ETXTBSY on reinstall

### 💼 Other

- Update ferngeist-gateway formula to v0.9.2
- Update ferngeist-gateway formula to v0.9.3

### 📚 Documentation

- *(remote)* State that the public URL appears live after tailscale login

### 🧪 Testing

- *(linux)* Match systemctl subcommand tokens in install test

### ⚙️ Miscellaneous Tasks

- *(lint)* Add golangci-lint config and squash pre-existing findings
## [0.9.2] - 2026-08-18

### 🐛 Bug Fixes

- *(windows)* Keep daemon alive across console close, close install console promptly

### 💼 Other

- Update ferngeist-gateway formula to v0.9.1

### 📚 Documentation

- Regenerate changelog for v0.9.1
## [0.9.1] - 2026-08-17

### 🐛 Bug Fixes

- *(service)* Unify public-URL env guard across platforms
## [0.9.0] - 2026-08-17

### 🚀 Features

- *(remote)* Tailscale adapter and provisioning lifecycle
- *(daemon)* Wire remote access provisioning with retry loop
- *(api)* Remote access state in status endpoint
- *(service)* Cross-platform env injection for remote access
- *(cmd)* Daemon install --remote and status auth link
- *(workspace)* Classify submodules and untracked dirs as directories

### 🐛 Bug Fixes

- *(adminclient)* Add authRequired/authURL to DaemonStatus struct
- *(config)* Apply persisted remote URL only when remote requested
- *(ci)* Linux env guard and remote retry race

### 💼 Other

- Update ferngeist-gateway formula to v0.8.0
- *(win)* Windowless wscript launcher, fix stop/restart, harden uninstall

### 📚 Documentation

- Remote access documentation
- Regenerate changelog for v0.9.0
## [0.8.0] - 2026-08-10

### 🚀 Features

- *(session)* Emit throttled live progress push notifications
- *(workspace)* Add read-only file/git browsing endpoints
- *(pairing)* Add credential grace period and in-grace refresh
- *(api)* Allow proof-validated refresh of expired credentials
- *(api)* Export protocol version and stabilize at v1
- *(ops)* Persist clean process exits and raise WS read limit
- *(session)* Add opt-in raw ACP frame log behind a toggle
- *(service)* Add launchd manager for macOS daemon install
- *(update)* Add release checker and checksum-verified downloader
- *(update)* Notify paired devices when a new stable release exists
- *(daemon)* Periodic update-available check with device push
- *(cli)* Add ferngeist-gateway update command
- *(release)* Goreleaser pipeline with brew, winget, nfpm, AUR channels
- *(release)* Signed apt repo + one-liner installer
- *(release)* Signed dnf/yum rpm repo on gh-pages-deb (rpm/ tree)
- *(release)* Self-hosted signed pacman repo; drop AUR
- *(release)* Homebrew/Linuxbrew formula on main
- *(install)* Universal installer for macOS/Linux/Windows
- *(build)* Android arm64 build for Termux; installer fetches it on Termux
- *(android)* Working Termux build — cgo/NDK DNS, argv fix, direct-update

### 🐛 Bug Fixes

- *(session)* Decouple stdout drain from client writes; collapse conn state into pump
- *(session)* Send agent-side ACP session id in session/close
- *(update)* Extract binary from release archive, stop service before swap, honor package channels
- *(update)* Refuse self-update when package-manager env gate is set
- *(ci)* Race-free session pump, deterministic teardown, inline-env plist
- *(service)* Launchd integration test + darwin Status state parsing
- *(install)* Detect Termux and force the binary channel
- *(release)* Ignore android/amd64 (forces cgo) in gateway build

### 💼 Other

- Update ferngeist-gateway formula to v0.8.0
- Update ferngeist-gateway formula to v0.8.0
- Update ferngeist-gateway formula to v0.8.0

### 🚜 Refactor

- *(session)* Route session/close through pump stdin seam
- *(session)* Extract load-recovery into its own module

### 📚 Documentation

- Document credential grace period and expired refresh
- Add contract changelog
- Add generated changelog
- Note grace-period refresh in contract changelog
- Per-OS installation guide
- Update changelog
- Bring install/README/changelog up to date

### 🧪 Testing

- *(service)* Launchd lifecycle integration test on macOS CI

### ⚙️ Miscellaneous Tasks

- Add macOS (amd64 + arm64) to release workflow
- Add Android build task
- Add git-cliff changelog tooling and tag-scoped release changelog
- *(release)* Allow replacing existing release assets on re-run
## [0.7.3] - 2026-07-20

### 🐛 Bug Fixes

- *(session)* Don't auto-restart session-leased runtimes; shut down session service
- *(session)* Enforce one live session per (device, agent)

### ⚙️ Miscellaneous Tasks

- Add Taskfile with build, run, test, and clean tasks
## [0.7.2] - 2026-06-26

### 🐛 Bug Fixes

- *(session)* Close conn when agent process exits
- *(catalog)* Query npm for local binary (if exists)
## [0.7.1] - 2026-06-24

### 🚀 Features

- *(session)* Reclaim crashed sessions fully; recover re-load on reconnect

### 🐛 Bug Fixes

- *(api)* Update integration test for crash reclamation
## [0.7.0] - 2026-06-22

### 🚀 Features

- Add attach token service, push notification interface, and session config
- *(session)* Add resilient session domain with stdio pump and inbound diagnostics
- *(mock-agent)* Add session/close handler for integration testing
- Wire session service into daemon and API server
- *(push)* Add push notification core — types, interfaces, log provider
- *(push)* Add FCM HTTP v1 provider
- *(push)* Add platform-agnostic dispatcher + token storage
- *(session)* Wire push notifications into session lifecycle

### 🐛 Bug Fixes

- Catalog binary priority; expand test coverage
- *(catalog)* Check binary PATH availability before preferring over npx/uvx

### 🚜 Refactor

- Extract API handlers separately
- Remove legacy bearer-token WebSocket session path
- Delete dead storage methods, orphaned types, and unused push interface method
- *(runtime)* Replace AttachStdio with AcquireLease/ReleaseLease
- Clean up pairing service and catalog validation
- *(session)* Split session.go into types and methods files
- *(session)* Replace positional callback with PushEvent struct, extract checkAndNotify

### 📚 Documentation

- Update api, architecture, config, and readme for resilient sessions
- Update docs

### 🧪 Testing

- Add resilient session integration and unit tests

### ⚙️ Miscellaneous Tasks

- Gitignore Firebase admin key and build artifacts
## [0.6.0] - 2026-04-28

### 🚀 Features

- *(desktop-helper)* Add initial local pairing and ACP bridge
- *(desktop-helper)* Rely on ACP registry launch metadata
- *(desktop-helper)* Restart runtimes with env overrides
- *(desktop-helper)* Add ferngeist CLI and admin client
- *(desktop-helper)* Add daemon status command
- *(desktop-helper)* Add hint when daemon is down
- *(desktop-helper)* Improve lifecycle and add tests
- *(desktop-helper)* Add pairing security config
- *(desktop-helper)* Add service management for Linux
- *(desktop-helper)* Add Windows service manager implementation
- *(desktop-helper)* Add daemon install host and public URL options
- *(desktop-helper)* Implement ACP JSON-RPC mock agent and fix resolveCommandPath

### 🐛 Bug Fixes

- *(desktop-helper)* Enforce helper API contract
- *(connection)* Keep idle helper ACP sessions alive

### 💼 Other

- *(desktop-helper)* Render pairing QR config

### 🚜 Refactor

- *(desktop-helper)* Extract daemon CLI
- *(desktop-helper)* Remove helperd shim and add CLI version flag
- Rename helper to gateway and update module path

### 📚 Documentation

- *(desktop-helper)* Add portable distribution
- Add complete docs

### ⚙️ Miscellaneous Tasks

- *(desktop-helper)* Switch to coder/websocket
- *(release)* Bump version to 0.3.0
- Use correct LDFLAGS
- Require buildVersion and update ldflags

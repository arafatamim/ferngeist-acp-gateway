## [0.8.0] - 2026-08-09

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
- *(release)* Goreleaser pipeline with brew, winget, nfpm channels
- *(release)* Publish signed apt repository to gh-pages-deb

### 🐛 Bug Fixes

- *(session)* Decouple stdout drain from client writes; collapse conn state into pump
- *(session)* Send agent-side ACP session id in session/close
- *(update)* Extract binary from release archive, stop service before swap, honor package channels
- *(update)* Refuse self-update when package-manager env gate is set
- *(release)* Pin apt signing key export to the key that signs InRelease
- *(release)* Commit git-cliff changelog before goreleaser to avoid dirty-tree abort

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

### 🧪 Testing

- *(service)* Launchd lifecycle integration test on macOS CI

### ⚙️ Miscellaneous Tasks

- Add macOS (amd64 + arm64) to release workflow
- Add Android build task
- Add git-cliff changelog tooling and tag-scoped release changelog
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

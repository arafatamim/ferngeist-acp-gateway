# Development

## Build

```powershell
go build ./...
```

## Run

```powershell
go run .\cmd\ferngeist daemon run
```

## Test

```powershell
go test ./...
```

## Optional local agent smoke test

```powershell
$env:FERNGEIST_RUN_REAL_AGENT_TESTS="1"
go test ./internal/runtime -run TestOptionalInstalledOpenCodeACPSmoke -v
```

## Multi-session E2E check

The multi-session-per-agent flow (two processes of the same agent, independent
resilient sessions, stop-all) is covered by unit/integration tests
(`TestStartNewLaunchesSeparateRuntime`, `TestCreateAllowsSameAgentParallelSessions`,
`TestAgentsListsMultipleRuntimesAndStartNew`). To verify against a **real
binary** end to end:

```bash
# 1. Build gateway + mock agent into a sandbox
go build -ldflags "-X main.buildVersion=dev" -o /tmp/fg/ferngeist-gateway ./cmd/ferngeist
go build -o /tmp/fg/sandbox/bin/mock-stdio-agent ./cmd/mock-stdio-agent

# 2. Run the daemon with the sandbox as its working directory (catalog detects
#    bin/mock-stdio-agent relative to cwd)
cd /tmp/fg/sandbox
FERNGEIST_GATEWAY_LISTEN_ADDR=127.0.0.1:5790 \
FERNGEIST_GATEWAY_ADMIN_ADDR=127.0.0.1:5791 \
FERNGEIST_GATEWAY_UPDATE_CHECK_ENABLED=0 \
  /tmp/fg/ferngeist-gateway daemon run

# 3. Drive it: POST /admin/v1/pairings/start (port 5791) -> GET the code ->
#    POST /v1/pair/complete -> POST /v1/agents/mock-acp/start {"new":true}
#    twice -> connect both runtimes with sessionMode:"resilient" -> ACP over
#    GET /v1/acp/{runtimeId}?sessionId=&attachToken= -> POST stop -> confirm
#    GET /v1/agents shows both runtimes stopped but still tracked.
```

Success looks like: two distinct runtime IDs, two independent gateway sessions
serving concurrent ACP conversations, notifications forwarded on both pumps,
resume + `session/load` working after a disconnect, and stop-all terminating
both processes.

## macOS launchd integration test

The launchd service manager (`internal/service/manager_darwin.go` +
`manager_darwin_common.go`) is verified against real launchd on GitHub Actions
`macos-latest` runners (which have a GUI login session) by
`TestLaunchdLifecycle`. Locally it requires a Mac with an active login
session:

```bash
FERNGEIST_RUN_REAL_AGENT_TESTS=1 go test ./internal/service -run TestLaunchdLifecycle -v
```

The test installs, restarts, stops, starts, reinstalls, and uninstalls the
LaunchAgent, cleaning up after itself. It is opt-in (env-gated) so ordinary
`go test ./...` runs stay hermetic.

## Self-update in dev builds

`ferngeist-gateway update` is gated on the build's `updateChannel` ldflag:
`""`/`"self"` installs may self-update, package-manager installs refuse. Dev
builds (`go run`) have `updateChannel` empty, so the command runs — but it
requires an installed daemon service and a tagged release, so it will fail with
a helpful error on a dev box (`daemon service is not installed`). To simulate a
package-manager install while developing:

```bash
go run -ldflags "-X main.buildVersion=0.0.0-dev -X main.updateChannel=brew" ./cmd/ferngeist update
```

## Notes

- The gateway is a local backend service for ACP-compatible agents.
- It exposes a unified WebSocket API.
- It is used as the backend for the Ferngeist Android app.
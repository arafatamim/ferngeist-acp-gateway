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

## Notes

- The gateway is a local backend service for ACP-compatible agents.
- It exposes a unified WebSocket API.
- It is used as the backend for the Ferngeist Android app.
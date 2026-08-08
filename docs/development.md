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
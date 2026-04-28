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

## Notes

- The gateway is a local backend service for ACP-compatible agents.
- It exposes a unified WebSocket API.
- It is used as the backend for the Ferngeist Android app.
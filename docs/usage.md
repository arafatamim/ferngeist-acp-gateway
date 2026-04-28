go run .\cmd\ferngeist daemon run
```

This starts the gateway on `127.0.0.1:5788` by default.

## Pairing

To pair a device during local development:

```powershell
go run .\cmd\ferngeist pair
```

To expose the gateway on your local network:

```powershell
go run .\cmd\ferngeist daemon run --lan
```

Then pair the device from the client app.

## Common commands

```powershell
go run .\cmd\ferngeist daemon status
go run .\cmd\ferngeist devices list
```

## Notes

- The gateway is a local backend service for ACP-compatible agents.
- It exposes a unified WebSocket API.
- It is used as the backend for the Ferngeist Android app.
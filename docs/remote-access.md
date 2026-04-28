# Remote Access

Ferngeist Gateway listens on `127.0.0.1:5788` by default.

## Modes

### Local only

Use this when the client and gateway are on the same machine or network and you do not need public access.

### LAN access

Use `daemon run --lan` when you want devices on the local network to reach the gateway during development.

```powershell
go run ./cmd/ferngeist daemon run --lan
go run ./cmd/ferngeist pair
```

### Public access

If the gateway must be reachable from outside the local network, put it behind a tunnel or reverse proxy and configure the public URL clients should use.

## Tunnel

### ngrok

```powershell
ngrok http 5788
go run ./cmd/ferngeist daemon install --public-url https://xxxx.ngrok.io
```

### Cloudflare Tunnel

#### Temporary

```powershell
cloudflared tunnel --url http://localhost:5788
go run ./cmd/ferngeist daemon install --public-url https://xxxx.trycloudflare.com
```

#### Permanent

> NOTE: This requires you to have your own domain and use Cloudflare as its authoritative DNS provider.

Follow the official Cloudflare guide for creating a remote tunnel: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/get-started/create-remote-tunnel/

Point the tunnel at `http://localhost:5788` and set `--public-url` to the hostname clients will use.

### Tailscale Serve and Funnel

Use `tailscale serve` for private access inside your tailnet, or `tailscale funnel` to expose the gateway publicly.

```powershell
tailscale serve --bg 5788
go run ./cmd/ferngeist daemon install --public-url https://your-machine.ts.net
```

For public sharing with Funnel, use the HTTPS URL Tailscale assigns and register it as the public URL.

```powershell
tailscale funnel --bg 5788
go run ./cmd/ferngeist daemon install --public-url https://your-machine.ts.net
```

### Reverse proxy

If you already have a reverse proxy, point it at `127.0.0.1:5788` and set the public URL to the address clients will use.

For a reverse proxy example, see [Caddy reverse proxy docs](https://caddyserver.com/docs/quick-starts/reverse-proxy#https-from-client-to-proxy).

```powershell
go run ./cmd/ferngeist daemon install --public-url https://your.domain.example
```

### Other tunnel providers

If you prefer a different tunnel service, refer to its own documentation:

- [Pinggy](https://pinggy.io/)
- [zrok](https://zrok.io/)
- [localtunnel](https://theboroer.github.io/localtunnel-www/)

## Notes

- The public URL should match the address the client will use.
- For local development, LAN mode is usually enough.
- For production use, prefer a stable reverse proxy or tunnel configuration.

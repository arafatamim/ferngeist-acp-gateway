package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// WebSocket keepalive tuning. Coder's websocket library does not auto-detect a
// half-open peer, so the server pings periodically; a missed pong closes the
// connection, which unblocks the read loop and releases the session.
const (
	wsKeepAliveInterval = 30 * time.Second
	wsKeepAlivePingWait = 10 * time.Second
)

// websocketWriteContext returns a context with the configured ACP WebSocket
// write timeout. Each outgoing message write is bounded by this deadline.
func websocketWriteContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), acpWebSocketWriteTimeout)
}

// keepAliveWebSocket pings the client on a fixed interval until ctx is cancelled.
// A ping that is not answered within wsKeepAlivePingWait is treated as a dead
// peer: the connection is force-closed so the read loop returns and the session
// detaches. This bounds how long a session can sit falsely "connected" behind a
// half-open socket (e.g. the mobile app was killed without a clean close).
func keepAliveWebSocket(ctx context.Context, conn *websocket.Conn, logger *slog.Logger) {
	ticker := time.NewTicker(wsKeepAliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, wsKeepAlivePingWait)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				if ctx.Err() == nil {
					logger.Warn("websocket keepalive ping failed; closing", "error", err)
					conn.CloseNow()
				}
				return
			}
		}
	}
}

// proxyWebSocketToStdio adapts ACP-over-WebSocket client messages into the
// newline-delimited stdio framing used by CLI ACP servers. It also mirrors the
// raw client payload into the runtime log buffer as `acp.stdin` traffic.
// The loop exits on normal WebSocket closure or any read/write error.
//
// Parameters writeFunc and closeFunc replace the old io.WriteCloser to support
// both legacy (stdin.Write + stdin.Close) and resilient (pump.WriteToAgent +
// no-op close) paths without duplicating the read loop.
//
// The optional logInbound callback is called for each inbound frame and is used
// by the resilient session path to asynchronously record client->agent diagnostics.
func proxyWebSocketToStdio(src *websocket.Conn, writeFunc func([]byte) error, closeFunc func(), runtimeID string, appendLog func(string, string, string), logInbound func([]byte), done chan<- error) {
	defer closeFunc()
	for {
		// ACP sessions can stay quiet between turns. Do not treat a short idle
		// period as a dead websocket just because no client frame arrived yet.
		messageType, payload, err := src.Read(context.Background())
		if err != nil {
			if closeStatus := websocket.CloseStatus(err); closeStatus == websocket.StatusNormalClosure || closeStatus == websocket.StatusGoingAway {
				done <- io.EOF
				return
			}
			done <- err
			return
		}
		if messageType != websocket.MessageText && messageType != websocket.MessageBinary {
			continue
		}
		if appendLog != nil {
			appendLog(runtimeID, "acp.stdin", string(payload))
		}
		if logInbound != nil {
			logInbound(payload)
		}
		if err := writeFunc(payload); err != nil {
			done <- err
			return
		}
	}
}

// websocketScheme determines the WebSocket scheme (ws/wss) to advertise in
// connection responses. It respects X-Forwarded-Proto for reverse proxy setups,
// falling back to the presence of TLS on the direct connection.
func websocketScheme(r *http.Request) string {
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); forwarded != "" {
		switch strings.ToLower(forwarded) {
		case "https", "wss":
			return "wss"
		case "http", "ws":
			return "ws"
		}
	}
	if r.TLS != nil {
		return "wss"
	}
	return "ws"
}

// websocketHost returns the host that clients should use to reach this gateway.
// It respects X-Forwarded-Host for reverse proxy configurations.
func websocketHost(r *http.Request) string {
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); forwarded != "" {
		return forwarded
	}
	return r.Host
}

// websocketHostWithPath returns the direct websocket endpoint without embedding
// auth material into the URL. Clients should use the returned bearer token via
// Authorization headers when opening the ACP socket.
func websocketHostWithPath(r *http.Request, path string) string {
	return fmt.Sprintf("%s%s", websocketHost(r), path)
}

func absoluteWebSocketURL(r *http.Request, path string) string {
	return fmt.Sprintf("%s://%s", websocketScheme(r), websocketHostWithPath(r, path))
}

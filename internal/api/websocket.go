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

// keepAliveWebSocket pings the client on a fixed interval until ctx is cancelled.
// A ping that is not answered within wsKeepAlivePingWait is treated as a dead
// peer: the connection is force-closed so the read loop returns and the session
// detaches. This bounds how long a session can sit falsely "connected" behind a
// half-open socket (e.g. the mobile app was killed without a clean close).
//
// A late pong is not proof of a dead peer while writes are still completing:
// the ping is queued behind everything already sent, so during a long
// session/load replay the phone answers only once it has read the bulk ahead of
// it. Closing then killed every attempt to load a long transcript at its first
// ping, in a reconnect loop. lastWrite reports the last completed client write;
// a peer that drained one within the last interval is alive (a half-open socket
// stops draining, and the pump's per-frame write timeout closes it).
func keepAliveWebSocket(ctx context.Context, conn *websocket.Conn, logger *slog.Logger, lastWrite func() time.Time) {
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
				if ctx.Err() == nil && time.Since(lastWrite()) < wsKeepAliveInterval {
					continue
				}
				if ctx.Err() == nil {
					logger.Warn("websocket keepalive ping failed; closing", "error", err)
					_ = conn.CloseNow()
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
// The caller owns the agent's stdin so a client disconnect leaves it open.
//
// The optional logInbound callback is called for each inbound frame and is used
// by the resilient session path to asynchronously record client->agent diagnostics.
func proxyWebSocketToStdio(ctx context.Context, src *websocket.Conn, writeFunc func([]byte) error, runtimeID string, appendLog func(string, string, string), logInbound func([]byte)) error {
	for {
		// ACP sessions can stay quiet between turns. Do not treat a short idle
		// period as a dead websocket just because no client frame arrived yet.
		messageType, payload, err := src.Read(ctx)
		if err != nil {
			if closeStatus := websocket.CloseStatus(err); closeStatus == websocket.StatusNormalClosure || closeStatus == websocket.StatusGoingAway {
				return io.EOF
			}
			return err
		}
		if messageType != websocket.MessageText && messageType != websocket.MessageBinary {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if appendLog != nil {
			appendLog(runtimeID, "acp.stdin", string(payload))
		}
		if logInbound != nil {
			logInbound(payload)
		}
		if err := writeFunc(payload); err != nil {
			return err
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

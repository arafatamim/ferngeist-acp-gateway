package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/turns"
)

// Turn stream limits. The client-visible ones are contract (C5); the slots
// bound concurrent snapshot builds and their buffers, not stream count.
const (
	turnStreamReadLimit       int64 = 32 * 1024
	turnStreamSnapshotSlots         = 8
	turnStreamSnapshotTimeout       = 20 * time.Second
	turnStreamWriteTimeout          = 10 * time.Second
	turnStreamRecheck               = 30 * time.Second
)

// Stream-level close codes, in the RFC 6455 private range.
const (
	turnStreamStatusUnauthorized websocket.StatusCode = 4401
	turnStreamStatusSlowConsumer websocket.StatusCode = 4408
	turnStreamStatusSuperseded   websocket.StatusCode = 4409
)

// turnsResponse is the GET /v1/turns body. GatewayStartedAt marks the daemon
// lifetime the outcomes belong to: they do not survive a restart.
type turnsResponse struct {
	GatewayStartedAt time.Time `json:"gatewayStartedAt"`
	turns.Snapshot
}

// turnStreamSnapshot is the stream's first frame: the GET /v1/turns body plus
// the frame type.
type turnStreamSnapshot struct {
	Type string `json:"type"`
	turnsResponse
}

// turnStreamChanges is one sample's difference from what this connection last
// received. Both arrays are always present; at least one is non-empty.
type turnStreamChanges struct {
	Type       string         `json:"type"`
	ObservedAt time.Time      `json:"observedAt"`
	Upserts    []turns.Status `json:"upserts"`
	Removed    []turns.Key    `json:"removed"`
}

// turnStream is one open stream: its socket, its subscription and the counters
// its close log reports. One goroutine (the handler's) writes frames.
type turnStream struct {
	server    *Server
	conn      *websocket.Conn
	sub       *turns.Subscription
	token     string
	lastWrite atomic.Int64 // UnixNano of the last completed frame
	messages  int
}

// handleTurns serves the cross-device status of every known turn from memory;
// it never touches an agent process or the listing path.
func (s *Server) handleTurns(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeRead); !ok {
		return
	}
	if s.sessionSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "session service not available")
		return
	}
	writeJSON(w, http.StatusOK, turnsResponse{GatewayStartedAt: s.startedAt, Snapshot: s.sessionSvc.TurnSnapshot()})
}

// handleTurnStream upgrades GET /v1/turns/stream and streams turn changes until
// the client leaves, its credential stops validating, or the gateway shuts
// down. Auth and scope are the same as GET /v1/turns and are checked before the
// upgrade, so a rejection is an ordinary HTTP response.
func (s *Server) handleTurnStream(w http.ResponseWriter, r *http.Request) {
	credential, ok := s.requireGatewayScope(w, r, pairing.ScopeRead)
	if !ok {
		return
	}
	if s.sessionSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "session service not available")
		return
	}
	if !s.enterTurnStream() {
		writeError(w, http.StatusServiceUnavailable, "gateway shutting down")
		return
	}
	defer s.streamHandlers.Done()

	release, ok := s.acquireStreamSlot(r.Context())
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "turn stream is at capacity")
		return
	}
	// The slot bounds a snapshot build, not the stream: free it once the frame
	// is written (and on every early return before that).
	defer release()

	snapshot, sub, err := s.sessionSvc.TurnFeed().Subscribe(r.Context(), credential.DeviceID)
	if err != nil {
		s.logger.Warn("turn stream subscribe failed", "device_id", credential.DeviceID, "error", err)
		writeError(w, http.StatusServiceUnavailable, "turn stream is at capacity")
		return
	}
	defer sub.Close()

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		s.logger.Warn("turn stream upgrade failed", "device_id", credential.DeviceID, "error", err)
		return
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(turnStreamReadLimit)
	// The client has nothing to say: read only to notice a close or a data
	// frame, which the stream answers with 1008 (or 1009 past the read limit).
	ctx, stopRead := readTurnStreamClient(r.Context(), conn)
	defer stopRead()

	stream := &turnStream{server: s, conn: conn, sub: sub, token: bearerToken(r)}
	stream.lastWrite.Store(time.Now().UnixNano())
	go keepAliveWebSocket(ctx, conn, s.logger, stream.lastWriteAt)

	end := stream.run(ctx, snapshot, release)
	if end.code != 0 {
		_ = conn.Close(end.code, end.reason)
	}
	s.logger.Info("turn stream closed",
		"code", int(end.code), "device_id", credential.DeviceID,
		"messages", end.messages, "error", end.err)
}

// enterTurnStream registers a stream handler for Shutdown to join, or reports
// false once Shutdown has begun.
func (s *Server) enterTurnStream() bool {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	select {
	case <-s.streamStop:
		return false
	default:
		s.streamHandlers.Add(1)
		return true
	}
}

// acquireStreamSlot takes one of the bounded snapshot slots, waiting within the
// snapshot budget, and returns an idempotent release.
func (s *Server) acquireStreamSlot(ctx context.Context) (func(), bool) {
	release := sync.OnceFunc(func() { <-s.streamSlots })
	select {
	case s.streamSlots <- struct{}{}:
		return release, true
	default:
	}
	timer := time.NewTimer(s.streamSnapshotTimeout)
	defer timer.Stop()
	select {
	case s.streamSlots <- struct{}{}:
		return release, true
	case <-s.streamStop:
		return nil, false
	case <-timer.C:
		return nil, false
	case <-ctx.Done():
		return nil, false
	}
}

// turnStreamEnd is how a stream ended: the close code to send (zero when the
// peer closed first or the socket is already broken, so no frame is sent), the
// reason, and why.
type turnStreamEnd struct {
	code     websocket.StatusCode
	reason   string
	err      error
	messages int
}

// run writes the snapshot, frees the snapshot slot, then every change until the
// subscription ends, the credential stops validating, or the gateway shuts
// down. The caller owns the close frame and the log line.
func (t *turnStream) run(ctx context.Context, snapshot turns.Snapshot, release func()) turnStreamEnd {
	payload, err := json.Marshal(turnStreamSnapshot{
		Type:          "snapshot",
		turnsResponse: turnsResponse{GatewayStartedAt: t.server.startedAt, Snapshot: snapshot},
	})
	if err != nil {
		return turnStreamEnd{code: websocket.StatusInternalError, err: err}
	}
	if err := t.writeFrame(ctx, payload, turnStreamSnapshotTimeout); err != nil {
		return turnStreamEnd{err: err}
	}
	release()
	// Only now do the device limit and the queue bound apply: until this write
	// lands, changes are consolidated into one catch-up message.
	t.sub.Ready()

	recheck := time.NewTicker(t.server.streamRecheck)
	defer recheck.Stop()
	for {
		select {
		case <-ctx.Done():
			return turnStreamEnd{messages: t.messages}
		case <-t.server.streamStop:
			return turnStreamEnd{code: websocket.StatusGoingAway, reason: "gateway shutting down", messages: t.messages}
		case <-recheck.C:
			if err := t.server.revalidateStreamCredential(t.token); err != nil {
				return turnStreamEnd{code: turnStreamStatusUnauthorized, reason: streamAuthReason(err), err: err, messages: t.messages}
			}
		case change, ok := <-t.sub.Changes():
			if !ok {
				err := t.sub.Err()
				code, reason := turnStreamClose(err)
				return turnStreamEnd{code: code, reason: reason, err: err, messages: t.messages}
			}
			end, ok := t.publishChange(ctx, change)
			if !ok {
				return end
			}
		}
	}
}

// publishChange revalidates the credential, then writes one changes frame. ok is
// false when the stream must end, with end saying how.
func (t *turnStream) publishChange(ctx context.Context, change turns.Change) (turnStreamEnd, bool) {
	if err := t.server.revalidateStreamCredential(t.token); err != nil {
		return turnStreamEnd{code: turnStreamStatusUnauthorized, reason: streamAuthReason(err), err: err, messages: t.messages}, false
	}
	payload, err := json.Marshal(turnStreamChanges{
		Type:       "changes",
		ObservedAt: change.ObservedAt,
		// Both arrays are always present, empty or not: a client applies them
		// without a nil check per frame.
		Upserts: append([]turns.Status{}, change.Upserts...),
		Removed: append([]turns.Key{}, change.Removed...),
	})
	if err != nil {
		return turnStreamEnd{code: websocket.StatusInternalError, err: err, messages: t.messages}, false
	}
	if err := t.writeFrame(ctx, payload, turnStreamWriteTimeout); err != nil {
		return turnStreamEnd{err: err, messages: t.messages}, false
	}
	return turnStreamEnd{}, true
}

// readTurnStreamClient watches the client's side of a stream, which may only
// carry control frames. The returned context is cancelled when the client
// closes or sends data; the returned stop function cancels and joins the
// reader, so the handler owns the goroutine's exit.
//
// A data frame closes the stream with 1008, or 1009 when reading it hit the
// read limit (coder/websocket closes that one itself). CloseRead is not used
// because it answers every data frame with 1008: it reads only the frame
// header and never hits the payload limit.
func readTurnStreamClient(ctx context.Context, conn *websocket.Conn) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		if _, _, err := conn.Read(ctx); err == nil {
			_ = conn.Close(websocket.StatusPolicyViolation, "unexpected data frame")
		}
	}()
	return ctx, func() {
		cancel()
		<-done
	}
}

// writeFrame writes one JSON text frame under its own deadline; a write that
// misses it closes the connection.
func (t *turnStream) writeFrame(ctx context.Context, payload []byte, timeout time.Duration) error {
	writeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := t.conn.Write(writeCtx, websocket.MessageText, payload); err != nil {
		return err
	}
	t.lastWrite.Store(time.Now().UnixNano())
	t.messages++
	return nil
}

// lastWriteAt is the last completed frame's time, which lets the keepalive
// treat a peer still draining a large snapshot as alive.
func (t *turnStream) lastWriteAt() time.Time {
	return time.Unix(0, t.lastWrite.Load()).UTC()
}

// revalidateStreamCredential re-checks the bearer token behind an open stream.
// The upgrade checked the proof; from then on the token's validity, not a fresh
// signature, is what can change.
func (s *Server) revalidateStreamCredential(token string) error {
	credential, err := s.pairing.ValidateCredential(token)
	if err != nil {
		return err
	}
	if strings.TrimSpace(credential.ProofPublicKey) == "" && !s.cfg.AllowLegacyBearerCredentials {
		return errLegacyCredentialsDisabled
	}
	return credential.RequireScope(pairing.ScopeRead)
}

// streamAuthReason is the close reason for a credential that stopped
// validating: the auth code clients branch on.
func streamAuthReason(err error) string {
	if code := authErrorCode(err); code != "" {
		return code
	}
	return credentialInvalid
}

// turnStreamClose maps why a subscription ended to the close code and reason
// the client acts on.
func turnStreamClose(err error) (websocket.StatusCode, string) {
	switch {
	case errors.Is(err, turns.ErrSlowConsumer):
		return turnStreamStatusSlowConsumer, "client fell behind"
	case errors.Is(err, turns.ErrSuperseded):
		return turnStreamStatusSuperseded, "superseded by a newer stream"
	case errors.Is(err, turns.ErrStopped):
		return websocket.StatusGoingAway, "gateway shutting down"
	default:
		return websocket.StatusInternalError, "turn stream failed"
	}
}

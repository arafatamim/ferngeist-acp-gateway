// Package session manages durable, reconnectable agent sessions for the gateway.
// This file contains all RuntimeSession method implementations — lifecycle
// (Create, Resume, Attach, Detach, Close), query (ListByDevice, GetPump,
// GetSessionStatus, FindReconnectableByRuntime), notification
// (sendPushNotification, handleProcessExit), diagnostics (LogInbound),
// and background goroutines (reaperLoop, reapExpired, Shutdown).
package session

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/push"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
	"github.com/coder/acp-go-sdk"
	"github.com/coder/websocket"
)

// Create establishes a new resilient session for a runtime. It is best-effort:
// if creation fails after the runtime was already acquired, the session
// record is cleaned up and the lease is released.
//
// Ordering matters: store record first so the session is visible even if
// subsequent steps fail (allowing cleanup). Then acquire the lease to ensure
// the runtime is alive and unleased. On failure at any step, delete the record
// and release any acquired lease to leave no orphaned state.
func (rs *RuntimeSession) Create(ctx context.Context, runtimeID, deviceID, agentID string) (*Session, string, error) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	if rs.cfg.MaxPerDevice > 0 {
		existing, err := rs.store.ListSessionsByDevice(ctx, deviceID)
		if err == nil && len(existing) >= rs.cfg.MaxPerDevice {
			return nil, "", ErrSessionLimitReached
		}
	}

	sessionID := generateID()

	now := time.Now().UTC()
	rec := storage.SessionRecord{
		SessionID:   sessionID,
		RuntimeID:   runtimeID,
		DeviceID:    deviceID,
		AgentID:     agentID,
		Status:      StatusActive,
		Leaseholder: sessionID,
		CreatedAt:   now,
	}
	if err := rs.store.SaveSession(ctx, rec); err != nil {
		return nil, "", err
	}

	pipes, err := rs.pm.AcquireLease(runtimeID, sessionID)
	if err != nil {
		// Lease acquisition failed — undo the store record so no orphaned session remains.
		rs.store.DeleteSession(ctx, sessionID)
		return nil, "", err
	}

	// The pump needs the concrete *runtime.LeasedPipes for Stdout access in the drain loop.
	lp, ok := pipes.(*runtime.LeasedPipes)
	if !ok {
		rs.store.DeleteSession(ctx, sessionID)
		rs.pm.ReleaseLease(runtimeID, sessionID)
		return nil, "", errors.New("unexpected pipe type from ProcessManager")
	}

	pumpCtx, pumpCancel := context.WithCancel(context.Background())

	onPushNotification := func(e PushEvent) {
		rs.mu.Lock()
		s, ok := rs.sessions[e.SessionID]
		rs.mu.Unlock()
		if !ok || s.DeviceID == "" {
			return
		}
		// Dispatch asynchronously: this runs on the stdout drain-loop goroutine,
		// and a push is a token lookup plus a network round-trip to the provider.
		// Blocking here would stall agent stdout draining — and any attached
		// client's live stream — until the push completes or times out.
		rs.sendPushNotification(s.DeviceID, e.AcpSessionID, e.Title, e.Body, e.Category)
	}

	pump := &StdioPump{
		pipes:              lp,
		runtimeID:          runtimeID,
		sessionID:          sessionID,
		logger:             rs.logger,
		appendLog:          rs.pm.AppendLog,
		onPushNotification: onPushNotification,
	}

	go pump.StdoutDrainLoop(pumpCtx)

	sess := &Session{
		ID:          sessionID,
		RuntimeID:   runtimeID,
		DeviceID:    deviceID,
		AgentID:     agentID,
		Status:      StatusActive,
		Leaseholder: sessionID,
		CreatedAt:   now,
		pump:        pump,
		leasedPipes: pipes,
		cancelPump:  pumpCancel,
	}
	rs.sessions[sessionID] = sess

	rs.pm.OnProcessExit(runtimeID, func(rid string) {
		rs.handleProcessExit(sessionID, runtimeID, deviceID, agentID)
	})

	attachToken, err := rs.tokenSvc.Mint(sessionID, deviceID, 5*time.Minute)
	if err != nil {
		attachToken = "" // best-effort: session is created but reconnection requires a token
	}

	return sess, attachToken, nil
}

// sendPushNotification dispatches a push notification asynchronously with a 10s
// timeout so a slow or failing provider never blocks the caller. No-op when push
// notifications are not configured (PushSvc is nil).
func (rs *RuntimeSession) sendPushNotification(deviceID, acpSessionID, title, body, category string) {
	if rs.cfg.PushSvc == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = rs.cfg.PushSvc.Notify(ctx, deviceID, push.Notification{
			Title:     title,
			Body:      body,
			Category:  category,
			ServerID:  rs.cfg.GatewayID,
			SessionID: acpSessionID,
		})
	}()
}

// handleProcessExit is called by the OnProcessExit callback when a runtime
// process terminates. It detects genuine crashes (exits not preceded by a
// Close that set StatusClosing), persists the failed status, and fires a push
// notification if the session belongs to a device.
func (rs *RuntimeSession) handleProcessExit(sessionID, runtimeID, deviceID, agentID string) {
	rs.mu.Lock()
	var deviceIDForPush string
	var acpSessionIDForPush string
	var crashed bool
	if s, ok := rs.sessions[sessionID]; ok {
		s.mu.Lock()
		// The supervisor fires this callback on every process exit — including
		// the intentional StopByRuntimeID issued by Close and the reaper, both
		// of which mark the session StatusClosing *before* stopping the runtime.
		// Only an exit from a non-closing session is a genuine crash; otherwise
		// we must not persist a bogus "failed" record or push a false alarm.
		if s.Status != StatusClosing {
			crashed = true
			s.Status = StatusFailed
			deviceIDForPush = s.DeviceID
			// ACP session id (the id the client navigates by), for the crash push.
			acpSessionIDForPush = s.pump.AcpSessionID()
		}
		s.mu.Unlock()
		if crashed {
			rs.store.SaveSession(context.Background(), storage.SessionRecord{
				SessionID:   sessionID,
				RuntimeID:   runtimeID,
				DeviceID:    deviceID,
				AgentID:     agentID,
				Status:      StatusFailed,
				Leaseholder: sessionID,
				CreatedAt:   s.CreatedAt,
			})
		}
	}
	rs.mu.Unlock()

	// Notify on a genuine crash regardless of client attachment; the client
	// decides whether to surface it based on its own foreground/background
	// state. Dispatched asynchronously inside sendPushNotification.
	if crashed && deviceIDForPush != "" {
		rs.sendPushNotification(deviceIDForPush, acpSessionIDForPush, "Agent Crashed", "Your agent has stopped unexpectedly.", push.CategoryAgentCrash)
	}
}

// FindReconnectableByRuntime returns the ID of the existing reconnectable
// session (active or disconnected) for a runtime owned by the given device, if
// one exists. A runtime has at most one session because the session holds the
// runtime's exclusive lease. This lets a reconnect reuse the live session — and
// its still-running agent — instead of attempting to create a second session,
// which would fail with ErrRuntimeLeaseHeld because the lease is still held.
func (rs *RuntimeSession) FindReconnectableByRuntime(runtimeID, deviceID string) (string, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for _, sess := range rs.sessions {
		sess.mu.Lock()
		match := sess.RuntimeID == runtimeID && sess.DeviceID == deviceID &&
			(sess.Status == StatusActive || sess.Status == StatusDisconnected)
		id := sess.ID
		sess.mu.Unlock()
		if match {
			return id, true
		}
	}
	return "", false
}

// Resume mints a new single-use attach token for reconnecting to an existing session.
func (rs *RuntimeSession) Resume(ctx context.Context, sessionID, deviceID string) (string, error) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	sess, ok := rs.sessions[sessionID]
	if !ok {
		return "", ErrSessionNotFound
	}

	if sess.DeviceID != deviceID {
		return "", ErrDeviceMismatch
	}

	if sess.Status != StatusActive && sess.Status != StatusDisconnected {
		return "", ErrSessionNotActive
	}

	token, err := rs.tokenSvc.Mint(sessionID, deviceID, 5*time.Minute)
	if err != nil {
		return "", err
	}
	return token, nil
}

// AttachClient validates the attach token and claims the session for a new
// client, taking over from any previously-bound connection. It returns the
// session's RuntimeID (so the handler can verify the path parameter) and a
// connection generation that the caller must pass to BindConn and DetachClient
// so a superseded connection cannot bind or detach over a newer one.
//
// Takeover (rather than rejecting with a 409) is deliberate: a resilient
// session's previous connection is almost always a dead mobile socket the
// gateway has not yet observed as closed (half-open TCP, no FIN). Refusing the
// reconnect would strand the session until the dead peer is detected, so a valid
// attach always wins and evicts the stale connection.
func (rs *RuntimeSession) AttachClient(ctx context.Context, sessionID, attachToken string) (string, int64, error) {
	validatedSessionID, claimDeviceID, err := rs.tokenSvc.Validate(attachToken)
	if err != nil {
		return "", 0, ErrAttachTokenInvalid
	}
	if validatedSessionID != sessionID {
		return "", 0, ErrAttachTokenInvalid
	}

	rs.mu.Lock()
	sess, ok := rs.sessions[sessionID]
	rs.mu.Unlock()

	if !ok {
		return "", 0, ErrSessionNotFound
	}

	sess.mu.Lock()
	if sess.DeviceID != claimDeviceID {
		sess.mu.Unlock()
		return "", 0, ErrDeviceMismatch
	}
	if sess.Status == StatusFailed || sess.Status == StatusClosing {
		sess.mu.Unlock()
		return "", 0, ErrSessionNotActive
	}

	// Supersede any prior connection. The live conn is set later by BindConn,
	// once the WebSocket upgrade succeeds; until then the pump has no client.
	oldConn := sess.currentConn
	sess.connGen++
	gen := sess.connGen
	sess.currentConn = nil
	sess.Status = StatusActive
	sess.DisconnectedAt = nil
	record := storage.SessionRecord{
		SessionID:   sess.ID,
		RuntimeID:   sess.RuntimeID,
		DeviceID:    sess.DeviceID,
		AgentID:     sess.AgentID,
		Status:      StatusActive,
		Leaseholder: sess.Leaseholder,
		CreatedAt:   sess.CreatedAt,
	}
	runtimeID := sess.RuntimeID
	pump := sess.pump
	sess.mu.Unlock()

	// Evict the superseded connection outside the lock. Closing it unblocks the
	// old handler's read loop, whose deferred DetachClient is fenced by gen and
	// therefore becomes a no-op.
	if oldConn != nil {
		oldConn.CloseNow()
	}
	pump.ClearClient()

	now := time.Now().UTC()
	record.LastClientConnectAt = &now
	rs.store.SaveSession(ctx, record)

	return runtimeID, gen, nil
}

// BindConn attaches the upgraded WebSocket to the session pump for the given
// generation. It returns false if a newer attach has already superseded this
// generation, in which case the caller should discard the connection.
func (rs *RuntimeSession) BindConn(sessionID string, conn *websocket.Conn, gen int64) bool {
	rs.mu.Lock()
	sess, ok := rs.sessions[sessionID]
	rs.mu.Unlock()
	if !ok {
		return false
	}

	sess.mu.Lock()
	if sess.connGen != gen {
		sess.mu.Unlock()
		return false
	}
	sess.currentConn = conn
	pump := sess.pump
	sess.mu.Unlock()

	pump.SetClient(conn)
	return true
}

// DetachClient marks the session as disconnected, but only when gen still
// matches the current connection generation. A superseded (evicted) connection
// calling DetachClient is a no-op, so it cannot clobber the connection that
// replaced it. The pump keeps running regardless.
func (rs *RuntimeSession) DetachClient(sessionID string, gen int64) error {
	rs.mu.Lock()
	sess, ok := rs.sessions[sessionID]
	rs.mu.Unlock()
	if !ok {
		return ErrSessionNotFound
	}

	sess.mu.Lock()
	if sess.connGen != gen {
		// A newer connection owns the session now; this detach is stale.
		sess.mu.Unlock()
		return nil
	}
	now := time.Now().UTC()
	sess.Status = StatusDisconnected
	sess.DisconnectedAt = &now
	sess.currentConn = nil
	pump := sess.pump
	record := storage.SessionRecord{
		SessionID:              sess.ID,
		RuntimeID:              sess.RuntimeID,
		DeviceID:               sess.DeviceID,
		AgentID:                sess.AgentID,
		Status:                 StatusDisconnected,
		Leaseholder:            sess.Leaseholder,
		CreatedAt:              sess.CreatedAt,
		LastClientDisconnectAt: &now,
		DisconnectedSince:      &now,
	}
	sess.mu.Unlock()

	pump.ClearClient()
	rs.store.SaveSession(context.Background(), record)

	return nil
}

// Close terminates a session. The order of operations ensures clean teardown:
//  1. Mark closing (persisted) — prevents reconnection during shutdown
//  2. Stop pump — the stdout drain loop is cancelled
//  3. Send ACP session/close — gives agent a chance to cancel in-flight work
//  4. Delete from in-memory registry — before stopping the runtime, so the
//     OnProcessExit callback (which fires during StopByRuntimeID) won't find the
//     session and will no-op safely instead of racing with teardown.
//  5. Stop runtime — 2-second graceful timeout, then force kill. The session is
//     already out of the map, so any concurrent lookup fails cleanly.
//  6. Release lease — clears the leaseholder on the process handle
//  7. Delete from storage — cascades to outbound/inbound rows
func (rs *RuntimeSession) Close(ctx context.Context, sessionID, deviceID string) error {
	rs.mu.Lock()
	sess, ok := rs.sessions[sessionID]
	if !ok {
		rs.mu.Unlock()
		return ErrSessionNotFound
	}
	if sess.DeviceID != deviceID {
		rs.mu.Unlock()
		return ErrDeviceMismatch
	}

	// Step 1: Mark the session as closing so no concurrent operation treats it as live.
	sess.mu.Lock()
	sess.Status = StatusClosing
	sess.mu.Unlock()

	rs.store.SaveSession(ctx, storage.SessionRecord{
		SessionID:   sess.ID,
		RuntimeID:   sess.RuntimeID,
		DeviceID:    sess.DeviceID,
		AgentID:     sess.AgentID,
		Status:      StatusClosing,
		Leaseholder: sess.Leaseholder,
		CreatedAt:   sess.CreatedAt,
	})

	// Step 2: Stop the stdout drain loop so no new frames enter the buffer.
	if sess.cancelPump != nil {
		sess.cancelPump()
	}

	// Step 3: If the agent supports session/close, send one last ACP request
	// so it can cancel in-progress work before the process is killed.
	// Uses acp.CloseSessionRequest for typed param construction.
	if sess.pump.SupportsClose() {
		closeMsg, _ := json.Marshal(struct {
			JSONRPC string                   `json:"jsonrpc"`
			Method  string                   `json:"method"`
			ID      string                   `json:"id"`
			Params  acp.CloseSessionRequest  `json:"params"`
		}{
			JSONRPC: "2.0",
			Method:  "session/close",
			ID:      "gw-close-" + sessionID,
			Params:  acp.CloseSessionRequest{SessionId: acp.SessionId(sessionID)},
		})
		_ = sess.leasedPipes.WriteToAgent(closeMsg)
	}

	delete(rs.sessions, sessionID)
	rs.mu.Unlock()

	rs.pm.StopByRuntimeID(sess.RuntimeID)

	rs.mu.Lock()
	rs.pm.ReleaseLease(sess.RuntimeID, sess.Leaseholder)

	rs.store.DeleteSession(ctx, sessionID)

	rs.mu.Unlock()

	return nil
}

// ListByDevice returns summaries of all sessions owned by a device.
func (rs *RuntimeSession) ListByDevice(ctx context.Context, deviceID string) ([]SessionSummary, error) {
	records, err := rs.store.ListSessionsByDevice(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	summaries := make([]SessionSummary, 0, len(records))
	for _, rec := range records {
		summaries = append(summaries, SessionSummary{
			SessionID: rec.SessionID,
			RuntimeID: rec.RuntimeID,
			AgentID:   rec.AgentID,
			Status:    rec.Status,
			CreatedAt: rec.CreatedAt,
		})
	}
	return summaries, nil
}

// GetPump returns the StdioPump for a session (used by HTTP handlers to call SetClient).
func (rs *RuntimeSession) GetPump(sessionID string) (*StdioPump, error) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	sess, ok := rs.sessions[sessionID]
	if !ok {
		return nil, ErrSessionNotFound
	}
	return sess.pump, nil
}

// GetSessionStatus returns the current status of a session.
func (rs *RuntimeSession) GetSessionStatus(sessionID string) (string, error) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	sess, ok := rs.sessions[sessionID]
	if !ok {
		return "", ErrSessionNotFound
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.Status, nil
}

// LogInbound asynchronously records a client->agent frame for audit purposes.
// It is non-blocking — if the diagnostic channel is full the frame is dropped
// and the dropped counter is incremented. The inbound sequence counter is
// automatically incremented per-session.
func (rs *RuntimeSession) LogInbound(sessionID string, payload string) {
	rs.mu.Lock()
	sess, ok := rs.sessions[sessionID]
	rs.mu.Unlock()
	if !ok {
		return
	}
	seq := sess.inboundSeq.Add(1)
	rs.inbound.send(inboundDiagnostic{SessionID: sessionID, Seq: seq, Payload: payload})
}

// reaperLoop periodically scans for sessions that have been disconnected longer
// than MaxDisconnected and closes them. Uses a single ticker goroutine instead
// of per-session time.AfterFunc timers, consistent with the supervisor's prune pattern.
func (rs *RuntimeSession) reaperLoop(ctx context.Context) {
	interval := rs.cfg.ReaperInterval
	if interval <= 0 {
		interval = defaultReaperInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	maxDisc := rs.cfg.MaxDisconnected
	if maxDisc <= 0 {
		maxDisc = defaultMaxDisconnected
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rs.reapExpired(maxDisc)
		}
	}
}

func (rs *RuntimeSession) reapExpired(maxDisc time.Duration) {
	now := time.Now().UTC()

	rs.mu.Lock()
	toClose := make(map[string]*Session)
	for id, sess := range rs.sessions {
		sess.mu.Lock()
		status := sess.Status
		discAt := sess.DisconnectedAt
		sess.mu.Unlock()

		if status == StatusDisconnected && discAt != nil {
			// Use the later of DisconnectedAt and lastStdoutAt so that an
			// actively-streaming agent extends the grace period automatically.
			discTime := *discAt
			if lastStdout := sess.pump.LastStdoutAt(); !lastStdout.IsZero() && lastStdout.After(discTime) {
				discTime = lastStdout
			}
			if now.Sub(discTime) > maxDisc {
				sess.mu.Lock()
				sess.Status = StatusClosing
				sess.mu.Unlock()
				toClose[id] = sess
			}
		}
	}
	rs.mu.Unlock()

	for id, sess := range toClose {
		if sess.cancelPump != nil {
			sess.cancelPump()
		}
		rs.pm.StopByRuntimeID(sess.RuntimeID)
		rs.pm.ReleaseLease(sess.RuntimeID, sess.Leaseholder)
		rs.store.DeleteSession(context.Background(), sess.ID)
		rs.mu.Lock()
		delete(rs.sessions, id)
		rs.mu.Unlock()
	}
}

// Shutdown stops the reaper, cancels all active session pumps, releases their
// leases, and stops the inbound diagnostic writer.
func (rs *RuntimeSession) Shutdown() {
	if rs.cancelReaper != nil {
		rs.cancelReaper()
	}
	rs.mu.Lock()
	for id, sess := range rs.sessions {
		if sess.cancelPump != nil {
			sess.cancelPump()
		}
		rs.pm.ReleaseLease(sess.RuntimeID, sess.Leaseholder)
		delete(rs.sessions, id)
	}
	rs.mu.Unlock()
	if rs.inbound != nil {
		rs.inbound.stop()
	}
}

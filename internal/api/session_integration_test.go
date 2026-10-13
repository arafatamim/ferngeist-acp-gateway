package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/push"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/session"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

const testGatewayID = "test-gateway-id"

type mockPushService struct {
	mu    sync.Mutex
	calls []pushCall
}

type pushCall struct {
	DeviceID     string
	Notification push.Notification
}

func (m *mockPushService) Notify(_ context.Context, deviceID string, n push.Notification) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, pushCall{DeviceID: deviceID, Notification: n})
	return nil
}

func (m *mockPushService) Calls() []pushCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := make([]pushCall, len(m.calls))
	copy(r, m.calls)
	return r
}

// waitForPushCount blocks until at least want push calls have been recorded, or
// fails the test after timeout.
func waitForPushCount(t *testing.T, m *mockPushService, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(m.Calls()) >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("expected at least %d push calls, got %d", want, len(m.Calls()))
}

type resilientTestHarness struct {
	t         *testing.T
	server    *Server
	store     *storage.SQLiteStore
	token     string
	runtimeID string
	pushSvc   *mockPushService
	httpSrv   *httptest.Server
}

type connectResilientResponse struct {
	RuntimeID   string `json:"runtimeId"`
	SessionID   string `json:"sessionId"`
	AttachToken string `json:"attachToken"`
}

func newResilientTestHarness(t *testing.T) *resilientTestHarness {
	t.Helper()

	baseDir := newHarnessBaseDir(t)
	buildMockAgent(t, baseDir)

	store, err := storage.Open(filepath.Join(baseDir, "test.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { store.Close() })

	server := newTestServerWithStore(baseDir, store)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tokenSvc := session.NewTokenService(logger)
	mockPush := &mockPushService{}
	sessionSvc := session.NewRuntimeSession(logger, store, server.runtime, tokenSvc, session.Config{
		MaxDisconnected: 5 * time.Minute,
		MaxPerDevice:    3,
		PushSvc:         mockPush,
		GatewayID:       testGatewayID,
	})
	server.sessionSvc = sessionSvc
	// Shutdown stops every agent process the service launched, so the process is
	// guaranteed dead before the harness temp dir is removed.
	t.Cleanup(sessionSvc.Shutdown)

	bearerToken := pairDevice(t, server)

	httpSrv := httptest.NewServer(server.Handler())
	t.Cleanup(httpSrv.Close)

	return &resilientTestHarness{
		t:       t,
		server:  server,
		store:   store,
		token:   bearerToken,
		pushSvc: mockPush,
		httpSrv: httpSrv,
	}
}

// connectResilient starts the mock agent and creates its resilient session in
// one step, recording the session's runtime id for the WebSocket path.
func (h *resilientTestHarness) connectResilient() connectResilientResponse {
	h.t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/v1/agents/mock-acp/connect", nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		h.t.Fatalf("connect status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp connectResilientResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		h.t.Fatalf("Unmarshal(connect) error = %v", err)
	}
	h.runtimeID = resp.RuntimeID
	return resp
}

func (h *resilientTestHarness) resumeSession(sessionID string) string {
	h.t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sessionID+"/resume", nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		h.t.Fatalf("resume status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		AttachToken string `json:"attachToken"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		h.t.Fatalf("Unmarshal(resume) error = %v", err)
	}
	return resp.AttachToken
}

func (h *resilientTestHarness) dialSessionWS(sessionID, attachToken string) *websocket.Conn {
	h.t.Helper()

	wsURL := "ws" + strings.TrimPrefix(h.httpSrv.URL, "http") +
		"/v1/acp/" + h.runtimeID +
		"?sessionId=" + sessionID +
		"&attachToken=" + attachToken

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		h.t.Fatalf("websocket.Dial(%s) error = %v", wsURL, err)
	}
	return conn
}

type acpMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func readWSMessage(t *testing.T, conn *websocket.Conn) acpMessage {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("conn.Read() error = %v", err)
	}

	var msg acpMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v", string(data), err)
	}
	return msg
}

func sendWSMessage(t *testing.T, conn *websocket.Conn, payload string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := conn.Write(ctx, websocket.MessageText, []byte(payload)); err != nil {
		t.Fatalf("conn.Write() error = %v", err)
	}
}

func assertResult(t *testing.T, msg acpMessage, expectedID string) json.RawMessage {
	t.Helper()

	if msg.Error != nil {
		t.Fatalf("unexpected ACP error: code=%d message=%s", msg.Error.Code, msg.Error.Message)
	}
	if msg.Result == nil {
		t.Fatalf("expected result, got notification (method=%s)", msg.Method)
	}
	if string(msg.ID) != `"`+expectedID+`"` {
		t.Fatalf("expected id %q, got %s", expectedID, string(msg.ID))
	}
	return msg.Result
}

func assertNotification(t *testing.T, msg acpMessage, expectedMethod string) {
	t.Helper()

	if msg.Method != expectedMethod {
		t.Fatalf("expected notification method %q, got %q", expectedMethod, msg.Method)
	}
	if msg.Result != nil {
		t.Fatalf("expected notification, got result (id=%s)", string(msg.ID))
	}
}

func waitForSessionStatus(t *testing.T, store *storage.SQLiteStore, sessionID, expectedStatus string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rec, err := store.GetSession(context.Background(), sessionID)
		if err == nil && rec.Status == expectedStatus {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	rec, err := store.GetSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}
	t.Fatalf("session %s status = %s, want %s (timeout %v)", sessionID, rec.Status, expectedStatus, timeout)
}

// waitForSessionDeleted polls until GetSession returns ErrNotFound (session was
// fully reclaimed), or the timeout expires.
func waitForSessionDeleted(t *testing.T, store *storage.SQLiteStore, sessionID string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := store.GetSession(context.Background(), sessionID)
		if err == storage.ErrNotFound {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	rec, err := store.GetSession(context.Background(), sessionID)
	if err != storage.ErrNotFound {
		if err != nil {
			t.Fatalf("GetSession() error = %v, want ErrNotFound", err)
		}
		t.Fatalf("session %s still exists with status %s, want deleted (timeout %v)", sessionID, rec.Status, timeout)
	}
}

// ---- Integration tests ----

func TestResilientSession_FullLifecycle(t *testing.T) {
	h := newResilientTestHarness(t)

	connectResp := h.connectResilient()
	if len(connectResp.SessionID) != 32 {
		t.Fatalf("SessionID = %q, want 32-char hex string", connectResp.SessionID)
	}
	if len(connectResp.AttachToken) != 64 {
		t.Fatalf("AttachToken = %q, want 64-char hex string", connectResp.AttachToken)
	}

	rec, err := h.store.GetSession(context.Background(), connectResp.SessionID)
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}
	// Born disconnected until the client attaches, so a never-attached session is reapable.
	if rec.Status != session.StatusDisconnected {
		t.Fatalf("session status = %s, want %s", rec.Status, session.StatusDisconnected)
	}

	conn := h.dialSessionWS(connectResp.SessionID, connectResp.AttachToken)
	defer func() { _ = conn.CloseNow() }()

	sendWSMessage(t, conn, `{"jsonrpc":"2.0","id":"1","method":"initialize","params":{"protocolVersion":1,"capabilities":{},"clientInfo":{"name":"test-client","version":"1.0.0"}}}`)
	msg := readWSMessage(t, conn)
	assertResult(t, msg, "1")

	sendWSMessage(t, conn, `{"jsonrpc":"2.0","id":"2","method":"authenticate","params":{}}`)
	msg = readWSMessage(t, conn)
	assertResult(t, msg, "2")

	sendWSMessage(t, conn, `{"jsonrpc":"2.0","id":"3","method":"session/new","params":{}}`)
	msg = readWSMessage(t, conn)
	assertResult(t, msg, "3")
	msg = readWSMessage(t, conn)
	assertNotification(t, msg, "session/update")

	sendWSMessage(t, conn, `{"jsonrpc":"2.0","id":"4","method":"session/prompt","params":{"sessionId":"mock_sess_1","prompt":[{"type":"text","text":"Hello"}]}}`)
	msg = readWSMessage(t, conn)
	assertNotification(t, msg, "session/update")
	msg = readWSMessage(t, conn)
	result := assertResult(t, msg, "4")

	var resultData struct {
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(result, &resultData); err != nil {
		t.Fatalf("Unmarshal result error = %v", err)
	}
	if resultData.StopReason != "end_turn" {
		t.Fatalf("stopReason = %q, want %q", resultData.StopReason, "end_turn")
	}

	// The turn above completed while a client was attached. The gateway sends a
	// push regardless of socket state — the client decides whether to display it —
	// so that turn-complete should have produced exactly one push already.
	waitForPushCount(t, h.pushSvc, 1, 5*time.Second)
	connectedPush := h.pushSvc.Calls()[0]
	if connectedPush.Notification.Category != push.CategoryTurnComplete {
		t.Fatalf("connected push category = %q, want %q", connectedPush.Notification.Category, push.CategoryTurnComplete)
	}
	// The push carries the ACP session id the client navigates and suppresses by
	// ("mock_sess_1", from the mock agent's session/new) — not the gateway's own
	// resilient session id (connectResp.SessionID), which is a different namespace.
	const acpSessionID = "mock_sess_1"
	if connectedPush.Notification.SessionID != acpSessionID {
		t.Fatalf("connected push sessionId = %q, want %q", connectedPush.Notification.SessionID, acpSessionID)
	}

	_ = conn.CloseNow()

	waitForSessionStatus(t, h.store, connectResp.SessionID, session.StatusDisconnected, 5*time.Second)

	// A turn completing with no client attached also fires a push.
	pump, err := h.server.sessionSvc.GetPump(connectResp.SessionID)
	if err != nil {
		t.Fatalf("GetPump() error = %v", err)
	}
	if err := pump.WriteToAgent([]byte(`{"jsonrpc":"2.0","id":"7","method":"session/prompt","params":{"sessionId":"mock_sess_1","prompt":[{"type":"text","text":"Push test"}]}}`)); err != nil {
		t.Fatalf("WriteToAgent() error = %v", err)
	}

	waitForPushCount(t, h.pushSvc, 2, 5*time.Second)
	disconnectedPush := h.pushSvc.Calls()[1]
	if disconnectedPush.Notification.SessionID != acpSessionID {
		t.Fatalf("push sessionId = %q, want %q", disconnectedPush.Notification.SessionID, acpSessionID)
	}
	if disconnectedPush.Notification.ServerID != testGatewayID {
		t.Fatalf("push serverId = %q, want %q", disconnectedPush.Notification.ServerID, testGatewayID)
	}

	newToken := h.resumeSession(connectResp.SessionID)
	if len(newToken) != 64 {
		t.Fatalf("resume attach token = %q, want 64-char hex string", newToken)
	}

	conn2 := h.dialSessionWS(connectResp.SessionID, newToken)
	defer func() { _ = conn2.CloseNow() }()

	sendWSMessage(t, conn2, `{"jsonrpc":"2.0","id":"5","method":"session/load","params":{"sessionId":"mock_sess_1"}}`)
	// The prompt issued while no client was attached completed detached; the
	// gateway kept its outcome and replays it ahead of the load reply.
	msg = readWSMessage(t, conn2)
	assertNotification(t, msg, "_ferngeist/turn_result")
	var detachedResult struct {
		SessionID  string `json:"sessionId"`
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(msg.Params, &detachedResult); err != nil {
		t.Fatalf("Unmarshal(turn_result params) error = %v", err)
	}
	if detachedResult.SessionID != acpSessionID {
		t.Fatalf("turn_result sessionId = %q, want %q", detachedResult.SessionID, acpSessionID)
	}
	if detachedResult.StopReason != "end_turn" {
		t.Fatalf("turn_result stopReason = %q, want %q", detachedResult.StopReason, "end_turn")
	}
	// ACP: the agent answers session/load first, then replays history.
	msg = readWSMessage(t, conn2)
	assertResult(t, msg, "5")
	msg = readWSMessage(t, conn2)
	assertNotification(t, msg, "session/update")
	msg = readWSMessage(t, conn2)
	assertNotification(t, msg, "session/update")

	sendWSMessage(t, conn2, `{"jsonrpc":"2.0","id":"6","method":"session/prompt","params":{"sessionId":"mock_sess_1","prompt":[{"type":"text","text":"Continue"}]}}`)
	msg = readWSMessage(t, conn2)
	assertNotification(t, msg, "session/update")
	msg = readWSMessage(t, conn2)
	result = assertResult(t, msg, "6")

	if err := json.Unmarshal(result, &resultData); err != nil {
		t.Fatalf("Unmarshal result error = %v", err)
	}
	if resultData.StopReason != "end_turn" {
		t.Fatalf("stopReason = %q, want %q (after reconnection)", resultData.StopReason, "end_turn")
	}

	_ = conn2.CloseNow()

	closeReq := httptest.NewRequest(http.MethodDelete, "/v1/sessions/"+connectResp.SessionID, nil)
	closeReq.Header.Set("Authorization", "Bearer "+h.token)
	closeRec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(closeRec, closeReq)
	if closeRec.Code != http.StatusNoContent {
		t.Fatalf("close status = %d, want %d, body=%s", closeRec.Code, http.StatusNoContent, closeRec.Body.String())
	}

	_, err = h.store.GetSession(context.Background(), connectResp.SessionID)
	if err == nil {
		t.Fatal("expected session to be deleted from store after close")
	}
}

func TestResilientSession_MultipleReconnects(t *testing.T) {
	h := newResilientTestHarness(t)
	connectResp := h.connectResilient()

	for i := 0; i < 3; i++ {
		var token string
		if i == 0 {
			token = connectResp.AttachToken
		} else {
			token = h.resumeSession(connectResp.SessionID)
		}

		conn := h.dialSessionWS(connectResp.SessionID, token)

		sendWSMessage(t, conn, `{"jsonrpc":"2.0","id":"1","method":"initialize","params":{"protocolVersion":1,"capabilities":{},"clientInfo":{"name":"test-client","version":"1.0.0"}}}`)
		msg := readWSMessage(t, conn)
		assertResult(t, msg, "1")

		_ = conn.CloseNow()

		if i < 2 {
			waitForSessionStatus(t, h.store, connectResp.SessionID, session.StatusDisconnected, 5*time.Second)
		}
	}

	closeReq := httptest.NewRequest(http.MethodDelete, "/v1/sessions/"+connectResp.SessionID, nil)
	closeReq.Header.Set("Authorization", "Bearer "+h.token)
	closeRec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(closeRec, closeReq)
	if closeRec.Code != http.StatusNoContent {
		t.Fatalf("close status = %d, want %d", closeRec.Code, http.StatusNoContent)
	}
}

// A second valid attach takes over the session rather than being rejected. A
// resilient session's previous connection is usually a dead mobile socket the
// gateway has not yet observed as closed, so refusing the reconnect would strand
// the session. The new connection wins and the old one is evicted.
func TestResilientSession_ConcurrentAttachTakesOver(t *testing.T) {
	h := newResilientTestHarness(t)
	connectResp := h.connectResilient()

	conn1 := h.dialSessionWS(connectResp.SessionID, connectResp.AttachToken)
	defer func() { _ = conn1.CloseNow() }()

	sendWSMessage(t, conn1, `{"jsonrpc":"2.0","id":"1","method":"initialize","params":{"protocolVersion":1,"capabilities":{},"clientInfo":{"name":"test-client","version":"1.0.0"}}}`)
	msg := readWSMessage(t, conn1)
	assertResult(t, msg, "1")

	// Reconnect while conn1 is still "attached" — this must succeed via takeover.
	secondToken := h.resumeSession(connectResp.SessionID)
	conn2 := h.dialSessionWS(connectResp.SessionID, secondToken)
	defer func() { _ = conn2.CloseNow() }()

	// The new connection can drive the agent.
	sendWSMessage(t, conn2, `{"jsonrpc":"2.0","id":"2","method":"authenticate","params":{}}`)
	msg = readWSMessage(t, conn2)
	assertResult(t, msg, "2")

	// The superseded connection is force-closed by the takeover.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := conn1.Read(ctx); err == nil {
		t.Fatal("expected the superseded connection to be closed by takeover")
	}
}

// Connecting always launches a fresh agent and session, so a second connect
// never reuses the first session. The client reconnects to an existing session
// via resume, so a re-connect must produce distinct credentials while leaving
// the original session reconnectable.
func TestResilientSession_ConnectLaunchesIndependentSessions(t *testing.T) {
	h := newResilientTestHarness(t)

	first := h.connectResilient()
	if first.SessionID == "" || first.AttachToken == "" || first.RuntimeID == "" {
		t.Fatalf("first connect returned empty session credentials: %+v", first)
	}

	conn1 := h.dialSessionWS(first.SessionID, first.AttachToken)
	sendWSMessage(t, conn1, `{"jsonrpc":"2.0","id":"1","method":"initialize","params":{"protocolVersion":1,"capabilities":{},"clientInfo":{"name":"test-client","version":"1.0.0"}}}`)
	assertResult(t, readWSMessage(t, conn1), "1")
	// Simulate the app dying without a clean close: drop the socket reference
	// without going through a graceful shutdown.
	_ = conn1.CloseNow()

	// Connecting again starts a new agent rather than reusing the first session.
	second := h.connectResilient()
	if second.SessionID == first.SessionID {
		t.Fatalf("connect should create a new session, both = %q", first.SessionID)
	}
	if second.RuntimeID == first.RuntimeID {
		t.Fatalf("connect should create a new runtime, both = %q", first.RuntimeID)
	}
	if second.AttachToken == "" {
		t.Fatal("second connect returned an empty attach token")
	}

	// The first session is untouched and still reconnectable via resume.
	if token := h.resumeSession(first.SessionID); len(token) != 64 {
		t.Fatalf("resume attach token = %q, want 64-char hex string", token)
	}
}

func TestResilientSession_AgentDeathDuringSession(t *testing.T) {
	h := newResilientTestHarness(t)
	connectResp := h.connectResilient()

	conn := h.dialSessionWS(connectResp.SessionID, connectResp.AttachToken)
	defer func() { _ = conn.CloseNow() }()

	sendWSMessage(t, conn, `{"jsonrpc":"2.0","id":"1","method":"initialize","params":{"protocolVersion":1,"capabilities":{},"clientInfo":{"name":"test-client","version":"1.0.0"}}}`)
	msg := readWSMessage(t, conn)
	assertResult(t, msg, "1")

	// Simulate the agent dying on its own: kill the process out from under the
	// gateway. The session's exit watcher reclaims the session and fires the
	// crash push.
	info, err := h.server.runtime.Get(connectResp.RuntimeID)
	if err != nil {
		t.Fatalf("runtime.Get() error = %v", err)
	}
	proc, err := os.FindProcess(info.PID)
	if err != nil {
		t.Fatalf("os.FindProcess(%d) error = %v", info.PID, err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}

	waitForSessionDeleted(t, h.store, connectResp.SessionID, 5*time.Second)
}

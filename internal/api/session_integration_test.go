package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/session"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/token"
)

type mockPushService struct {
	mu    sync.Mutex
	calls []pushCall
}

type pushCall struct {
	DeviceID string
	Title    string
	Body     string
	Data     map[string]string
}

func (m *mockPushService) SendNotification(_ context.Context, deviceID, title, body string, data map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, pushCall{DeviceID: deviceID, Title: title, Body: body, Data: data})
	return nil
}

func (m *mockPushService) Calls() []pushCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := make([]pushCall, len(m.calls))
	copy(r, m.calls)
	return r
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
	SessionID   string `json:"sessionId"`
	AttachToken string `json:"attachToken"`
}

func newResilientTestHarness(t *testing.T) *resilientTestHarness {
	t.Helper()

	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	store, err := storage.Open(filepath.Join(baseDir, "test.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { store.Close() })

	server := newTestServerWithStore(baseDir, store)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tokenSvc := token.New(logger)
	mockPush := &mockPushService{}
	sessionSvc := session.NewRuntimeSession(logger, store, server.runtime, tokenSvc, session.Config{
		MaxDisconnected: 5 * time.Minute,
		MaxPerDevice:    3,
		PushSvc:         mockPush,
	})
	server.sessionSvc = sessionSvc
	t.Cleanup(sessionSvc.Shutdown)

	bearerToken := pairDevice(t, server)

	startReq := httptest.NewRequest(http.MethodPost, "/v1/agents/mock-acp/start", nil)
	startReq.Header.Set("Authorization", "Bearer "+bearerToken)
	startRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusOK {
		t.Fatalf("agent start status = %d, body=%s", startRec.Code, startRec.Body.String())
	}
	var startResp runtimeStartResponse
	if err := json.Unmarshal(startRec.Body.Bytes(), &startResp); err != nil {
		t.Fatalf("Unmarshal(start) error = %v", err)
	}

	t.Cleanup(func() {
		stopReq := httptest.NewRequest(http.MethodPost, "/v1/agents/mock-acp/stop", nil)
		stopReq.Header.Set("Authorization", "Bearer "+bearerToken)
		stopRec := httptest.NewRecorder()
		server.Handler().ServeHTTP(stopRec, stopReq)
	})

	httpSrv := httptest.NewServer(server.Handler())
	t.Cleanup(httpSrv.Close)

	return &resilientTestHarness{
		t:         t,
		server:    server,
		store:     store,
		token:     bearerToken,
		runtimeID: startResp.Runtime.ID,
		pushSvc:   mockPush,
		httpSrv:   httpSrv,
	}
}

func (h *resilientTestHarness) connectResilient() connectResilientResponse {
	h.t.Helper()

	body, _ := json.Marshal(map[string]string{"sessionMode": "resilient"})
	req := httptest.NewRequest(http.MethodPost, "/v1/runtimes/"+h.runtimeID+"/connect", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		h.t.Fatalf("connect status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp connectResilientResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		h.t.Fatalf("Unmarshal(connect) error = %v", err)
	}
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
	if rec.Status != session.StatusActive {
		t.Fatalf("session status = %s, want %s", rec.Status, session.StatusActive)
	}

	conn := h.dialSessionWS(connectResp.SessionID, connectResp.AttachToken)
	defer conn.CloseNow()

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

	conn.CloseNow()

	waitForSessionStatus(t, h.store, connectResp.SessionID, session.StatusDisconnected, 5*time.Second)

	// After disconnection, trigger a prompt via the pump directly to
	// verify the push notification fires (no client to receive it).
	pump, err := h.server.sessionSvc.GetPump(connectResp.SessionID)
	if err != nil {
		t.Fatalf("GetPump() error = %v", err)
	}
	if err := pump.WriteToAgent([]byte(`{"jsonrpc":"2.0","id":"7","method":"session/prompt","params":{"sessionId":"mock_sess_1","prompt":[{"type":"text","text":"Push test"}]}}`)); err != nil {
		t.Fatalf("WriteToAgent() error = %v", err)
	}

	// Wait for push notification to be recorded.
	var calls []pushCall
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		calls = h.pushSvc.Calls()
		if len(calls) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 push call after disconnect, got %d", len(calls))
	}
	if calls[0].Data["sessionId"] != connectResp.SessionID {
		t.Fatalf("push data sessionId = %q, want %q", calls[0].Data["sessionId"], connectResp.SessionID)
	}
	if calls[0].Data["runtimeId"] != h.runtimeID {
		t.Fatalf("push data runtimeId = %q, want %q", calls[0].Data["runtimeId"], h.runtimeID)
	}

	newToken := h.resumeSession(connectResp.SessionID)
	if len(newToken) != 64 {
		t.Fatalf("resume attach token = %q, want 64-char hex string", newToken)
	}

	conn2 := h.dialSessionWS(connectResp.SessionID, newToken)
	defer conn2.CloseNow()

	sendWSMessage(t, conn2, `{"jsonrpc":"2.0","id":"5","method":"session/load","params":{"sessionId":"mock_sess_1"}}`)
	msg = readWSMessage(t, conn2)
	assertNotification(t, msg, "session/update")
	msg = readWSMessage(t, conn2)
	assertNotification(t, msg, "session/update")
	msg = readWSMessage(t, conn2)
	assertResult(t, msg, "5")

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

	conn2.CloseNow()

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

		conn.CloseNow()

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
	defer conn1.CloseNow()

	sendWSMessage(t, conn1, `{"jsonrpc":"2.0","id":"1","method":"initialize","params":{"protocolVersion":1,"capabilities":{},"clientInfo":{"name":"test-client","version":"1.0.0"}}}`)
	msg := readWSMessage(t, conn1)
	assertResult(t, msg, "1")

	// Reconnect while conn1 is still "attached" — this must succeed via takeover.
	secondToken := h.resumeSession(connectResp.SessionID)
	conn2 := h.dialSessionWS(connectResp.SessionID, secondToken)
	defer conn2.CloseNow()

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

// When the app is killed and reconnects, it re-runs the connect flow against the
// still-running runtime. The gateway must hand back the existing session (the
// runtime lease is still held, so creating a new one would fail and strand the
// client without session credentials) and the client must be able to attach.
func TestResilientSession_ReconnectReusesExistingSession(t *testing.T) {
	h := newResilientTestHarness(t)

	first := h.connectResilient()
	if first.SessionID == "" || first.AttachToken == "" {
		t.Fatalf("first connect returned empty session credentials: %+v", first)
	}

	conn1 := h.dialSessionWS(first.SessionID, first.AttachToken)
	sendWSMessage(t, conn1, `{"jsonrpc":"2.0","id":"1","method":"initialize","params":{"protocolVersion":1,"capabilities":{},"clientInfo":{"name":"test-client","version":"1.0.0"}}}`)
	assertResult(t, readWSMessage(t, conn1), "1")
	// Simulate the app dying without a clean close: drop the socket reference
	// without going through a graceful shutdown.
	conn1.CloseNow()

	// Reconnect path: connect again against the same runtime.
	second := h.connectResilient()
	if second.SessionID != first.SessionID {
		t.Fatalf("reconnect should reuse session %q, got %q", first.SessionID, second.SessionID)
	}
	if second.AttachToken == "" {
		t.Fatal("reconnect returned an empty attach token")
	}

	conn2 := h.dialSessionWS(second.SessionID, second.AttachToken)
	defer conn2.CloseNow()
	sendWSMessage(t, conn2, `{"jsonrpc":"2.0","id":"2","method":"authenticate","params":{}}`)
	assertResult(t, readWSMessage(t, conn2), "2")
}

func TestResilientSession_AgentDeathDuringSession(t *testing.T) {
	h := newResilientTestHarness(t)
	connectResp := h.connectResilient()

	conn := h.dialSessionWS(connectResp.SessionID, connectResp.AttachToken)
	defer conn.CloseNow()

	sendWSMessage(t, conn, `{"jsonrpc":"2.0","id":"1","method":"initialize","params":{"protocolVersion":1,"capabilities":{},"clientInfo":{"name":"test-client","version":"1.0.0"}}}`)
	msg := readWSMessage(t, conn)
	assertResult(t, msg, "1")

	if _, err := h.server.runtime.StopByRuntimeID(h.runtimeID); err != nil {
		t.Fatalf("StopByRuntimeID() error = %v", err)
	}

	waitForSessionStatus(t, h.store, connectResp.SessionID, session.StatusFailed, 5*time.Second)
}

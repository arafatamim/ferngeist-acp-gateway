package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/session"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/turns"
)

type turnsBody struct {
	GatewayStartedAt time.Time      `json:"gatewayStartedAt"`
	ObservedAt       time.Time      `json:"observedAt"`
	Turns            []turns.Status `json:"turns"`
}

func getTurns(t *testing.T, server *Server, token string) (turnsBody, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/turns", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/turns = %d: %s", rec.Code, rec.Body.String())
	}
	var body turnsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body, rec.Body.String()
}

// awaitTurn polls GET /v1/turns until the chat's turn matches want.
func awaitTurn(t *testing.T, server *Server, token, chat string, want func(turns.Status) bool) (turns.Status, string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		body, raw := getTurns(t, server, token)
		for _, st := range body.Turns {
			if st.SessionID == chat && want(st) {
				return st, raw
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn %q never matched: %s", chat, raw)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A turn one device starts is visible to another: running with the approval
// its agent waits on, then its outcome, which outlives the gateway session.
// The body names no device and carries no request payload.
func TestTurnsShowsAnotherDevicesTurnsAndOutcomes(t *testing.T) {
	gate := make(chan struct{}, 4)
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-gate:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(llm.Close)
	// An unread request body never cancels its context when the agent dies, so
	// release every held model call before Close waits on them.
	t.Cleanup(func() { close(gate) })
	t.Setenv("FERNGEIST_MOCK_LLM_ENDPOINT", llm.URL)
	t.Setenv("FERNGEIST_MOCK_PERMISSION", "1")
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)
	server := newTestServerWithBaseDir(baseDir)
	phone, tablet := pairDevice(t, server), pairDevice(t, server)
	rs := attachTestSessionService(t, server)

	if body, raw := getTurns(t, server, tablet); body.Turns == nil || len(body.Turns) != 0 || !strings.Contains(raw, `"turns":[]`) {
		t.Fatalf("fresh gateway turns = %s", raw)
	}
	prompt := func(sessionID, chat string) {
		pump, err := rs.GetPump(sessionID)
		if err != nil {
			t.Fatal(err)
		}
		frame := `{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"` + chat + `","prompt":[]}}`
		if err := pump.WriteToAgent([]byte(frame)); err != nil {
			t.Fatal(err)
		}
	}
	first := connectAgent(t, server, phone, "mock-acp")
	prompt(first.SessionID, "chat-a")
	running, raw := awaitTurn(t, server, tablet, "chat-a", func(st turns.Status) bool { return len(st.Attention) > 0 })
	if running.State != turns.StateRunning || strings.Join(running.Attention, ",") != turns.AttentionPermission ||
		running.AgentID != "mock-acp" || running.GatewaySessionID != first.SessionID || running.RuntimeID != first.RuntimeID {
		t.Fatalf("running turn = %+v", running)
	}
	for _, leak := range []string{"device", "options", "toolCall", "perm-"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("turns body leaks %q: %s", leak, raw)
		}
	}
	gate <- struct{}{}
	done, _ := awaitTurn(t, server, tablet, "chat-a", func(st turns.Status) bool { return st.State != turns.StateRunning })
	if done.State != turns.StateSucceeded || done.StopReason != "end_turn" || len(done.Attention) != 0 {
		t.Fatalf("finished turn = %+v", done)
	}

	second := connectAgent(t, server, phone, "mock-acp")
	prompt(second.SessionID, "chat-b")
	awaitTurn(t, server, tablet, "chat-b", func(st turns.Status) bool { return st.State == turns.StateRunning })
	closeReq := httptest.NewRequest(http.MethodDelete, "/v1/sessions/"+second.SessionID, nil)
	closeReq.Header.Set("Authorization", "Bearer "+phone)
	closeRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(closeRec, closeReq)
	if closeRec.Code/100 != 2 {
		t.Fatalf("close session = %d: %s", closeRec.Code, closeRec.Body.String())
	}
	body, _ := getTurns(t, server, tablet)
	states := map[string]string{}
	for _, st := range body.Turns {
		states[st.SessionID] = st.State
	}
	if states["chat-a"] != turns.StateSucceeded || states["chat-b"] != turns.StateCancelled || len(states) != 2 {
		t.Fatalf("turns after close = %+v", body.Turns)
	}
}

// gatewayStartedAt is the same on every request to one gateway and differs on
// a restarted one, whose turns start empty.
func TestTurnsGatewayStartedAtMarksTheDaemonLifetime(t *testing.T) {
	server := newTestServer()
	token := pairDevice(t, server)
	attachTestSessionService(t, server)
	first, _ := getTurns(t, server, token)
	second, _ := getTurns(t, server, token)
	if first.GatewayStartedAt.IsZero() || !first.GatewayStartedAt.Equal(second.GatewayStartedAt) {
		t.Fatalf("gatewayStartedAt = %v then %v, want one stable value", first.GatewayStartedAt, second.GatewayStartedAt)
	}
	if second.ObservedAt.Before(first.ObservedAt) || second.ObservedAt.Location() != time.UTC {
		t.Fatalf("observedAt = %v then %v", first.ObservedAt, second.ObservedAt)
	}

	restarted := newTestServer()
	restartedToken := pairDevice(t, restarted)
	attachTestSessionService(t, restarted)
	if got, _ := getTurns(t, restarted, restartedToken); got.GatewayStartedAt.Equal(first.GatewayStartedAt) {
		t.Fatalf("restarted gateway reuses gatewayStartedAt %v", got.GatewayStartedAt)
	}
}

func newTurnTimingHarness(t *testing.T) (*resilientTestHarness, chan<- struct{}) {
	t.Helper()
	gate := make(chan struct{}, 1)
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-gate:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(llm.Close)
	// Release held model calls even if an assertion fails before completion.
	t.Cleanup(func() { close(gate) })
	t.Setenv("FERNGEIST_MOCK_LLM_ENDPOINT", llm.URL)
	t.Setenv("FERNGEIST_MOCK_PERMISSION", "1")
	return newResilientTestHarness(t), gate
}

func assertTurnTiming(t *testing.T, h *resilientTestHarness, want turns.Status) {
	t.Helper()
	before := time.Now().UTC()
	body, raw := getTurns(t, h.server, h.token)
	after := time.Now().UTC()
	if body.ObservedAt.Before(before) || body.ObservedAt.After(after) {
		t.Fatalf("observedAt outside request interval [%v, %v]: %s", before, after, raw)
	}
	if len(body.Turns) != 1 {
		t.Fatalf("want one turn: %s", raw)
	}
	got := body.Turns[0]
	if got.State != want.State || !got.StartedAt.Equal(want.StartedAt) || !got.ChangedAt.Equal(want.ChangedAt) {
		t.Fatalf("turn timing changed: got %+v, want %+v", got, want)
	}
	for _, stamp := range []time.Time{body.ObservedAt, got.StartedAt, got.ChangedAt} {
		if stamp.IsZero() || stamp.Location() != time.UTC {
			t.Fatalf("timestamp must be nonzero UTC: %s", raw)
		}
	}
}

// The API must preserve prompt timing while attention and socket ownership
// change; a refresh must not make an old turn look newly started or finished.
func TestTurnsPreservesTimingAcrossAttentionReconnectAndCompletion(t *testing.T) {
	h, gate := newTurnTimingHarness(t)
	connected := h.connectResilient()
	conn := h.dialSessionWS(connected.SessionID, connected.AttachToken)
	t.Cleanup(func() { _ = conn.CloseNow() }) // The harness owns process cleanup.
	beforePrompt := time.Now().UTC()
	sendWSMessage(t, conn, `{"jsonrpc":"2.0","id":"1","method":"session/prompt","params":{"sessionId":"mock_sess_1","prompt":[]}}`)
	running, _ := awaitTurn(t, h.server, h.token, "mock_sess_1", func(st turns.Status) bool {
		return st.State == turns.StateRunning && len(st.Attention) > 0
	})
	if running.StartedAt.Before(beforePrompt) || running.StartedAt.After(time.Now()) || !running.ChangedAt.Equal(running.StartedAt) {
		t.Fatalf("prompt start timing = %+v", running)
	}
	assertTurnTiming(t, h, running)

	if err := conn.CloseNow(); err != nil {
		t.Fatal(err)
	}
	waitForSessionStatus(t, h.store, connected.SessionID, session.StatusDisconnected, 5*time.Second)
	assertTurnTiming(t, h, running)
	conn = h.dialSessionWS(connected.SessionID, h.resumeSession(connected.SessionID))
	assertTurnTiming(t, h, running)
	sendWSMessage(t, conn, `{"jsonrpc":"2.0","id":"perm-mock_sess_1","result":{"outcome":{"outcome":"selected","optionId":"allow"}}}`)
	awaitTurn(t, h.server, h.token, "mock_sess_1", func(st turns.Status) bool {
		return st.State == turns.StateRunning && len(st.Attention) == 0
	})
	assertTurnTiming(t, h, running)
	assertCompletedTurnTiming(t, h, conn, gate, running)
}

func assertCompletedTurnTiming(t *testing.T, h *resilientTestHarness, conn *websocket.Conn, gate chan<- struct{}, running turns.Status) {
	t.Helper()
	beforeCompletion := time.Now().UTC()
	gate <- struct{}{}
	done, _ := awaitTurn(t, h.server, h.token, "mock_sess_1", func(st turns.Status) bool {
		return st.State == turns.StateSucceeded
	})
	if !done.StartedAt.Equal(running.StartedAt) || done.ChangedAt.Before(beforeCompletion) || done.ChangedAt.After(time.Now()) {
		t.Fatalf("completion timing = %+v, running = %+v", done, running)
	}
	assertTurnTiming(t, h, done)

	// A later load produces a reply and session/update history without a prompt.
	sendWSMessage(t, conn, `{"jsonrpc":"2.0","id":"2","method":"session/load","params":{"sessionId":"mock_sess_1"}}`)
	msg := readWSMessage(t, conn)
	for string(msg.ID) != `"2"` {
		msg = readWSMessage(t, conn)
	}
	assertResult(t, msg, "2")
	for range 2 {
		msg = readWSMessage(t, conn)
		assertNotification(t, msg, "session/update")
		var params struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			t.Fatal(err)
		}
		if params.SessionID != done.SessionID {
			t.Fatalf("replay chat = %q, want %q", params.SessionID, done.SessionID)
		}
	}
	assertTurnTiming(t, h, done)
}

func TestTurnsRefusesUnauthorizedOrUnavailable(t *testing.T) {
	baseDir := t.TempDir()
	store, err := storage.Open(filepath.Join(baseDir, "pairings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const controlOnly = "control-only-token"
	if err := store.SavePairing(context.Background(), storage.PairingRecord{
		DeviceID: "dev-control", DeviceName: "control", Token: controlOnly,
		ExpiresAt: time.Now().Add(time.Hour), Scopes: []string{pairing.ScopeControl},
	}); err != nil {
		t.Fatal(err)
	}
	server := newTestServerWithStore(baseDir, store)
	readToken := pairDevice(t, server)

	for _, tc := range []struct {
		name, token string
		want        int
	}{
		{"no credential", "", http.StatusUnauthorized},
		{"no read scope", controlOnly, http.StatusForbidden},
		{"no session service", readToken, http.StatusServiceUnavailable},
	} {
		req := httptest.NewRequest(http.MethodGet, "/v1/turns", nil)
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}
}

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

	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
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

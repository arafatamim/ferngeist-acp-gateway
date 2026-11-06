package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
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
		if err := pump.WriteToAgent(0, []byte(frame)); err != nil {
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

// The stream shares GET /v1/turns' auth gate: every rejection happens before the
// upgrade, as an ordinary HTTP response.
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

	for _, path := range []string{"/v1/turns", "/v1/turns/stream"} {
		for _, tc := range []struct {
			name, token string
			want        int
		}{
			{"no credential", "", http.StatusUnauthorized},
			{"no read scope", controlOnly, http.StatusForbidden},
			{"no session service", readToken, http.StatusServiceUnavailable},
		} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("%s %s: status %d, want %d (%s)", path, tc.name, rec.Code, tc.want, rec.Body.String())
			}
		}
	}
}

// --- Turn status stream ---

type turnStreamFrame struct {
	Type             string         `json:"type"`
	GatewayStartedAt time.Time      `json:"gatewayStartedAt"`
	ObservedAt       time.Time      `json:"observedAt"`
	Turns            []turns.Status `json:"turns"`
	Upserts          []turns.Status `json:"upserts"`
	Removed          []turns.Key    `json:"removed"`
}

// newTurnStreamServer attaches a session service, pairs one device and starts
// an HTTP listener, the three things an open stream needs.
func newTurnStreamServer(t *testing.T, server *Server) (*session.RuntimeSession, pairCompleteResponse, *httptest.Server) {
	t.Helper()
	rs := attachTestSessionService(t, server)
	device := pairDeviceResponse(t, server)
	httpSrv := httptest.NewServer(server.Handler())
	t.Cleanup(httpSrv.Close)
	return rs, device, httpSrv
}

// dialTurnStream opens GET /v1/turns/stream with a pairing bearer token.
func dialTurnStream(t *testing.T, httpSrv *httptest.Server, token string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	url := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/v1/turns/stream"
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("websocket.Dial(%s) error = %v", url, err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func readTurnStreamFrame(t *testing.T, conn *websocket.Conn) (turnStreamFrame, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("conn.Read() error = %v", err)
	}
	var frame turnStreamFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v", string(data), err)
	}
	return frame, string(data)
}

// readTurnStreamOpen reads the first frame and asserts it is the snapshot.
func readTurnStreamOpen(t *testing.T, conn *websocket.Conn) turnStreamFrame {
	t.Helper()
	frame, raw := readTurnStreamFrame(t, conn)
	if frame.Type != "snapshot" {
		t.Fatalf("first frame type = %q, want snapshot: %s", frame.Type, raw)
	}
	return frame
}

func readTurnStreamClose(t *testing.T, conn *websocket.Conn) (websocket.StatusCode, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	code := websocket.CloseStatus(err)
	if code == -1 {
		t.Fatalf("conn.Read() error = %v, want a close frame", err)
	}
	var closeErr websocket.CloseError
	if errors.As(err, &closeErr) {
		return code, closeErr.Reason
	}
	return code, ""
}

// awaitTurnUpsert reads changes frames until one upserts a turn matching want.
func awaitTurnUpsert(t *testing.T, conn *websocket.Conn, want func(turns.Status) bool) turns.Status {
	t.Helper()
	for range 32 {
		frame, raw := readTurnStreamFrame(t, conn)
		if frame.Type != "changes" {
			t.Fatalf("frame type = %q, want changes: %s", frame.Type, raw)
		}
		if len(frame.Upserts) == 0 {
			t.Fatalf("changes frame with no upserts: %s", raw)
		}
		if !strings.Contains(raw, `"upserts":`) || !strings.Contains(raw, `"removed":`) {
			t.Fatalf("changes frame omits an array: %s", raw)
		}
		for _, st := range frame.Upserts {
			if want(st) {
				return st
			}
		}
	}
	t.Fatal("no matching upsert arrived")
	return turns.Status{}
}

// A shutting-down gateway refuses new streams before the upgrade, so the client
// backs off instead of holding a socket that will never carry a snapshot.
func TestTurnStreamRefusesAfterShutdown(t *testing.T) {
	server := newTestServer()
	_, device, _ := newTurnStreamServer(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/turns/stream", nil)
	req.Header.Set("Authorization", "Bearer "+device.Token)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("stream after shutdown = %d, want 503 (%s)", rec.Code, rec.Body.String())
	}
}

// Streams opening while Shutdown runs must never Add to the handler group
// concurrently with its Wait. Under -race this catches the misuse often, not
// always: it is a regression probe, not a proof.
func TestTurnStreamShutdownRacesNewStreams(t *testing.T) {
	server := newTestServer()
	_, device, _ := newTurnStreamServer(t, server)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 20 {
				req := httptest.NewRequest(http.MethodGet, "/v1/turns/stream", nil)
				req.Header.Set("Authorization", "Bearer "+device.Token)
				server.Handler().ServeHTTP(httptest.NewRecorder(), req)
			}
		}()
	}
	close(start)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
}

// The first frame is the GET /v1/turns body plus a type, with the same
// gatewayStartedAt, so a client can seed its state from either.
func TestTurnStreamOpensWithTheTurnsSnapshot(t *testing.T) {
	server := newTestServer()
	_, device, httpSrv := newTurnStreamServer(t, server)
	body, _ := getTurns(t, server, device.Token)

	conn := dialTurnStream(t, httpSrv, device.Token)
	frame, raw := readTurnStreamFrame(t, conn)
	if frame.Type != "snapshot" {
		t.Fatalf("first frame = %s", raw)
	}
	if !frame.GatewayStartedAt.Equal(body.GatewayStartedAt) {
		t.Fatalf("stream gatewayStartedAt = %v, GET /v1/turns = %v", frame.GatewayStartedAt, body.GatewayStartedAt)
	}
	if frame.ObservedAt.IsZero() || frame.ObservedAt.Location() != time.UTC {
		t.Fatalf("observedAt = %v", frame.ObservedAt)
	}
	if !strings.Contains(raw, `"turns":[]`) {
		t.Fatalf("empty snapshot must carry turns as an array: %s", raw)
	}
}

// A turn another device starts reaches this device's stream: running with the
// approval its agent waits on, then its outcome. No request content crosses.
func TestTurnStreamCarriesAnotherDevicesTurn(t *testing.T) {
	gate := make(chan struct{}, 4)
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-gate:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(llm.Close)
	t.Cleanup(func() { close(gate) })
	t.Setenv("FERNGEIST_MOCK_LLM_ENDPOINT", llm.URL)
	t.Setenv("FERNGEIST_MOCK_PERMISSION", "1")
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)
	server := newTestServerWithBaseDir(baseDir)
	rs, reader, httpSrv := newTurnStreamServer(t, server)
	writer := pairDevice(t, server)

	conn := dialTurnStream(t, httpSrv, reader.Token)
	if frame := readTurnStreamOpen(t, conn); len(frame.Turns) != 0 {
		t.Fatalf("fresh stream snapshot = %+v", frame.Turns)
	}

	connected := connectAgent(t, server, writer, "mock-acp")
	pump, err := rs.GetPump(connected.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	frame := `{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"chat-a","prompt":[]}}`
	if err := pump.WriteToAgent(0, []byte(frame)); err != nil {
		t.Fatal(err)
	}
	running := awaitTurnUpsert(t, conn, func(st turns.Status) bool {
		return st.SessionID == "chat-a" && len(st.Attention) > 0
	})
	if running.State != turns.StateRunning || strings.Join(running.Attention, ",") != turns.AttentionPermission ||
		running.GatewaySessionID != connected.SessionID || running.RuntimeID != connected.RuntimeID {
		t.Fatalf("running upsert = %+v", running)
	}

	gate <- struct{}{}
	done := awaitTurnUpsert(t, conn, func(st turns.Status) bool {
		return st.SessionID == "chat-a" && st.State != turns.StateRunning
	})
	if done.State != turns.StateSucceeded || done.StopReason != "end_turn" || len(done.Attention) != 0 {
		t.Fatalf("finished upsert = %+v", done)
	}
}

// An open stream notices a credential that stopped validating on its next write
// and closes with 4401, carrying the auth code the client branches on.
func TestTurnStreamClosesWhenTheCredentialIsRevoked(t *testing.T) {
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)
	server := newTestServerWithBaseDir(baseDir)
	rs, device, httpSrv := newTurnStreamServer(t, server)

	conn := dialTurnStream(t, httpSrv, device.Token)
	readTurnStreamOpen(t, conn)
	connected := connectAgent(t, server, device.Token, "mock-acp")
	pump, err := rs.GetPump(connected.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.pairing.RevokeDevice(device.DeviceID); err != nil {
		t.Fatal(err)
	}
	// The prompt is only here to make the feed publish a change; the handler
	// must revalidate before it writes that change.
	if err := pump.WriteToAgent(0, []byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"chat-a","prompt":[]}}`)); err != nil {
		t.Fatal(err)
	}

	code, reason := readTurnStreamClose(t, conn)
	if code != 4401 || reason != credentialInvalid {
		t.Fatalf("close = %d %q, want 4401 %q", code, reason, credentialInvalid)
	}
}

// An idle stream is revalidated on a timer, so revocation does not wait for
// traffic that may never come.
func TestTurnStreamRevalidatesAnIdleStream(t *testing.T) {
	server := newTestServer()
	_, device, httpSrv := newTurnStreamServer(t, server)
	server.streamRecheck = 20 * time.Millisecond
	conn := dialTurnStream(t, httpSrv, device.Token)
	readTurnStreamOpen(t, conn)

	if _, err := server.pairing.RevokeDevice(device.DeviceID); err != nil {
		t.Fatal(err)
	}
	code, reason := readTurnStreamClose(t, conn)
	if code != 4401 || reason != credentialInvalid {
		t.Fatalf("close = %d %q, want 4401 %q", code, reason, credentialInvalid)
	}
}

// The client never sends data: a text frame closes with 1008, and one over the
// 32 KiB read limit with 1009.
func TestTurnStreamClosesOnAClientDataFrame(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload []byte
		want    websocket.StatusCode
	}{
		{"under the read limit", []byte(`{"hello":true}`), websocket.StatusPolicyViolation},
		{"over the read limit", []byte(strings.Repeat("x", 33*1024)), websocket.StatusMessageTooBig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, device, httpSrv := newTurnStreamServer(t, newTestServer())
			conn := dialTurnStream(t, httpSrv, device.Token)
			readTurnStreamOpen(t, conn)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// A rejected write is fine: the close frame still arrives.
			_ = conn.Write(ctx, websocket.MessageText, tc.payload)
			if code, _ := readTurnStreamClose(t, conn); code != tc.want {
				t.Fatalf("close = %d, want %d", code, tc.want)
			}
		})
	}
}

// Shutdown closes every open stream with 1001 and waits for the handlers, so a
// client reconnects instead of seeing a bare 1006 from a dropped socket.
func TestTurnStreamClosesWith1001OnShutdown(t *testing.T) {
	server := newTestServer()
	rs, device, httpSrv := newTurnStreamServer(t, server)
	conn := dialTurnStream(t, httpSrv, device.Token)
	readTurnStreamOpen(t, conn)

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- server.Shutdown(ctx)
	}()
	code, reason := readTurnStreamClose(t, conn)
	if code != websocket.StatusGoingAway || reason != "gateway shutting down" {
		t.Fatalf("close = %d %q", code, reason)
	}
	if err := <-done; err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if got := rs.TurnFeed().Stats().Subscribers; got != 0 {
		t.Fatalf("subscriptions after shutdown = %d, want 0", got)
	}
}

// A device's fifth stream closes its oldest with 4409 once the new one has its
// snapshot; the other three keep running.
func TestTurnStreamSupersedesTheDevicesOldestWith4409(t *testing.T) {
	server := newTestServer()
	rs, device, httpSrv := newTurnStreamServer(t, server)
	streams := make([]*websocket.Conn, 0, 5)
	for range 5 {
		conn := dialTurnStream(t, httpSrv, device.Token)
		readTurnStreamOpen(t, conn)
		streams = append(streams, conn)
	}

	if code, _ := readTurnStreamClose(t, streams[0]); code != 4409 {
		t.Fatalf("oldest stream close = %d, want 4409", code)
	}
	if got := rs.TurnFeed().Stats().Subscribers; got != 4 {
		t.Fatalf("subscriptions = %d, want 4", got)
	}
}

// The 257th subscription is refused before the upgrade, and the refusal leaves
// nothing behind: once the cap clears, a stream opens.
func TestTurnStreamAnswers503AtTheGlobalCap(t *testing.T) {
	server := newTestServer()
	rs, device, httpSrv := newTurnStreamServer(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	held := make([]*turns.Subscription, 0, 256)
	defer func() {
		for _, sub := range held {
			sub.Close()
		}
	}()
	for range 256 {
		_, sub, err := rs.TurnFeed().Subscribe(ctx, "dev-other")
		if err != nil {
			t.Fatalf("Subscribe() error = %v", err)
		}
		held = append(held, sub)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/turns/stream", nil)
	req.Header.Set("Authorization", "Bearer "+device.Token)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("stream at the cap = %d, want 503 (%s)", rec.Code, rec.Body.String())
	}

	for _, sub := range held {
		sub.Close()
	}
	held = nil
	conn := dialTurnStream(t, httpSrv, device.Token)
	readTurnStreamOpen(t, conn)
}

// The 9th concurrent snapshot build waits for a slot and gives up with 503;
// the refusal releases the slot it never took.
func TestTurnStreamAnswers503WhenSnapshotSlotsAreExhausted(t *testing.T) {
	server := newTestServer()
	_, device, _ := newTurnStreamServer(t, server)
	for range turnStreamSnapshotSlots {
		server.streamSlots <- struct{}{}
	}
	server.streamSnapshotTimeout = 25 * time.Millisecond

	getStream := func() int {
		req := httptest.NewRequest(http.MethodGet, "/v1/turns/stream", nil)
		req.Header.Set("Authorization", "Bearer "+device.Token)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	if got := getStream(); got != http.StatusServiceUnavailable {
		t.Fatalf("stream without a slot = %d, want 503", got)
	}
	for range turnStreamSnapshotSlots {
		<-server.streamSlots
	}
	if len(server.streamSlots) != 0 {
		t.Fatalf("stacked slots after a refused stream = %d", len(server.streamSlots))
	}
	// With a slot free the request reaches the upgrade, which a plain GET fails.
	if got := getStream(); got != http.StatusUpgradeRequired {
		t.Fatalf("stream with a slot = %d, want %d", got, http.StatusUpgradeRequired)
	}
}

// A request that authenticates but never upgrades leaves a live stream alone,
// even from the same device at its stream limit.
func TestTurnStreamFailedUpgradeEvictsNothing(t *testing.T) {
	server := newTestServer()
	rs, device, httpSrv := newTurnStreamServer(t, server)
	for range 4 {
		conn := dialTurnStream(t, httpSrv, device.Token)
		readTurnStreamOpen(t, conn)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/turns/stream", nil)
	req.Header.Set("Authorization", "Bearer "+device.Token)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUpgradeRequired {
		t.Fatalf("plain GET = %d, want %d", rec.Code, http.StatusUpgradeRequired)
	}
	if got := rs.TurnFeed().Stats().Subscribers; got != 4 {
		t.Fatalf("subscriptions after a failed upgrade = %d, want 4", got)
	}
}

// The diagnostics summary reports the feed's health beside runtime state, so a
// feed that stops sampling is visible without a new endpoint.
func TestTurnStreamStatsReachDiagnostics(t *testing.T) {
	server := newTestServer()
	rs, device, httpSrv := newTurnStreamServer(t, server)
	conn := dialTurnStream(t, httpSrv, device.Token)
	readTurnStreamOpen(t, conn)

	stats := rs.TurnFeed().Stats()
	req := httptest.NewRequest(http.MethodGet, "/v1/diagnostics/summary", nil)
	req.Header.Set("Authorization", "Bearer "+device.Token)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("diagnostics summary = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		TurnStream turnStreamSummary `json:"turnStream"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.TurnStream.Open != 1 || stats.Subscribers != 1 {
		t.Fatalf("open streams: diagnostics %d, feed %d", body.TurnStream.Open, stats.Subscribers)
	}
	if body.TurnStream.Samples == 0 || body.TurnStream.Samples != stats.Samples {
		t.Fatalf("samples: diagnostics %d, feed %d", body.TurnStream.Samples, stats.Samples)
	}
	if !body.TurnStream.LastSampleAt.Equal(stats.LastSampleAt) {
		t.Fatalf("lastSampleAt: diagnostics %v, feed %v", body.TurnStream.LastSampleAt, stats.LastSampleAt)
	}
}

// Every way the feed ends a subscription maps to the close code its client acts
// on. 4408 is checked here, not end to end: a client that stops reading cannot
// fill the feed's queue, because the kernel's socket buffers absorb the
// handler's small frames. The supersede test covers the same close path live.
func TestTurnStreamClosesASlowConsumerWith4408(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want websocket.StatusCode
	}{
		{turns.ErrSlowConsumer, 4408},
		{turns.ErrSuperseded, 4409},
		{turns.ErrStopped, websocket.StatusGoingAway},
		{errors.New("other"), websocket.StatusInternalError},
	} {
		if code, _ := turnStreamClose(tc.err); code != tc.want {
			t.Errorf("turnStreamClose(%v) = %d, want %d", tc.err, code, tc.want)
		}
	}
}

package session

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/push"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
	"github.com/coder/websocket"
)

// mockProcessManager implements ProcessManager for testing.
type mockProcessManager struct {
	mu            sync.Mutex
	leases        map[string]string
	exitCallbacks map[string]func(string)
	stopped       map[string]bool
	logs          []string
}

func newMockPM() *mockProcessManager {
	return &mockProcessManager{
		leases:        make(map[string]string),
		exitCallbacks: make(map[string]func(string)),
		stopped:       make(map[string]bool),
	}
}

func (m *mockProcessManager) AcquireLease(runtimeID, leaseholder string) (runtime.Pipes, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.leases[runtimeID]; ok && existing != "" {
		return nil, ErrRuntimeLeaseHeld
	}
	m.leases[runtimeID] = leaseholder
	return &runtime.LeasedPipes{
		Stdin:     nopWriteCloser{},
		Stdout:    io.NopCloser(strings.NewReader("")),
		RuntimeID: runtimeID,
	}, nil
}

func (m *mockProcessManager) ReleaseLease(runtimeID, leaseholder string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.leases[runtimeID]; !ok || existing != leaseholder {
		return ErrRuntimeLeaseHeld
	}
	m.leases[runtimeID] = ""
	return nil
}

func (m *mockProcessManager) OnProcessExit(runtimeID string, callback func(string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.exitCallbacks[runtimeID] = callback
}

func (m *mockProcessManager) StopByRuntimeID(runtimeID string) (runtime.Runtime, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped[runtimeID] = true
	return runtime.Runtime{}, nil
}

func (m *mockProcessManager) AppendLog(runtimeID, stream, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logs = append(m.logs, runtimeID+"/"+stream+": "+message)
}

func (m *mockProcessManager) triggerExit(runtimeID string) {
	m.mu.Lock()
	cb, ok := m.exitCallbacks[runtimeID]
	m.mu.Unlock()
	if ok {
		cb(runtimeID)
	}
}

// recordingPushService captures Notify calls for assertions. Notify is invoked
// asynchronously by the session, so reads poll via waitForPushCount.
type recordingPushService struct {
	mu    sync.Mutex
	calls []push.Notification
}

func (r *recordingPushService) Notify(_ context.Context, _ string, n push.Notification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, n)
	return nil
}

func (r *recordingPushService) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *recordingPushService) last() push.Notification {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[len(r.calls)-1]
}

func waitForPushCount(t *testing.T, r *recordingPushService, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.count() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected >= %d push calls, got %d", want, r.count())
}

func setupPushTest(t *testing.T) (*RuntimeSession, *mockProcessManager, *recordingPushService) {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	mockPM := newMockPM()
	pushSvc := &recordingPushService{}
	rs := NewRuntimeSession(slog.New(slog.NewTextHandler(io.Discard, nil)), store, mockPM, newMockTokenSvc(), Config{
		MaxDisconnected: 5 * time.Minute,
		GatewayID:       "gw-1",
		PushSvc:         pushSvc,
	})
	t.Cleanup(rs.Shutdown)
	return rs, mockPM, pushSvc
}

// TestProcessExitCrashSendsPush verifies a genuine, unexpected process exit
// delivers an agent-crash push and marks the session failed.
func TestProcessExitCrashSendsPush(t *testing.T) {
	rs, mockPM, pushSvc := setupPushTest(t)
	ctx := context.Background()

	sess, _, err := rs.Create(ctx, "rt-crash", "dev-crash", "agent-crash")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Simulate the agent's session/new response having been snooped, so the push
	// carries the ACP session id the client navigates by — not the gateway's own
	// resilient session id (sess.ID), which lives in a different namespace.
	const acpID = "ses_crash_test"
	sess.pump.acpMu.Lock()
	sess.pump.acpSessionID = acpID
	sess.pump.acpMu.Unlock()

	// Agent dies on its own while the session is active — a genuine crash.
	mockPM.triggerExit("rt-crash")

	waitForPushCount(t, pushSvc, 1)
	n := pushSvc.last()
	if n.Category != push.CategoryAgentCrash {
		t.Errorf("category = %q, want %q", n.Category, push.CategoryAgentCrash)
	}
	if n.SessionID != acpID || n.ServerID != "gw-1" {
		t.Errorf("notification = %+v, want sessionID=%q serverID=gw-1", n, acpID)
	}
	if st, _ := rs.GetSessionStatus(sess.ID); st != StatusFailed {
		t.Errorf("status = %q, want %q", st, StatusFailed)
	}
}

// TestProcessExitDuringCloseSendsNoCrashPush verifies that an intentional
// teardown (session already StatusClosing, as Close and the reaper set it before
// stopping the runtime) does not deliver a false crash push or clobber the status.
func TestProcessExitDuringCloseSendsNoCrashPush(t *testing.T) {
	rs, mockPM, pushSvc := setupPushTest(t)
	ctx := context.Background()

	sess, _, err := rs.Create(ctx, "rt-close", "dev-close", "agent-close")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Mark the session closing, mirroring Close/reaper before StopByRuntimeID.
	rs.mu.Lock()
	s := rs.sessions[sess.ID]
	rs.mu.Unlock()
	s.mu.Lock()
	s.Status = StatusClosing
	s.mu.Unlock()

	mockPM.triggerExit("rt-close")

	// The guard decision is synchronous, so no push goroutine is spawned; sleep a
	// little anyway to give any erroneous async push time to land before asserting.
	time.Sleep(150 * time.Millisecond)
	if c := pushSvc.count(); c != 0 {
		t.Fatalf("expected no crash push on intentional close, got %d", c)
	}
	if st, _ := rs.GetSessionStatus(sess.ID); st != StatusClosing {
		t.Errorf("status = %q, want %q (closing must be preserved, not overwritten to failed)", st, StatusClosing)
	}
}

type nopWriteCloser struct{}

func (n nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (n nopWriteCloser) Close() error                { return nil }

type mockTokenService struct {
	mu     sync.Mutex
	tokens map[string]tokenClaimTest
}

type tokenClaimTest struct {
	sessionID string
	deviceID  string
	valid     bool
}

func newMockTokenSvc() *mockTokenService {
	return &mockTokenService{
		tokens: make(map[string]tokenClaimTest),
	}
}

func (m *mockTokenService) Mint(sessionID, deviceID string, ttl time.Duration) (string, error) {
	token := generateID()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens[token] = tokenClaimTest{sessionID: sessionID, deviceID: deviceID, valid: true}
	return token, nil
}

func (m *mockTokenService) Validate(token string) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	claim, ok := m.tokens[token]
	if !ok || !claim.valid {
		return "", "", ErrAttachTokenInvalid
	}
	delete(m.tokens, token)
	return claim.sessionID, claim.deviceID, nil
}

func setupTest(t *testing.T, overrides Config) (*RuntimeSession, *storage.SQLiteStore, *mockProcessManager, *mockTokenService, context.Context) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	mockPM := newMockPM()
	mockToken := newMockTokenSvc()

	cfg := Config{
		MaxDisconnected: 5 * time.Minute,
		MaxPerDevice:    3,
	}
	if overrides.MaxDisconnected > 0 {
		cfg.MaxDisconnected = overrides.MaxDisconnected
	}
	if overrides.MaxPerDevice > 0 {
		cfg.MaxPerDevice = overrides.MaxPerDevice
	}
	if overrides.ReaperInterval > 0 {
		cfg.ReaperInterval = overrides.ReaperInterval
	}

	rs := NewRuntimeSession(slog.New(slog.NewTextHandler(io.Discard, nil)), store, mockPM, mockToken, cfg)
	return rs, store, mockPM, mockToken, ctx
}

// --- Session lifecycle ---

func TestCreateReturnsSessionWithID(t *testing.T) {
	rs, store, _, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, attachToken, err := rs.Create(ctx, "rt-caa", "dev-caa", "agent-caa")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess == nil || sess.ID == "" {
		t.Fatal("expected non-nil session with ID")
	}
	if attachToken == "" {
		t.Fatal("expected non-empty attach token")
	}

	rec, err := store.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if rec.Status != StatusActive {
		t.Errorf("expected status active in store, got %s", rec.Status)
	}
}

func TestAttachClientWithValidTokenReturnsRuntimeID(t *testing.T) {
	rs, store, _, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, attachToken, err := rs.Create(ctx, "rt-caa2", "dev-caa2", "agent-caa2")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	rID, _, err := rs.AttachClient(ctx, sess.ID, attachToken)
	if err != nil {
		t.Fatalf("AttachClient: %v", err)
	}
	if rID != "rt-caa2" {
		t.Errorf("expected runtimeID rt-caa2, got %s", rID)
	}

	_, _, err = rs.AttachClient(ctx, sess.ID, attachToken)
	if err == nil {
		t.Error("expected error on reused attach token")
	}
}

func TestCreateSessionLimit(t *testing.T) {
	rs, store, _, _, ctx := setupTest(t, Config{MaxPerDevice: 1})
	defer rs.Shutdown()
	defer store.Close()

	_, _, err := rs.Create(ctx, "rt-lim-1", "dev-lim", "agent-1")
	if err != nil {
		t.Fatalf("Create 1: %v", err)
	}

	_, _, err = rs.Create(ctx, "rt-lim-2", "dev-lim", "agent-2")
	if err != ErrSessionLimitReached {
		t.Errorf("expected ErrSessionLimitReached, got %v", err)
	}
}

func TestCreateDuplicateRuntime(t *testing.T) {
	rs, store, _, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	runtimeID := "rt-dup"
	_, _, err := rs.Create(ctx, runtimeID, "dev-dup", "agent-dup")
	if err != nil {
		t.Fatalf("Create 1: %v", err)
	}

	_, _, err = rs.Create(ctx, runtimeID, "dev-dup-2", "agent-dup-2")
	if !errors.Is(err, ErrRuntimeLeaseHeld) {
		t.Fatalf("expected ErrRuntimeLeaseHeld, got %v", err)
	}
}

func TestResumeDeviceMismatch(t *testing.T) {
	rs, store, _, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, "rt-rdm", "dev-a", "agent-a")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err = rs.Resume(ctx, sess.ID, "dev-b")
	if err != ErrDeviceMismatch {
		t.Errorf("expected ErrDeviceMismatch, got %v", err)
	}
}

func TestResumeSessionNotFound(t *testing.T) {
	rs, store, _, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	_, err := rs.Resume(ctx, "nonexistent-session", "dev-x")
	if err != ErrSessionNotFound {
		t.Errorf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestAttachClientInvalidToken(t *testing.T) {
	rs, store, _, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	_, _, err := rs.AttachClient(ctx, "session-x", "badtoken")
	if err != ErrAttachTokenInvalid {
		t.Errorf("expected ErrAttachTokenInvalid, got %v", err)
	}
}

func TestAttachClientSecondAttachTakesOver(t *testing.T) {
	rs, store, _, mockTokenSvc, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, token1, err := rs.Create(ctx, "rt-cc", "dev-cc", "agent-cc")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A new valid attach must take over from the previous connection rather than
	// being rejected — a resilient session's old socket is usually already dead.
	_, gen1, err := rs.AttachClient(ctx, sess.ID, token1)
	if err != nil {
		t.Fatalf("first AttachClient: %v", err)
	}

	token2, _ := mockTokenSvc.Mint(sess.ID, "dev-cc", 5*time.Minute)
	_, gen2, err := rs.AttachClient(ctx, sess.ID, token2)
	if err != nil {
		t.Fatalf("second AttachClient should take over, got: %v", err)
	}
	if gen2 <= gen1 {
		t.Errorf("expected takeover to advance the connection generation, got gen1=%d gen2=%d", gen1, gen2)
	}

	// The superseded connection's detach must not clobber the live session.
	if err := rs.DetachClient(sess.ID, gen1); err != nil {
		t.Fatalf("stale DetachClient: %v", err)
	}
	if status, _ := rs.GetSessionStatus(sess.ID); status != StatusActive {
		t.Errorf("expected session to stay active after a stale detach, got %s", status)
	}

	// The current connection's detach does transition the session.
	if err := rs.DetachClient(sess.ID, gen2); err != nil {
		t.Fatalf("current DetachClient: %v", err)
	}
	if status, _ := rs.GetSessionStatus(sess.ID); status != StatusDisconnected {
		t.Errorf("expected disconnected after current detach, got %s", status)
	}
}

func TestAttachClientSessionNotFound(t *testing.T) {
	rs, store, _, mockTokenSvc, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	token, _ := mockTokenSvc.Mint("nonexistent-session", "dev-x", 5*time.Minute)
	_, _, err := rs.AttachClient(ctx, "nonexistent-session", token)
	if err != ErrSessionNotFound {
		t.Errorf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestDetachClientSetsStatusDisconnected(t *testing.T) {
	rs, store, _, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, token, err := rs.Create(ctx, "rt-dar", "dev-dar", "agent-dar")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, gen, err := rs.AttachClient(ctx, sess.ID, token)
	if err != nil {
		t.Fatalf("AttachClient: %v", err)
	}

	if err := rs.DetachClient(sess.ID, gen); err != nil {
		t.Fatalf("DetachClient: %v", err)
	}

	rec, err := store.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetSession after detach: %v", err)
	}
	if rec.Status != StatusDisconnected {
		t.Errorf("expected status disconnected, got %s", rec.Status)
	}
}

func TestAttachClientAfterDetachResumesSession(t *testing.T) {
	rs, store, _, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, token, err := rs.Create(ctx, "rt-dar2", "dev-dar2", "agent-dar2")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, gen, err := rs.AttachClient(ctx, sess.ID, token)
	if err != nil {
		t.Fatalf("AttachClient: %v", err)
	}

	if err := rs.DetachClient(sess.ID, gen); err != nil {
		t.Fatalf("DetachClient: %v", err)
	}

	newToken, err := rs.Resume(ctx, sess.ID, "dev-dar2")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if newToken == "" {
		t.Fatal("expected non-empty resume token")
	}

	_, _, err = rs.AttachClient(ctx, sess.ID, newToken)
	if err != nil {
		t.Fatalf("AttachClient after resume: %v", err)
	}

	rec, err := store.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetSession after reattach: %v", err)
	}
	if rec.Status != StatusActive {
		t.Errorf("expected status active after reattach, got %s", rec.Status)
	}
}

func TestClose(t *testing.T) {
	rs, store, mockPM, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, "rt-close", "dev-close", "agent-close")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := rs.Close(ctx, sess.ID, "dev-close"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if !mockPM.stopped[sess.RuntimeID] {
		t.Error("expected runtime to be stopped")
	}

	_, err = rs.GetSessionStatus(sess.ID)
	if err != ErrSessionNotFound {
		t.Error("expected session removed from in-memory map")
	}

	_, err = store.GetSession(ctx, sess.ID)
	if err == nil {
		t.Error("expected session deleted from store")
	}
}

func TestCloseDeviceMismatch(t *testing.T) {
	rs, store, _, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, "rt-cdm", "dev-a", "agent-a")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	err = rs.Close(ctx, sess.ID, "dev-b")
	if err != ErrDeviceMismatch {
		t.Errorf("expected ErrDeviceMismatch, got %v", err)
	}
}

func TestCloseSessionNotFound(t *testing.T) {
	rs, store, _, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	err := rs.Close(ctx, "nonexistent", "dev-x")
	if err != ErrSessionNotFound {
		t.Errorf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestListByDevice(t *testing.T) {
	rs, store, _, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	_, _, err := rs.Create(ctx, "rt-lbd-1", "dev-lbd-a", "agent-1")
	if err != nil {
		t.Fatalf("Create 1: %v", err)
	}
	_, _, err = rs.Create(ctx, "rt-lbd-2", "dev-lbd-a", "agent-2")
	if err != nil {
		t.Fatalf("Create 2: %v", err)
	}
	_, _, err = rs.Create(ctx, "rt-lbd-3", "dev-lbd-b", "agent-3")
	if err != nil {
		t.Fatalf("Create 3: %v", err)
	}

	summaries, err := rs.ListByDevice(ctx, "dev-lbd-a")
	if err != nil {
		t.Fatalf("ListByDevice: %v", err)
	}
	if len(summaries) != 2 {
		t.Errorf("expected 2 sessions, got %d", len(summaries))
	}
}

func TestGetPump(t *testing.T) {
	rs, store, _, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, "rt-gp", "dev-gp", "agent-gp")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	pump, err := rs.GetPump(sess.ID)
	if err != nil {
		t.Fatalf("GetPump: %v", err)
	}
	if pump == nil {
		t.Fatal("expected non-nil pump")
	}

	_, err = rs.GetPump("nonexistent")
	if err != ErrSessionNotFound {
		t.Errorf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestGetSessionStatus(t *testing.T) {
	rs, store, _, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, "rt-gss", "dev-gss", "agent-gss")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	status, err := rs.GetSessionStatus(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionStatus: %v", err)
	}
	if status != StatusActive {
		t.Errorf("expected active, got %s", status)
	}

	if err := rs.DetachClient(sess.ID, 0); err != nil {
		t.Fatalf("DetachClient: %v", err)
	}

	status, err = rs.GetSessionStatus(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionStatus after detach: %v", err)
	}
	if status != StatusDisconnected {
		t.Errorf("expected disconnected, got %s", status)
	}

	_, err = rs.GetSessionStatus("nonexistent")
	if err != ErrSessionNotFound {
		t.Errorf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestLogInbound(t *testing.T) {
	rs, store, _, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, "rt-log", "dev-log", "agent-log")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	rs.LogInbound(sess.ID, `{"jsonrpc":"2.0","method":"test","params":{}}`)

	rs.LogInbound("nonexistent", `{"jsonrpc":"2.0","method":"test","params":{}}`)
}

func TestInboundWriterSend(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "inbound_send_test.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	w := newInboundWriter(store)
	defer w.stop()

	ok := w.send(inboundDiagnostic{
		SessionID: "test-session",
		Seq:       1,
		Payload:   `{"jsonrpc":"2.0","method":"test"}`,
	})
	if !ok {
		t.Error("expected send to succeed")
	}
}

func TestInboundWriterDropped(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "inbound_drop_test.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	w := &inboundWriter{
		store: store,
		ch:    make(chan inboundDiagnostic, 1),
	}

	ok := w.send(inboundDiagnostic{SessionID: "s1", Seq: 1, Payload: "p1"})
	if !ok {
		t.Error("expected first send to succeed")
	}

	ok = w.send(inboundDiagnostic{SessionID: "s1", Seq: 2, Payload: "p2"})
	if ok {
		t.Error("expected second send to be dropped when buffer is full")
	}
}

// --- Pump (StdioPump) ---

func TestPumpStdoutDrainLoopWithWebSocket(t *testing.T) {
	serverCh := make(chan *websocket.Conn, 1)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, aErr := websocket.Accept(w, r, nil)
		if aErr != nil {
			return
		}
		serverCh <- c
	}))
	defer s.Close()

	wsURL := "ws://" + s.Listener.Addr().String() + "/"
	_, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	sc := <-serverCh
	if sc == nil {
		t.Fatal("server connection not established")
	}

	r, w := io.Pipe()
	pump := &StdioPump{
		pipes: &runtime.LeasedPipes{
			Stdin:  nopWriteCloser{},
			Stdout: r,
		},
		runtimeID: "rt-drain-ws",
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		appendLog: func(string, string, string) {},
	}
	pump.SetClient(sc)

	ctx, cancel := context.WithCancel(context.Background())
	go pump.StdoutDrainLoop(ctx)

	w.Write([]byte("hello\n"))
	time.Sleep(50 * time.Millisecond)

	cancel()
	w.Write([]byte("second\n"))
	time.Sleep(50 * time.Millisecond)

	w.Close()
	r.Close()
	sc.Close(websocket.StatusNormalClosure, "")
	time.Sleep(10 * time.Millisecond)
}

func TestPumpStdoutDrainLoopWebSocketWriteError(t *testing.T) {
	serverCh := make(chan *websocket.Conn, 1)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, aErr := websocket.Accept(w, r, nil)
		if aErr != nil {
			return
		}
		serverCh <- c
	}))
	defer s.Close()

	wsURL := "ws://" + s.Listener.Addr().String() + "/"
	_, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	sc := <-serverCh
	sc.Close(websocket.StatusNormalClosure, "")

	r, w := io.Pipe()
	pump := &StdioPump{
		pipes: &runtime.LeasedPipes{
			Stdin:  nopWriteCloser{},
			Stdout: r,
		},
		runtimeID: "rt-drain-ws-err",
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	pump.SetClient(sc)

	ctx, cancel := context.WithCancel(context.Background())
	go pump.StdoutDrainLoop(ctx)

	w.Write([]byte("hello\n"))
	time.Sleep(100 * time.Millisecond)

	cancel()
	w.Write([]byte("second\n"))
	time.Sleep(50 * time.Millisecond)
	w.Close()
	r.Close()
}

func TestPumpSetAndClearClient(t *testing.T) {
	mockPM := newMockPM()
	pi, err := mockPM.AcquireLease("pump-test", "test")
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	pipes := pi.(*runtime.LeasedPipes)

	pump := &StdioPump{
		pipes:     pipes,
		runtimeID: "pump-test",
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	pump.SetClient(nil)
	pump.clientMu.Lock()
	if pump.client != nil {
		t.Error("expected client to be nil after SetClient(nil)")
	}
	pump.clientMu.Unlock()

	pump.ClearClient()
	pump.clientMu.Lock()
	if pump.client != nil {
		t.Error("expected client to be nil after ClearClient()")
	}
	pump.clientMu.Unlock()
}

func TestPumpSupportsClose(t *testing.T) {
	mockPM := newMockPM()
	pi, err := mockPM.AcquireLease("pump-sc", "test")
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	pipes := pi.(*runtime.LeasedPipes)

	pump := &StdioPump{
		pipes:     pipes,
		runtimeID: "pump-sc",
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	if pump.SupportsClose() {
		t.Error("expected SupportsClose to be false initially")
	}

	pump.snoopInitialize(`{"result":{"protocolVersion":1,"agentCapabilities":{"sessionCapabilities":{"close":{}}}}}`)

	if !pump.SupportsClose() {
		t.Error("expected SupportsClose to be true after snooping initialize")
	}
}

func TestPumpWriteToAgent(t *testing.T) {
	mockPM := newMockPM()
	pi, err := mockPM.AcquireLease("pump-wt", "test")
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	pipes := pi.(*runtime.LeasedPipes)

	pump := &StdioPump{
		pipes:     pipes,
		runtimeID: "pump-wt",
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	if err := pump.WriteToAgent([]byte("hello")); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- Store fallback (cold-start recovery) ---

func TestResumeStoreFallbackNotFound(t *testing.T) {
	rs, store, _, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	_, err := rs.Resume(ctx, "resume-store-notfound", "dev-x")
	if err != ErrSessionNotFound {
		t.Errorf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestAttachClientStoreFallbackNotFound(t *testing.T) {
	rs, store, _, mockTokenSvc, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	token, _ := mockTokenSvc.Mint("ac-fallback-notfound", "dev-x", 5*time.Minute)
	_, _, err := rs.AttachClient(ctx, "ac-fallback-notfound", token)
	if err != ErrSessionNotFound {
		t.Errorf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestCloseWithSupportsClose(t *testing.T) {
	rs, store, mockPM, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, "rt-close-sc", "dev-close-sc", "agent-close-sc")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	sess.pump.supportsClose.Store(true)

	if err := rs.Close(ctx, sess.ID, "dev-close-sc"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if !mockPM.stopped[sess.RuntimeID] {
		t.Error("expected runtime to be stopped")
	}
}

// --- Reaper ---

func TestReaperLoopShutdown(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reaper_shutdown_test.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	mockPM := newMockPM()
	mockTokenSvc := newMockTokenSvc()
	rs := NewRuntimeSession(slog.New(slog.NewTextHandler(io.Discard, nil)), store, mockPM, mockTokenSvc, Config{})

	if rs.cancelReaper == nil {
		t.Fatal("expected cancelReaper to be non-nil after NewRuntimeSession")
	}

	rs.Shutdown()

	rs.mu.Lock()
	if len(rs.sessions) != 0 {
		t.Errorf("expected 0 sessions after shutdown, got %d", len(rs.sessions))
	}
	rs.mu.Unlock()

	rs.reapExpired(0)
}

func TestListByDeviceStoreError(t *testing.T) {
	rs, store, _, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	store.Close()

	_, err := rs.ListByDevice(ctx, "dev-any")
	if err == nil {
		t.Error("expected error from closed store")
	}
}

func TestDetachClientNotFound(t *testing.T) {
	rs, store, _, _, _ := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	err := rs.DetachClient("nonexistent", 0)
	if err != ErrSessionNotFound {
		t.Errorf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestPumpStdoutDrainLoop(t *testing.T) {
	r, w := io.Pipe()

	pump := &StdioPump{
		pipes: &runtime.LeasedPipes{
			Stdin:  nopWriteCloser{},
			Stdout: r,
		},
		runtimeID: "rt-drain",
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		appendLog: func(string, string, string) {},
	}

	ctx, cancel := context.WithCancel(context.Background())
	go pump.StdoutDrainLoop(ctx)

	_, err := w.Write([]byte("hello\n"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	cancel()
	w.Write([]byte("second\n"))
	time.Sleep(50 * time.Millisecond)

	w.Close()
	r.Close()
	time.Sleep(10 * time.Millisecond)
}

func TestPumpStdoutDrainLoopWithFramesAndAppendLog(t *testing.T) {
	r, w := io.Pipe()

	var logged bool
	pump := &StdioPump{
		pipes: &runtime.LeasedPipes{
			Stdin:  nopWriteCloser{},
			Stdout: r,
		},
		runtimeID: "rt-drain-log",
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		appendLog: func(rid, stream, msg string) {
			logged = true
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	go pump.StdoutDrainLoop(ctx)

	w.Write([]byte("line1\n"))
	time.Sleep(50 * time.Millisecond)

	cancel()
	w.Write([]byte("line2\n"))
	time.Sleep(50 * time.Millisecond)

	w.Close()
	r.Close()
	time.Sleep(10 * time.Millisecond)

	if !logged {
		t.Error("expected appendLog to be called")
	}
}

func TestPumpSnoopInitializeAlreadySet(t *testing.T) {
	pump := &StdioPump{}
	pump.supportsClose.Store(true)
	pump.snoopInitialize(`{"result":{"agentCapabilities":{"sessionCapabilities":{"close":false}}}}`)
	if !pump.SupportsClose() {
		t.Error("expected SupportsClose still true")
	}
}

// TestPumpSnoopSessionID verifies the ACP session id is captured from a
// session/new response, ignored on unrelated frames, and not overwritten once set.
func TestPumpSnoopSessionID(t *testing.T) {
	pump := &StdioPump{}

	// Unrelated frames don't set it.
	pump.snoopSessionID(`{"result":{"protocolVersion":1}}`)
	pump.snoopSessionID(`{"method":"session/update","params":{}}`)
	if got := pump.AcpSessionID(); got != "" {
		t.Fatalf("AcpSessionID = %q, want empty before session/new", got)
	}

	// The session/new response carries result.sessionId — captured here.
	pump.snoopSessionID(`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"ses_abc123"}}`)
	if got := pump.AcpSessionID(); got != "ses_abc123" {
		t.Fatalf("AcpSessionID = %q, want %q", got, "ses_abc123")
	}

	// A later session/new (e.g. reconnect) does not clobber the captured id.
	pump.snoopSessionID(`{"result":{"sessionId":"ses_other"}}`)
	if got := pump.AcpSessionID(); got != "ses_abc123" {
		t.Fatalf("AcpSessionID = %q, want it to stay %q", got, "ses_abc123")
	}
}

// TestPumpSnoopInboundSessionID verifies the ACP session id is captured from a
// client→agent frame's params.sessionId — the path that covers resilient
// reconnects, where the agent never re-emits a session/new response.
func TestPumpSnoopInboundSessionID(t *testing.T) {
	pump := &StdioPump{}

	// Frames without params.sessionId don't set it (e.g. initialize).
	pump.snoopInboundSessionID([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`))
	if got := pump.AcpSessionID(); got != "" {
		t.Fatalf("AcpSessionID = %q, want empty before a frame carries sessionId", got)
	}

	// A session/prompt (or session/load) carries params.sessionId — captured here.
	pump.snoopInboundSessionID([]byte(`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"ses_inbound","prompt":[]}}`))
	if got := pump.AcpSessionID(); got != "ses_inbound" {
		t.Fatalf("AcpSessionID = %q, want %q", got, "ses_inbound")
	}

	// A later inbound frame does not clobber the captured id.
	pump.snoopInboundSessionID([]byte(`{"method":"session/cancel","params":{"sessionId":"ses_other"}}`))
	if got := pump.AcpSessionID(); got != "ses_inbound" {
		t.Fatalf("AcpSessionID = %q, want it to stay %q", got, "ses_inbound")
	}
}

// TestPumpTurnCompletePushUsesACPSessionID verifies the drain loop snoops the
// ACP session id from session/new and then stamps it onto a turn-complete push.
func TestPumpTurnCompletePushUsesACPSessionID(t *testing.T) {
	r, w := io.Pipe()

	pushCh := make(chan push.Notification, 4)
	pump := &StdioPump{
		pipes: &runtime.LeasedPipes{
			Stdin:  nopWriteCloser{},
			Stdout: r,
		},
		runtimeID: "rt-acp-push",
		sessionID: "resilient-id",
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		appendLog: func(string, string, string) {},
		onPushNotification: func(e PushEvent) {
			pushCh <- push.Notification{Category: e.Category, ServerID: e.SessionID, SessionID: e.AcpSessionID}
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pump.StdoutDrainLoop(ctx)

	// session/new first so the ACP id is captured before the turn completes.
	w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"ses_live"}}` + "\n"))
	w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"stopReason":"end_turn"}}` + "\n"))

	select {
	case n := <-pushCh:
		if n.Category != push.CategoryTurnComplete {
			t.Fatalf("category = %q, want %q", n.Category, push.CategoryTurnComplete)
		}
		if n.SessionID != "ses_live" {
			t.Fatalf("push SessionID = %q, want the snooped ACP id %q", n.SessionID, "ses_live")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected a turn-complete push")
	}

	w.Close()
	r.Close()
}

func TestIsTurnCompleteWithAllStopReasons(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{"end_turn", `{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`, true},
		{"max_tokens", `{"jsonrpc":"2.0","id":1,"result":{"stopReason":"max_tokens"}}`, true},
		{"refusal", `{"jsonrpc":"2.0","id":1,"result":{"stopReason":"refusal"}}`, true},
		{"cancelled", `{"jsonrpc":"2.0","id":1,"result":{"stopReason":"cancelled"}}`, true},
		{"empty_stop_reason", `{"jsonrpc":"2.0","id":1,"result":{"stopReason":""}}`, false},
		{"no_stop_reason", `{"jsonrpc":"2.0","id":1,"result":{"foo":"bar"}}`, false},
		{"no_result", `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"err"}}`, false},
		{"notification", `{"jsonrpc":"2.0","method":"session/update","params":{}}`, false},
		{"request", `{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTurnComplete([]byte(tt.data)); got != tt.want {
				t.Errorf("isTurnComplete(%q) = %v, want %v", tt.data, got, tt.want)
			}
		})
	}
}

func TestIsPermissionRequest(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{"permission_request", `{"jsonrpc":"2.0","method":"session/request_permission","params":{"sessionId":"s1","request":{}}}`, true},
		{"session_update", `{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"plan"}}}`, false},
		{"prompt_response", `{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`, false},
		{"error_response", `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"err"}}`, false},
		{"cancel_notification", `{"jsonrpc":"2.0","method":"session/cancel","params":{}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPermissionRequest([]byte(tt.data)); got != tt.want {
				t.Errorf("isPermissionRequest(%q) = %v, want %v", tt.data, got, tt.want)
			}
		})
	}
}

func TestIsJSONRPCError(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{"internal_error", `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"internal error"}}`, true},
		{"method_not_found", `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`, true},
		{"invalid_params", `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"invalid params","data":{}}}`, true},
		{"prompt_response", `{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`, false},
		{"notification", `{"jsonrpc":"2.0","method":"session/update","params":{}}`, false},
		{"request", `{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{}}`, false},
		{"malformed", `not json`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isJSONRPCError([]byte(tt.data)); got != tt.want {
				t.Errorf("isJSONRPCError(%q) = %v, want %v", tt.data, got, tt.want)
			}
		})
	}
}

func TestCreateStoreError(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "create_err_test.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	store.Close()

	mockPM := newMockPM()
	mockTokenSvc := newMockTokenSvc()
	rs := NewRuntimeSession(slog.New(slog.NewTextHandler(io.Discard, nil)), store, mockPM, mockTokenSvc, Config{})
	defer rs.Shutdown()
	defer store.Close()

	_, _, err = rs.Create(context.Background(), "rt-err", "dev-err", "agent-err")
	if err == nil {
		t.Error("expected error from closed store")
	}
}

func TestResumeInMemoryStatusClosing(t *testing.T) {
	rs, store, _, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, "rt-rs-cl", "dev-rs-cl", "agent-rs-cl")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	sess.mu.Lock()
	sess.Status = StatusClosing
	sess.mu.Unlock()

	_, err = rs.Resume(ctx, sess.ID, "dev-rs-cl")
	if err != ErrSessionNotActive {
		t.Errorf("expected ErrSessionNotActive, got %v", err)
	}
}

func TestAttachClientTokenSessionIDMismatch(t *testing.T) {
	rs, store, _, mockTokenSvc, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	token, _ := mockTokenSvc.Mint("session-a", "dev-x", 5*time.Minute)
	_, _, err := rs.AttachClient(ctx, "session-b", token)
	if err != ErrAttachTokenInvalid {
		t.Errorf("expected ErrAttachTokenInvalid, got %v", err)
	}
}

func TestAttachClientInMemoryDeviceMismatch(t *testing.T) {
	rs, store, _, mockTokenSvc, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, "rt-ac-dmim", "dev-ac-dmim", "agent-ac-dmim")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	token, _ := mockTokenSvc.Mint(sess.ID, "dev-wrong", 5*time.Minute)
	_, _, err = rs.AttachClient(ctx, sess.ID, token)
	if err != ErrDeviceMismatch {
		t.Errorf("expected ErrDeviceMismatch, got %v", err)
	}
}

func TestAttachClientInMemoryStatusFailed(t *testing.T) {
	rs, store, _, mockTokenSvc, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, "rt-ac-fail", "dev-ac-fail", "agent-ac-fail")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	sess.mu.Lock()
	sess.Status = StatusFailed
	sess.mu.Unlock()

	token, _ := mockTokenSvc.Mint(sess.ID, "dev-ac-fail", 5*time.Minute)
	_, _, err = rs.AttachClient(ctx, sess.ID, token)
	if err != ErrSessionNotActive {
		t.Errorf("expected ErrSessionNotActive, got %v", err)
	}
}

func TestReaperLoopDefensiveDefaults(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reaper_def_test.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	rs := &RuntimeSession{
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:    store,
		pm:       newMockPM(),
		tokenSvc: newMockTokenSvc(),
		cfg:      Config{},
		sessions: make(map[string]*Session),
		inbound:  newInboundWriter(store),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		rs.reaperLoop(ctx)
		close(done)
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reaper loop did not exit within timeout after cancel")
	}
	rs.inbound.stop()
}

func TestReaperLoopTickerFires(t *testing.T) {
	rs, store, _, _, ctx := setupTest(t, Config{ReaperInterval: time.Millisecond, MaxDisconnected: time.Millisecond})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, "rt-reap-tick", "dev-reap-tick", "agent-reap-tick")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := rs.DetachClient(sess.ID, 0); err != nil {
		t.Fatalf("DetachClient: %v", err)
	}

	// Poll for the reaper to fire rather than asserting after one fixed sleep:
	// the reap pipeline (tick → stop → release → delete) takes a variable amount
	// of time, and under -race or a loaded CI box a single 5ms wait flakes.
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err = rs.GetSessionStatus(sess.ID)
		if err == ErrSessionNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected session to be reaped (ErrSessionNotFound), got %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestReapExpiredSkipsActive(t *testing.T) {
	rs, store, _, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, "rt-reap-skip", "dev-reap-skip", "agent-reap-skip")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	rs.reapExpired(0)

	status, err := rs.GetSessionStatus(sess.ID)
	if err != nil {
		t.Errorf("expected session to still exist, got %v", err)
	}
	if status != StatusActive {
		t.Errorf("expected status active, got %s", status)
	}
}

func TestReapExpired(t *testing.T) {
	rs, store, mockPM, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, "rt-reap", "dev-reap", "agent-reap")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := rs.DetachClient(sess.ID, 0); err != nil {
		t.Fatalf("DetachClient: %v", err)
	}

	rs.reapExpired(0)

	_, err = rs.GetSessionStatus(sess.ID)
	if err != ErrSessionNotFound {
		t.Errorf("expected session to be reaped (ErrSessionNotFound), got %v", err)
	}

	if !mockPM.stopped[sess.RuntimeID] {
		t.Error("expected runtime to be stopped")
	}

	_, err = store.GetSession(ctx, sess.ID)
	if err == nil {
		t.Error("expected session to be deleted from store")
	}
}

func TestReapExpiredSkipsStreamingAgent(t *testing.T) {
	rs, store, _, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, "rt-stream", "dev-stream", "agent-stream")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := rs.DetachClient(sess.ID, 0); err != nil {
		t.Fatalf("DetachClient: %v", err)
	}

	// Simulate an agent that has emitted stdout recently.
	sess.pump.lastStdoutMu.Lock()
	sess.pump.lastStdoutAt = time.Now()
	sess.pump.lastStdoutMu.Unlock()

	// maxDisc = 5min, but lastStdoutAt is "now" → extends the grace period.
	rs.reapExpired(5 * time.Minute)

	status, err := rs.GetSessionStatus(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionStatus: %v", err)
	}
	if status != StatusDisconnected {
		t.Errorf("expected session to survive (StatusDisconnected), got %s", status)
	}
}

func TestReapExpiredReapsIdleAgent(t *testing.T) {
	rs, store, mockPM, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, "rt-idle", "dev-idle", "agent-idle")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := rs.DetachClient(sess.ID, 0); err != nil {
		t.Fatalf("DetachClient: %v", err)
	}

	// Agent hasn't produced stdout for a long time.
	sess.pump.lastStdoutMu.Lock()
	sess.pump.lastStdoutAt = time.Now().Add(-1 * time.Hour)
	sess.pump.lastStdoutMu.Unlock()

	rs.reapExpired(0)

	_, err = rs.GetSessionStatus(sess.ID)
	if err != ErrSessionNotFound {
		t.Errorf("expected session to be reaped (ErrSessionNotFound), got %v", err)
	}

	if !mockPM.stopped[sess.RuntimeID] {
		t.Error("expected runtime to be stopped")
	}
}

func TestCreateEmptyRuntimeID(t *testing.T) {
	rs, store, _, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, token, err := rs.Create(ctx, "", "dev-empty", "agent-empty")
	if err != nil {
		t.Fatalf("Create with empty runtimeID: %v", err)
	}
	if sess == nil || sess.ID == "" {
		t.Fatal("expected non-nil session with ID")
	}
	if token == "" {
		t.Fatal("expected non-empty attach token")
	}

	_, _, err = rs.Create(ctx, "", "dev-empty-2", "agent-empty-2")
	if !errors.Is(err, ErrRuntimeLeaseHeld) {
		t.Errorf("expected ErrRuntimeLeaseHeld on duplicate empty runtimeID, got %v", err)
	}
}

func TestCreateEmptyDeviceID(t *testing.T) {
	rs, store, _, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, token, err := rs.Create(ctx, "rt-empty-dev", "", "agent-empty")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess == nil || sess.ID == "" {
		t.Fatal("expected non-nil session with ID")
	}
	if token == "" {
		t.Fatal("expected non-empty attach token")
	}
}

func TestAttachClientOnDisconnectedSession(t *testing.T) {
	rs, store, _, mockTokenSvc, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, token, err := rs.Create(ctx, "rt-att-disc", "dev-att-disc", "agent-att-disc")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, gen, err := rs.AttachClient(ctx, sess.ID, token)
	if err != nil {
		t.Fatalf("first AttachClient: %v", err)
	}

	if err := rs.DetachClient(sess.ID, gen); err != nil {
		t.Fatalf("DetachClient: %v", err)
	}

	newToken, _ := mockTokenSvc.Mint(sess.ID, "dev-att-disc", 5*time.Minute)
	_, _, err = rs.AttachClient(ctx, sess.ID, newToken)
	if err != nil {
		t.Fatalf("AttachClient on disconnected session: %v", err)
	}

	status, err := rs.GetSessionStatus(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionStatus: %v", err)
	}
	if status != StatusActive {
		t.Errorf("expected status active after reattach, got %s", status)
	}
}

func TestPumpReplaysInitializeAfterCaching(t *testing.T) {
	p := &StdioPump{logger: slog.Default()}

	initReq := []byte(`{"jsonrpc":"2.0","id":"1","method":"initialize","params":{}}`)

	// Before the agent responds there is nothing to replay: the first
	// initialize must reach the agent.
	if p.MaybeReplayInitialize(initReq) {
		t.Fatal("first initialize must not be replayed (no cached response yet)")
	}

	// Agent's initialize response is cached (identified by result.protocolVersion).
	p.snoopInitialize(`{"jsonrpc":"2.0","id":"1","result":{"protocolVersion":1,"agentCapabilities":{"sessionCapabilities":{"close":{}}}}}`)
	if !p.initResponseCached() {
		t.Fatal("expected initialize response to be cached")
	}
	if !p.SupportsClose() {
		t.Error("expected supportsClose to be detected from the cached response")
	}

	// Non-initialize frames are never intercepted.
	if p.MaybeReplayInitialize([]byte(`{"jsonrpc":"2.0","id":"7","method":"session/load","params":{}}`)) {
		t.Error("non-initialize frame must not be replayed")
	}

	// A reconnecting client's duplicate initialize is intercepted (handled even
	// with no attached client conn).
	if !p.MaybeReplayInitialize([]byte(`{"jsonrpc":"2.0","id":"42","method":"initialize","params":{}}`)) {
		t.Error("duplicate initialize must be replayed from cache")
	}
}

func TestPumpSnoopIgnoresNonInitializeResults(t *testing.T) {
	p := &StdioPump{logger: slog.Default()}
	// A prompt response has a result object but no protocolVersion; it must not
	// be mistaken for the initialize response.
	p.snoopInitialize(`{"jsonrpc":"2.0","id":"5","result":{"stopReason":"end_turn"}}`)
	if p.initResponseCached() {
		t.Fatal("non-initialize result must not be cached as the initialize response")
	}
}

func TestRewriteResponseIDSwapsID(t *testing.T) {
	cached := []byte(`{"jsonrpc":"2.0","id":"1","result":{"protocolVersion":1}}`)

	// String id.
	out, ok := rewriteResponseID(cached, []byte(`"99"`))
	if !ok {
		t.Fatal("rewriteResponseID returned not-ok for valid input")
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal rewritten response: %v", err)
	}
	if got["id"] != "99" {
		t.Errorf("expected id swapped to \"99\", got %v", got["id"])
	}
	if _, hasResult := got["result"]; !hasResult {
		t.Error("rewritten response lost its result payload")
	}

	// Numeric id is preserved verbatim.
	out, ok = rewriteResponseID(cached, []byte(`7`))
	if !ok {
		t.Fatal("rewriteResponseID returned not-ok for numeric id")
	}
	if !strings.Contains(string(out), `"id":7`) {
		t.Errorf("expected numeric id 7 in rewritten response, got %s", string(out))
	}

	// Malformed cache is rejected so the caller falls back to forwarding.
	if _, ok := rewriteResponseID([]byte(`not json`), []byte(`"1"`)); ok {
		t.Error("expected rewriteResponseID to reject malformed cache")
	}
}

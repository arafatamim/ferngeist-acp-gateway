package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/catalog"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/push"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
	"github.com/coder/websocket"
)

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

// assertProcessRunning fails if the session's real agent process has already
// exited. Replaces the old mock's "runtime was stopped" bookkeeping.
func assertProcessRunning(t *testing.T, sess *Session) {
	t.Helper()
	select {
	case <-sess.proc.Done():
		t.Error("expected agent process to still be running")
	default:
	}
}

// waitProcessStopped waits (bounded) for the session's real agent process to
// exit, the observable equivalent of the old mock's stopped flag.
func waitProcessStopped(t *testing.T, sess *Session) {
	t.Helper()
	select {
	case <-sess.proc.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("expected agent process to have stopped")
	}
}

func setupPushTest(t *testing.T) (*RuntimeSession, catalog.Agent, *recordingPushService) {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pushSvc := &recordingPushService{}
	rs := NewRuntimeSession(logger, store, runtime.NewSupervisor(logger, t.TempDir(), nil, nil), NewTokenService(logger), Config{
		MaxDisconnected: 5 * time.Minute,
		GatewayID:       "gw-1",
		PushSvc:         pushSvc,
	})
	t.Cleanup(rs.Shutdown)
	return rs, mockAgent(t), pushSvc
}

// TestProcessExitCrashSendsPush verifies a genuine, unexpected process exit
// delivers an agent-crash push and marks the session failed.
func TestProcessExitCrashSendsPush(t *testing.T) {
	rs, agent, pushSvc := setupPushTest(t)
	ctx := context.Background()

	sess, _, err := rs.Create(ctx, agent, "dev-crash", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Simulate the agent's session/new response having been observed, so the push
	// carries the ACP session id the client navigates by — not the gateway's own
	// resilient session id (sess.ID), which lives in a different namespace.
	const acpID = "ses_crash_test"
	sess.pump.acp.mu.Lock()
	sess.pump.acp.sessionID = acpID
	sess.pump.acp.cwdBySession = map[string]string{acpID: "/work/repo"}
	sess.pump.acp.mu.Unlock()

	// Agent dies on its own while the session is active — a genuine crash.
	_ = sess.proc.Cmd.Process.Kill()

	waitForPushCount(t, pushSvc, 1)
	n := pushSvc.last()
	if n.Category != push.CategoryAgentCrash {
		t.Errorf("category = %q, want %q", n.Category, push.CategoryAgentCrash)
	}
	if n.SessionID != acpID || n.ServerID != "gw-1" || n.Cwd != "/work/repo" {
		t.Errorf("notification = %+v, want sessionID=%q serverID=gw-1 cwd=/work/repo", n, acpID)
	}
	// A genuine crash fully reclaims the session: it is removed from the registry
	// (so its dead runtime and per-device quota slot are freed) rather than
	// lingering as a "failed" record.
	if _, err := rs.GetSessionStatus(sess.ID); err != ErrSessionNotFound {
		t.Errorf("expected crashed session to be reclaimed (ErrSessionNotFound), got %v", err)
	}
}

// TestProcessExitDuringCloseSendsNoCrashPush verifies that an intentional
// teardown (session already StatusClosing, as Close and the reaper set it before
// stopping the runtime) does not deliver a false crash push or clobber the status.
func TestProcessExitDuringCloseSendsNoCrashPush(t *testing.T) {
	rs, agent, pushSvc := setupPushTest(t)
	ctx := context.Background()

	sess, _, err := rs.Create(ctx, agent, "dev-close", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Mark the session closing, mirroring Close/reaper before stopping the process.
	rs.mu.Lock()
	s := rs.sessions[sess.ID]
	rs.mu.Unlock()
	s.mu.Lock()
	s.Status = StatusClosing
	s.mu.Unlock()

	_ = sess.proc.Cmd.Process.Kill()

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

func setupTest(t *testing.T, overrides Config) (*RuntimeSession, *storage.SQLiteStore, catalog.Agent, *TokenService, context.Context) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tokenSvc := NewTokenService(logger)

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

	rs := NewRuntimeSession(logger, store, runtime.NewSupervisor(logger, t.TempDir(), nil, nil), tokenSvc, cfg)
	return rs, store, mockAgent(t), tokenSvc, ctx
}

// --- Session lifecycle ---

func TestCreateReturnsSessionWithID(t *testing.T) {
	rs, store, agent, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, attachToken, err := rs.Create(ctx, agent, "dev-caa", nil)
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
	if rec.Status != StatusDisconnected {
		t.Errorf("expected status disconnected in store (not yet attached), got %s", rec.Status)
	}
}

func TestAttachClientWithValidTokenReturnsRuntimeID(t *testing.T) {
	rs, store, agent, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, attachToken, err := rs.Create(ctx, agent, "dev-caa2", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	rID, _, err := rs.AttachClient(ctx, sess.ID, attachToken)
	if err != nil {
		t.Fatalf("AttachClient: %v", err)
	}
	if rID == "" || rID != sess.RuntimeID {
		t.Errorf("expected runtimeID %q, got %q", sess.RuntimeID, rID)
	}

	_, _, err = rs.AttachClient(ctx, sess.ID, attachToken)
	if err == nil {
		t.Error("expected error on reused attach token")
	}
}

func TestCreateSessionLimit(t *testing.T) {
	rs, store, agent, _, ctx := setupTest(t, Config{MaxPerDevice: 1})
	defer rs.Shutdown()
	defer store.Close()

	_, _, err := rs.Create(ctx, agent, "dev-lim", nil)
	if err != nil {
		t.Fatalf("Create 1: %v", err)
	}

	_, _, err = rs.Create(ctx, agent, "dev-lim", nil)
	if err != ErrSessionLimitReached {
		t.Errorf("expected ErrSessionLimitReached, got %v", err)
	}
}

// TestCreateSessionLimitIgnoresDeadSessions verifies that failed/closing records —
// which persist in storage after crashes and across daemon restarts — do not count
// against the per-device limit. Otherwise a device that crashed N agents could never
// start another session, even with nothing running.
func TestCreateSessionLimitIgnoresDeadSessions(t *testing.T) {
	rs, store, agent, _, ctx := setupTest(t, Config{MaxPerDevice: 1})
	defer rs.Shutdown()
	defer store.Close()

	// Pre-seed a dead (failed) record for the device, as a prior crash would leave.
	if err := store.SaveSession(ctx, storage.SessionRecord{
		SessionID: "dead-1",
		RuntimeID: "rt-dead",
		DeviceID:  "dev-quota",
		AgentID:   "agent-dead",
		Status:    StatusFailed,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed failed session: %v", err)
	}

	// Despite the device being "at" the limit by raw row count, a live session is allowed.
	if _, _, err := rs.Create(ctx, agent, "dev-quota", nil); err != nil {
		t.Fatalf("expected Create to ignore the dead record, got %v", err)
	}

	// A second live session does hit the limit.
	if _, _, err := rs.Create(ctx, agent, "dev-quota", nil); err != ErrSessionLimitReached {
		t.Errorf("expected ErrSessionLimitReached for second live session, got %v", err)
	}
}

// TestCreateSessionLimitHoldsUnderConcurrency verifies concurrent connects from
// one device cannot exceed the quota: the launch runs outside the lock, so each
// in-flight Create must hold its slot.
func TestCreateSessionLimitHoldsUnderConcurrency(t *testing.T) {
	rs, store, agent, _, ctx := setupTest(t, Config{MaxPerDevice: 2})
	defer rs.Shutdown()
	defer store.Close()

	var wg sync.WaitGroup
	var created atomic.Int32
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := rs.Create(ctx, agent, "dev-race", nil)
			switch err {
			case nil:
				created.Add(1)
			case ErrSessionLimitReached:
			default:
				t.Errorf("Create: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := created.Load(); got != 2 {
		t.Fatalf("created %d sessions, want 2", got)
	}
}

// TestShutdownStopsAgentsAndKeepsRecords verifies Shutdown stops each agent as
// an intentional stop but keeps the session record for startup reconciliation.
func TestShutdownStopsAgentsAndKeepsRecords(t *testing.T) {
	rs, store, agent, _, ctx := setupTest(t, Config{})
	defer store.Close()

	sess, _, err := rs.Create(ctx, agent, "dev-shutdown", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rs.Shutdown()

	rt, err := rs.runtimeSvc.Get(sess.RuntimeID)
	if err != nil {
		t.Fatalf("Get runtime: %v", err)
	}
	if rt.Status != runtime.StatusStopped {
		t.Errorf("runtime status = %q, want %q", rt.Status, runtime.StatusStopped)
	}
	if _, err := store.GetSession(ctx, sess.ID); err != nil {
		t.Errorf("session record should survive shutdown: %v", err)
	}
}

// TestCreateAllowsSameAgentParallelSessions verifies that two sessions for the
// same (device, agent) coexist: neither is superseded, both agent processes stay
// running, and both count toward the per-device quota.
func TestCreateAllowsSameAgentParallelSessions(t *testing.T) {
	rs, store, agent, _, ctx := setupTest(t, Config{MaxPerDevice: 5})
	defer rs.Shutdown()
	defer store.Close()

	first, _, err := rs.Create(ctx, agent, "dev-par", nil)
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	second, _, err := rs.Create(ctx, agent, "dev-par", nil)
	if err != nil {
		t.Fatalf("Create second (same agent, different runtime): %v", err)
	}
	if first.ID == second.ID {
		t.Fatal("expected distinct sessions")
	}
	if first.RuntimeID == second.RuntimeID {
		t.Fatal("expected each Create to launch its own runtime")
	}

	// Both live; neither agent process was stopped by Create.
	assertProcessRunning(t, first)
	assertProcessRunning(t, second)

	records, err := store.ListSessionsByDevice(ctx, "dev-par")
	if err != nil {
		t.Fatalf("ListSessionsByDevice: %v", err)
	}
	live := 0
	for _, rec := range records {
		if rec.Status == StatusActive || rec.Status == StatusDisconnected {
			live++
		}
	}
	if live != 2 {
		t.Errorf("expected 2 live sessions, got %d", live)
	}
}

// TestStopByAgentSparesOtherDevices stops one device's sessions for an agent;
// another device running the same agent keeps its session and process.
func TestStopByAgentSparesOtherDevices(t *testing.T) {
	rs, store, agent, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	mine, _, err := rs.Create(ctx, agent, "dev-a", nil)
	if err != nil {
		t.Fatalf("Create dev-a: %v", err)
	}
	theirs, _, err := rs.Create(ctx, agent, "dev-b", nil)
	if err != nil {
		t.Fatalf("Create dev-b: %v", err)
	}

	stopped, err := rs.StopByAgent(ctx, agent.ID, "dev-a")
	if err != nil {
		t.Fatalf("StopByAgent: %v", err)
	}
	if stopped.ID != mine.RuntimeID {
		t.Errorf("stopped runtime %q, want dev-a's %q", stopped.ID, mine.RuntimeID)
	}
	assertProcessRunning(t, theirs)
	if _, err := store.GetSession(ctx, theirs.ID); err != nil {
		t.Errorf("dev-b session should survive, got %v", err)
	}
	if _, err := rs.StopByAgent(ctx, agent.ID, "dev-a"); err != ErrSessionNotFound {
		t.Errorf("second stop for dev-a = %v, want ErrSessionNotFound", err)
	}
}

// TestCreateDoesNotSupersedeDifferentAgent verifies the supersede logic is scoped
// to the same agent: a live session for a different agent on the same device is
// left intact (and so still counts toward the per-device quota).
func TestCreateDoesNotSupersedeDifferentAgent(t *testing.T) {
	rs, store, agent, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	first, _, err := rs.Create(ctx, agent, "dev-multi", nil)
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}

	if _, _, err := rs.Create(ctx, agent, "dev-multi", nil); err != nil {
		t.Fatalf("Create second (different agent): %v", err)
	}

	// The first agent's session and process are untouched.
	assertProcessRunning(t, first)
	if _, err := store.GetSession(ctx, first.ID); err != nil {
		t.Errorf("expected first session to survive, got %v", err)
	}
}

func TestResumeDeviceMismatch(t *testing.T) {
	rs, store, agent, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, agent, "dev-a", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err = rs.Resume(ctx, sess.ID, "dev-b")
	if err != ErrDeviceMismatch {
		t.Errorf("expected ErrDeviceMismatch, got %v", err)
	}
}

// TestResumeRacesAttach runs Resume against attach/detach status writes; it
// guards the sess.mu discipline under go test -race.
func TestResumeRacesAttach(t *testing.T) {
	rs, store, agent, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, token, err := rs.Create(ctx, agent, "dev-race", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			_, _ = rs.Resume(ctx, sess.ID, "dev-race")
		}
	}()
	for i := 0; i < 50; i++ {
		_, gen, err := rs.AttachClient(ctx, sess.ID, token)
		if err != nil {
			t.Fatalf("AttachClient: %v", err)
		}
		if err := rs.DetachClient(sess.ID, gen); err != nil {
			t.Fatalf("DetachClient: %v", err)
		}
		if token, err = rs.Resume(ctx, sess.ID, "dev-race"); err != nil {
			t.Fatalf("Resume: %v", err)
		}
	}
	<-done
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
	rs, store, agent, tokenSvc, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, token1, err := rs.Create(ctx, agent, "dev-cc", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A new valid attach must take over from the previous connection rather than
	// being rejected — a resilient session's old socket is usually already dead.
	_, gen1, err := rs.AttachClient(ctx, sess.ID, token1)
	if err != nil {
		t.Fatalf("first AttachClient: %v", err)
	}

	token2, _ := tokenSvc.Mint(sess.ID, "dev-cc", 5*time.Minute)
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
	rs, store, _, tokenSvc, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	token, _ := tokenSvc.Mint("nonexistent-session", "dev-x", 5*time.Minute)
	_, _, err := rs.AttachClient(ctx, "nonexistent-session", token)
	if err != ErrSessionNotFound {
		t.Errorf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestDetachClientSetsStatusDisconnected(t *testing.T) {
	rs, store, agent, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, token, err := rs.Create(ctx, agent, "dev-dar", nil)
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
	rs, store, agent, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, token, err := rs.Create(ctx, agent, "dev-dar2", nil)
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
	rs, store, agent, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, agent, "dev-close", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := rs.Close(ctx, sess.ID, "dev-close"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	waitProcessStopped(t, sess)

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
	rs, store, agent, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, agent, "dev-a", nil)
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
	rs, store, agent, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	_, _, err := rs.Create(ctx, agent, "dev-lbd-a", nil)
	if err != nil {
		t.Fatalf("Create 1: %v", err)
	}
	_, _, err = rs.Create(ctx, agent, "dev-lbd-a", nil)
	if err != nil {
		t.Fatalf("Create 2: %v", err)
	}
	_, _, err = rs.Create(ctx, agent, "dev-lbd-b", nil)
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

func TestListByDeviceReportsEachRuntimesAcpSessionIDs(t *testing.T) {
	rs, store, agent, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	loaded, _, err := rs.Create(ctx, agent, "dev-acp", nil)
	if err != nil {
		t.Fatalf("Create 1: %v", err)
	}
	if _, _, err := rs.Create(ctx, agent, "dev-acp", nil); err != nil {
		t.Fatalf("Create 2: %v", err)
	}
	for _, sid := range []string{"ses_b", "ses_a"} {
		frame := `{"jsonrpc":"2.0","id":"` + sid + `","method":"session/load","params":{"sessionId":"` + sid + `","cwd":"/proj"}}`
		if err := loaded.pump.WriteToAgent([]byte(frame)); err != nil {
			t.Fatalf("WriteToAgent: %v", err)
		}
	}

	summaries, err := rs.ListByDevice(ctx, "dev-acp")
	if err != nil {
		t.Fatalf("ListByDevice: %v", err)
	}
	for _, s := range summaries {
		want := []string{}
		if s.SessionID == loaded.ID {
			want = []string{"ses_a", "ses_b"}
		}
		if !slices.Equal(s.AcpSessionIDs, want) || s.AcpSessionIDs == nil {
			t.Errorf("session %s acpSessionIds = %#v, want %#v", s.SessionID, s.AcpSessionIDs, want)
		}
	}
}

func TestGetPump(t *testing.T) {
	rs, store, agent, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, agent, "dev-gp", nil)
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
	rs, store, agent, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, agent, "dev-gss", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	status, err := rs.GetSessionStatus(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionStatus: %v", err)
	}
	if status != StatusDisconnected {
		t.Errorf("expected disconnected (not yet attached), got %s", status)
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
	rs, store, agent, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, agent, "dev-log", nil)
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
		proc: &runtime.Process{
			Stdin:  nopWriteCloser{},
			Stdout: r,
		},
		runtimeID: "rt-drain-ws",
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		appendLog: func(string, string, string) {},
	}
	gen := pump.Attach()
	if !pump.Bind(sc, gen) {
		t.Fatal("Bind after Attach should succeed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	go pump.StdoutDrainLoop(ctx)

	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("w.Write() error = %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	cancel()
	if _, err := w.Write([]byte("second\n")); err != nil {
		t.Fatalf("w.Write() error = %v", err)
	}
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
		proc: &runtime.Process{
			Stdin:  nopWriteCloser{},
			Stdout: r,
		},
		runtimeID: "rt-drain-ws-err",
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	gen := pump.Attach()
	if !pump.Bind(sc, gen) {
		t.Fatal("Bind after Attach should succeed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	go pump.StdoutDrainLoop(ctx)

	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("w.Write() error = %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	cancel()
	_, _ = w.Write([]byte("second\n"))
	time.Sleep(50 * time.Millisecond)
	w.Close()
	r.Close()
}

// TestPumpStdoutDrainLoopOversizedFrame verifies that a single agent stdout
// line exceeding the old 1 MiB scanner cap no longer kills the drain loop or
// closes the client WebSocket. Regression for the "connection dropped while
// loading a session" bug: a large session/update / session/load response made
// bufio.Scanner exit with ErrTooLong, silently closing the WS.
func TestPumpStdoutDrainLoopOversizedFrame(t *testing.T) {
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
	client, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close(websocket.StatusNormalClosure, "")
	// The coder/websocket client-side default read limit is 32 KiB; the test
	// receives a 2 MiB frame, so raise it.
	client.SetReadLimit(-1) // unlimited: tests must not impose the server's limit

	sc := <-serverCh
	if sc == nil {
		t.Fatal("server connection not established")
	}

	r, w := io.Pipe()
	pump := &StdioPump{
		proc: &runtime.Process{
			Stdin:  nopWriteCloser{},
			Stdout: r,
		},
		runtimeID: "rt-drain-ws-big",
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		appendLog: func(string, string, string) {},
	}
	gen := pump.Attach()
	if !pump.Bind(sc, gen) {
		t.Fatal("Bind after Attach should succeed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	go pump.StdoutDrainLoop(ctx)
	defer cancel()

	// A single line well over the old 1 MiB scanner cap. The old Scanner would
	// exit with ErrTooLong here and close the WS; the streaming reader must
	// pass it through (memory-bounded) and keep draining.
	big := strings.Repeat("x", 2*1024*1024) // 2 MiB
	if _, err := w.Write([]byte(big)); err != nil {
		t.Fatalf("write big frame: %v", err)
	}
	if _, err := w.Write([]byte("\n")); err != nil {
		t.Fatalf("write newline: %v", err)
	}
	if _, err := w.Write([]byte("after\n")); err != nil {
		t.Fatalf("write after frame: %v", err)
	}
	// Closing the writer makes the reader see EOF after the buffered frames,
	// so the reads below are deterministic (no reliance on flush timing).
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}

	// The client must still receive the small frame after the oversized one,
	// proving the drain loop survived and kept forwarding. The reader strips
	// the newline delimiter, so the frame content matches the input exactly.
	_, msg, err := client.Read(context.Background())
	if err != nil {
		t.Fatalf("client read (big frame): %v", err)
	}
	if string(msg) != big {
		t.Fatalf("big frame mismatch: got %d bytes, want %d", len(msg), len(big))
	}

	// Read deadline guards against a hang if the loop died before forwarding
	// the small frame.
	readCtx, readCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readCancel()
	_, msg, err = client.Read(readCtx)
	if err != nil {
		t.Fatalf("client read (after frame): %v", err)
	}
	if string(msg) != "after" {
		t.Fatalf("after frame = %q, want %q", string(msg), "after")
	}
}

func TestPumpAttachBindDetach(t *testing.T) {
	pump := &StdioPump{
		runtimeID: "pump-test",
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	conn := newTestWSConn(t)

	// No client initially; Attach returns gen 1 with nothing to evict.
	gen1 := pump.Attach()
	if gen1 != 1 {
		t.Errorf("first Attach gen = %d, want 1", gen1)
	}
	pump.clientMu.Lock()
	if pump.client != nil {
		t.Error("expected client to be nil after Attach")
	}
	pump.clientMu.Unlock()

	// A stale Bind (from a superseded gen) must be rejected.
	if pump.Bind(newTestWSConn(t), gen1-1) {
		t.Error("Bind with stale generation should be rejected")
	}

	// A current Bind attaches.
	if !pump.Bind(conn, gen1) {
		t.Error("Bind with current generation should succeed")
	}
	pump.clientMu.Lock()
	if pump.client != conn {
		t.Error("expected client to be bound after current Bind")
	}
	pump.clientMu.Unlock()

	// A second Attach evicts the bound conn and bumps the generation.
	gen2 := pump.Attach()
	if gen2 != gen1+1 {
		t.Errorf("second Attach gen = %d, want %d", gen2, gen1+1)
	}
	pump.clientMu.Lock()
	if pump.client != nil {
		t.Error("expected client to be nil after second Attach")
	}
	pump.clientMu.Unlock()

	// A stale Detach must not clear a newer client.
	if pump.Detach(gen1) {
		t.Error("stale Detach should be a no-op")
	}

	// The current Detach clears the client and reports success.
	if !pump.Detach(gen2) {
		t.Error("current Detach should succeed")
	}
	pump.clientMu.Lock()
	if pump.client != nil {
		t.Error("expected client to be nil after current Detach")
	}
	pump.clientMu.Unlock()
}

// newTestWSConn returns a server-side WebSocket connection backed by a real
// local upgrade (a zero-value *websocket.Conn panics in CloseNow). The client
// side is closed in cleanup.
func newTestWSConn(t *testing.T) *websocket.Conn {
	t.Helper()
	serverCh := make(chan *websocket.Conn, 1)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		serverCh <- c
	}))
	t.Cleanup(s.Close)

	client, _, err := websocket.Dial(context.Background(), "ws://"+s.Listener.Addr().String()+"/", nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { client.Close(websocket.StatusNormalClosure, "") })

	sc := <-serverCh
	if sc == nil {
		t.Fatal("server connection not established")
	}
	t.Cleanup(func() { _ = sc.CloseNow() })
	return sc
}

func TestPumpSupportsClose(t *testing.T) {
	pump := &StdioPump{
		runtimeID: "pump-sc",
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	if pump.SupportsClose() {
		t.Error("expected SupportsClose to be false initially")
	}

	feedInitialize(pump, `{"result":{"protocolVersion":1,"agentCapabilities":{"sessionCapabilities":{"close":{}}}}}`)

	if !pump.SupportsClose() {
		t.Error("expected SupportsClose to be true after observing initialize")
	}
}

func TestPumpWriteToAgent(t *testing.T) {
	pump := &StdioPump{
		proc: &runtime.Process{
			Stdin:  nopWriteCloser{},
			Stdout: io.NopCloser(strings.NewReader("")),
		},
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
	rs, store, _, tokenSvc, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	token, _ := tokenSvc.Mint("ac-fallback-notfound", "dev-x", 5*time.Minute)
	_, _, err := rs.AttachClient(ctx, "ac-fallback-notfound", token)
	if err != ErrSessionNotFound {
		t.Errorf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestCloseWithSupportsClose(t *testing.T) {
	rs, store, agent, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, agent, "dev-close-sc", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	markCloseCapable(sess.pump)

	if err := rs.Close(ctx, sess.ID, "dev-close-sc"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	waitProcessStopped(t, sess)
}

// TestCloseWithSupportsCloseRoutesThroughPump verifies the ACP session/close
// frame is written via the pump (WriteSessionClose) — the only stdin writer —
// rather than reaching the agent's stdin directly, so the teardown handshake is
// frame-logged like every other inbound frame.
func TestCloseWithSupportsCloseRoutesThroughPump(t *testing.T) {
	rs, store, _, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	// Create a pump whose process pipes are set before any drain-loop goroutine
	// starts, so the recording stdin is used for the whole session (mutating the
	// pump's proc after Create would race the already-running StdoutDrainLoop).
	var mu sync.Mutex
	var writes []string
	pump := &StdioPump{
		proc: &runtime.Process{
			Stdin:  recordingWriteCloser{record: func(b []byte) { mu.Lock(); writes = append(writes, string(b)); mu.Unlock() }},
			Stdout: io.NopCloser(strings.NewReader("")),
		},
		runtimeID:    "rt-close-routed",
		sessionID:    "sess-close-routed",
		agentID:      "agent-close-routed",
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		appendLog:    rs.runtimeSvc.AppendLog,
		loadRecovery: newLoadRecovery(rs.logger),
	}
	markCloseCapable(pump)
	// The pump recorded the agent-side ACP session id from session/new; the
	// gateway's session/close must address the agent with it, not the
	// gateway's own resilient id.
	pump.acp.mu.Lock()
	pump.acp.sessionID = "ses_agent_1"
	pump.acp.mu.Unlock()

	sess := &Session{
		ID:        "sess-close-routed",
		RuntimeID: "rt-close-routed",
		DeviceID:  "dev-close-routed",
		AgentID:   "agent-close-routed",
		Status:    StatusActive,
		CreatedAt: time.Now().UTC(),
		pump:      pump,
	}
	rs.mu.Lock()
	rs.sessions[sess.ID] = sess
	rs.mu.Unlock()

	if err := rs.Close(ctx, sess.ID, "dev-close-routed"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(writes) != 1 {
		t.Fatalf("expected exactly one stdin write (session/close), got %d", len(writes))
	}
	var frame struct {
		Method string `json:"method"`
		Params struct {
			SessionID string `json:"sessionId"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(writes[0]), &frame); err != nil {
		t.Fatalf("Unmarshal(session/close): %v", err)
	}
	if frame.Method != "session/close" {
		t.Errorf("stdin write method = %q, want %q", frame.Method, "session/close")
	}
	if frame.Params.SessionID != "ses_agent_1" {
		t.Errorf("session/close params.sessionId = %q, want the agent-side ACP id %q (got the gateway id instead?)", frame.Params.SessionID, "ses_agent_1")
	}
}

// recordingWriteCloser is an io.WriteCloser that records written bytes.
type recordingWriteCloser struct {
	record func([]byte)
}

func (w recordingWriteCloser) Write(p []byte) (int, error) {
	w.record(p)
	return len(p), nil
}

func (w recordingWriteCloser) Close() error { return nil }

// TestWriteSessionCloseSkipsClientObservation verifies that WriteSessionClose does
// not contaminate the pump's client-derived caches: the gateway's own
// session/close frame carries a params.sessionId that must never be captured as
// the ACP session id (it is the resilient, non-ACP id).
func TestWriteSessionCloseSkipsClientObservation(t *testing.T) {
	pump := &StdioPump{
		proc: &runtime.Process{
			Stdin:  nopWriteCloser{},
			Stdout: io.NopCloser(strings.NewReader("")),
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	if err := pump.WriteSessionClose([]byte(`{"jsonrpc":"2.0","method":"session/close","params":{"sessionId":"gw-sess-123"}}`)); err != nil {
		t.Fatalf("WriteSessionClose: %v", err)
	}
	if got := pump.AcpSessionID(); got != "" {
		t.Errorf("AcpSessionID = %q after gateway session/close, want empty (client-derived only)", got)
	}
	if got := pump.AcpCwd(); got != "" {
		t.Errorf("AcpCwd = %q after gateway session/close, want empty (client-derived only)", got)
	}

	// Sanity: WriteToAgent on the same frame WOULD capture it.
	if err := pump.WriteToAgent([]byte(`{"jsonrpc":"2.0","method":"session/prompt","params":{"sessionId":"ses_client_1"}}`)); err != nil {
		t.Fatalf("WriteToAgent: %v", err)
	}
	if got := pump.AcpSessionID(); got != "ses_client_1" {
		t.Errorf("AcpSessionID = %q after WriteToAgent, want %q", got, "ses_client_1")
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

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tokenSvc := NewTokenService(logger)
	rs := NewRuntimeSession(logger, store, runtime.NewSupervisor(logger, t.TempDir(), nil, nil), tokenSvc, Config{})

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
		proc: &runtime.Process{
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
	_, _ = w.Write([]byte("second\n"))
	time.Sleep(50 * time.Millisecond)

	w.Close()
	r.Close()
	time.Sleep(10 * time.Millisecond)
}

func TestPumpStdoutDrainLoopWithFramesAndAppendLog(t *testing.T) {
	r, w := io.Pipe()

	var loggedMu sync.Mutex
	var logged bool
	pump := &StdioPump{
		proc: &runtime.Process{
			Stdin:  nopWriteCloser{},
			Stdout: r,
		},
		runtimeID: "rt-drain-log",
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		appendLog: func(rid, stream, msg string) {
			loggedMu.Lock()
			logged = true
			loggedMu.Unlock()
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	go pump.StdoutDrainLoop(ctx)

	_, _ = w.Write([]byte("line1\n"))
	time.Sleep(50 * time.Millisecond)

	cancel()
	_, _ = w.Write([]byte("line2\n"))
	time.Sleep(50 * time.Millisecond)

	w.Close()
	r.Close()
	time.Sleep(10 * time.Millisecond)

	loggedMu.Lock()
	gotLogged := logged
	loggedMu.Unlock()
	if !gotLogged {
		t.Error("expected appendLog to be called")
	}
}

func TestPumpInitializeCacheAlreadySet(t *testing.T) {
	pump := &StdioPump{}
	markCloseCapable(pump)
	feedInitialize(pump, `{"result":{"agentCapabilities":{"sessionCapabilities":{"close":false}}}}`)
	if !pump.SupportsClose() {
		t.Error("expected SupportsClose still true")
	}
}

// TestPumpObservesSessionID verifies the ACP session id is captured from a
// session/new response, ignored on unrelated frames, and not overwritten once set.
func TestPumpObservesSessionID(t *testing.T) {
	pump := &StdioPump{}

	// Unrelated frames don't set it.
	feedSessionID(t, pump, `{"result":{"protocolVersion":1}}`)
	feedSessionID(t, pump, `{"method":"session/update","params":{}}`)
	if got := pump.AcpSessionID(); got != "" {
		t.Fatalf("AcpSessionID = %q, want empty before session/new", got)
	}

	// The session/new response carries result.sessionId — captured here.
	feedSessionID(t, pump, `{"jsonrpc":"2.0","id":1,"result":{"sessionId":"ses_abc123"}}`)
	if got := pump.AcpSessionID(); got != "ses_abc123" {
		t.Fatalf("AcpSessionID = %q, want %q", got, "ses_abc123")
	}

	// A later session/new (e.g. reconnect) does not clobber the captured id.
	feedSessionID(t, pump, `{"result":{"sessionId":"ses_other"}}`)
	if got := pump.AcpSessionID(); got != "ses_abc123" {
		t.Fatalf("AcpSessionID = %q, want it to stay %q", got, "ses_abc123")
	}
}

// TestPumpObservesInboundSessionID verifies the ACP session id is captured from a
// client→agent frame's params.sessionId — the path that covers resilient
// reconnects, where the agent never re-emits a session/new response.
func TestPumpObservesInboundSessionID(t *testing.T) {
	pump := &StdioPump{}

	// Frames without params.sessionId don't set it (e.g. initialize).
	pump.acp.observeInbound(parseClientFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)))
	if got := pump.AcpSessionID(); got != "" {
		t.Fatalf("AcpSessionID = %q, want empty before a frame carries sessionId", got)
	}

	// A session/prompt (or session/load) carries params.sessionId — captured here.
	pump.acp.observeInbound(parseClientFrame([]byte(`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"ses_inbound","prompt":[]}}`)))
	if got := pump.AcpSessionID(); got != "ses_inbound" {
		t.Fatalf("AcpSessionID = %q, want %q", got, "ses_inbound")
	}

	// A later inbound frame does not clobber the captured id.
	pump.acp.observeInbound(parseClientFrame([]byte(`{"method":"session/cancel","params":{"sessionId":"ses_other"}}`)))
	if got := pump.AcpSessionID(); got != "ses_inbound" {
		t.Fatalf("AcpSessionID = %q, want it to stay %q", got, "ses_inbound")
	}
}

// TestPumpTurnCompletePushUsesACPSessionID verifies the drain loop records the
// ACP session id from session/new and then stamps it onto a turn-complete push.
func TestPumpTurnCompletePushUsesACPSessionID(t *testing.T) {
	r, w := io.Pipe()

	pushCh := make(chan push.Notification, 4)
	pump := &StdioPump{
		proc: &runtime.Process{
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
	_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"ses_live"}}` + "\n"))
	_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"stopReason":"end_turn"}}` + "\n"))

	select {
	case n := <-pushCh:
		if n.Category != push.CategoryTurnComplete {
			t.Fatalf("category = %q, want %q", n.Category, push.CategoryTurnComplete)
		}
		if n.SessionID != "ses_live" {
			t.Fatalf("push SessionID = %q, want the observed ACP id %q", n.SessionID, "ses_live")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected a turn-complete push")
	}

	w.Close()
	r.Close()
}

// TestPumpSwallowedFailureDetection verifies the swallowed-LLM-failure
// heuristic: a prompt whose result is a clean success with stopReason=end_turn,
// zero usage, and no streamed content is rewritten to a JSON-RPC error
// response; a turn with content, usage, or a different stop reason passes
// through unchanged.
func TestPumpSwallowedFailureDetection(t *testing.T) {
	events := make(chan PushEvent, 4)
	pump := &StdioPump{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		onPushNotification: func(e PushEvent) {
			events <- e
		},
	}
	pump.acp.observeInbound(parseClientFrame([]byte(`{"jsonrpc":"2.0","id":9,"method":"session/prompt","params":{"sessionId":"s","prompt":[]}}`)))

	// Swallowed failure: end_turn, zero usage, no content → error frame.
	out, handled := turnReplacement(t, pump, `{"jsonrpc":"2.0","id":9,"result":{"stopReason":"end_turn"}}`)
	if !handled {
		t.Fatal("expected swallowed failure to be rewritten")
	}
	var resp struct {
		ID    json.Number `json:"id"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("replacement is not valid JSON: %v", err)
	}
	if resp.ID.String() != "9" {
		t.Fatalf("replacement id = %s, want 9", resp.ID)
	}
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Fatalf("replacement must carry error code -32000, got %s", out)
	}

	// Healthy turn: content streamed → passthrough.
	pump.acp.observeInbound(parseClientFrame([]byte(`{"jsonrpc":"2.0","id":10,"method":"session/prompt","params":{}}`)))
	turnReplacement(t, pump, `{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk"}}}`)
	if _, handled := turnReplacement(t, pump, `{"jsonrpc":"2.0","id":10,"result":{"stopReason":"end_turn"}}`); handled {
		t.Fatal("healthy turn with content must not be rewritten")
	}

	// Non-zero usage → passthrough.
	pump.acp.observeInbound(parseClientFrame([]byte(`{"jsonrpc":"2.0","id":11,"method":"session/prompt","params":{}}`)))
	if _, handled := turnReplacement(t, pump, `{"jsonrpc":"2.0","id":11,"result":{"stopReason":"end_turn","usage":{"inputTokens":30,"outputTokens":12}}}`); handled {
		t.Fatal("turn with usage must not be rewritten")
	}

	// Non-end_turn stop reason (e.g. refusal) → passthrough.
	pump.acp.observeInbound(parseClientFrame([]byte(`{"jsonrpc":"2.0","id":12,"method":"session/prompt","params":{}}`)))
	if _, handled := turnReplacement(t, pump, `{"jsonrpc":"2.0","id":12,"result":{"stopReason":"refusal"}}`); handled {
		t.Fatal("refusal turn must not be rewritten")
	}

	// Result without a tracked prompt (e.g. initialize) → passthrough.
	pump.acp.observeInbound(parseClientFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)))
	if _, handled := turnReplacement(t, pump, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}`); handled {
		t.Fatal("initialize result must not be rewritten")
	}
}

func TestCreateStoreError(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "create_err_test.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	store.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tokenSvc := NewTokenService(logger)
	rs := NewRuntimeSession(logger, store, runtime.NewSupervisor(logger, t.TempDir(), nil, nil), tokenSvc, Config{})
	defer rs.Shutdown()
	defer store.Close()

	_, _, err = rs.Create(context.Background(), mockAgent(t), "dev-err", nil)
	if err == nil {
		t.Error("expected error from closed store")
	}
}

func TestResumeInMemoryStatusClosing(t *testing.T) {
	rs, store, agent, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, agent, "dev-rs-cl", nil)
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
	rs, store, _, tokenSvc, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	token, _ := tokenSvc.Mint("session-a", "dev-x", 5*time.Minute)
	_, _, err := rs.AttachClient(ctx, "session-b", token)
	if err != ErrAttachTokenInvalid {
		t.Errorf("expected ErrAttachTokenInvalid, got %v", err)
	}
}

func TestAttachClientInMemoryDeviceMismatch(t *testing.T) {
	rs, store, agent, tokenSvc, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, agent, "dev-ac-dmim", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	token, _ := tokenSvc.Mint(sess.ID, "dev-wrong", 5*time.Minute)
	_, _, err = rs.AttachClient(ctx, sess.ID, token)
	if err != ErrDeviceMismatch {
		t.Errorf("expected ErrDeviceMismatch, got %v", err)
	}
}

func TestAttachClientInMemoryStatusFailed(t *testing.T) {
	rs, store, agent, tokenSvc, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, agent, "dev-ac-fail", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	sess.mu.Lock()
	sess.Status = StatusFailed
	sess.mu.Unlock()

	token, _ := tokenSvc.Mint(sess.ID, "dev-ac-fail", 5*time.Minute)
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
		tokenSvc: NewTokenService(slog.New(slog.NewTextHandler(io.Discard, nil))),
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
	rs, store, agent, _, ctx := setupTest(t, Config{ReaperInterval: time.Millisecond, MaxDisconnected: time.Millisecond})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, agent, "dev-reap-tick", nil)
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
	rs, store, agent, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, tok, err := rs.Create(ctx, agent, "dev-reap-skip", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, _, err := rs.AttachClient(ctx, sess.ID, tok); err != nil {
		t.Fatalf("AttachClient: %v", err)
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
	rs, store, agent, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, agent, "dev-reap", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := rs.DetachClient(sess.ID, 0); err != nil {
		t.Fatalf("DetachClient: %v", err)
	}

	// Backdate the disconnect so reapExpired(0) is deterministic: Detach sets
	// DisconnectedAt=now and reap compares with time.Now(), which can land on
	// the same tick (Sub==0, not > 0) and flake.
	past := time.Now().UTC().Add(-time.Hour)
	sess.DisconnectedAt = &past

	rs.reapExpired(0)

	_, err = rs.GetSessionStatus(sess.ID)
	if err != ErrSessionNotFound {
		t.Errorf("expected session to be reaped (ErrSessionNotFound), got %v", err)
	}

	waitProcessStopped(t, sess)

	_, err = store.GetSession(ctx, sess.ID)
	if err == nil {
		t.Error("expected session to be deleted from store")
	}
}

func TestReapExpiredSkipsStreamingAgent(t *testing.T) {
	rs, store, agent, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, agent, "dev-stream", nil)
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
	rs, store, agent, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, agent, "dev-idle", nil)
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

	// Same determinism fix as TestReapExpired: backdate the disconnect.
	pastIdle := time.Now().UTC().Add(-time.Hour)
	sess.DisconnectedAt = &pastIdle

	rs.reapExpired(0)

	_, err = rs.GetSessionStatus(sess.ID)
	if err != ErrSessionNotFound {
		t.Errorf("expected session to be reaped (ErrSessionNotFound), got %v", err)
	}

	waitProcessStopped(t, sess)
}

func TestCreateEmptyDeviceID(t *testing.T) {
	rs, store, agent, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, token, err := rs.Create(ctx, agent, "", nil)
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
	rs, store, agent, tokenSvc, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, token, err := rs.Create(ctx, agent, "dev-att-disc", nil)
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

	newToken, _ := tokenSvc.Mint(sess.ID, "dev-att-disc", 5*time.Minute)
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

	initReq := parseClientFrame([]byte(`{"jsonrpc":"2.0","id":"1","method":"initialize","params":{}}`))

	// Before the agent responds there is nothing to replay: the first
	// initialize must reach the agent.
	if p.replayInitialize(initReq) {
		t.Fatal("first initialize must not be replayed (no cached response yet)")
	}

	// Agent's initialize response is cached (identified by result.protocolVersion).
	feedInitialize(p, `{"jsonrpc":"2.0","id":"1","result":{"protocolVersion":1,"agentCapabilities":{"sessionCapabilities":{"close":{}}}}}`)
	if !p.initResponseCached() {
		t.Fatal("expected initialize response to be cached")
	}
	if !p.SupportsClose() {
		t.Error("expected supportsClose to be detected from the cached response")
	}

	// Non-initialize frames are never intercepted.
	if p.replayInitialize(parseClientFrame([]byte(`{"jsonrpc":"2.0","id":"7","method":"session/load","params":{}}`))) {
		t.Error("non-initialize frame must not be replayed")
	}

	// A reconnecting client's duplicate initialize is intercepted (handled even
	// with no attached client conn).
	if !p.replayInitialize(parseClientFrame([]byte(`{"jsonrpc":"2.0","id":"42","method":"initialize","params":{}}`))) {
		t.Error("duplicate initialize must be replayed from cache")
	}
}

func TestPumpInitializeIgnoresNonInitializeResults(t *testing.T) {
	p := &StdioPump{logger: slog.Default()}
	// A prompt response has a result object but no protocolVersion; it must not
	// be mistaken for the initialize response.
	feedInitialize(p, `{"jsonrpc":"2.0","id":"5","result":{"stopReason":"end_turn"}}`)
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

func TestWorkingDirByRuntime(t *testing.T) {
	rs, store, _, _, _ := setupTest(t, Config{MaxDisconnected: 5 * time.Minute})
	defer rs.Shutdown()
	defer store.Close()

	// No sessions yet -> ErrSessionNotFound.
	if _, err := rs.WorkingDir("rt-1", ""); err != ErrSessionNotFound {
		t.Fatalf("WorkingDir(no session) error = %v, want ErrSessionNotFound", err)
	}

	// A session with no sniffed cwd -> ErrCwdUnknown.
	pump1 := newRecoveryPump()
	rs.mu.Lock()
	rs.sessions["sess-1"] = &Session{ID: "sess-1", RuntimeID: "rt-1", pump: pump1}
	rs.mu.Unlock()
	if _, err := rs.WorkingDir("rt-1", ""); err != ErrCwdUnknown {
		t.Fatalf("WorkingDir(no cwd) error = %v, want ErrCwdUnknown", err)
	}

	// After the pump captures a cwd, WorkingDir returns it.
	pump1.acp.observeInbound(parseClientFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/proj"}}`)))
	got, err := rs.WorkingDir("rt-1", "")
	if err != nil {
		t.Fatalf("WorkingDir(with cwd) error = %v", err)
	}
	if got != "/proj" {
		t.Fatalf("WorkingDir() = %q, want /proj", got)
	}

	// Two ACP sessions on one agent: each resolves to its own project, while
	// the unscoped lookup still follows the most recently opened one.
	pump1.acp.observeInbound(parseClientFrame([]byte(`{"jsonrpc":"2.0","id":2,"method":"session/load","params":{"sessionId":"acp-x","cwd":"/x"}}`)))
	pump1.acp.observeInbound(parseClientFrame([]byte(`{"jsonrpc":"2.0","id":3,"method":"session/load","params":{"sessionId":"acp-y","cwd":"/y"}}`)))
	if got, err := rs.WorkingDir("rt-1", "acp-x"); err != nil || got != "/x" {
		t.Fatalf("WorkingDir(acp-x) = (%q, %v), want /x", got, err)
	}
	if got, err := rs.WorkingDir("rt-1", ""); err != nil || got != "/y" {
		t.Fatalf("WorkingDir(unscoped) = (%q, %v), want /y", got, err)
	}
	if _, err := rs.WorkingDir("rt-1", "acp-unknown"); err != ErrCwdUnknown {
		t.Fatalf("WorkingDir(unknown acp session) error = %v, want ErrCwdUnknown", err)
	}
}

// TestPumpAttachBindRaceFencesEvictedConn locks in the connection-takeover
// invariant: a concurrent Attach + stale Bind must never leave the pump with
// the evicted connection bound. Regression guard for the resurrection window
// where the old generation check (sess.mu) and the client set (clientMu) were
// different locks — a stale Bind could land after a newer Attach evicted the
// conn, and the pump would then write to a closed, superseded socket.
func TestPumpAttachBindRaceFencesEvictedConn(t *testing.T) {
	pump := &StdioPump{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	genA := pump.Attach()
	connA := newTestWSConn(t)
	if !pump.Bind(connA, genA) {
		t.Fatal("initial Bind should succeed")
	}

	// A takeover supersedes connA (genB > genA, connA evicted). From here on
	// connA's generation is permanently stale.
	genB := pump.Attach()
	if genB <= genA {
		t.Fatalf("takeover Attach gen = %d, want > %d", genB, genA)
	}

	// Hammer stale Binds of the evicted conn against further takeovers. The
	// single-lock fence means a stale Bind can never win the race and rebind
	// the evicted connection after a takeover cleared it — the resurrection
	// window that existed when the gen check (sess.mu) and the client set
	// (clientMu) were different locks.
	const iters = 200
	var wg sync.WaitGroup
	var staleAccepted atomic.Int64
	for i := 0; i < iters; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if pump.Bind(connA, genA) {
				staleAccepted.Add(1)
			}
		}()
		go func() {
			defer wg.Done()
			pump.Attach()
		}()
	}
	wg.Wait()

	if n := staleAccepted.Load(); n != 0 {
		t.Fatalf("stale Bind accepted %d times; the generation fence is broken", n)
	}

	// The final Attach's generation owns the pump; the pump must not be bound
	// to the long-evicted connA.
	pump.clientMu.Lock()
	if pump.client == connA {
		pump.clientMu.Unlock()
		t.Fatal("pump still holds the evicted connection after takeover race")
	}
	pump.clientMu.Unlock()
}

// TestCreateNeverAttachedIsReaped verifies a session whose client never attaches
// (the app died before opening the WebSocket) is born disconnected, so the
// reaper reclaims its process and quota slot after MaxDisconnected.
func TestCreateNeverAttachedIsReaped(t *testing.T) {
	rs, store, agent, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, agent, "dev-never", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rec, err := store.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if rec.Status != StatusDisconnected || rec.DisconnectedSince == nil {
		t.Fatalf("stored record = status %q, DisconnectedSince %v; want disconnected with a timestamp", rec.Status, rec.DisconnectedSince)
	}

	// Within the grace period the session survives.
	rs.reapExpired(time.Hour)
	if _, err := rs.GetSessionStatus(sess.ID); err != nil {
		t.Fatalf("session reaped before MaxDisconnected: %v", err)
	}

	past := time.Now().UTC().Add(-time.Hour)
	sess.mu.Lock()
	sess.DisconnectedAt = &past
	sess.mu.Unlock()
	rs.reapExpired(time.Minute)

	if _, err := rs.GetSessionStatus(sess.ID); err != ErrSessionNotFound {
		t.Errorf("expected never-attached session to be reaped, got %v", err)
	}
	waitProcessStopped(t, sess)
	if _, err := store.GetSession(ctx, sess.ID); err == nil {
		t.Error("expected session to be deleted from store")
	}
}

// TestProcessExitIntentionalReclaimsWithoutPush verifies that an intentional
// stop — the session is marked closing before its process is terminated —
// reclaims the session without sending an "Agent Crashed" push.
func TestProcessExitIntentionalReclaimsWithoutPush(t *testing.T) {
	rs, agent, pushSvc := setupPushTest(t)
	ctx := context.Background()

	sess, _, err := rs.Create(ctx, agent, "dev-stop", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := rs.Close(ctx, sess.ID, "dev-stop"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitProcessStopped(t, sess)

	if _, err := rs.GetSessionStatus(sess.ID); err != ErrSessionNotFound {
		t.Fatalf("expected session reclaimed (ErrSessionNotFound), got %v", err)
	}
	if _, _, err := rs.Create(ctx, agent, "dev-stop", nil); err != nil {
		t.Fatalf("expected a fresh Create to succeed, got %v", err)
	}
	// The push, if erroneously sent, is dispatched asynchronously.
	time.Sleep(150 * time.Millisecond)
	if c := pushSvc.count(); c != 0 {
		t.Fatalf("expected no push for an intentional stop, got %d", c)
	}
}

// blockedWriteCloser models an agent that stopped reading stdin: Write blocks
// until released (as a full pipe buffer would, until the process is killed).
type blockedWriteCloser struct{ release chan struct{} }

func (b blockedWriteCloser) Write(p []byte) (int, error) { <-b.release; return len(p), nil }
func (b blockedWriteCloser) Close() error                { return nil }

// TestCloseDoesNotHoldRegistryLockDuringBlockedWrite verifies a session/close
// write stuck on an unresponsive agent does not freeze the registry shared by
// every other session, and that Close still completes once the wait is bounded.
func TestCloseDoesNotHoldRegistryLockDuringBlockedWrite(t *testing.T) {
	rs, store, agent, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	release := make(chan struct{})
	defer close(release)
	pump := &StdioPump{
		proc: &runtime.Process{
			Stdin:  blockedWriteCloser{release: release},
			Stdout: io.NopCloser(strings.NewReader("")),
		},
		runtimeID:    "rt-stuck",
		sessionID:    "sess-stuck",
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		appendLog:    rs.runtimeSvc.AppendLog,
		loadRecovery: newLoadRecovery(rs.logger),
	}
	markCloseCapable(pump)
	stuck := &Session{
		ID: "sess-stuck", RuntimeID: "rt-stuck", DeviceID: "dev-stuck", AgentID: "agent-stuck",
		Status: StatusActive, CreatedAt: time.Now().UTC(), pump: pump,
	}
	rs.mu.Lock()
	rs.sessions[stuck.ID] = stuck
	rs.mu.Unlock()

	other, _, err := rs.Create(ctx, agent, "dev-other", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	closed := make(chan error, 1)
	go func() { closed <- rs.Close(ctx, stuck.ID, "dev-stuck") }()

	// Wait until the stuck session has left the registry (Close is past its
	// locked section and blocked in the write), then use the registry.
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := rs.GetPump(stuck.ID); err == ErrSessionNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Close never removed the session from the registry")
		}
		time.Sleep(5 * time.Millisecond)
	}
	done := make(chan struct{})
	go func() {
		_, _ = rs.GetSessionStatus(other.ID)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(closeWriteTimeout / 2):
		t.Fatal("registry lock held while session/close write is blocked")
	}

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(closeWriteTimeout + 3*time.Second):
		t.Fatal("Close did not return after the bounded wait")
	}
}

// A disconnected session whose agent waits on an unanswered request outlives
// MaxDisconnected; once answered it is reaped as usual.
func TestReapSparesSessionAwaitingClient(t *testing.T) {
	rs, store, agent, _, ctx := setupTest(t, Config{})
	defer rs.Shutdown()
	defer store.Close()

	sess, _, err := rs.Create(ctx, agent, "dev-wait", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sess.pump.handleStdoutLine(permissionRequest)
	past := time.Now().UTC().Add(-2 * time.Hour)
	sess.DisconnectedAt = &past
	sess.pump.lastStdoutAt = past

	rs.reapExpired(time.Hour)
	if _, err := rs.GetSessionStatus(sess.ID); err != nil {
		t.Fatalf("session awaiting the client was reaped: %v", err)
	}

	sess.pump.reqIDs.outbound(1, parseClientFrame([]byte(`{"jsonrpc":"2.0","id":7,"result":{"outcome":{"outcome":"cancelled"}}}`)))
	rs.reapExpired(time.Hour)
	if _, err := rs.GetSessionStatus(sess.ID); err != ErrSessionNotFound {
		t.Fatalf("answered session not reaped: %v", err)
	}
}

func TestRunningTurnsIncludesAllDevicesAndOnlyTheNamedAgent(t *testing.T) {
	rs := &RuntimeSession{sessions: make(map[string]*Session)}
	for _, entry := range []struct{ device, agent, chat string }{
		{"phone", "agent", "chat-a"},
		{"tablet", "agent", "chat-b"},
		{"phone", "other-agent", "chat-c"},
		{"laptop", "agent", "chat-a"},
	} {
		p := newRecoveryPump()
		p.prepareOutbound(parseClientFrame([]byte(fmt.Sprintf(`{"id":1,"method":"session/prompt","params":{"sessionId":%q}}`, entry.chat))))
		rs.sessions[entry.device+"/"+entry.chat] = &Session{DeviceID: entry.device, AgentID: entry.agent, pump: p}
	}
	rs.sessions["idle"] = &Session{AgentID: "agent", pump: newRecoveryPump()}
	rs.sessions["no-pump"] = &Session{AgentID: "agent"}
	got := rs.RunningTurns("agent")
	if !slices.Equal(got, []string{"chat-a", "chat-b"}) {
		t.Fatalf("running turns = %v", got)
	}
	if got := rs.RunningTurns("missing"); len(got) != 0 {
		t.Fatalf("unknown agent = %v", got)
	}
}

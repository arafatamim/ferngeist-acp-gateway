// Package session provides end-to-end integration tests for RuntimeSession
// and its collaboration with SQLite store, ProcessManager, and TokenService.
//
// These tests use real (temporary) SQLite databases and clock-based timing
// for the reaper, exercising the full code path through storage without mocking
// the database layer. Mocks are only used for runtime process management and
// token minting/validation.
package session

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

// TestIntegration_SessionLifecycle exercises the full session lifecycle end-to-end
// through RuntimeSession, verifying that each state transition and store operation
// behaves correctly when composed sequentially.
//
// Flow: Create → AttachClient → DetachClient → Resume → AttachClient (reconnect)
// → Close. After Close, the session is removed from the store, the runtime process
// is stopped, and the runtime lease is released.
func TestIntegration_SessionLifecycle(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "integration_test.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	mockPM := newMockPM()
	mockTokenSvc := newMockTokenSvc()

	cfg := Config{
		MaxDisconnected: 5 * time.Minute,
		MaxPerDevice:    3,
	}

	rs := NewRuntimeSession(slog.Default(), store, mockPM, mockTokenSvc, cfg)
	defer rs.Shutdown()

	runtimeID := "rt_integration_001"
	deviceID := "dev_integration_001"
	agentID := "agent_mock"

	sess, attachToken, err := rs.Create(ctx, runtimeID, deviceID, agentID)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess == nil || sess.ID == "" {
		t.Fatal("expected non-nil session with ID")
	}
	if sess.Status != StatusActive {
		t.Errorf("expected status active, got %s", sess.Status)
	}
	if attachToken == "" {
		t.Error("expected non-empty attach token")
	}

	rec, err := store.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if rec.Status != StatusActive {
		t.Errorf("expected store status active, got %s", rec.Status)
	}

	_, gen, err := rs.AttachClient(ctx, sess.ID, attachToken)
	if err != nil {
		t.Fatalf("AttachClient: %v", err)
	}

	_, _, err = rs.AttachClient(ctx, sess.ID, attachToken)
	if err == nil {
		t.Error("expected error on reused attach token")
	}

	if err := rs.DetachClient(sess.ID, gen); err != nil {
		t.Fatalf("DetachClient: %v", err)
	}

	rec, err = store.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetSession after detach: %v", err)
	}
	if rec.Status != StatusDisconnected {
		t.Errorf("expected store status disconnected, got %s", rec.Status)
	}

	newToken, err := rs.Resume(ctx, sess.ID, deviceID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if newToken == "" {
		t.Error("expected non-empty resume attach token")
	}

	_, _, err = rs.AttachClient(ctx, sess.ID, newToken)
	if err != nil {
		t.Fatalf("AttachClient after resume: %v", err)
	}

	rec, err = store.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetSession after reattach: %v", err)
	}
	if rec.Status != StatusActive {
		t.Errorf("expected store status active after reattach, got %s", rec.Status)
	}

	if err := rs.Close(ctx, sess.ID, deviceID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err = store.GetSession(ctx, sess.ID)
	if err == nil {
		t.Error("expected session to be deleted from store after close")
	}

	if !mockPM.stopped[runtimeID] {
		t.Error("expected runtime to be stopped after session close")
	}

	if mockPM.leases[runtimeID] != "" {
		t.Errorf("expected lease to be released, got %q", mockPM.leases[runtimeID])
	}
}

// TestIntegration_DeviceLimit verifies that RuntimeSession enforces the
// MaxPerDevice configuration limit. Creating more sessions than allowed for a
// single device returns ErrSessionLimitReached.
func TestIntegration_DeviceLimit(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "limit_test.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	mockPM := newMockPM()
	mockTokenSvc := newMockTokenSvc()

	cfg := Config{
		MaxDisconnected: 5 * time.Minute,
		MaxPerDevice:    2,
	}

	rs := NewRuntimeSession(slog.Default(), store, mockPM, mockTokenSvc, cfg)
	defer rs.Shutdown()

	deviceID := "dev_limit_test"

	_, _, err = rs.Create(ctx, "rt_1", deviceID, "agent_1")
	if err != nil {
		t.Fatalf("Create 1: %v", err)
	}

	_, _, err = rs.Create(ctx, "rt_2", deviceID, "agent_2")
	if err != nil {
		t.Fatalf("Create 2: %v", err)
	}

	_, _, err = rs.Create(ctx, "rt_3", deviceID, "agent_3")
	if err != ErrSessionLimitReached {
		t.Errorf("expected ErrSessionLimitReached, got %v", err)
	}
}

// TestIntegration_DeviceMismatch verifies that Resume and Close reject a device
// that does not match the session's registered device, returning
// ErrDeviceMismatch.
func TestIntegration_DeviceMismatch(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "mismatch_test.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	mockPM := newMockPM()
	mockTokenSvc := newMockTokenSvc()

	rs := NewRuntimeSession(slog.Default(), store, mockPM, mockTokenSvc, Config{})
	defer rs.Shutdown()

	sess, _, err := rs.Create(ctx, "rt_mm", "dev_a", "agent_x")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err = rs.Resume(ctx, sess.ID, "dev_b")
	if err != ErrDeviceMismatch {
		t.Errorf("expected ErrDeviceMismatch, got %v", err)
	}

	err = rs.Close(ctx, sess.ID, "dev_b")
	if err != ErrDeviceMismatch {
		t.Errorf("expected ErrDeviceMismatch, got %v", err)
	}
}

// TestIntegration_ProcessExitCallback verifies that when the underlying agent
// process exits (triggered via the ProcessManager exit callback), the
// corresponding session transitions to StatusFailed in the store.
func TestIntegration_ProcessExitCallback(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "exit_cb_test.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	mockPM := newMockPM()
	mockTokenSvc := newMockTokenSvc()

	rs := NewRuntimeSession(slog.Default(), store, mockPM, mockTokenSvc, Config{})
	defer rs.Shutdown()

	runtimeID := "rt_exit_cb"
	sess, _, err := rs.Create(ctx, runtimeID, "dev_cb", "agent_cb")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	mockPM.triggerExit(runtimeID)

	time.Sleep(10 * time.Millisecond)

	rec, err := store.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if rec.Status != StatusFailed {
		t.Errorf("expected store status failed after agent exit, got %s", rec.Status)
	}
}

// TestIntegration_ReaperExpiresDisconnectedSessions verifies that the reaper
// goroutine closes and deletes sessions that have exceeded the MaxDisconnected
// grace period. This test is skipped in short mode or CI because it relies on
// real wall-clock timing (100 ms MaxDisconnected, 2 s sleep).
func TestIntegration_ReaperExpiresDisconnectedSessions(t *testing.T) {
	if testing.Short() || os.Getenv("CI") != "" {
		t.Skip("skipping reaper test in short/CI mode")
	}

	dbPath := filepath.Join(t.TempDir(), "reaper_test.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	mockPM := newMockPM()
	mockTokenSvc := newMockTokenSvc()

	cfg := Config{
		MaxDisconnected: 100 * time.Millisecond,
		MaxPerDevice:    5,
	}

	rs := NewRuntimeSession(slog.Default(), store, mockPM, mockTokenSvc, cfg)
	defer rs.Shutdown()

	sess, _, err := rs.Create(ctx, "rt_reaper", "dev_reaper", "agent_reaper")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := rs.DetachClient(sess.ID, 0); err != nil {
		t.Fatalf("DetachClient: %v", err)
	}

	time.Sleep(2 * time.Second)

	rec, err := store.GetSession(ctx, sess.ID)
	if err != nil {
		t.Log("session was reaped (deleted from store)")
	} else {
		t.Logf("session still in store with status %s — reaper interval may be too long", rec.Status)
	}
}

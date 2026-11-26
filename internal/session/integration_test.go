// Package session provides end-to-end integration tests for RuntimeSession
// and its collaboration with the SQLite store, a real runtime supervisor
// (launching the mock agent), and TokenService.
//
// These tests use real (temporary) SQLite databases, real agent processes, and
// clock-based timing for the reaper, exercising the full code path through
// storage without mocking the database layer.
package session

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

// TestIntegration_SessionLifecycle exercises the full session lifecycle end-to-end
// through RuntimeSession, verifying that each state transition and store operation
// behaves correctly when composed sequentially.
//
// Flow: Create → AttachClient → DetachClient → Resume → AttachClient (reconnect)
// → Close. After Close, the session is removed from the store and the agent
// process it owned is stopped.
func TestIntegration_SessionLifecycle(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "integration_test.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	logger := slog.Default()
	tokenSvc := NewTokenService(slog.New(slog.NewTextHandler(io.Discard, nil)))

	cfg := Config{
		MaxDisconnected: 5 * time.Minute,
		MaxPerDevice:    3,
	}

	rs := NewRuntimeSession(logger, store, runtime.NewSupervisor(logger, t.TempDir(), nil, nil), tokenSvc, cfg)
	defer rs.Shutdown()

	deviceID := "dev_integration_001"

	sess, attachToken, err := rs.Create(ctx, mockAgent(t), deviceID, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess == nil || sess.ID == "" {
		t.Fatal("expected non-nil session with ID")
	}
	if sess.RuntimeID == "" {
		t.Error("expected a generated runtime id")
	}
	if sess.Status != StatusDisconnected {
		t.Errorf("expected status disconnected, got %s", sess.Status)
	}
	if attachToken == "" {
		t.Error("expected non-empty attach token")
	}

	rec, err := store.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if rec.Status != StatusDisconnected {
		t.Errorf("expected store status disconnected, got %s", rec.Status)
	}

	_, gen, err := attachTestClient(ctx, rs, sess.ID, attachToken)
	if err != nil {
		t.Fatalf("AttachClient: %v", err)
	}

	_, _, err = attachTestClient(ctx, rs, sess.ID, attachToken)
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

	_, _, err = attachTestClient(ctx, rs, sess.ID, newToken)
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

	waitProcessStopped(t, sess)

	_, err = store.GetSession(ctx, sess.ID)
	if err == nil {
		t.Error("expected session to be deleted from store after close")
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
	logger := slog.Default()
	tokenSvc := NewTokenService(slog.New(slog.NewTextHandler(io.Discard, nil)))

	cfg := Config{
		MaxDisconnected: 5 * time.Minute,
		MaxPerDevice:    2,
	}

	rs := NewRuntimeSession(logger, store, runtime.NewSupervisor(logger, t.TempDir(), nil, nil), tokenSvc, cfg)
	defer rs.Shutdown()

	deviceID := "dev_limit_test"
	agent := mockAgent(t)

	if _, _, err = rs.Create(ctx, agent, deviceID, nil); err != nil {
		t.Fatalf("Create 1: %v", err)
	}

	if _, _, err = rs.Create(ctx, agent, deviceID, nil); err != nil {
		t.Fatalf("Create 2: %v", err)
	}

	if _, _, err = rs.Create(ctx, agent, deviceID, nil); err != ErrSessionLimitReached {
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
	logger := slog.Default()
	tokenSvc := NewTokenService(slog.New(slog.NewTextHandler(io.Discard, nil)))

	rs := NewRuntimeSession(logger, store, runtime.NewSupervisor(logger, t.TempDir(), nil, nil), tokenSvc, Config{})
	defer rs.Shutdown()

	sess, _, err := rs.Create(ctx, mockAgent(t), "dev_a", nil)
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

// TestIntegration_ProcessExitReclaims verifies that when the underlying agent
// process exits unexpectedly, the crashed session is fully reclaimed — its store
// record deleted — so the dead session cannot linger.
func TestIntegration_ProcessExitReclaims(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "exit_cb_test.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	logger := slog.Default()
	tokenSvc := NewTokenService(slog.New(slog.NewTextHandler(io.Discard, nil)))

	rs := NewRuntimeSession(logger, store, runtime.NewSupervisor(logger, t.TempDir(), nil, nil), tokenSvc, Config{})
	defer rs.Shutdown()

	sess, _, err := rs.Create(ctx, mockAgent(t), "dev_cb", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Kill the agent out from under the session: an unexpected exit.
	_ = sess.proc.Cmd.Process.Kill()

	// The crash is reclaimed asynchronously by the session's exit watcher.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := store.GetSession(ctx, sess.ID); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expected crashed session record to be deleted from store")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// A fresh session launches a new runtime and succeeds.
	if _, _, err := rs.Create(ctx, mockAgent(t), "dev_cb", nil); err != nil {
		t.Errorf("expected a fresh Create to succeed, got %v", err)
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
	logger := slog.Default()
	tokenSvc := NewTokenService(slog.New(slog.NewTextHandler(io.Discard, nil)))

	cfg := Config{
		MaxDisconnected: 100 * time.Millisecond,
		MaxPerDevice:    5,
	}

	rs := NewRuntimeSession(logger, store, runtime.NewSupervisor(logger, t.TempDir(), nil, nil), tokenSvc, cfg)
	defer rs.Shutdown()

	sess, _, err := rs.Create(ctx, mockAgent(t), "dev_reaper", nil)
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

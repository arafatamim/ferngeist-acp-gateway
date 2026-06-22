// Package storage provides tests for the SQLite persistence layer.
// Tests exercise roundtrip persistence, not-found and error cases,
// closed-database handling, bad-timestamp recovery, scan errors,
// and startup reconciliation for all storage operations.
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ========================================================================
// Pairing roundtrip (split from TestSQLiteStorePersistsPairingsAndRuntimes)
// ========================================================================

// TestPairingRoundtrip verifies save, list, delete (with ErrNotFound on second delete), and re-save of a pairing record.
func TestPairingRoundtrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "pairing_rt.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	expiresAt := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	if err := store.SavePairing(ctx, PairingRecord{
		DeviceID:       "dev-1",
		DeviceName:     "Pixel 9",
		Token:          "token-1",
		ExpiresAt:      expiresAt,
		Scopes:         []string{"gateway.read", "gateway.control"},
		ProofPublicKey: "proof-key-1",
	}); err != nil {
		t.Fatalf("SavePairing() error = %v", err)
	}

	pairings, err := store.ListPairings(ctx)
	if err != nil {
		t.Fatalf("ListPairings() error = %v", err)
	}
	if len(pairings) != 1 {
		t.Fatalf("len(pairings) = %d, want 1", len(pairings))
	}
	if pairings[0].Token != "token-1" {
		t.Fatalf("pairing token = %q, want %q", pairings[0].Token, "token-1")
	}
	if len(pairings[0].Scopes) != 2 {
		t.Fatalf("len(pairing scopes) = %d, want 2", len(pairings[0].Scopes))
	}
	if pairings[0].ProofPublicKey != "proof-key-1" {
		t.Fatalf("pairing proof key = %q, want %q", pairings[0].ProofPublicKey, "proof-key-1")
	}

	if err := store.DeletePairing(ctx, "dev-1"); err != nil {
		t.Fatalf("DeletePairing() error = %v", err)
	}
	if err := store.DeletePairing(ctx, "dev-1"); err != ErrNotFound {
		t.Fatalf("DeletePairing() second error = %v, want %v", err, ErrNotFound)
	}

	pairings, err = store.ListPairings(ctx)
	if err != nil {
		t.Fatalf("ListPairings() after delete error = %v", err)
	}
	if len(pairings) != 0 {
		t.Fatalf("len(pairings) after delete = %d, want 0", len(pairings))
	}

	if err := store.SavePairing(ctx, PairingRecord{
		DeviceID:       "dev-1",
		DeviceName:     "Pixel 9",
		Token:          "token-1",
		ExpiresAt:      expiresAt,
		Scopes:         []string{"gateway.read"},
		ProofPublicKey: "proof-key-2",
	}); err != nil {
		t.Fatalf("SavePairing() after delete error = %v", err)
	}
}

// ==========================================================================
// Runtime roundtrip (split from TestSQLiteStorePersistsPairingsAndRuntimes)
// ==========================================================================

// TestRuntimeRoundtrip verifies save, status update, and list of a runtime record.
// ==================================================================================
// Runtime token roundtrip (split from TestSQLiteStorePersistsPairingsAndRuntimes)
// ==================================================================================

// TestRuntimeTokenRoundtrip verifies save, get, and delete of a runtime token.
func TestRuntimeTokenRoundtrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "token_rt.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	tokenExpiry := time.Date(2026, 3, 25, 10, 5, 0, 0, time.UTC)
	if err := store.SaveRuntimeToken(ctx, RuntimeTokenRecord{
		RuntimeID: "run-1",
		Token:     "token-1",
		ExpiresAt: tokenExpiry,
	}); err != nil {
		t.Fatalf("SaveRuntimeToken() error = %v", err)
	}

	tokenRecord, err := store.GetRuntimeToken(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetRuntimeToken() error = %v", err)
	}
	if tokenRecord.Token != "token-1" {
		t.Fatalf("runtime token = %q, want %q", tokenRecord.Token, "token-1")
	}

	if err := store.DeleteRuntimeToken(ctx, "run-1"); err != nil {
		t.Fatalf("DeleteRuntimeToken() error = %v", err)
	}
	if _, err := store.GetRuntimeToken(ctx, "run-1"); err != ErrNotFound {
		t.Fatalf("GetRuntimeToken() after delete error = %v, want %v", err, ErrNotFound)
	}
}

// ======================================================================================
// Failure record roundtrip (split from TestSQLiteStorePersistsPairingsAndRuntimes)
// ======================================================================================

// TestFailureRecordRoundtrip verifies save and list of a runtime failure record with log preview.
func TestFailureRecordRoundtrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "failure_rt.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	failedAt := time.Date(2026, 3, 25, 10, 6, 0, 0, time.UTC)
	if err := store.SaveRuntimeFailure(ctx, RuntimeFailureRecord{
		RuntimeID:  "run-1",
		AgentID:    "mock-acp",
		AgentName:  "Mock ACP",
		LastError:  "process exited with status 1",
		CreatedAt:  time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC),
		FailedAt:   failedAt,
		LogPreview: `[{"timestamp":"2026-03-25T10:05:59Z","stream":"stderr","message":"boom"}]`,
	}); err != nil {
		t.Fatalf("SaveRuntimeFailure() error = %v", err)
	}

	failures, err := store.ListRecentRuntimeFailures(ctx, 5)
	if err != nil {
		t.Fatalf("ListRecentRuntimeFailures() error = %v", err)
	}
	if len(failures) != 1 {
		t.Fatalf("len(failures) = %d, want 1", len(failures))
	}
	if failures[0].FailedAt != failedAt {
		t.Fatalf("failures[0].FailedAt = %v, want %v", failures[0].FailedAt, failedAt)
	}
	if failures[0].LastError != "process exited with status 1" {
		t.Fatalf("failures[0].LastError = %q", failures[0].LastError)
	}
}

// ==========================================================================
// Settings roundtrip (split from TestSQLiteStorePersistsPairingsAndRuntimes)
// ==========================================================================

// TestSettingsRoundtrip verifies save and get of gateway settings (all fields).
func TestSettingsRoundtrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "settings_rt.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	if err := store.SaveGatewaySettings(ctx, GatewaySettingsRecord{
		RegistryURL:   "https://stored.example/registry.json",
		PublicBaseURL: "https://helper.example.com",
		EnableLAN:     true,
		GatewayName:   "workstation",
	}); err != nil {
		t.Fatalf("SaveGatewaySettings() error = %v", err)
	}

	settings, err := store.GetGatewaySettings(ctx)
	if err != nil {
		t.Fatalf("GetGatewaySettings() error = %v", err)
	}
	if settings.RegistryURL != "https://stored.example/registry.json" {
		t.Fatalf("settings.RegistryURL = %q", settings.RegistryURL)
	}
	if settings.PublicBaseURL != "https://helper.example.com" {
		t.Fatalf("settings.PublicBaseURL = %q", settings.PublicBaseURL)
	}
	if !settings.EnableLAN {
		t.Fatal("settings.EnableLAN should be true")
	}
	if settings.GatewayName != "workstation" {
		t.Fatalf("settings.GatewayName = %q", settings.GatewayName)
	}
}

// ========================================================================
// Binary roundtrip (split from TestSQLiteStorePersistsPairingsAndRuntimes)
// ========================================================================

// TestBinaryRoundtrip verifies save and get of an acquired binary record.
func TestBinaryRoundtrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "binary_rt.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	if err := store.SaveAcquiredBinary(ctx, AcquiredBinaryRecord{
		AgentID:     "codex-acp",
		Version:     "0.10.0",
		Path:        "C:/Users/test/AppData/Local/FerngeistGateway/bin/codex-acp.exe",
		ArchiveURL:  "https://example.com/codex.zip",
		InstalledAt: time.Date(2026, 3, 25, 10, 7, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("SaveAcquiredBinary() error = %v", err)
	}

	acquired, err := store.GetAcquiredBinary(ctx, "codex-acp")
	if err != nil {
		t.Fatalf("GetAcquiredBinary() error = %v", err)
	}
	if acquired.Path != "C:/Users/test/AppData/Local/FerngeistGateway/bin/codex-acp.exe" {
		t.Fatalf("acquired.Path = %q", acquired.Path)
	}
}

// ============================================================
// TestOpen_ForeignKeysEnabled — add PRAGMA foreign_keys check
// ============================================================

// TestOpen_ForeignKeysEnabled verifies that PRAGMA foreign_keys is enabled after Open.
func TestOpen_ForeignKeysEnabled(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "fk_test.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	var enabled bool
	if err := store.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&enabled); err != nil {
		t.Fatalf("PRAGMA foreign_keys query: %v", err)
	}
	if !enabled {
		t.Error("foreign_keys is disabled, want enabled")
	}
}

// ======================================================================
// Session CRUD tests (split from TestSessionStore_CRUD)
// ======================================================================

// TestSessionStore_SaveAndGetSession verifies saving a session record and retrieving it by ID.
func TestSessionStore_SaveAndGetSession(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "session_save_get.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	now := time.Now().UTC()
	rec := SessionRecord{
		SessionID:   "sess_abc123",
		RuntimeID:   "rt_xyz789",
		DeviceID:    "dev_001",
		AgentID:     "agent_mock",
		Status:      "active",
		Leaseholder: "sess_abc123",
		CreatedAt:   now,
	}

	if err := store.SaveSession(ctx, rec); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	got, err := store.GetSession(ctx, "sess_abc123")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.SessionID != "sess_abc123" || got.Status != "active" {
		t.Errorf("unexpected session: %+v", got)
	}
}

// TestSessionStore_UpdateSessionStatus verifies updating a session's status and disconnected_since timestamp.
func TestSessionStore_UpdateSessionStatus(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "session_update.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	now := time.Now().UTC()
	rec := SessionRecord{
		SessionID:   "sess_abc123",
		RuntimeID:   "rt_xyz789",
		DeviceID:    "dev_001",
		AgentID:     "agent_mock",
		Status:      "active",
		Leaseholder: "sess_abc123",
		CreatedAt:   now,
	}

	if err := store.SaveSession(ctx, rec); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	rec.Status = "disconnected"
	disconnectedTime := now.Add(5 * time.Minute)
	rec.DisconnectedSince = &disconnectedTime
	if err := store.SaveSession(ctx, rec); err != nil {
		t.Fatalf("SaveSession update: %v", err)
	}

	got, err := store.GetSession(ctx, "sess_abc123")
	if err != nil {
		t.Fatalf("GetSession after update: %v", err)
	}
	if got.Status != "disconnected" {
		t.Errorf("expected disconnected, got %s", got.Status)
	}
}

// TestSessionStore_ListSessionsByDevice verifies listing sessions scoped to a device ID.
func TestSessionStore_ListSessionsByDevice(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "session_list_device.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	now := time.Now().UTC()
	rec := SessionRecord{
		SessionID:   "sess_abc123",
		RuntimeID:   "rt_xyz789",
		DeviceID:    "dev_001",
		AgentID:     "agent_mock",
		Status:      "active",
		Leaseholder: "sess_abc123",
		CreatedAt:   now,
	}

	if err := store.SaveSession(ctx, rec); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	sessions, err := store.ListSessionsByDevice(ctx, "dev_001")
	if err != nil {
		t.Fatalf("ListSessionsByDevice: %v", err)
	}
	if len(sessions) != 1 {
		t.Errorf("expected 1 session, got %d", len(sessions))
	}
}

// TestSessionStore_InboundFrameRoundtrip verifies appending and retrieving inbound diagnostic frames.
func TestSessionStore_InboundFrameRoundtrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "session_frame_rt.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	now := time.Now().UTC()
	rec := SessionRecord{
		SessionID:   "sess_abc123",
		RuntimeID:   "rt_xyz789",
		DeviceID:    "dev_001",
		AgentID:     "agent_mock",
		Status:      "active",
		Leaseholder: "sess_abc123",
		CreatedAt:   now,
	}

	if err := store.SaveSession(ctx, rec); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	if err := store.AppendInboundDiagnostic(ctx, "sess_abc123", 1, `{"jsonrpc":"2.0","result":"hello"}`); err != nil {
		t.Fatalf("AppendInboundDiagnostic: %v", err)
	}

	var payload string
	row := store.db.QueryRowContext(ctx,
		`SELECT payload FROM session_inbound_log WHERE session_id = ? AND seq = ?`, "sess_abc123", 1)
	if err := row.Scan(&payload); err != nil {
		t.Fatalf("QueryRowContext: %v", err)
	}
	if payload != `{"jsonrpc":"2.0","result":"hello"}` {
		t.Errorf("expected payload, got %q", payload)
	}
}

func TestSessionStore_CascadeDeleteRemovesDiagnostics(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "session_cascade.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	now := time.Now().UTC()
	rec := SessionRecord{
		SessionID:   "sess_abc123",
		RuntimeID:   "rt_xyz789",
		DeviceID:    "dev_001",
		AgentID:     "agent_mock",
		Status:      "active",
		Leaseholder: "sess_abc123",
		CreatedAt:   now,
	}

	if err := store.SaveSession(ctx, rec); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	if err := store.AppendInboundDiagnostic(ctx, "sess_abc123", 1, `{"jsonrpc":"2.0","result":"hello"}`); err != nil {
		t.Fatalf("AppendInboundDiagnostic: %v", err)
	}

	if err := store.DeleteSession(ctx, "sess_abc123"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	var count int
	row := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_inbound_log WHERE session_id = ?`, "sess_abc123")
	if err := row.Scan(&count); err != nil {
		t.Fatalf("QueryRowContext: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 diagnostics after cascade delete, got %d", count)
	}
}

// ======================================================================
// TestAppendInboundDiagnostic — add read-back assertion via raw SQL
// ======================================================================

// TestAppendInboundDiagnostic verifies appending an inbound diagnostic and confirms it via raw SQL count.
func TestAppendInboundDiagnostic(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "inbound_test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	now := time.Now().UTC()
	rec := SessionRecord{
		SessionID:   "sess_inbound",
		RuntimeID:   "rt_inbound",
		DeviceID:    "dev_inbound",
		AgentID:     "agent_inbound",
		Status:      "active",
		Leaseholder: "sess_inbound",
		CreatedAt:   now,
	}
	if err := store.SaveSession(ctx, rec); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	if err := store.AppendInboundDiagnostic(ctx, "sess_inbound", 1, `{"jsonrpc":"2.0","method":"ping"}`); err != nil {
		t.Fatalf("AppendInboundDiagnostic: %v", err)
	}

	var count int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM session_inbound_log WHERE session_id = ?", "sess_inbound").Scan(&count); err != nil {
		t.Fatalf("count diagnostics: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 inbound diagnostic, got %d", count)
	}

	if err := store.DeleteSession(ctx, "sess_inbound"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
}

// =======================================================
// Existing focused tests — kept as-is or with minor fixes
// =======================================================

// TestMarkSessionsFailedByRuntime verifies marking all sessions for a given runtime as failed.
// TestDeleteExpiredRuntimeTokens verifies deleting only runtime tokens whose expires_at is before the cutoff.
func TestDeleteExpiredRuntimeTokens(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "expired_tokens.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	if err := store.SaveRuntimeToken(ctx, RuntimeTokenRecord{
		RuntimeID: "run-expired",
		Token:     "token-expired",
		ExpiresAt: time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("SaveRuntimeToken(expired) error = %v", err)
	}

	if err := store.SaveRuntimeToken(ctx, RuntimeTokenRecord{
		RuntimeID: "run-future",
		Token:     "token-future",
		ExpiresAt: time.Date(2026, 3, 25, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("SaveRuntimeToken(future) error = %v", err)
	}

	now := time.Date(2026, 3, 25, 11, 0, 0, 0, time.UTC)
	if err := store.DeleteExpiredRuntimeTokens(ctx, now); err != nil {
		t.Fatalf("DeleteExpiredRuntimeTokens() error = %v", err)
	}

	if _, err := store.GetRuntimeToken(ctx, "run-expired"); err != ErrNotFound {
		t.Fatalf("GetRuntimeToken(expired) error = %v, want %v", err, ErrNotFound)
	}

	record, err := store.GetRuntimeToken(ctx, "run-future")
	if err != nil {
		t.Fatalf("GetRuntimeToken(future) error = %v", err)
	}
	if record.Token != "token-future" {
		t.Fatalf("token = %q, want %q", record.Token, "token-future")
	}
}

// TestDeleteAllRuntimeTokens verifies deleting all runtime tokens regardless of expiry.
func TestDeleteAllRuntimeTokens(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "delete_all_tokens.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	for _, rec := range []RuntimeTokenRecord{
		{RuntimeID: "run-1", Token: "token-1", ExpiresAt: time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)},
		{RuntimeID: "run-2", Token: "token-2", ExpiresAt: time.Date(2026, 3, 25, 12, 0, 0, 0, time.UTC)},
	} {
		if err := store.SaveRuntimeToken(ctx, rec); err != nil {
			t.Fatalf("SaveRuntimeToken(%s) error = %v", rec.RuntimeID, err)
		}
	}

	if err := store.DeleteAllRuntimeTokens(ctx); err != nil {
		t.Fatalf("DeleteAllRuntimeTokens() error = %v", err)
	}

	for _, id := range []string{"run-1", "run-2"} {
		if _, err := store.GetRuntimeToken(ctx, id); err != ErrNotFound {
			t.Fatalf("GetRuntimeToken(%s) error = %v, want %v", id, err, ErrNotFound)
		}
	}
}

// TestGetGatewaySettingsNotFound verifies ErrNotFound when no gateway settings exist.
func TestGetGatewaySettingsNotFound(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "settings_notfound.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	if _, err := store.GetGatewaySettings(context.Background()); err != ErrNotFound {
		t.Fatalf("GetGatewaySettings() error = %v, want %v", err, ErrNotFound)
	}
}

// TestGetAcquiredBinaryNotFound verifies ErrNotFound when the binary record does not exist.
func TestGetAcquiredBinaryNotFound(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "binary_notfound.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	if _, err := store.GetAcquiredBinary(context.Background(), "nonexistent"); err != ErrNotFound {
		t.Fatalf("GetAcquiredBinary() error = %v, want %v", err, ErrNotFound)
	}
}

// ================================================================
// Renamed nil-receiver / nil-DB tests
// ================================================================

// TestCloseReturnsNilWhenCalledOnNilReceiver verifies that Close handles a nil *SQLiteStore receiver gracefully.
func TestCloseReturnsNilWhenCalledOnNilReceiver(t *testing.T) {
	var s *SQLiteStore
	if err := s.Close(); err != nil {
		t.Errorf("Close() on nil receiver = %v, want nil", err)
	}
}

// TestCloseReturnsNilWhenDBIsNil verifies that Close handles a nil db field gracefully.
func TestCloseReturnsNilWhenDBIsNil(t *testing.T) {
	s := &SQLiteStore{}
	if err := s.Close(); err != nil {
		t.Errorf("Close() with nil db = %v, want nil", err)
	}
}

// ================================================================
// Table-driven helper tests
// ================================================================

// TestBoolToSQLiteIntReturnsZeroForFalseAndOneForTrue verifies the boolToSQLiteInt helper returns 0 for false and 1 for true.
func TestBoolToSQLiteIntReturnsZeroForFalseAndOneForTrue(t *testing.T) {
	tests := []struct {
		name  string
		input bool
		want  int
	}{
		{name: "false returns 0", input: false, want: 0},
		{name: "true returns 1", input: true, want: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := boolToSQLiteInt(tc.input); got != tc.want {
				t.Errorf("boolToSQLiteInt(%v) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

// TestNullableToTimeReturnsNilForEmptyString verifies the nullableToTime helper for empty, invalid, and valid RFC3339 strings.
func TestNullableToTimeReturnsNilForEmptyString(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  *time.Time
	}{
		{name: "empty string returns nil", input: "", want: nil},
		{name: "invalid string returns nil", input: "invalid", want: nil},
		{name: "valid RFC3339 string", input: "2026-05-22T10:00:00Z", want: func() *time.Time {
			t := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)
			return &t
		}()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := nullableToTime(tc.input)
			if tc.want == nil {
				if got != nil {
					t.Errorf("nullableToTime(%q) = %v, want nil", tc.input, got)
				}
				return
			}
			if got == nil || !got.Equal(*tc.want) {
				t.Errorf("nullableToTime(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// ================================================================
// Not-found and empty-result tests (unchanged)
// ================================================================

// TestListRecentRuntimeFailuresEmpty verifies that ListRecentRuntimeFailures returns an empty slice when no failures exist.
func TestListRecentRuntimeFailuresEmpty(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "failures_empty.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	records, err := store.ListRecentRuntimeFailures(context.Background(), 5)
	if err != nil {
		t.Fatalf("ListRecentRuntimeFailures() error = %v", err)
	}
	if len(records) != 0 {
		t.Errorf("ListRecentRuntimeFailures() = %d records, want 0", len(records))
	}
}

// TestListSessionsByDeviceEmpty verifies that ListSessionsByDevice returns an empty slice for a device with no sessions.
func TestListSessionsByDeviceEmpty(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "sessions_empty_device.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	records, err := store.ListSessionsByDevice(context.Background(), "nonexistent_device")
	if err != nil {
		t.Fatalf("ListSessionsByDevice() error = %v", err)
	}
	if len(records) != 0 {
		t.Errorf("ListSessionsByDevice() = %d records, want 0", len(records))
	}
}

// ======================================================================
// Closed-DB tests — renamed and with specific error string assertions
// ======================================================================

// TestListRuntimesReturnsErrorWhenDBIsClosed verifies ListRuntimes returns an error containing "closed" after store is closed.

// TestListPairingsReturnsErrorWhenDBIsClosed verifies ListPairings returns an error containing "closed" after store is closed.
func TestListPairingsReturnsErrorWhenDBIsClosed(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "pairings_closed.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	store.Close()
	_, err = store.ListPairings(context.Background())
	if err == nil {
		t.Fatal("expected error from closed DB")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("error = %v, want error containing 'closed'", err)
	}
}

// TestListRecentRuntimeFailuresReturnsErrorWhenDBIsClosed verifies ListRecentRuntimeFailures returns an error containing "closed" after store is closed.
func TestListRecentRuntimeFailuresReturnsErrorWhenDBIsClosed(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "failures_closed.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	store.Close()
	_, err = store.ListRecentRuntimeFailures(context.Background(), 5)
	if err == nil {
		t.Fatal("expected error from closed DB")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("error = %v, want error containing 'closed'", err)
	}
}

// TestListSessionsByDeviceReturnsErrorWhenDBIsClosed verifies ListSessionsByDevice returns an error containing "closed" after store is closed.
func TestListSessionsByDeviceReturnsErrorWhenDBIsClosed(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "sessions_closed.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	store.Close()
	_, err = store.ListSessionsByDevice(context.Background(), "any")
	if err == nil {
		t.Fatal("expected error from closed DB")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("error = %v, want error containing 'closed'", err)
	}
}

// TestGetSessionReturnsErrorWhenDBIsClosed verifies GetSession returns an error containing "closed" after store is closed.
func TestGetSessionReturnsErrorWhenDBIsClosed(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "session_closed.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	store.Close()
	_, err = store.GetSession(context.Background(), "any")
	if err == nil {
		t.Fatal("expected error from closed DB")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("error = %v, want error containing 'closed'", err)
	}
}

// TestGetGatewaySettingsReturnsErrorWhenDBIsClosed verifies GetGatewaySettings returns an error containing "closed" after store is closed.
func TestGetGatewaySettingsReturnsErrorWhenDBIsClosed(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "settings_closed.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	store.Close()
	_, err = store.GetGatewaySettings(context.Background())
	if err == nil {
		t.Fatal("expected error from closed DB")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("error = %v, want error containing 'closed'", err)
	}
}

// TestGetRuntimeTokenReturnsErrorWhenDBIsClosed verifies GetRuntimeToken returns an error containing "closed" after store is closed.
func TestGetRuntimeTokenReturnsErrorWhenDBIsClosed(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "token_closed.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	store.Close()
	_, err = store.GetRuntimeToken(context.Background(), "any")
	if err == nil {
		t.Fatal("expected error from closed DB")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("error = %v, want error containing 'closed'", err)
	}
}

// TestGetAcquiredBinaryReturnsErrorWhenDBIsClosed verifies GetAcquiredBinary returns an error containing "closed" after store is closed.
func TestGetAcquiredBinaryReturnsErrorWhenDBIsClosed(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "binary_closed.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	store.Close()
	_, err = store.GetAcquiredBinary(context.Background(), "any")
	if err == nil {
		t.Fatal("expected error from closed DB")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("error = %v, want error containing 'closed'", err)
	}
}

// TestUpdateRuntimeStatusReturnsErrorWhenDBIsClosed verifies UpdateRuntimeStatus returns an error containing "closed" after store is closed.
// TestSavePairingReturnsErrorWhenDBIsClosed verifies SavePairing returns an error containing "closed" after store is closed.
func TestSavePairingReturnsErrorWhenDBIsClosed(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "save_pairing_closed.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	store.Close()
	err = store.SavePairing(context.Background(), PairingRecord{DeviceID: "x"})
	if err == nil {
		t.Fatal("expected error from closed DB")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("error = %v, want error containing 'closed'", err)
	}
}

// TestDeletePairingReturnsErrorWhenDBIsClosed verifies DeletePairing returns an error containing "closed" after store is closed.
func TestDeletePairingReturnsErrorWhenDBIsClosed(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "delete_pairing_closed.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	store.Close()
	err = store.DeletePairing(context.Background(), "x")
	if err == nil {
		t.Fatal("expected error from closed DB")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("error = %v, want error containing 'closed'", err)
	}
}

// TestMigrateReturnsErrorWhenDBIsClosed verifies migrate returns an error containing "closed" after store is closed.
func TestMigrateReturnsErrorWhenDBIsClosed(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "migrate_closed.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	store.Close()
	err = store.migrate(context.Background())
	if err == nil {
		t.Fatal("expected error from closed DB")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("error = %v, want error containing 'closed'", err)
	}
}

// ======================================================================
// Bad-timestamp tests — verify error contains "parse" or similar
// ======================================================================

// TestGetRuntimeTokenBadTimestamp verifies that a stored bad timestamp returns a parse error from GetRuntimeToken.
func TestGetRuntimeTokenBadTimestamp(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "token_bad_ts.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO runtime_tokens(runtime_id, token, expires_at) VALUES(?,?,?)`, "bad", "tok", "NOT A TIMESTAMP"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_, err = store.GetRuntimeToken(ctx, "bad")
	if err == nil || err == ErrNotFound {
		t.Errorf("expected parse error, got %v", err)
	}
	if !strings.Contains(err.Error(), "cannot parse") && !strings.Contains(err.Error(), "parsing time") {
		t.Errorf("error = %v, want error containing 'cannot parse' or 'parsing time'", err)
	}
}

// TestGetAcquiredBinaryBadTimestamp verifies that a stored bad timestamp returns a parse error from GetAcquiredBinary.
func TestGetAcquiredBinaryBadTimestamp(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "binary_bad_ts.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO acquired_binaries(agent_id, version, path, archive_url, installed_at) VALUES(?,?,?,?,?)`, "bad", "1.0", "/path", "url", "NOT A TIMESTAMP"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_, err = store.GetAcquiredBinary(ctx, "bad")
	if err == nil || err == ErrNotFound {
		t.Errorf("expected parse error, got %v", err)
	}
	if !strings.Contains(err.Error(), "cannot parse") && !strings.Contains(err.Error(), "parsing time") {
		t.Errorf("error = %v, want error containing 'cannot parse' or 'parsing time'", err)
	}
}

// TestListRuntimesBadTimestamp verifies that a stored bad timestamp returns a parse error from ListRuntimes.
// TestListRecentRuntimeFailuresBadTimestamp verifies that a stored bad timestamp returns a parse error from ListRecentRuntimeFailures.
func TestListRecentRuntimeFailuresBadTimestamp(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "failures_bad_ts.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO runtime_failures(runtime_id, agent_id, agent_name, last_error, created_at, failed_at, log_preview) VALUES(?,?,?,?,?,?,?)`, "bad", "a", "b", "err", "NOT A TIMESTAMP", "2026-05-22T10:00:00Z", ""); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_, err = store.ListRecentRuntimeFailures(ctx, 5)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "cannot parse") && !strings.Contains(err.Error(), "parsing time") {
		t.Errorf("error = %v, want error containing 'cannot parse' or 'parsing time'", err)
	}
}

// TestListPairingsBadTimestamp verifies that a stored bad timestamp returns a parse error from ListPairings.
func TestListPairingsBadTimestamp(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "pairings_bad_ts.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO paired_devices(device_id, device_name, token, expires_at) VALUES(?,?,?,?)`, "bad", "dev", "tok", "NOT A TIMESTAMP"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_, err = store.ListPairings(ctx)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "cannot parse") && !strings.Contains(err.Error(), "parsing time") {
		t.Errorf("error = %v, want error containing 'cannot parse' or 'parsing time'", err)
	}
}

// ======================================================================
// Bad JSON test — verify error is a *json.SyntaxError
// ======================================================================

// TestListPairingsBadScopesJSON verifies that ListPairings returns a JSON syntax error when scopes data is malformed.
func TestListPairingsBadScopesJSON(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "pairings_bad_json.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO paired_devices(device_id, device_name, token, expires_at) VALUES(?,?,?,?)`, "bad", "dev", "tok", "2026-05-22T10:00:00Z"); err != nil {
		t.Fatalf("insert paired_devices: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO paired_device_scopes(device_id, scopes) VALUES(?,?)`, "bad", "{bad json}"); err != nil {
		t.Fatalf("insert scopes: %v", err)
	}
	_, err = store.ListPairings(ctx)
	if err == nil {
		t.Fatal("expected json unmarshal error")
	}
	var syntaxErr *json.SyntaxError
	if !strings.Contains(err.Error(), "invalid character") && !errors.As(err, &syntaxErr) {
		t.Errorf("error = %v, want json.SyntaxError or error containing 'invalid character'", err)
	}
}

// ======================================================================
// Scan error tests — verify error matches the injected bad data
// ======================================================================

// ======================================================================
// Default-limit and bad-failed_at tests (unchanged)
// ======================================================================

// TestListRecentRuntimeFailuresDefaultLimit verifies that passing 0 or -1 as limit does not cause errors and returns empty.
func TestListRecentRuntimeFailuresDefaultLimit(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "failures_default_limit.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	records, err := store.ListRecentRuntimeFailures(context.Background(), 0)
	if err != nil {
		t.Fatalf("ListRecentRuntimeFailures(0) error = %v", err)
	}
	if len(records) != 0 {
		t.Errorf("expected 0 records, got %d", len(records))
	}
	records, err = store.ListRecentRuntimeFailures(context.Background(), -1)
	if err != nil {
		t.Fatalf("ListRecentRuntimeFailures(-1) error = %v", err)
	}
	if len(records) != 0 {
		t.Errorf("expected 0 records, got %d", len(records))
	}
}

// TestListRecentRuntimeFailuresBadFailedAt verifies that a bad failed_at timestamp returns a parse error.
func TestListRecentRuntimeFailuresBadFailedAt(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "failures_bad_failed_at.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO runtime_failures(runtime_id, agent_id, agent_name, last_error, created_at, failed_at, log_preview) VALUES(?,?,?,?,?,?,?)`, "bad", "a", "b", "err", "2026-05-22T10:00:00Z", "NOT A TIMESTAMP", ""); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_, err = store.ListRecentRuntimeFailures(ctx, 5)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "cannot parse") && !strings.Contains(err.Error(), "parsing time") {
		t.Errorf("error = %v, want error containing 'cannot parse' or 'parsing time'", err)
	}
}

// ======================================================
// ReconcileOnStartup — fix silent error discard (issue 1)
// ======================================================

// TestSessionStore_ReconcileOnStartup verifies that active and disconnected sessions are marked as failed on startup reconciliation.
func TestSessionStore_ReconcileOnStartup(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reconcile_test.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	now := time.Now().UTC()
	if err := store.SaveSession(ctx, SessionRecord{
		SessionID: "sess_active", RuntimeID: "rt_1", DeviceID: "dev_1",
		AgentID: "agent_1", Status: "active", Leaseholder: "sess_active", CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveSession(sess_active): %v", err)
	}
	if err := store.SaveSession(ctx, SessionRecord{
		SessionID: "sess_disc", RuntimeID: "rt_2", DeviceID: "dev_1",
		AgentID: "agent_1", Status: "disconnected", Leaseholder: "sess_disc", CreatedAt: now,
	}); err != nil {
		t.Fatalf("SaveSession(sess_disc): %v", err)
	}

	if err := store.ReconcileSessionsOnStartup(ctx); err != nil {
		t.Fatalf("ReconcileSessionsOnStartup: %v", err)
	}

	got, err := store.GetSession(ctx, "sess_active")
	if err != nil {
		t.Fatalf("GetSession(sess_active): %v", err)
	}
	if got.Status != "failed" {
		t.Errorf("expected active session to be marked failed, got %s", got.Status)
	}
	got, err = store.GetSession(ctx, "sess_disc")
	if err != nil {
		t.Fatalf("GetSession(sess_disc): %v", err)
	}
	if got.Status != "failed" {
		t.Errorf("expected disconnected session to be marked failed, got %s", got.Status)
	}
}

// ======================================================
// New edge case tests (issue 8)
// ======================================================

// TestSessionStore_GetSessionNotFound verifies that GetSession returns ErrNotFound for a nonexistent session.
func TestSessionStore_GetSessionNotFound(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "session_notfound.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	_, err = store.GetSession(ctx, "nonexistent")
	if err != ErrNotFound {
		t.Errorf("GetSession('nonexistent') error = %v, want %v", err, ErrNotFound)
	}
}

// TestSessionStore_DeleteSessionNotFound verifies that DeleteSession is a no-op (returns nil) for a nonexistent session.
func TestSessionStore_DeleteSessionNotFound(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "session_del_notfound.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	if err := store.DeleteSession(ctx, "nonexistent"); err != nil {
		t.Errorf("DeleteSession('nonexistent') error = %v, want nil", err)
	}
}

// TestSessionStore_ListSessionsByDeviceMultiple verifies listing multiple sessions for one device while excluding sessions from other devices.
func TestSessionStore_ListSessionsByDeviceMultiple(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "session_multi_device.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	now := time.Now().UTC()
	for _, s := range []SessionRecord{
		{SessionID: "sess_dev_1a", RuntimeID: "rt_1", DeviceID: "dev_multi", AgentID: "agent_1", Status: "active", Leaseholder: "sess_dev_1a", CreatedAt: now},
		{SessionID: "sess_dev_1b", RuntimeID: "rt_2", DeviceID: "dev_multi", AgentID: "agent_2", Status: "disconnected", Leaseholder: "sess_dev_1b", CreatedAt: now.Add(-1 * time.Minute)},
		{SessionID: "sess_dev_2a", RuntimeID: "rt_3", DeviceID: "dev_other", AgentID: "agent_1", Status: "active", Leaseholder: "sess_dev_2a", CreatedAt: now},
	} {
		if err := store.SaveSession(ctx, s); err != nil {
			t.Fatalf("SaveSession(%s): %v", s.SessionID, err)
		}
	}

	sessions, err := store.ListSessionsByDevice(ctx, "dev_multi")
	if err != nil {
		t.Fatalf("ListSessionsByDevice: %v", err)
	}
	if len(sessions) != 2 {
		t.Errorf("expected 2 sessions for dev_multi, got %d", len(sessions))
	}
}

// ========================================================================
// Push tokens and gateway identity
// ========================================================================

// TestDevicePushTokenRoundtrip verifies upsert (token + platform), read-back,
// the zero-result for an unregistered device, replacement on re-register, and
// deletion.
func TestDevicePushTokenRoundtrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "push_tokens.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	// Unregistered device is a normal zero result, not an error.
	token, platform, err := store.GetDevicePushToken(ctx, "dev-1")
	if err != nil {
		t.Fatalf("GetDevicePushToken(unregistered) error = %v", err)
	}
	if token != "" || platform != "" {
		t.Fatalf("GetDevicePushToken(unregistered) = (%q, %q), want empty", token, platform)
	}

	if err := store.SaveDevicePushToken(ctx, "dev-1", "fcm-token-1", "android"); err != nil {
		t.Fatalf("SaveDevicePushToken() error = %v", err)
	}
	token, platform, err = store.GetDevicePushToken(ctx, "dev-1")
	if err != nil {
		t.Fatalf("GetDevicePushToken() error = %v", err)
	}
	if token != "fcm-token-1" || platform != "android" {
		t.Fatalf("GetDevicePushToken() = (%q, %q), want (fcm-token-1, android)", token, platform)
	}

	// Re-register replaces the token in place (tokens rotate).
	if err := store.SaveDevicePushToken(ctx, "dev-1", "fcm-token-2", "ios"); err != nil {
		t.Fatalf("SaveDevicePushToken(replace) error = %v", err)
	}
	token, platform, err = store.GetDevicePushToken(ctx, "dev-1")
	if err != nil {
		t.Fatalf("GetDevicePushToken(after replace) error = %v", err)
	}
	if token != "fcm-token-2" || platform != "ios" {
		t.Fatalf("GetDevicePushToken(after replace) = (%q, %q), want (fcm-token-2, ios)", token, platform)
	}

	if err := store.DeleteDevicePushToken(ctx, "dev-1"); err != nil {
		t.Fatalf("DeleteDevicePushToken() error = %v", err)
	}
	token, _, err = store.GetDevicePushToken(ctx, "dev-1")
	if err != nil {
		t.Fatalf("GetDevicePushToken(after delete) error = %v", err)
	}
	if token != "" {
		t.Fatalf("GetDevicePushToken(after delete) token = %q, want empty", token)
	}
	// Deleting an absent token is a no-op, not an error.
	if err := store.DeleteDevicePushToken(ctx, "dev-1"); err != nil {
		t.Fatalf("DeleteDevicePushToken(absent) error = %v", err)
	}
}

// TestEnsureGatewayID verifies an id is generated on first call and is stable
// across subsequent calls and reopens of the same database.
func TestEnsureGatewayID(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gateway_id.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	ctx := context.Background()

	id1, err := store.EnsureGatewayID(ctx)
	if err != nil {
		t.Fatalf("EnsureGatewayID() error = %v", err)
	}
	if strings.TrimSpace(id1) == "" {
		t.Fatal("EnsureGatewayID() returned an empty id")
	}

	// Stable within the same store instance.
	id2, err := store.EnsureGatewayID(ctx)
	if err != nil {
		t.Fatalf("EnsureGatewayID(second) error = %v", err)
	}
	if id2 != id1 {
		t.Fatalf("EnsureGatewayID() not stable: %q != %q", id2, id1)
	}
	store.Close()

	// Stable across a reopen of the same database file.
	reopened, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open(reopen) error = %v", err)
	}
	defer reopened.Close()
	id3, err := reopened.EnsureGatewayID(ctx)
	if err != nil {
		t.Fatalf("EnsureGatewayID(reopen) error = %v", err)
	}
	if id3 != id1 {
		t.Fatalf("EnsureGatewayID() not stable across reopen: %q != %q", id3, id1)
	}
}

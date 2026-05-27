package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/api"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/config"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

func preserveAndClearGatewayEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"FERNGEIST_GATEWAY_REGISTRY_URL",
		"FERNGEIST_GATEWAY_PUBLIC_BASE_URL",
		"FERNGEIST_GATEWAY_ENABLE_LAN",
		"FERNGEIST_GATEWAY_NAME",
	}
	for _, k := range keys {
		old := os.Getenv(k)
		os.Unsetenv(k)
		t.Cleanup(func() {
			if old != "" {
				os.Setenv(k, old)
			}
		})
	}
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func openTestStore(t *testing.T) *storage.SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "daemon.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestDiscoveryTXTRecords_returns_gateway_name_from_config(t *testing.T) {
	cfg := config.Config{GatewayName: "my-gateway"}
	records := DiscoveryTXTRecords(cfg, 0)
	if len(records) < 1 {
		t.Fatal("expected at least 1 record")
	}
	want := "gateway_name=my-gateway"
	if records[0] != want {
		t.Errorf("records[0] = %q, want %q", records[0], want)
	}
}

func TestDiscoveryTXTRecords_includes_gateway_version(t *testing.T) {
	records := DiscoveryTXTRecords(config.Config{}, 0)
	if len(records) < 2 {
		t.Fatal("expected at least 2 records")
	}
	want := "gateway_version=dev"
	if records[1] != want {
		t.Errorf("records[1] = %q, want %q", records[1], want)
	}
}

func TestDiscoveryTXTRecords_includes_protocol_version(t *testing.T) {
	records := DiscoveryTXTRecords(config.Config{}, 0)
	if len(records) < 3 {
		t.Fatal("expected at least 3 records")
	}
	want := "protocol_version=v1alpha1"
	if records[2] != want {
		t.Errorf("records[2] = %q, want %q", records[2], want)
	}
}

func TestDiscoveryTXTRecords_sets_pairing_required_true_when_no_devices(t *testing.T) {
	records := DiscoveryTXTRecords(config.Config{}, 0)
	if len(records) < 4 {
		t.Fatal("expected at least 4 records")
	}
	want := "pairing_required=true"
	if records[3] != want {
		t.Errorf("records[3] = %q, want %q", records[3], want)
	}
}

func TestDiscoveryTXTRecords_sets_pairing_required_false_when_devices_exist(t *testing.T) {
	records := DiscoveryTXTRecords(config.Config{}, 5)
	if len(records) < 4 {
		t.Fatal("expected at least 4 records")
	}
	want := "pairing_required=false"
	if records[3] != want {
		t.Errorf("records[3] = %q, want %q", records[3], want)
	}
}

func TestDiscoveryTXTRecords_returns_all_four_records(t *testing.T) {
	records := DiscoveryTXTRecords(config.Config{}, 0)
	if len(records) != 4 {
		t.Errorf("got %d records, want 4", len(records))
	}
}

func TestApplyPersistedSettings_applies_persisted_settings_when_store_has_them(t *testing.T) {
	preserveAndClearGatewayEnv(t)
	logger := testLogger(t)
	store := openTestStore(t)

	err := store.SaveGatewaySettings(context.Background(), storage.GatewaySettingsRecord{
		RegistryURL:   "https://stored.example/registry.json",
		PublicBaseURL: "https://stored.example.com",
		EnableLAN:     true,
		GatewayName:   "stored-gateway",
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		RegistryURL: "https://default.example/registry.json",
		GatewayName: "default-gateway",
	}

	updated := ApplyPersistedSettings(logger, store, cfg)

	if updated.RegistryURL != "https://stored.example/registry.json" {
		t.Errorf("RegistryURL = %q", updated.RegistryURL)
	}
	if updated.PublicBaseURL != "https://stored.example.com" {
		t.Errorf("PublicBaseURL = %q", updated.PublicBaseURL)
	}
	if !updated.EnableLAN {
		t.Error("EnableLAN should be true")
	}
	if updated.GatewayName != "stored-gateway" {
		t.Errorf("GatewayName = %q", updated.GatewayName)
	}
}

func TestApplyPersistedSettings_seeds_default_settings_when_store_is_empty(t *testing.T) {
	preserveAndClearGatewayEnv(t)
	logger := testLogger(t)
	store := openTestStore(t)

	cfg := config.Config{
		RegistryURL:   "https://default.example/registry.json",
		PublicBaseURL: "https://default.example.com",
		EnableLAN:     true,
		GatewayName:   "default-gateway",
	}

	_ = ApplyPersistedSettings(logger, store, cfg)

	record, err := store.GetGatewaySettings(context.Background())
	if err != nil {
		t.Fatalf("GetGatewaySettings: %v", err)
	}
	if record.RegistryURL != "https://default.example/registry.json" {
		t.Errorf("RegistryURL = %q", record.RegistryURL)
	}
	if record.PublicBaseURL != "https://default.example.com" {
		t.Errorf("PublicBaseURL = %q", record.PublicBaseURL)
	}
	if !record.EnableLAN {
		t.Error("EnableLAN should be true")
	}
	if record.GatewayName != "default-gateway" {
		t.Errorf("GatewayName = %q", record.GatewayName)
	}
}

func TestApplyPersistedSettings_does_not_mutate_returned_config_when_settings_exist(t *testing.T) {
	preserveAndClearGatewayEnv(t)
	logger := testLogger(t)
	store := openTestStore(t)

	err := store.SaveGatewaySettings(context.Background(), storage.GatewaySettingsRecord{
		RegistryURL:   "https://stored.example/registry.json",
		PublicBaseURL: "https://stored.example.com",
		EnableLAN:     true,
		GatewayName:   "stored-gateway",
	})
	if err != nil {
		t.Fatal(err)
	}

	original := config.Config{
		RegistryURL: "https://original.example/registry.json",
		GatewayName: "original-gateway",
	}

	_ = ApplyPersistedSettings(logger, store, original)

	if original.RegistryURL != "https://original.example/registry.json" {
		t.Error("original RegistryURL was mutated")
	}
	if original.GatewayName != "original-gateway" {
		t.Error("original GatewayName was mutated")
	}
}

func TestApplyPersistedSettings_warns_on_store_error(t *testing.T) {
	preserveAndClearGatewayEnv(t)
	logger := testLogger(t)
	store := openTestStore(t)
	store.Close()

	cfg := config.Config{
		RegistryURL: "https://original.example/registry.json",
		GatewayName: "original-gateway",
	}

	result := ApplyPersistedSettings(logger, store, cfg)

	if result.RegistryURL != "https://original.example/registry.json" {
		t.Error("RegistryURL was changed")
	}
	if result.GatewayName != "original-gateway" {
		t.Error("GatewayName was changed")
	}
}

func TestRun_shuts_down_on_context_cancellation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	t.Setenv("FERNGEIST_GATEWAY_LISTEN_ADDR", "127.0.0.1:0")
	t.Setenv("FERNGEIST_GATEWAY_ADMIN_ADDR", "127.0.0.1:0")
	t.Setenv("FERNGEIST_GATEWAY_STATE_DB", filepath.Join(t.TempDir(), "state.db"))
	t.Setenv("FERNGEIST_GATEWAY_LOG_DIR", t.TempDir())
	t.Setenv("FERNGEIST_GATEWAY_MANAGED_BIN_DIR", t.TempDir())
	t.Setenv("FERNGEIST_GATEWAY_ENABLE_LAN", "0")

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, api.BuildInfo{Version: "test"})
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Logf("Run returned: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Run to complete")
	}
}

func TestRun_shuts_down_cleanly_with_lan_enabled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	t.Setenv("FERNGEIST_GATEWAY_LISTEN_ADDR", "127.0.0.1:0")
	t.Setenv("FERNGEIST_GATEWAY_ADMIN_ADDR", "127.0.0.1:0")
	t.Setenv("FERNGEIST_GATEWAY_STATE_DB", filepath.Join(t.TempDir(), "state.db"))
	t.Setenv("FERNGEIST_GATEWAY_LOG_DIR", t.TempDir())
	t.Setenv("FERNGEIST_GATEWAY_MANAGED_BIN_DIR", t.TempDir())
	t.Setenv("FERNGEIST_GATEWAY_ENABLE_LAN", "1")

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, api.BuildInfo{Version: "test"})
	}()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Logf("Run returned: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Run to complete")
	}
}

func TestRun_returns_error_when_server_listen_fails(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	t.Setenv("FERNGEIST_GATEWAY_LISTEN_ADDR", "127.0.0.1:-1")
	t.Setenv("FERNGEIST_GATEWAY_ADMIN_ADDR", "127.0.0.1:-1")
	t.Setenv("FERNGEIST_GATEWAY_STATE_DB", filepath.Join(t.TempDir(), "state.db"))
	t.Setenv("FERNGEIST_GATEWAY_LOG_DIR", t.TempDir())
	t.Setenv("FERNGEIST_GATEWAY_MANAGED_BIN_DIR", t.TempDir())
	t.Setenv("FERNGEIST_GATEWAY_ENABLE_LAN", "0")

	ctx := context.Background()
	err := Run(ctx, api.BuildInfo{Version: "test"})
	if err == nil {
		t.Fatal("expected error from Run when listen fails")
	}
}

func TestRun_returns_error_when_state_db_unavailable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	dir := t.TempDir()
	dbDir := filepath.Join(dir, "statedb")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("FERNGEIST_GATEWAY_LISTEN_ADDR", "127.0.0.1:0")
	t.Setenv("FERNGEIST_GATEWAY_ADMIN_ADDR", "127.0.0.1:0")
	t.Setenv("FERNGEIST_GATEWAY_STATE_DB", dbDir)
	t.Setenv("FERNGEIST_GATEWAY_LOG_DIR", t.TempDir())
	t.Setenv("FERNGEIST_GATEWAY_MANAGED_BIN_DIR", t.TempDir())
	t.Setenv("FERNGEIST_GATEWAY_ENABLE_LAN", "0")

	ctx := context.Background()
	err := Run(ctx, api.BuildInfo{Version: "test"})
	if err == nil {
		t.Fatal("expected error from Run when state DB is a directory")
	}
	if !strings.Contains(err.Error(), "state database") {
		t.Errorf("error = %v, want error containing 'state database'", err)
	}
}

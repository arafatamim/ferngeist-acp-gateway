package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/acquire"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/api"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/catalog"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/config"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/discovery"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/gateway"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/logging"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
	acpregistry "github.com/arafatamim/ferngeist-acp-gateway/internal/registry"
	gatewayruntime "github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

// Run boots the full gateway daemon and blocks until the context is cancelled or
// one of the HTTP surfaces exits unexpectedly.
func Run(ctx context.Context, build api.BuildInfo) error {
	cfg := config.Load()
	logger, logSvc, err := logging.New(cfg.LogLevel, cfg.LogDir, cfg.LogMaxSize, cfg.LogMaxBackups)
	if err != nil {
		return fmt.Errorf("initialize logger: %w", err)
	}
	defer logSvc.Close()

	store, err := storage.Open(cfg.StateDBPath)
	if err != nil {
		return fmt.Errorf("open state database: %w", err)
	}
	defer store.Close()

	if err := store.DeleteAllRuntimeTokens(context.Background()); err != nil {
		return fmt.Errorf("clear stale runtime tokens: %w", err)
	}
	cfg = ApplyPersistedSettings(logger, store, cfg)

	registryClient := acpregistry.New(cfg.RegistryURL, 6*time.Hour)
	catalogSvc := catalog.NewWithBaseDirAndRegistry(".", registryClient)
	installer := acquire.New(logger, cfg.ManagedBinDir, store)
	runtimeSvc := gatewayruntime.NewSupervisorWithBaseDirAndInstaller(logger, ".", store, installer)
	pairingSvc := pairing.NewServiceWithOptions(logger, store, pairing.Options{
		ArmTTL:                 cfg.PairingArmTTL,
		CredentialTTL:          cfg.CredentialTTL,
		AllowDiagnosticsExport: cfg.AllowDiagnosticsExport,
		AllowRuntimeRestartEnv: cfg.AllowRuntimeRestartEnv,
	})
	gatewaySvc := gateway.New(logger, store)
	discoverySvc := discovery.New(logger)

	if cfg.EnableLAN {
		if _, portText, err := net.SplitHostPort(cfg.ListenAddr); err == nil {
			if port, parseErr := net.LookupPort("tcp", portText); parseErr == nil {
				if err := discoverySvc.Start(cfg.GatewayName, port, DiscoveryTXTRecords(cfg, pairingSvc.ActiveDeviceCount())); err != nil {
					logger.Warn("mdns discovery unavailable", slog.String("error", err.Error()))
				}
			}
		}
	}
	defer discoverySvc.Stop()

	server := api.NewServer(
		cfg,
		build,
		logger,
		catalogSvc,
		runtimeSvc,
		pairingSvc,
		gatewaySvc,
		discoverySvc,
		logSvc,
		registryClient,
	)

	logger.Info("starting gateway daemon",
		slog.String("listen_addr", cfg.ListenAddr),
		slog.String("admin_listen_addr", cfg.AdminListenAddr),
		slog.Bool("lan_enabled", cfg.EnableLAN),
	)

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown requested")
	case err = <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown failed: %w", err)
	}
	if err := runtimeSvc.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("runtime shutdown failed: %w", err)
	}

	logger.Info("gateway daemon stopped")
	return nil
}

// DiscoveryTXTRecords keeps the mDNS payload intentionally small and stable so
// Android can make fast pairing decisions without another round-trip.
func DiscoveryTXTRecords(cfg config.Config, pairedDeviceCount int) []string {
	return []string{
		"gateway_name=" + cfg.GatewayName,
		"gateway_version=dev",
		"protocol_version=v1alpha1",
		fmt.Sprintf("pairing_required=%t", pairedDeviceCount == 0),
	}
}

// ApplyPersistedSettings treats SQLite as the source of user defaults while
// still letting process-level environment variables win for local debugging and
// packaged deployments.
func ApplyPersistedSettings(logger *slog.Logger, store *storage.SQLiteStore, cfg config.Config) config.Config {
	record, err := store.GetGatewaySettings(context.Background())
	if err == nil {
		enableLAN := record.EnableLAN
		return cfg.ApplyPersistedSettings(config.PersistedSettings{
			RegistryURL:   record.RegistryURL,
			PublicBaseURL: record.PublicBaseURL,
			EnableLAN:     &enableLAN,
			GatewayName:   record.GatewayName,
		})
	}
	if !errors.Is(err, storage.ErrNotFound) {
		logger.Warn("failed to load gateway settings", slog.String("error", err.Error()))
	}

	if err := store.SaveGatewaySettings(context.Background(), storage.GatewaySettingsRecord{
		RegistryURL:   cfg.RegistryURL,
		PublicBaseURL: cfg.PublicBaseURL,
		EnableLAN:     cfg.EnableLAN,
		GatewayName:   cfg.GatewayName,
	}); err != nil {
		logger.Warn("failed to seed gateway settings", slog.String("error", err.Error()))
	}
	return cfg
}

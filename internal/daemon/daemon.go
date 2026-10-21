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
	"github.com/arafatamim/ferngeist-acp-gateway/internal/logging"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/push"
	acpregistry "github.com/arafatamim/ferngeist-acp-gateway/internal/registry"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/remote"
	gatewayruntime "github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/session"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/update"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/workspace"
)

// tsnetLogf receives the embedded Tailscale node's internal subsystem lines.
// Run points it at the daemon's structured logger so they surface as
// "component=tsnet" Debug entries instead of raw stderr text; the default
// drops them so the package-level seam needs no logger.
var tsnetLogf = func(string, ...any) {}

// tsnetUserLogf receives tsnet's user-facing lines (state path, login link,
// auth loop). Run points it at the daemon's structured logger at Info level —
// these are the lines an operator needs to act on, and without this sink
// tsnet prints them through the standard library logger in a different
// format.
var tsnetUserLogf = func(string, ...any) {}

// remoteProvision is the seam tests override to avoid real Tailscale.
var remoteProvision = func(ctx context.Context, cfg config.Config, localAddr string, port int, loginHook func(string)) (*remote.Result, error) {
	p := remote.NewProvisioner(remote.NewCLI(remote.ExecRunner{}), remote.NewTSNet)
	p.SetLogf(tsnetLogf)
	p.SetUserLogf(tsnetUserLogf)
	p.SetLoginHook(loginHook)
	return p.Provision(ctx, cfg, localAddr, port)
}

// Run boots the full gateway daemon and blocks until the context is cancelled or
// one of the HTTP surfaces exits unexpectedly.
func Run(ctx context.Context, build api.BuildInfo) error {
	cfg := config.Load()
	logger, logSvc, err := logging.New(cfg.LogLevel, cfg.LogDir, cfg.LogMaxSize, cfg.LogMaxBackups)
	if err != nil {
		return fmt.Errorf("initialize logger: %w", err)
	}
	routeTsnetLogs(logger)
	defer logSvc.Close()

	store, err := storage.Open(cfg.StateDBPath)
	if err != nil {
		return fmt.Errorf("open state database: %w", err)
	}
	defer store.Close()

	// Establish this gateway's stable instance id (generated once, persisted).
	// Clients receive it at pairing and use it to address the gateway and resolve
	// its pushes for deep-linking.
	gatewayID, err := store.EnsureGatewayID(context.Background())
	if err != nil {
		return fmt.Errorf("establish gateway id: %w", err)
	}
	cfg.GatewayID = gatewayID

	// Reconcile sessions orphaned by a previous shutdown.
	// Any session that was active or disconnected at shutdown is now stale
	// since its backing process no longer exists.
	if err := store.ReconcileSessionsOnStartup(context.Background()); err != nil {
		logger.Warn("failed to reconcile stale sessions on startup", slog.String("error", err.Error()))
	}

	cfg = ApplyPersistedSettings(logger, store, cfg)

	registryClient := acpregistry.New(cfg.RegistryURL, 6*time.Hour)
	defer registryClient.Close()
	catalogSvc := catalog.New(".", registryClient)
	catalogSvc.SetNpmResolver(catalog.ResolveNpmBinaryNames)
	// Custom agents live in SQLite; the catalog pulls them on every refresh so
	// client edits show up without a daemon restart.
	catalogSvc.SetCustomProvider(customAgentProvider(store, logger))
	installer := acquire.New(logger, cfg.ManagedBinDir, store)
	installer.SetJSRuntime(cfg.JSRuntime)
	installer.PruneNpm() // before any agent launches from an install dir
	runtimeSvc := gatewayruntime.NewSupervisor(logger, ".", store, installer)
	// Pre-boot the next process of each agent in use, so a connect skips the
	// agent's startup (see runtime.spareIdleTTL for how long one idles).
	runtimeSvc.EnableSpares()
	defer runtimeSvc.Close() // after serve's session shutdown, before store.Close
	pairingSvc := pairing.NewServiceWithOptions(logger, store, pairing.Options{
		ArmTTL:                 cfg.PairingArmTTL,
		CredentialTTL:          cfg.CredentialTTL,
		GracePeriod:            cfg.CredentialGracePeriod,
		AllowDiagnosticsExport: cfg.AllowDiagnosticsExport,
		AllowRuntimeRestartEnv: cfg.AllowRuntimeRestartEnv,
	})
	tokenSvc := session.NewTokenService(logger)
	pushSvc, vapidPublicKey, err := newPushService(logger, store)
	if err != nil {
		return fmt.Errorf("establish web push keys: %w", err)
	}
	cfg.VAPIDPublicKey = vapidPublicKey

	access := newRemoteAccess(ctx, &cfg, store, pushSvc, logger)
	startUpdateNotifier(ctx, cfg, build, pushSvc, store)
	// The push path only needs the branch behind a chat's cwd, so the session
	// layer gets that one workspace lookup rather than the whole workspace
	// service, whose full instance (with its chat callback) the API layer
	// builds. This accessor-only instance shares the store and its BranchOf is
	// a read, so the two never contend.
	worktrees := workspace.New(func() *storage.SQLiteStore { return store }, logger, time.Now, nil)
	sessionSvc := session.NewRuntimeSession(logger, store, runtimeSvc, tokenSvc, session.Config{
		MaxDisconnected:    cfg.SessionMaxDisconnected,
		MaxPerDevice:       cfg.MaxSessionsPerDevice,
		ProgressInterval:   cfg.ProgressInterval,
		PushSvc:            pushSvc,
		BranchOf:           worktrees.BranchOf,
		GatewayID:          cfg.GatewayID,
		FrameLogEnabled:    cfg.FrameLogEnabled,
		FrameLogDir:        cfg.LogDir,
		FrameLogMaxSize:    cfg.LogMaxSize,
		FrameLogMaxBackups: cfg.LogMaxBackups,
	})
	discoverySvc := discovery.New(logger)

	if cfg.EnableLAN {
		startDiscovery(cfg, discoverySvc, pairingSvc, logger)
	}
	defer discoverySvc.Stop()

	server := api.NewServer(api.Deps{
		Config:    cfg,
		Build:     build,
		Logger:    logger,
		Catalog:   catalogSvc,
		Runtime:   runtimeSvc,
		Pairing:   pairingSvc,
		Discovery: discoverySvc,
		Logs:      logSvc,
		Registry:  registryClient,
		Store:     store,
		Session:   sessionSvc,
	})
	if access != nil {
		server.SetRemoteSetup(access.Snapshot)
		if cfg.TailscaleAuthKey == "" {
			// Asynchronous: first boot with a human login, or a tailnet that
			// still needs HTTPS/Funnel enabled. Start owns the retry loop and
			// hands the live public URL to the status path on success, so
			// status reports it without a restart.
			access.Start(ctx, server.SetPublicURL)
		}
	}

	logger.Info("starting gateway daemon",
		slog.String("listen_addr", cfg.ListenAddr),
		slog.String("admin_listen_addr", cfg.AdminListenAddr),
		slog.Bool("lan_enabled", cfg.EnableLAN),
	)
	return serve(ctx, server, access, sessionSvc, logger)
}

// serve runs the API server until ctx is cancelled or the server fails, then
// drains. A ListenAndServe failure still runs the whole shutdown sequence (HTTP
// drain, remote close, session shutdown): returning early would orphan the
// agent processes. The serve error is reported after cleanup completes.
func serve(ctx context.Context, server *api.Server, access *remote.Access, sessionSvc *session.RuntimeSession, logger *slog.Logger) error {
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()

	var serveErr error
	select {
	case <-ctx.Done():
		logger.Info("shutdown requested")
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("api server failed; draining before exit", slog.String("error", err.Error()))
			serveErr = err
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var shutdownErr error
	// Drain HTTP first so no request starts an agent during teardown, then
	// stop remote access.
	if err := server.Shutdown(shutdownCtx); err != nil {
		shutdownErr = fmt.Errorf("graceful shutdown failed: %w", err)
	}
	if access != nil {
		if err := access.Close(); err != nil {
			logger.Warn("remote access shutdown", slog.String("error", err.Error()))
		}
	}
	// Stop the session service (reaper, inbound diagnostic writer, and all
	// session pumps), which also stops every agent process it owns.
	sessionSvc.Shutdown()

	logger.Info("gateway daemon stopped")
	return errors.Join(serveErr, shutdownErr)
}

// routeTsnetLogs sends the embedded Tailscale node's chatter into the
// structured log: subsystem lines at Debug (diagnostic; a fresh node is
// created on every retry attempt), user-facing lines at Info (state path,
// login link, auth loop — the operator needs these). Without the second sink
// tsnet prints the user-facing lines via the standard library logger in a
// different format.
func routeTsnetLogs(logger *slog.Logger) {
	tsnetLogf = func(format string, args ...any) {
		logger.Debug("tsnet", slog.String("detail", fmt.Sprintf(format, args...)))
	}
	tsnetUserLogf = func(format string, args ...any) {
		logger.Info("tsnet", slog.String("detail", fmt.Sprintf(format, args...)))
	}
}

// customAgentProvider feeds the catalog the custom agents stored in SQLite.
func customAgentProvider(store *storage.SQLiteStore, logger *slog.Logger) func() []catalog.CustomAgent {
	return func() []catalog.CustomAgent {
		records, err := store.ListCustomAgents(context.Background())
		if err != nil {
			logger.Warn("list custom agents failed", slog.String("error", err.Error()))
			return nil
		}
		out := make([]catalog.CustomAgent, 0, len(records))
		for _, record := range records {
			out = append(out, catalog.CustomAgent{
				ID:          record.ID,
				DisplayName: record.DisplayName,
				Command:     record.Command,
				Args:        record.Args,
				Hint:        record.Hint,
			})
		}
		return out
	}
}

// newRemoteAccess sets up remote access provisioning: Tailscale CLI when
// available, embedded tsnet node otherwise. Best-effort — a provisioning
// failure never prevents the daemon from booting. With an auth key login is
// non-interactive, so it provisions now and records the URL in cfg before the
// API server starts; otherwise the caller runs Start's background retry loop,
// so fixing a tailnet setting is enough — no restart. Returns nil when remote
// access is off or the listen address is unusable.
func newRemoteAccess(ctx context.Context, cfg *config.Config, store *storage.SQLiteStore, pushSvc push.PushService, logger *slog.Logger) *remote.Access {
	if cfg.TailscaleMode == "off" {
		return nil
	}
	port, err := portOf(cfg.ListenAddr)
	if err != nil {
		logger.Warn("remote access disabled: invalid listen address", slog.String("error", err.Error()))
		return nil
	}
	access := remote.New(remoteProvision, store, pushSvc, logger, *cfg, port)
	if cfg.TailscaleAuthKey != "" {
		// Failures here are configuration errors — log once, don't retry.
		if res, err := access.Provision(ctx); err != nil {
			remote.LogSetupIssue(logger, err)
		} else if res != nil {
			cfg.PublicBaseURL = res.URL
		}
	}
	return access
}

// startUpdateNotifier runs the periodic update-available check: it fetches the
// latest stable release and pushes a notification to paired devices when a
// newer version exists. It never applies the update — the user runs
// `ferngeist-gateway update`. Package-manager builds (apt, pacman, brew) are
// updated by their package manager, so they never announce it.
func startUpdateNotifier(ctx context.Context, cfg config.Config, build api.BuildInfo, pushSvc push.PushService, store *storage.SQLiteStore) {
	selfUpdating := build.UpdateChannel == "" || build.UpdateChannel == "self"
	if !cfg.UpdateCheckEnabled || !selfUpdating {
		return
	}
	checker := update.NewChecker("arafatamim/ferngeist-acp-gateway")
	notifier := update.NewNotifier(checker, pushSvc, store.GetPairedDeviceIDs)
	notifier.Interval = cfg.UpdateCheckInterval
	go notifier.Run(ctx, build.Version)
}

// startDiscovery advertises the gateway over mDNS on the LAN.
func startDiscovery(cfg config.Config, discoverySvc *discovery.Service, pairingSvc *pairing.Service, logger *slog.Logger) {
	port, err := portOf(cfg.ListenAddr)
	if err != nil {
		return
	}
	if err := discoverySvc.Start(cfg.GatewayName, port, DiscoveryTXTRecords(cfg, pairingSvc.ActiveDeviceCount())); err != nil {
		logger.Warn("mdns discovery unavailable", slog.String("error", err.Error()))
	}
}

// portOf extracts the TCP port from a listen address like "127.0.0.1:5788".
func portOf(addr string) (int, error) {
	_, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}
	return net.LookupPort("tcp", portText)
}

// newPushService builds the platform-neutral push dispatcher with the Web Push
// provider, signing with this gateway's VAPID key (generated once, persisted).
// It returns the public key clients subscribe against. Web Push needs no
// third-party credentials, so delivery is always on.
func newPushService(logger *slog.Logger, store *storage.SQLiteStore) (push.PushService, string, error) {
	pushLogger := logger.With("component", "push")
	publicKey, privateKey, err := store.EnsureVAPIDKeys(context.Background(), push.GenerateVAPIDKeys)
	if err != nil {
		return nil, "", err
	}
	return push.NewDispatcher(store, map[string]push.Provider{
		push.PlatformWebPush: push.NewWebPushProvider(publicKey, privateKey, pushLogger),
	}, pushLogger), publicKey, nil
}

// DiscoveryTXTRecords keeps the mDNS payload intentionally small and stable so
// Android can make fast pairing decisions without another round-trip.
func DiscoveryTXTRecords(cfg config.Config, pairedDeviceCount int) []string {
	return []string{
		"gateway_name=" + cfg.GatewayName,
		"gateway_version=dev",
		"protocol_version=" + api.ProtocolVersion,
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

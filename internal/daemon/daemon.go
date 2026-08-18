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
	"github.com/arafatamim/ferngeist-acp-gateway/internal/push"
	acpregistry "github.com/arafatamim/ferngeist-acp-gateway/internal/registry"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/remote"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/remoteaccess"
	gatewayruntime "github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/session"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/token"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/update"
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

// remoteResultCh delivers the provisioned result from the asynchronous path to
// the shutdown sequence without racing on the shared variable.
var remoteResultCh = make(chan *remote.Result, 1)

// remoteURLCh delivers the provisioned public URL from the asynchronous path
// to the status path, so remoteStatus reports the live URL without a restart.
var remoteURLCh = make(chan string, 1)

// remoteRetryInterval is the pause between background provisioning attempts
// while the operator fixes tailnet settings (HTTPS/Funnel). Overridden in
// tests to keep them fast.
var remoteRetryInterval = 15 * time.Second

// Run boots the full gateway daemon and blocks until the context is cancelled or
// one of the HTTP surfaces exits unexpectedly.
func Run(ctx context.Context, build api.BuildInfo) error {
	cfg := config.Load()
	logger, logSvc, err := logging.New(cfg.LogLevel, cfg.LogDir, cfg.LogMaxSize, cfg.LogMaxBackups)
	if err != nil {
		return fmt.Errorf("initialize logger: %w", err)
	}
	// Route the embedded Tailscale node's chatter into the structured log:
	// subsystem lines at Debug (diagnostic; a fresh node is created on every
	// retry attempt), user-facing lines at Info (state path, login link, auth
	// loop — the operator needs these). Without the second sink tsnet prints
	// the user-facing lines via the standard library logger in a different
	// format.
	tsnetLogf = func(format string, args ...any) {
		logger.Debug("tsnet", slog.String("detail", fmt.Sprintf(format, args...)))
	}
	tsnetUserLogf = func(format string, args ...any) {
		logger.Info("tsnet", slog.String("detail", fmt.Sprintf(format, args...)))
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
	catalogSvc := catalog.NewWithBaseDirAndRegistry(".", registryClient)
	catalogSvc.SetNpmResolver(catalog.ResolveNpmBinaryNames)
	installer := acquire.New(logger, cfg.ManagedBinDir, store)
	runtimeSvc := gatewayruntime.NewSupervisorWithBaseDirAndInstaller(logger, ".", store, installer)
	pairingSvc := pairing.NewServiceWithOptions(logger, store, pairing.Options{
		ArmTTL:                 cfg.PairingArmTTL,
		CredentialTTL:          cfg.CredentialTTL,
		GracePeriod:            cfg.CredentialGracePeriod,
		AllowDiagnosticsExport: cfg.AllowDiagnosticsExport,
		AllowRuntimeRestartEnv: cfg.AllowRuntimeRestartEnv,
	})
	gatewaySvc := gateway.New(logger, store)
	tokenSvc := token.New(logger)
	pushSvc := newPushService(ctx, logger, store, cfg.FCMCredentialsFile)

	// Remote access provisioning: Tailscale CLI when available, embedded tsnet
	// node otherwise. Best-effort — a provisioning failure never prevents the
	// daemon from booting. Synchronous when an auth key makes login
	// non-interactive (the URL is then known before the API server starts);
	// otherwise asynchronous so first boot with a human login isn't blocked.
	var remoteResult *remote.Result
	var access *remoteaccess.Access
	if cfg.TailscaleMode != "off" {
		port, parseErr := portOf(cfg.ListenAddr)
		if parseErr != nil {
			logger.Warn("remote access disabled: invalid listen address", slog.String("error", parseErr.Error()))
		} else {
			access = remoteaccess.New(remoteProvision, store, pushSvc, logger, cfg, port)
			if cfg.TailscaleAuthKey != "" {
				// Synchronous: an auth key makes login non-interactive, so the
				// URL is known before the API server starts. Failures here are
				// configuration errors — log once, don't retry.
				if res, err := access.Provision(ctx); err != nil {
					remoteaccess.LogSetupIssue(logger, err)
				} else if res != nil {
					remoteResult = res
					// Known before the server starts: report locally this boot too.
					cfg.PublicBaseURL = res.URL
				}
			} else {
				// Asynchronous: first boot with a human login, or a tailnet
				// that still needs HTTPS/Funnel enabled. Retry on a slow timer
				// so fixing the setting in the browser is enough — the gateway
				// picks up on its own, no restart. The actionable hint is
				// logged once; later attempts stay quiet until one succeeds.
				// Capture the interval on the main goroutine: tests swap the
				// package var while Run is live, and reading it from the
				// retry loop would race the write.
				retryInterval := remoteRetryInterval
				go func() {
					attempt := 0
					for {
						res, err := access.Provision(ctx)
						if err == nil && res != nil {
							remoteResultCh <- res
							// Also hand the URL to the status path: remoteStatus
							// must report the provisioned URL without a restart.
							select {
							case remoteURLCh <- res.URL:
							default:
							}
							return
						}
						attempt++
						if attempt == 1 {
							remoteaccess.LogSetupIssue(logger, err)
						} else if err != nil {
							logger.Debug("remote access retry pending", slog.String("error", err.Error()))
						}
						select {
						case <-ctx.Done():
							return
						case <-time.After(retryInterval):
						}
					}
				}()
			}
		}
	}

	// Periodic update-available check: fetch the latest stable release and push
	// a notification to paired devices when a newer version exists. Never
	// applies the update — the user runs `ferngeist-gateway update`.
	if cfg.UpdateCheckEnabled {
		checker := update.NewChecker("arafatamim/ferngeist-acp-gateway")
		notifier := update.NewNotifier(checker, pushSvc, store.GetPairedDeviceIDs)
		notifier.Interval = cfg.UpdateCheckInterval
		go notifier.Run(ctx, build.Version)
	}
	sessionSvc := session.NewRuntimeSession(logger, store, runtimeSvc, tokenSvc, session.Config{
		MaxDisconnected:    cfg.SessionMaxDisconnected,
		MaxPerDevice:       cfg.MaxSessionsPerDevice,
		ProgressInterval:   cfg.ProgressInterval,
		PushSvc:            pushSvc,
		GatewayID:          cfg.GatewayID,
		FrameLogEnabled:    cfg.FrameLogEnabled,
		FrameLogDir:        cfg.LogDir,
		FrameLogMaxSize:    cfg.LogMaxSize,
		FrameLogMaxBackups: cfg.LogMaxBackups,
	})
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
		store,
		sessionSvc,
	)
	if access != nil {
		server.SetRemoteSetup(func() *remote.RemoteSetupSnapshot { return access.Snapshot() })
	}

	// Apply the asynchronously provisioned public URL to the server once it
	// arrives: status must report the live remote URL without a restart.
	go func() {
		select {
		case publicURL := <-remoteURLCh:
			server.SetPublicURL(publicURL)
		case <-ctx.Done():
		}
	}()

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
	// Stop remote access first so no new inbound traffic arrives while the API
	// server drains. An in-flight successful provisioning gets a short grace
	// window to hand off its result; a provision still waiting on interactive
	// login cannot be cancelled (tsnet's Up is not ctx-cancellable) and is
	// simply abandoned — the process exit cleans it up.
	select {
	case res := <-remoteResultCh:
		remoteResult = res
	case <-time.After(500 * time.Millisecond):
	}
	if remoteResult != nil && remoteResult.Close != nil {
		if err := remoteResult.Close(); err != nil {
			logger.Warn("remote access shutdown", slog.String("error", err.Error()))
		}
	}
	// Stop the session service first (reaper, inbound diagnostic writer, and all
	// session pumps) while the backing runtimes are still alive, then stop the
	// runtimes. Skipping this leaks the reaper and inbound-writer goroutines.
	sessionSvc.Shutdown()
	if err := runtimeSvc.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("runtime shutdown failed: %w", err)
	}

	logger.Info("gateway daemon stopped")
	return nil
}

// portOf extracts the TCP port from a listen address like "127.0.0.1:5788".
func portOf(addr string) (int, error) {
	_, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}
	return net.LookupPort("tcp", portText)
}

// newPushService builds the platform-neutral push dispatcher and registers the
// Android delivery provider: FCM HTTP v1 when a service-account credentials file
// is configured, otherwise a log-only provider. A misconfigured credentials file
// is non-fatal — the daemon logs the error and degrades to the log provider so a
// bad push config never prevents the gateway from booting. Additional platforms
// (iOS/web) register more providers here without touching the dispatcher or the
// session layer.
func newPushService(ctx context.Context, logger *slog.Logger, store *storage.SQLiteStore, credentialsFile string) push.PushService {
	pushLogger := logger.With("component", "push")

	var androidProvider push.Provider
	if credentialsFile == "" {
		logger.Info("push notifications: no FCM credentials configured, using log-only provider")
		androidProvider = push.NewLogProvider(pushLogger)
	} else if fcm, err := push.NewFCMProvider(ctx, credentialsFile, pushLogger); err != nil {
		logger.Warn("push notifications: FCM init failed, falling back to log-only provider",
			slog.String("error", err.Error()))
		androidProvider = push.NewLogProvider(pushLogger)
	} else {
		logger.Info("push notifications: FCM HTTP v1 delivery enabled")
		androidProvider = fcm
	}

	return push.NewDispatcher(store, map[string]push.Provider{
		"android": androidProvider,
	}, pushLogger)
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

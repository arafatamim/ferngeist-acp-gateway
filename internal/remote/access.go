package remote

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/config"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/push"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

// ProvisionFunc establishes remote access. loginHook fires once per pending
// interactive Tailscale login with the URL the operator must open — the
// module owns the hook so the caller never touches snapshot state directly.
type ProvisionFunc func(ctx context.Context, cfg config.Config, localAddr string, port int, loginHook func(string)) (*Result, error)

// RetryInterval is the pause between background provisioning attempts while
// the operator fixes tailnet settings (HTTPS/Funnel). Overridden in tests to
// keep them fast.
var RetryInterval = 15 * time.Second

// closeGrace bounds how long Close waits for an in-flight provisioning to hand
// off its result.
const closeGrace = 500 * time.Millisecond

// errAccessClosed is returned by a provisioning that finished after Close.
var errAccessClosed = errors.New("remote access closed")

// Access provisions remote access and manages its lifecycle: it owns the
// retry loop, persists the public URL, notifies paired devices, and surfaces
// provisioning state (auth pending, setup blockers) to the status API.
type Access struct {
	provision ProvisionFunc
	store     *storage.SQLiteStore
	pushSvc   push.PushService
	logger    *slog.Logger
	cfg       config.Config
	port      int

	snapshot atomic.Pointer[RemoteSetupSnapshot]
	cancel   context.CancelFunc
	done     chan struct{}

	mu     sync.Mutex
	result *Result // the provisioned node, closed by Close
	closed bool    // set by Close; a later provision closes its own result
}

// New creates an Access module. provision is the seam for tests; in production
// it wraps Provisioner with logging hooks.
func New(provision ProvisionFunc, store *storage.SQLiteStore, pushSvc push.PushService, logger *slog.Logger, cfg config.Config, port int) *Access {
	return &Access{
		provision: provision,
		store:     store,
		pushSvc:   pushSvc,
		logger:    logger,
		cfg:       cfg,
		port:      port,
	}
}

// loginHook stores the auth-required snapshot so the status API can surface
// the login link while the node stays alive for the human to finish logging in.
func (a *Access) loginHook(authURL string) {
	a.logger.Info("remote access waiting on one-time Tailscale login", slog.String("url", authURL))
	a.snapshot.Store(&RemoteSetupSnapshot{AuthRequired: true, AuthURL: authURL})
}

// Provision calls the underlying provisioner, then persists the public URL
// and notifies paired devices on success. On any error (including pending
// interactive login), the provisioning snapshot is stored for the status API.
func (a *Access) Provision(ctx context.Context) (*Result, error) {
	res, err := a.provision(ctx, a.cfg, a.cfg.ListenAddr, a.port, a.loginHook)
	if err != nil {
		a.snapshot.Store(snapshotFor(err))
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		// Close has already run and will not see this result, so close it
		// here rather than leak the node.
		if res.Close != nil {
			_ = res.Close()
		}
		return nil, errAccessClosed
	}
	a.result = res
	a.mu.Unlock()

	// Persist the public URL so the daemon reports it on restart without
	// re-provisioning.
	if a.store != nil {
		if rec, saveErr := a.store.GetGatewaySettings(ctx); saveErr == nil {
			rec.PublicBaseURL = res.URL
			if saveErr := a.store.SaveGatewaySettings(ctx, rec); saveErr != nil {
				a.logger.Warn("failed to persist public url", slog.String("error", saveErr.Error()))
			}
		}
	}

	// Notify paired devices of the new URL so they can reconnect.
	if a.store != nil && a.pushSvc != nil {
		if deviceIDs, listErr := a.store.GetPairedDeviceIDs(ctx); listErr != nil {
			a.logger.Warn("failed to list paired devices for url push", slog.String("error", listErr.Error()))
		} else {
			for _, deviceID := range deviceIDs {
				if pushErr := a.pushSvc.Notify(ctx, deviceID, push.Notification{
					Title:    "Gateway online",
					Body:     res.URL,
					Category: push.CategoryGatewayURL,
					ServerID: a.cfg.GatewayID,
				}); pushErr != nil {
					a.logger.Warn("failed to push gateway url", slog.String("error", pushErr.Error()))
				}
			}
		}
	}

	a.logger.Info("remote access ready", slog.String("url", res.URL), slog.String("mode", res.Mode))
	a.snapshot.Store(nil) // clear any prior blocker
	return res, nil
}

// Start launches the background retry loop that provisions remote access until
// it succeeds, so fixing a tailnet setting in the browser is enough — the
// gateway picks up on its own, no restart. The first failure is logged with
// its actionable hint; later attempts stay quiet until one succeeds. onURL is
// invoked with the public URL as soon as provisioning succeeds.
func (a *Access) Start(ctx context.Context, onURL func(string)) {
	// Capture the interval on the calling goroutine: tests swap the package
	// var while Start is live, and reading it from the loop would race.
	retryInterval := RetryInterval
	ctx, a.cancel = context.WithCancel(ctx)
	a.done = make(chan struct{})
	go func() {
		defer close(a.done)
		for attempt := 1; ; attempt++ {
			res, err := a.Provision(ctx)
			if errors.Is(err, errAccessClosed) {
				return
			}
			if err == nil && res != nil {
				if onURL != nil {
					onURL(res.URL)
				}
				return
			}
			if attempt == 1 {
				LogSetupIssue(a.logger, err)
			} else if err != nil {
				a.logger.Debug("remote access retry pending", slog.String("error", err.Error()))
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(retryInterval):
			}
		}
	}()
}

// Close stops the retry loop and closes a provisioned result. An in-flight
// successful provisioning gets a short grace window to hand off its result; one
// that finishes later closes its own result. A provision still waiting on
// interactive login cannot be cancelled (tsnet's Up is not ctx-cancellable)
// and is abandoned — process exit cleans it up.
func (a *Access) Close() error {
	if a.cancel != nil {
		a.cancel()
		select {
		case <-a.done:
		case <-time.After(closeGrace):
		}
	}
	a.mu.Lock()
	a.closed = true
	res := a.result
	a.result = nil
	a.mu.Unlock()
	if res != nil && res.Close != nil {
		return res.Close()
	}
	return nil
}

// Snapshot returns the current provisioning state for the status API. Nil
// means no pending issue — provisioning either succeeded or hasn't started.
func (a *Access) Snapshot() *RemoteSetupSnapshot {
	return a.snapshot.Load()
}

// URL returns the URL of the currently provisioned remote access, if any.
func (a *Access) URL() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.result == nil {
		return ""
	}
	return a.result.URL
}

// snapshotFor converts a provisioning error into a status-API snapshot.
func snapshotFor(err error) *RemoteSetupSnapshot {
	snap := &RemoteSetupSnapshot{}
	var authErr *ErrAuthRequired
	if errors.As(err, &authErr) {
		snap.AuthRequired = true
		snap.AuthURL = authErr.AuthURL
	} else if err != nil {
		snap.Hint = FunnelSetupHint(err)
		snap.LastErr = err.Error()
	}
	return snap
}

// LogSetupIssue reports a provisioning failure, upgrading to plain-English
// guidance when the error is a tailnet setting the operator must change.
func LogSetupIssue(logger *slog.Logger, err error) {
	if err == nil {
		return
	}
	if hint := FunnelSetupHint(err); hint != "" {
		logger.Warn("remote access waiting on a tailnet setting",
			slog.String("hint", hint),
			slog.String("error", err.Error()))
		return
	}
	logger.Warn("remote access unavailable", slog.String("error", err.Error()))
}

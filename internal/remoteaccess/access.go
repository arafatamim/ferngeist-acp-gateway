// Package remoteaccess provisions outbound-only remote access for the gateway
// and manages its lifecycle: detect adapter (Tailscale CLI or embedded tsnet),
// provision funnel, persist the public URL, and notify paired devices.
package remoteaccess

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/config"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/push"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/remote"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

// ProvisionFunc establishes remote access. loginHook fires once per pending
// interactive Tailscale login with the URL the operator must open — the
// module owns the hook so the caller never touches snapshot state directly.
type ProvisionFunc func(ctx context.Context, cfg config.Config, localAddr string, port int, loginHook func(string)) (*remote.Result, error)

// Access provisions remote access and manages its side effects: persisting
// the public URL, notifying paired devices, and surfacing provisioning state
// (auth pending, setup blockers) to the status API.
type Access struct {
	provision ProvisionFunc
	store     *storage.SQLiteStore
	pushSvc   push.PushService
	logger    *slog.Logger
	cfg       config.Config
	port      int
	snapshot  atomic.Pointer[remote.RemoteSetupSnapshot]
}

// loginHook stores the auth-required snapshot so the status API can surface
// the login link while the node stays alive for the human to finish logging in.
func (a *Access) loginHook(authURL string) {
	a.logger.Info("remote access waiting on one-time Tailscale login", slog.String("url", authURL))
	a.snapshot.Store(&remote.RemoteSetupSnapshot{AuthRequired: true, AuthURL: authURL})
}

// New creates an Access module. provision is the seam for tests; in production
// it wraps remote.Provisioner with logging hooks.
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

// Provision calls the underlying provisioner, then persists the public URL
// and notifies paired devices on success. On any error (including pending
// interactive login), the provisioning snapshot is stored for the status API.
func (a *Access) Provision(ctx context.Context) (*remote.Result, error) {
	res, err := a.provision(ctx, a.cfg, a.cfg.ListenAddr, a.port, a.loginHook)
	if err != nil {
		a.snapshot.Store(snapshotFor(err))
		return nil, err
	}
	if res == nil {
		return nil, nil
	}

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

// Snapshot returns the current provisioning state for the status API. Nil
// means no pending issue — provisioning either succeeded or hasn't started.
func (a *Access) Snapshot() *remote.RemoteSetupSnapshot {
	return a.snapshot.Load()
}

// snapshotFor converts a provisioning error into a status-API snapshot.
func snapshotFor(err error) *remote.RemoteSetupSnapshot {
	snap := &remote.RemoteSetupSnapshot{}
	var authErr *remote.ErrAuthRequired
	if errors.As(err, &authErr) {
		snap.AuthRequired = true
		snap.AuthURL = authErr.AuthURL
	} else if err != nil {
		snap.Hint = remote.FunnelSetupHint(err)
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
	if hint := remote.FunnelSetupHint(err); hint != "" {
		logger.Warn("remote access waiting on a tailnet setting",
			slog.String("hint", hint),
			slog.String("error", err.Error()))
		return
	}
	logger.Warn("remote access unavailable", slog.String("error", err.Error()))
}

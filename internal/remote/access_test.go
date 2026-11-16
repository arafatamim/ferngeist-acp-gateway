package remote

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/config"
)

func TestProvisionSuccessClearsSnapshot(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	provisioned := false

	a := New(
		func(ctx context.Context, cfg config.Config, localAddr string, port int, _ func(string)) (*Result, error) {
			provisioned = true
			return &Result{Mode: "tsnet", URL: "https://gw.tail.ts.net"}, nil
		},
		nil, nil, logger, config.Config{}, 5788,
	)

	res, err := a.Provision(context.Background())
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !provisioned {
		t.Fatal("provisioner was not called")
	}
	if res.URL != "https://gw.tail.ts.net" {
		t.Fatalf("URL = %q", res.URL)
	}
	if a.Snapshot().State != "ready" {
		t.Fatal("snapshot should report ready after success")
	}
}

// A provisioning that outlives Close's grace window must close its own result:
// Close has already run and nothing else would release the node.
func TestProvisionAfterCloseClosesItsResult(t *testing.T) {
	release := make(chan struct{})
	closed := make(chan struct{})
	a := New(
		func(context.Context, config.Config, string, int, func(string)) (*Result, error) {
			<-release
			return &Result{URL: "https://gw.tail.ts.net", Close: func() error {
				close(closed)
				return nil
			}}, nil
		},
		nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), config.Config{}, 5788,
	)
	a.Start(context.Background(), nil)
	if err := a.Close(); err != nil { // returns after the grace window
		t.Fatalf("Close: %v", err)
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("late provisioning result was never closed")
	}
}

func TestProvisionAuthRequiredStoresSnapshot(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(
		func(ctx context.Context, cfg config.Config, localAddr string, port int, _ func(string)) (*Result, error) {
			return nil, &ErrAuthRequired{AuthURL: "https://login.tailscale.com/a/test"}
		},
		nil, nil, logger, config.Config{}, 5788,
	)

	_, err := a.Provision(context.Background())
	if err == nil {
		t.Fatal("expected error from auth-required provision")
	}
	snap := a.Snapshot()
	if snap == nil {
		t.Fatal("snapshot should be non-nil on auth required")
	}
	if !snap.AuthRequired {
		t.Fatal("AuthRequired should be true")
	}
	if snap.AuthURL != "https://login.tailscale.com/a/test" {
		t.Fatalf("AuthURL = %q", snap.AuthURL)
	}
}

func TestProvisionHintStoresSnapshot(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(
		func(ctx context.Context, cfg config.Config, localAddr string, port int, _ func(string)) (*Result, error) {
			return nil, errors.New("tsnet funnel: Funnel not available; HTTPS must be enabled. See https://tailscale.com/s/https.")
		},
		nil, nil, logger, config.Config{}, 5788,
	)

	_, err := a.Provision(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	snap := a.Snapshot()
	if snap == nil {
		t.Fatal("snapshot should be non-nil on hint")
	}
	if snap.Hint == "" {
		t.Fatal("Hint should be populated for HTTPS-not-enabled error")
	}
	if snap.LastErr == "" {
		t.Fatal("LastErr should be populated")
	}
}

func TestProvisionNilResultNoSideEffects(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(
		func(ctx context.Context, cfg config.Config, localAddr string, port int, _ func(string)) (*Result, error) {
			return nil, nil // disabled
		},
		nil, nil, logger, config.Config{}, 5788,
	)

	res, err := a.Provision(context.Background())
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res != nil {
		t.Fatalf("result should be nil for disabled mode, got %+v", res)
	}
	if a.Snapshot().State != "pending" {
		t.Fatal("snapshot should remain pending when provision returns nil")
	}
}

func TestProvisionClearsPriorSnapshot(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	call := 0
	a := New(
		func(ctx context.Context, cfg config.Config, localAddr string, port int, _ func(string)) (*Result, error) {
			call++
			if call == 1 {
				return nil, &ErrAuthRequired{AuthURL: "https://login.tailscale.com/a/pending"}
			}
			return &Result{Mode: "tsnet", URL: "https://gw.tail.ts.net"}, nil
		},
		nil, nil, logger, config.Config{}, 5788,
	)

	// First call: auth required → snapshot stored
	_, err := a.Provision(context.Background())
	if err == nil {
		t.Fatal("expected error from auth-required provision")
	}
	if snap := a.Snapshot(); snap == nil || !snap.AuthRequired {
		t.Fatal("first call should store auth-required snapshot")
	}

	// Second call: success → snapshot cleared
	if _, err := a.Provision(context.Background()); err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if a.Snapshot().State != "ready" {
		t.Fatal("snapshot should report ready after successful provision")
	}
}

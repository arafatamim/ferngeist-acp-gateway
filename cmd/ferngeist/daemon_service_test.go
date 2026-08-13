package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/adminclient"
)

func TestWaitForRemoteSetup_returns_login_link_when_auth_pending(t *testing.T) {
	fetch := func(context.Context) (adminclient.DaemonStatus, error) {
		return adminclient.DaemonStatus{Remote: adminclient.RemoteStatus{
			AuthRequired: true,
			AuthURL:      "https://login.tailscale.com/a/clitest",
		}}, nil
	}
	status, err := waitForRemoteSetup(context.Background(), fetch, 2*time.Second)
	if err != nil {
		t.Fatalf("waitForRemoteSetup returned error: %v", err)
	}
	if !status.Remote.AuthRequired || status.Remote.AuthURL != "https://login.tailscale.com/a/clitest" {
		t.Fatalf("Remote = %+v, want auth-required with the login link", status.Remote)
	}
}

func TestWaitForRemoteSetup_returns_public_url_when_ready(t *testing.T) {
	fetch := func(context.Context) (adminclient.DaemonStatus, error) {
		return adminclient.DaemonStatus{Remote: adminclient.RemoteStatus{
			PublicURL: "https://gw.tail1234.ts.net",
		}}, nil
	}
	status, err := waitForRemoteSetup(context.Background(), fetch, 2*time.Second)
	if err != nil {
		t.Fatalf("waitForRemoteSetup returned error: %v", err)
	}
	if status.Remote.PublicURL != "https://gw.tail1234.ts.net" {
		t.Fatalf("PublicURL = %q, want the ready URL", status.Remote.PublicURL)
	}
}

func TestWaitForRemoteSetup_keeps_polling_while_daemon_unreachable(t *testing.T) {
	calls := 0
	fetch := func(context.Context) (adminclient.DaemonStatus, error) {
		calls++
		if calls < 3 {
			return adminclient.DaemonStatus{}, errors.New("daemon not up yet")
		}
		return adminclient.DaemonStatus{Remote: adminclient.RemoteStatus{
			AuthRequired: true,
			AuthURL:      "https://login.tailscale.com/a/clitest2",
		}}, nil
	}
	status, err := waitForRemoteSetup(context.Background(), fetch, 5*time.Second)
	if err != nil {
		t.Fatalf("waitForRemoteSetup returned error: %v", err)
	}
	if !status.Remote.AuthRequired {
		t.Fatalf("Remote = %+v, want auth-required after transient failures", status.Remote)
	}
	if calls < 3 {
		t.Fatalf("fetch called %d times, want at least 3 (transient failures retried)", calls)
	}
}

func TestWaitForRemoteSetup_times_out_with_last_status(t *testing.T) {
	fetch := func(context.Context) (adminclient.DaemonStatus, error) {
		return adminclient.DaemonStatus{}, nil
	}
	_, err := waitForRemoteSetup(context.Background(), fetch, 1500*time.Millisecond)
	if err == nil {
		t.Fatal("waitForRemoteSetup returned nil error, want budget expiry")
	}
}

package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/adminclient"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/service"
)

func TestDaemonStatusShowsWhereToFindThePendingTailscaleLink(t *testing.T) {
	var output bytes.Buffer
	writer := tabwriter.NewWriter(&output, 0, 0, 2, ' ', 0)
	writeDaemonStatus(writer, adminclient.DaemonStatus{
		Remote: adminclient.RemoteStatus{AuthRequired: true},
	})
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "ferngeist-gateway logs") {
		t.Fatalf("status should direct the operator to the local logs: %s", output.String())
	}
}

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
			PublicURL: "https://gw.tail1234.ts.net", ProvisioningState: "ready",
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

func TestWaitForRemoteSetupIgnoresRememberedURLUntilReady(t *testing.T) {
	calls := 0
	fetch := func(context.Context) (adminclient.DaemonStatus, error) {
		calls++
		state := "pending"
		if calls > 1 {
			state = "ready"
		}
		return adminclient.DaemonStatus{Remote: adminclient.RemoteStatus{PublicURL: "https://gw.tail.ts.net", ProvisioningState: state}}, nil
	}
	status, err := waitForRemoteSetup(context.Background(), fetch, 3*time.Second)
	if err != nil || status.Remote.ProvisioningState != "ready" || calls < 2 {
		t.Fatalf("status = %+v, calls = %d, error = %v", status.Remote, calls, err)
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

type savedOptionsManager struct {
	service.Manager
	saved service.InstallOptions
	ok    bool
}

func (m savedOptionsManager) SavedInstallOptions() (service.InstallOptions, bool) {
	return m.saved, m.ok
}

func TestResolveInstallOptionsKeepSettings(t *testing.T) {
	flags := service.InstallOptions{Host: "0.0.0.0", Port: 5788}
	saved := service.InstallOptions{Host: "127.0.0.1", Port: 9000, TailscaleMode: "auto"}

	if got := resolveInstallOptions(savedOptionsManager{saved: saved, ok: true}, flags, true); got != saved {
		t.Fatalf("keepSettings with saved options = %+v, want %+v", got, saved)
	}
	if got := resolveInstallOptions(savedOptionsManager{saved: saved, ok: true}, flags, false); got != flags {
		t.Fatalf("without keepSettings = %+v, want flags %+v", got, flags)
	}
	if got := resolveInstallOptions(savedOptionsManager{}, flags, true); got != flags {
		t.Fatalf("keepSettings without saved options = %+v, want flags %+v", got, flags)
	}
}

func TestPermissionDeniedHintIsPlatformAppropriate(t *testing.T) {
	if h := permissionDeniedHint("windows"); !strings.Contains(h, "elevated") {
		t.Fatalf("windows hint = %q", h)
	}
	if h := permissionDeniedHint("linux"); !strings.Contains(h, "do not use sudo") {
		t.Fatalf("linux hint = %q", h)
	}
}

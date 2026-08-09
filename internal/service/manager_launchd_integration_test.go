//go:build darwin

package service

import (
	"os"
	"testing"
	"time"
)

// TestLaunchdLifecycle is an opt-in integration test (FERNGEIST_RUN_REAL_AGENT_TESTS=1)
// that exercises the launchd manager against real launchd on a macOS host
// (e.g. a GitHub Actions macos-latest runner, which has a GUI login session for
// the runner user). It runs the full service lifecycle:
//   - Install: writes binary, env file, plist under $HOME and loads it
//   - Status: reports Installed and ActiveState
//   - Restart / Stop / Start: control calls against the loaded agent
//   - Re-install idempotency: a second Install succeeds
//   - Uninstall: unloads and removes the plist
//
// The test cleans up after itself via t.Cleanup so a failure mid-lifecycle
// cannot leave a stale LaunchAgent behind.
func TestLaunchdLifecycle(t *testing.T) {
	if os.Getenv("FERNGEIST_RUN_REAL_AGENT_TESTS") != "1" {
		t.Skip("set FERNGEIST_RUN_REAL_AGENT_TESTS=1 to run launchd integration tests")
	}

	m := NewManager()

	t.Cleanup(func() {
		_ = m.Uninstall(false)
	})

	// Fresh start: nothing installed.
	status, err := m.Status()
	if err != nil {
		t.Fatalf("Status() before install error = %v", err)
	}
	if status.Installed {
		t.Fatalf("Status() before install = Installed, want not installed")
	}

	if err := m.Install(InstallOptions{}); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	status, err = m.Status()
	if err != nil {
		t.Fatalf("Status() after install error = %v", err)
	}
	if !status.Installed {
		t.Fatal("Status() after install = not installed, want installed")
	}
	if status.UnitPath == "" {
		t.Fatal("Status() after install has empty UnitPath")
	}

	// Restart a running agent: must succeed.
	if err := m.Restart(); err != nil {
		t.Fatalf("Restart() error = %v", err)
	}

	// Stop: agent should be stopped (launchctl kill SIGTERM). Right after a
	// kickstart restart, launchd may not have finished re-registering the
	// service, so "kill" can transiently report "No process to signal".
	// Retry briefly before failing.
	stopErr := m.Stop()
	for attempt := 0; stopErr != nil && attempt < 5; attempt++ {
		time.Sleep(200 * time.Millisecond)
		stopErr = m.Stop()
	}
	if stopErr != nil {
		t.Fatalf("Stop() error = %v", stopErr)
	}

	// Start: agent should come back.
	if err := m.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	// Installing over an existing install must succeed (idempotent).
	if err := m.Install(InstallOptions{}); err != nil {
		t.Fatalf("Install() (idempotent) error = %v", err)
	}

	if err := m.Uninstall(false); err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}

	status, err = m.Status()
	if err != nil {
		t.Fatalf("Status() after uninstall error = %v", err)
	}
	if status.Installed {
		t.Fatal("Status() after uninstall = installed, want not installed")
	}
}

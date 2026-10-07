//go:build darwin

package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLaunchdLifecycle is an opt-in integration test (FERNGEIST_RUN_REAL_AGENT_TESTS=1)
// that exercises the launchd manager against real launchd on a macOS host
// (e.g. a GitHub Actions macos-latest runner, which has a GUI login session for
// the runner user). It runs the full service lifecycle:
//   - Install: writes binary, env file, plist under $HOME and loads it
//   - Status: reports Installed and ActiveState
//   - Restart / Stop / Start: Stop must leave it installed but inactive
//   - Re-install idempotency: a second Install succeeds
//   - Uninstall: unloads and removes the plist
//
// The test cleans up after itself via t.Cleanup so a failure mid-lifecycle
// cannot leave a stale LaunchAgent behind.
func TestLaunchdLifecycle(t *testing.T) {
	if os.Getenv("FERNGEIST_RUN_REAL_AGENT_TESTS") != "1" {
		t.Skip("set FERNGEIST_RUN_REAL_AGENT_TESTS=1 to run launchd integration tests")
	}

	// Install a REAL gateway binary (not the test binary, which has no
	// buildVersion and would exit immediately under launchd). Build
	// ./cmd/ferngeist with a buildVersion ldflag into a temp dir and point
	// installCopySource at it.
	realBin := filepath.Join(t.TempDir(), "ferngeist-gateway")
	cmd := exec.Command("go", "build", "-ldflags", "-X main.buildVersion=test", "-o", realBin, "../../cmd/ferngeist")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build real gateway binary: %v\n%s", err, out)
	}
	oldSource := installCopySource
	installCopySource = func() (string, error) { return realBin, nil }
	t.Cleanup(func() { installCopySource = oldSource })

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

	// Wait until the service is running (with a generous budget on a loaded CI
	// runner) before stopping it.
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, err := m.Status()
		if err == nil && st.ActiveState == "active" {
			break
		}
		if time.Now().After(deadline) {
			// Surface the daemon's own log + launchd's view so CI shows the
			// real startup error.
			if home, herr := os.UserHomeDir(); herr == nil {
				base := filepath.Join(home, "Library", "Application Support", "Ferngeist Gateway", "logs")
				if data, err := os.ReadFile(filepath.Join(base, "daemon.err.log")); err == nil && len(data) > 0 {
					t.Fatalf("service did not become active after Restart: last status %+v err %v\n--- daemon.err.log ---\n%s", st, err, data)
				}
			}
			if out, err := exec.Command("launchctl", "print", "gui/"+fmt.Sprintf("%d", os.Getuid())+"/"+darwinLabel).CombinedOutput(); err == nil {
				t.Fatalf("service did not become active after Restart: last status %+v err %v\n--- launchctl print ---\n%s", st, err, out)
			}
			t.Fatalf("service did not become active after Restart: last status %+v err %v", st, err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Stop boots the job out, so KeepAlive must not respawn it: the service
	// stays installed but is no longer active.
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	time.Sleep(2 * time.Second) // give a (buggy) KeepAlive respawn time to show up
	st, err := m.Status()
	if err != nil {
		t.Fatalf("Status() after stop error = %v", err)
	}
	if !st.Installed || st.ActiveState == "active" {
		t.Fatalf("Status() after stop = %+v, want installed and not active", st)
	}

	// Start: agent should come back.
	if err := m.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitLaunchdActive(t, m)

	// Reinstalling over an existing install must succeed and replace the
	// running instance's configuration.
	if err := m.Install(InstallOptions{Port: 5799}); err != nil {
		t.Fatalf("Install() (reinstall) error = %v", err)
	}
	waitLaunchdActive(t, m)
	if saved, ok := m.SavedInstallOptions(); !ok || saved.Port != 5799 {
		t.Fatalf("SavedInstallOptions() = %+v, %v, want port 5799", saved, ok)
	}
	out, err := exec.Command("launchctl", "print", darwinServiceTarget()).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "127.0.0.1:5799") {
		t.Fatalf("reinstalled job does not carry the new listen addr: %v\n%s", err, out)
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

// waitLaunchdActive polls until the service reports active.
func waitLaunchdActive(t *testing.T, m Manager) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, err := m.Status()
		if err == nil && st.ActiveState == "active" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("service did not become active: last status %+v err %v", st, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

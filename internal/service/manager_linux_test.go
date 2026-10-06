//go:build linux && !android

package service

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
)

// TestLinuxManagerType verifies linux builds select the linux manager.
func TestLinuxManagerType(t *testing.T) {
	m := NewManager()
	if _, ok := m.(*linuxManager); !ok {
		t.Fatalf("NewManager() = %T, want *linuxManager", m)
	}
}

// seedServiceBinarySource makes copyCurrentBinary's source a readable
// executable that is not the install target, the way a `daemon install` run
// from a user's downloads directory would be.
func seedServiceBinarySource(t *testing.T) {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	prev := copyCurrentBinarySource
	copyCurrentBinarySource = func() (string, error) { return self, nil }
	t.Cleanup(func() { copyCurrentBinarySource = prev })
}

// TestLinuxInstallRestartsService verifies Install's lifecycle on reinstall:
// stop the running daemon before swapping the binary (ETXTBSY on Linux), then
// enable --now and restart so the new build takes over. `enable --now` alone
// does NOT restart an already-running service, which would leave the old
// daemon serving the previous version. The fake systemctl records the calls.
func TestLinuxInstallRestartsService(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	logPath := newFakeSystemctl(t, nil)
	seedServiceBinarySource(t)

	// Simulate an existing install whose binary is already in place.
	paths, err := resolveLinuxPaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.binaryPath, []byte("previous build"), 0o755); err != nil {
		t.Fatal(err)
	}

	m := &linuxManager{}
	if err := m.Install(InstallOptions{}); err != nil {
		t.Fatal(err)
	}

	var sawStop, enableNow, restart bool
	for _, call := range systemctlCalls(t, logPath) {
		fields := strings.Fields(call)
		// Every invocation is `systemctl --user <subcommand> ...`.
		if len(fields) >= 2 && fields[0] == "--user" {
			switch fields[1] {
			case "stop":
				sawStop = true
			case "enable":
				if len(fields) >= 3 && fields[2] == "--now" {
					enableNow = true
				}
			case "restart":
				restart = true
			}
		}
	}
	if !sawStop {
		t.Fatalf("systemctl calls = %v, want stop before binary copy", systemctlCalls(t, logPath))
	}
	if !enableNow {
		t.Fatalf("systemctl calls = %v, want enable --now", systemctlCalls(t, logPath))
	}
	if !restart {
		t.Fatalf("systemctl calls = %v, want restart after enable", systemctlCalls(t, logPath))
	}
}

// TestLinuxInstallSkipsSelfCopy verifies Install is idempotent when invoked
// via the service binary itself: copying a running executable onto itself
// would fail with ETXTBSY on Linux. The copy source is pointed at the target
// (the running service binary) and install must succeed.
func TestLinuxInstallSkipsSelfCopy(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	newFakeSystemctl(t, nil)

	paths, err := resolveLinuxPaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Simulate the running daemon: the copy source IS the service binary.
	prev := copyCurrentBinarySource
	copyCurrentBinarySource = func() (string, error) { return paths.binaryPath, nil }
	defer func() { copyCurrentBinarySource = prev }()

	m := &linuxManager{}
	if err := m.Install(InstallOptions{}); err != nil {
		t.Fatal(err)
	}
}

// TestLinuxInstallFreshMachineToleratesNeverLoadedUnit is the regression test
// for `daemon install` failing on a machine that has never run the service:
//
//	systemctl --user stop ferngeist-gateway.service failed: Failed to stop
//	ferngeist-gateway.service: Unit ferngeist-gateway.service not loaded.
//
// systemd's "not loaded." describes a machine that has never loaded the unit,
// not a failure worth reporting, so install must carry on and finish the job.
func TestLinuxInstallFreshMachineToleratesNeverLoadedUnit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	logPath := newFakeSystemctl(t, map[string]fakeFailure{
		"stop": {
			exitCode: 5,
			message:  "Failed to stop " + linuxUnitName + ": Unit " + linuxUnitName + " not loaded.",
		},
	})
	seedServiceBinarySource(t)

	m := &linuxManager{}
	if err := m.Install(InstallOptions{}); err != nil {
		t.Fatalf("Install on a fresh machine = %v, want nil (systemd reports a never-loaded unit as \"not loaded\")", err)
	}
	assertSystemctlFailed(t, logPath, "stop")

	// Install must have run to completion, not merely swallowed the error.
	paths, err := resolveLinuxPaths()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.binaryPath); err != nil {
		t.Fatalf("service binary not installed after a tolerated stop failure: %v", err)
	}
	if _, err := os.Stat(paths.unitPath); err != nil {
		t.Fatalf("systemd unit not written after a tolerated stop failure: %v", err)
	}

	var enableNow, restart bool
	for _, call := range systemctlCalls(t, logPath) {
		fields := strings.Fields(call)
		if len(fields) >= 2 && fields[0] == "--user" {
			switch fields[1] {
			case "enable":
				enableNow = len(fields) >= 3 && fields[2] == "--now"
			case "restart":
				restart = true
			}
		}
	}
	if !enableNow || !restart {
		t.Fatalf("systemctl calls = %v, want enable --now and restart after the tolerated failure", systemctlCalls(t, logPath))
	}
}

// TestLinuxUninstallFreshMachineToleratesMissingUnitFile covers the other half
// of the same contract: `disable --now` on a machine with no unit file prints
//
//	Failed to disable unit: Unit file ferngeist-gateway.service does not exist.
//
// which is a no-op, not a failure to report.
func TestLinuxUninstallFreshMachineToleratesMissingUnitFile(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	t.Setenv("HOME", t.TempDir())

	logPath := newFakeSystemctl(t, map[string]fakeFailure{
		"disable": {
			exitCode: 1,
			message:  "Failed to disable unit: Unit file " + linuxUnitName + " does not exist.",
		},
	})

	m := &linuxManager{}
	if err := m.Uninstall(false); err != nil {
		t.Fatalf("Uninstall on a fresh machine = %v, want nil", err)
	}
	assertSystemctlFailed(t, logPath, "disable")
}

// TestLinuxInstallSurfacesUnrelatedSystemctlFailure is the counterweight to the
// tolerance above: widening the "no such unit" match must not turn real failures
// into silent successes. A permission error must abort the install before the
// binary is swapped, and must reach the operator with systemd's own words.
func TestLinuxInstallSurfacesUnrelatedSystemctlFailure(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	logPath := newFakeSystemctl(t, map[string]fakeFailure{
		"stop": {
			exitCode: 1,
			message:  "Failed to stop " + linuxUnitName + ": Access denied",
		},
	})
	seedServiceBinarySource(t)

	m := &linuxManager{}
	err := m.Install(InstallOptions{})
	if err == nil {
		t.Fatal("Install = nil, want the systemctl failure reported")
	}
	if errors.Is(err, errSystemctlUnitMissing) {
		t.Fatalf("Install error %v misclassified as a missing unit", err)
	}
	if !strings.Contains(err.Error(), "Access denied") {
		t.Fatalf("Install error %v does not carry systemd's message", err)
	}
	assertSystemctlFailed(t, logPath, "stop")

	paths, err := resolveLinuxPaths()
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(paths.binaryPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("service binary exists after a failed stop (stat err = %v), want the install aborted before the swap", statErr)
	}
	for _, call := range systemctlCalls(t, logPath) {
		if strings.Contains(call, "enable") || strings.Contains(call, "restart") {
			t.Fatalf("systemctl calls = %v, want no enable/restart after the stop failure", systemctlCalls(t, logPath))
		}
	}
}

// TestLinuxStatusReportsNotInstalledWhenUnitNotLoaded verifies the third
// classification call site: `show` failing for a unit systemd has never loaded
// means "not installed", which is a reportable state rather than an error.
func TestLinuxStatusReportsNotInstalledWhenUnitNotLoaded(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	t.Setenv("HOME", t.TempDir())

	logPath := newFakeSystemctl(t, map[string]fakeFailure{
		"show": {
			exitCode: 5,
			message:  "Failed to show " + linuxUnitName + ": Unit " + linuxUnitName + " not loaded.",
		},
	})

	m := &linuxManager{}
	status, err := m.Status()
	if err != nil {
		t.Fatalf("Status = %v, want a not-installed report", err)
	}
	if status.Installed {
		t.Fatalf("Status.Installed = true for a unit systemd has never loaded")
	}
	assertSystemctlFailed(t, logPath, "show")
}

// TestLinuxUninstallFailureIsReported keeps Uninstall honest in the other
// direction: a real systemd refusal must not be swallowed as "nothing to do".
func TestLinuxUninstallFailureIsReported(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	t.Setenv("HOME", t.TempDir())

	logPath := newFakeSystemctl(t, map[string]fakeFailure{
		"disable": {
			exitCode: 1,
			message:  "Failed to disable unit: Access denied",
		},
	})

	m := &linuxManager{}
	err := m.Uninstall(false)
	if err == nil {
		t.Fatal("Uninstall = nil, want the systemctl failure reported")
	}
	if errors.Is(err, errSystemctlUnitMissing) {
		t.Fatalf("Uninstall error %v misclassified as a missing unit", err)
	}
	assertSystemctlFailed(t, logPath, "disable")
}

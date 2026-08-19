//go:build linux && !android

package service

import (
	"os"
	"path/filepath"
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

	fake := filepath.Join(t.TempDir(), "systemctl")
	// NOTE: the fake must be a real script; systemctlOutput runs
	// `systemctl --user ...` via exec.
	script := `#!/bin/sh
echo "$@" >> "$SYSTEMCTL_LOG"
exit 0
`
	logPath := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("SYSTEMCTL_LOG", logPath)
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(fake))

	// Place a real executable so copyCurrentBinary works.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	paths, err := resolveLinuxPaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.binaryPath, contents, 0o755); err != nil {
		t.Fatal(err)
	}

	m := &linuxManager{}
	if err := m.Install(InstallOptions{}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Split(strings.TrimSpace(string(data)), "\n")

	var sawStop, enableNow, restart bool
	for _, call := range calls {
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
		t.Fatalf("systemctl calls = %v, want stop before binary copy", calls)
	}
	if !enableNow {
		t.Fatalf("systemctl calls = %v, want enable --now", calls)
	}
	if !restart {
		t.Fatalf("systemctl calls = %v, want restart after enable", calls)
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

	fake := filepath.Join(t.TempDir(), "systemctl")
	script := `#!/bin/sh
echo "$@" >> "$SYSTEMCTL_LOG"
exit 0
`
	logPath := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("SYSTEMCTL_LOG", logPath)
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(fake))

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

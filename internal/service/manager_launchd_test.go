package service

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDarwinManagerType verifies darwin builds select the darwin manager.
func TestDarwinManagerType(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin only")
	}
	m := NewManager()
	if _, ok := m.(*darwinManager); !ok {
		t.Fatalf("NewManager() = %T, want *darwinManager", m)
	}
}

// TestDarwinPathsUsesHome verifies path resolution lands under $HOME when
// HOME is overridden (works on any OS for the pure-Go path logic).
func TestDarwinPathsUsesHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix path style only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	paths, err := resolveDarwinPaths()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(paths.launchAgentsDir, home) {
		t.Fatalf("launchAgentsDir %q not under HOME %q", paths.launchAgentsDir, home)
	}
	if paths.binaryPath == "" || paths.plistPath == "" || paths.envPath == "" {
		t.Fatalf("expected non-empty paths, got %+v", paths)
	}
}

// TestDarwinInstallWritesPlist verifies Install writes a plist containing the
// binary path, env file, RunAtLoad, and KeepAlive. It uses a fake launchctl
// (a shell script) placed on PATH so no real launchd is touched.
func TestDarwinInstallWritesPlist(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell helper only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	fake := filepath.Join(t.TempDir(), "launchctl")
	script := "#!/bin/sh\nif [ \"$1\" = \"load\" ]; then exit 0; fi\nexit 0\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(fake))

	// Place a real executable so copyCurrentBinaryDarwin works.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, "Library", "Application Support", "Ferngeist Gateway", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(bin, "ferngeist-gateway")
	contents, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, contents, 0o755); err != nil {
		t.Fatal(err)
	}

	m := &darwinManager{}
	if err := m.Install(InstallOptions{}); err != nil {
		t.Fatal(err)
	}

	paths, err := resolveDarwinPaths()
	if err != nil {
		t.Fatal(err)
	}
	plist, err := os.ReadFile(paths.plistPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(plist)
	for _, want := range []string{"RunAtLoad", "KeepAlive", "ProgramArguments", "EnvironmentVariables", paths.binaryPath} {
		if !strings.Contains(text, want) {
			t.Errorf("plist missing %q; full plist:\n%s", want, text)
		}
	}
	// The env is carried inline (launchd never reads the env file), so the
	// plist must not reference the env path.
	if strings.Contains(text, paths.envPath) {
		t.Errorf("plist unexpectedly references env file %q; env must be inline", paths.envPath)
	}
}

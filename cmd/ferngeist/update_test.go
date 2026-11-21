package main

import (
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestServiceBinaryPathUnderSudoTargetsSudoingUser pins the OpenRC update
// flow: `sudo ferngeist-gateway update` must swap the sudoing user's
// installed binary, not a phantom under /root. The expected home comes from
// /etc/passwd (user.Current), an oracle independent of $HOME — which the
// test points elsewhere so a regressed non-sudo path cannot pass it.
func TestServiceBinaryPathUnderSudoTargetsSudoingUser(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("sudo-home targeting matters on linux (OpenRC service management)")
	}
	u, err := user.Current()
	if err != nil {
		t.Skipf("cannot resolve current user: %v", err)
	}
	prev := invokingEUID
	invokingEUID = func() int { return 0 }
	t.Cleanup(func() { invokingEUID = prev })
	t.Setenv("SUDO_USER", u.Username)
	t.Setenv("HOME", "/nonexistent-root-home")

	got, err := serviceBinaryPath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(u.HomeDir, ".local", "share", "ferngeist-gateway", "bin", "ferngeist-gateway")
	if got != want {
		t.Fatalf("serviceBinaryPath() under sudo = %q, want the sudoing user's %q", got, want)
	}
}

// TestRunUpdateRefusesPackageChannel verifies the binary-level gate: a build
// with updateChannel != "self" (set via ldflags by goreleaser for deb/rpm
// packages) refuses to self-update before any network work.
func TestRunUpdateRefusesPackageChannel(t *testing.T) {
	for _, ch := range []string{"apt", "deb", "rpm", "brew", "winget", "pacman", "msi"} {
		t.Run(ch, func(t *testing.T) {
			old := updateChannel
			updateChannel = ch
			defer func() { updateChannel = old }()

			err := runUpdate()
			if err == nil {
				t.Fatalf("runUpdate() = nil, want refusal for updateChannel=%q", ch)
			}
			if !strings.Contains(err.Error(), "package manager") {
				t.Fatalf("runUpdate() error = %q, want package-manager refusal", err)
			}
		})
	}
}

func TestSameFileAsAny(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if !sameFileAsAny(a, []string{b, a}) {
		t.Error("same path not detected")
	}
	if sameFileAsAny(a, []string{b, filepath.Join(dir, "missing")}) {
		t.Error("different files reported as same")
	}
	if sameFileAsAny(filepath.Join(dir, "missing"), []string{a}) {
		t.Error("missing path reported as same")
	}
}

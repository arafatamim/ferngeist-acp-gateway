package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

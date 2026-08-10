package main

import (
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

// TestRunUpdateHonorsEnvGate verifies the env-var gate: even a "self" build
// refuses when FERNGEIST_GATEWAY_UPDATE_CHECK_ENABLED is disabled (the
// postinstall.sh defense for package installs whose ldflag is self).
func TestRunUpdateHonorsEnvGate(t *testing.T) {
	old := updateChannel
	updateChannel = "self"
	defer func() { updateChannel = old }()

	for _, v := range []string{"0", "false", "FALSE", ""} {
		t.Run("env="+v, func(t *testing.T) {
			t.Setenv("FERNGEIST_GATEWAY_UPDATE_CHECK_ENABLED", v)
			err := runUpdate()
			if err == nil {
				t.Fatalf("runUpdate() = nil, want refusal with env=%q", v)
			}
			if !strings.Contains(err.Error(), "package manager") {
				t.Fatalf("runUpdate() error = %q, want package-manager refusal", err)
			}
		})
	}
}

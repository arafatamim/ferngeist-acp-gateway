package service

// The messages below are the exact strings systemd prints (stdout and stderr,
// as CombinedOutput captures them). They are the contract this package parses,
// so they belong in a test that runs on every platform — no exec, no Linux, no
// fake binary.

import (
	"errors"
	"strings"
	"testing"
)

func TestIsSystemctlUnitMissingMessage(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    bool
	}{
		{
			// The fresh-install regression: systemd says "not loaded", and an
			// earlier predicate that only knew "could not be found"/"not-found"
			// aborted `daemon install` on a machine that had never had the unit.
			name:    "stop on a never-installed unit",
			message: "Failed to stop ferngeist-gateway.service: Unit ferngeist-gateway.service not loaded.",
			want:    true,
		},
		{
			name:    "start on a never-installed unit",
			message: "Failed to start ferngeist-gateway.service: Unit ferngeist-gateway.service not loaded.",
			want:    true,
		},
		{
			name:    "restart on a removed unit",
			message: "Failed to restart ferngeist-gateway.service: Unit ferngeist-gateway.service not found.",
			want:    true,
		},
		{
			name:    "older systemd prose",
			message: "Failed to stop ferngeist-gateway.service: Unit ferngeist-gateway.service could not be found.",
			want:    true,
		},
		{
			// `disable --now` uses a different sentence than `stop`; the
			// predicate has to know both or Uninstall breaks on a fresh machine.
			name:    "disable on a unit file that does not exist",
			message: "Failed to disable unit: Unit file ferngeist-gateway.service does not exist.",
			want:    true,
		},
		{
			name:    "enable on a unit file that does not exist",
			message: "Failed to enable unit: Unit file ferngeist-gateway.service does not exist.",
			want:    true,
		},
		{
			name:    "show reports not-found",
			message: "Failed to show ferngeist-gateway.service: Unit ferngeist-gateway.service not-found.",
			want:    true,
		},
		{
			// CombinedOutput interleaves warnings with the failure; the match
			// must survive a multi-line capture.
			name: "warning preceding the failure",
			message: "Warning: The unit file, source configuration file or drop-ins of ferngeist-gateway.service changed on disk. Run 'systemctl --user daemon-reload' to reload units.\n" +
				"Failed to stop ferngeist-gateway.service: Unit ferngeist-gateway.service not loaded.",
			want: true,
		},
		{
			name:    "trailing whitespace from CombinedOutput",
			message: "  Failed to stop ferngeist-gateway.service: Unit ferngeist-gateway.service not loaded.\n",
			want:    true,
		},
		{
			name:    "different unit",
			message: "Failed to stop other.service: Unit other.service not loaded.",
			want:    false,
		},
		{
			// Guards the pattern anchor: a prefix-sharing unit name must not
			// satisfy the "our unit" patterns.
			name:    "unit name that shares our prefix",
			message: "Failed to stop ferngeist-gateway-worker.service: Unit ferngeist-gateway-worker.service not loaded.",
			want:    false,
		},
		{
			name:    "permission denied",
			message: "Failed to stop ferngeist-gateway.service: Access denied",
			want:    false,
		},
		{
			name:    "no user systemd session",
			message: "Failed to connect to bus: No such file or directory",
			want:    false,
		},
		{
			name:    "unit failed, not missing",
			message: "Job for ferngeist-gateway.service failed because a timeout was exceeded.",
			want:    false,
		},
		{
			name:    "bogus unit name from systemd",
			message: "Failed to stop ferngeist-gateway.service: Unit ferngeist-gateway.service is not loaded properly: Invalid argument.",
			want:    false,
		},
		{
			name:    "empty output",
			message: "",
			want:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSystemctlUnitMissingMessage(linuxUnitName, tc.message); got != tc.want {
				t.Fatalf("isSystemctlUnitMissingMessage(%q) = %v, want %v", tc.message, got, tc.want)
			}
		})
	}
}

// TestNewSystemctlErrorClassifiesMissingUnit verifies the exec boundary is what
// classifies the failure, so call sites never re-parse systemd's prose: the
// missing-unit case wraps the sentinel (errors.Is), everything else stays a
// plain error that aborts the operation.
func TestNewSystemctlErrorClassifiesMissingUnit(t *testing.T) {
	args := []string{"stop", linuxUnitName}

	missing := newSystemctlError(args, linuxUnitName,
		"Failed to stop ferngeist-gateway.service: Unit ferngeist-gateway.service not loaded.")
	if !errors.Is(missing, errSystemctlUnitMissing) {
		t.Fatalf("errors.Is(%v, errSystemctlUnitMissing) = false, want true", missing)
	}
	if !isSystemctlUnitMissing(missing) {
		t.Fatalf("isSystemctlUnitMissing(%v) = false, want true", missing)
	}
	// Operator-facing context must survive: the failing call and systemd's own
	// words, so `daemon install` failures stay diagnosable.
	for _, want := range []string{"systemctl --user stop " + linuxUnitName, "not loaded"} {
		if !strings.Contains(missing.Error(), want) {
			t.Fatalf("error %q does not mention %q", missing, want)
		}
	}

	real := newSystemctlError(args, linuxUnitName,
		"Failed to stop ferngeist-gateway.service: Access denied")
	if errors.Is(real, errSystemctlUnitMissing) {
		t.Fatalf("errors.Is(%v, errSystemctlUnitMissing) = true, want false for a real failure", real)
	}
	if !strings.Contains(real.Error(), "Access denied") {
		t.Fatalf("error %q does not carry systemd's message", real)
	}
}

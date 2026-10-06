package service

// This file has no build tag on purpose. Classifying systemctl's output is pure
// string logic — no exec, no Linux syscalls — and the bug it guards against
// (misreading systemd's prose) is invisible to any test that runs a *fake*
// systemctl. Keeping it platform-neutral means it is unit-tested on every
// developer machine and in the Linux CI job alike.

import (
	"errors"
	"fmt"
	"strings"
)

// errSystemctlUnitMissing is wrapped into every systemctl error that means
// systemd has no usable unit by that name: the unit file was never installed,
// was removed, or has not been loaded since the last `daemon-reload`. Callers
// treat it as a benign no-op for lifecycle actions, because "stop a unit that
// does not exist" and "disable a unit that does not exist" already describe the
// desired end state.
var errSystemctlUnitMissing = errors.New("systemd has no such unit")

// systemctlUnitMissingPatterns lists the message shapes real systemd prints
// when a unit is unknown. systemd reports this as prose, not a stable code, and
// the wording differs per subcommand, so each shape must be named explicitly:
//
//	stop/start/restart  Failed to stop X.service: Unit X.service not loaded.
//	disable --now       Failed to disable unit: Unit file X.service does not exist.
//	show                Failed to show X.service: Unit X.service not-found.
//
// The unit name is interpolated (with its ".service" suffix) so the patterns
// are anchored to our unit: a message about a different unit — including the
// prefix-sharing "ferngeist-gateway-worker.service" — can never match.
var systemctlUnitMissingPatterns = []string{
	"unit file %s does not exist",
	"unit %s not loaded",
	"unit %s not found",
	"unit %s could not be found",
	"unit %s not-found",
	"unit %s does not exist",
}

// isSystemctlUnitMissingMessage reports whether a systemctl message — stdout and
// stderr combined, as exec captures them — means the unit is unknown.
func isSystemctlUnitMissingMessage(unitName, message string) bool {
	unit := strings.ToLower(unitName)
	lowered := strings.ToLower(message)
	for _, pattern := range systemctlUnitMissingPatterns {
		if strings.Contains(lowered, fmt.Sprintf(pattern, unit)) {
			return true
		}
	}
	return false
}

// newSystemctlError builds the error for a failing systemctl invocation,
// classifying the "no such unit" case once, here at the exec boundary, so call
// sites use errors.Is instead of re-parsing systemd's prose.
func newSystemctlError(args []string, unitName, message string) error {
	call := "systemctl --user " + strings.Join(args, " ")
	if isSystemctlUnitMissingMessage(unitName, message) {
		return fmt.Errorf("%s failed: %w: %s", call, errSystemctlUnitMissing, message)
	}
	return fmt.Errorf("%s failed: %s", call, message)
}

// isSystemctlUnitMissing reports whether err means systemd had no such unit.
func isSystemctlUnitMissing(err error) bool {
	return errors.Is(err, errSystemctlUnitMissing)
}

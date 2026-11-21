package service

// This file has no build tag on purpose, mirroring systemctl_errors.go: the
// logic is pure string work — no exec, no Linux syscalls — and the bug it
// guards against (misreading OpenRC's prose) is invisible to any test that
// runs a *fake* rc-update. Keeping it platform-neutral means it is unit-tested
// on every developer machine and in the Linux CI job alike.

import (
	"fmt"
	"strings"
)

// rcUpdateNotInRunlevelPatterns lists the message shapes rc-update prints when
// `del` finds nothing to remove (src/rc-update/rc-update.c). The service name
// is interpolated so a message about another service can never match.
var rcUpdateNotInRunlevelPatterns = []string{
	"service `%s' is not in the runlevel",
	"service `%s' not found in any of the specified runlevels",
}

func isRcUpdateNotInRunlevelMessage(serviceName, message string) bool {
	lowered := strings.ToLower(message)
	service := strings.ToLower(serviceName)
	for _, pattern := range rcUpdateNotInRunlevelPatterns {
		if strings.Contains(lowered, fmt.Sprintf(pattern, service)) {
			return true
		}
	}
	return false
}

// openrcStatusStates maps `rc-service <name> status` prose onto the
// systemd-shaped states `daemon status` prints. rc-service reports status with
// informational exit codes (stopped is 3, crashed is 32), so the message
// text — not the exit code — is the signal.
var openrcStatusStates = map[string]struct{ active, sub string }{
	"status: started":  {"active", "running"},
	"status: starting": {"activating", ""},
	"status: stopping": {"deactivating", ""},
	"status: stopped":  {"inactive", ""},
	"status: inactive": {"inactive", "inactive"},
	"status: crashed":  {"failed", "crashed"},
}

// openrcStatusFromMessage returns the mapped states for rc-service status
// output. ok is false when no marker matched: the message then belongs to an
// error the caller must surface, not a state it should guess.
func openrcStatusFromMessage(message string) (activeState, subState string, ok bool) {
	lowered := strings.ToLower(strings.TrimSpace(message))
	for marker, states := range openrcStatusStates {
		if strings.Contains(lowered, marker) {
			return states.active, states.sub, true
		}
	}
	return "", "", false
}

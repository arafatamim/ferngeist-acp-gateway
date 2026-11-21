//go:build linux && !android

package service

// Test support for the fake rc-service/rc-update the OpenRC manager tests
// drive. Tagged like its consumers (all linux), unlike fake_systemctl_test.go
// which stays untagged for its cross-platform parse tests. The manager tests
// script refusals through these rules — rc-service status reports with
// informational exit codes (stopped is 3, crashed is 32), and the fakes must
// be able to exit nonzero while still printing output, or every error branch
// of the manager stays unreachable.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRCRule scripts one subcommand: its exit code (first line) and the output
// rc-service/rc-update prints with it.
type fakeRCRule struct {
	exitCode int
	output   string
}

// fakeRCServiceScript records every invocation in $RCSERVICE_LOG and, when a
// rule exists for the subcommand (the last argument), prints the rule's output
// and exits with its code — rc-service status reports with informational exit
// codes (stopped is 3, crashed is 32), so the fakes must be able to exit
// nonzero while still printing the status marker.
const fakeRCServiceScript = `#!/bin/sh
echo "$@" >> "$RCSERVICE_LOG"
sub="$(eval echo \${$#})"
rule="$RCSERVICE_RULES/$sub"
if [ -f "$rule" ]; then
	code=$(sed -n 1p "$rule")
	sed -n '2,$p' "$rule"
	exit "$code"
fi
exit 0
`

// fakeRCUpdateScript mirrors it for rc-update; rules are keyed by the action
// (add/del) because that is rc-update's first argument.
const fakeRCUpdateScript = `#!/bin/sh
echo "$@" >> "$RCUPDATE_LOG"
rule="$RCUPDATE_RULES/$1"
if [ -f "$rule" ]; then
	code=$(sed -n 1p "$rule")
	sed -n '2,$p' "$rule"
	exit "$code"
fi
exit 0
`

// newFakeOpenRC puts fake rc-service and rc-update binaries first on PATH and
// returns the paths of both call logs.
func newFakeOpenRC(t *testing.T, serviceRules, updateRules map[string]fakeRCRule) (serviceLog, updateLog string) {
	t.Helper()

	dir := t.TempDir()
	for name, script := range map[string]string{
		"rc-service": fakeRCServiceScript,
		"rc-update":  fakeRCUpdateScript,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	serviceLog = filepath.Join(t.TempDir(), "rc-service.log")
	t.Setenv("RCSERVICE_LOG", serviceLog)
	updateLog = filepath.Join(t.TempDir(), "rc-update.log")
	t.Setenv("RCUPDATE_LOG", updateLog)

	serviceRulesDir := t.TempDir()
	t.Setenv("RCSERVICE_RULES", serviceRulesDir)
	for sub, rule := range serviceRules {
		writeFakeRCRule(t, filepath.Join(serviceRulesDir, sub), rule)
	}
	updateRulesDir := t.TempDir()
	t.Setenv("RCUPDATE_RULES", updateRulesDir)
	for action, rule := range updateRules {
		writeFakeRCRule(t, filepath.Join(updateRulesDir, action), rule)
	}

	// Keep the real PATH behind the fakes: the scripts themselves need sh and sed.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return serviceLog, updateLog
}

func writeFakeRCRule(t *testing.T, path string, rule fakeRCRule) {
	t.Helper()
	content := fmt.Sprintf("%d\n%s\n", rule.exitCode, rule.output)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// rcCalls returns the recorded invocations of the fake named by logPath.
func rcCalls(t *testing.T, logPath string) []string {
	t.Helper()

	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read call log: %v", err)
	}
	var calls []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			calls = append(calls, line)
		}
	}
	return calls
}

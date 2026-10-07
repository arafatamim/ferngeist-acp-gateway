package service

// Test support for the fake systemctl the linux manager tests drive. It lives
// in an untagged file on purpose: the parsing of the call log is pure string
// work, so it is unit-tested everywhere instead of only on Linux.
//
// The failure mode this guards is subtle — a fake that always exits 0 makes
// every error branch of the manager unreachable, so a suite can be green while
// never once seeing systemd refuse. TestFakeSystemctlRecordsScriptedFailures
// asserts the fake really can refuse, with the exit code and stderr text real
// systemd would produce.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeFailure scripts one systemctl subcommand to fail the way systemd does.
type fakeFailure struct {
	exitCode int
	message  string
}

// fakeSystemctlScript records every invocation in $SYSTEMCTL_LOG and, when
// $SYSTEMCTL_RULES/<subcommand> exists, writes its second and later lines to
// stderr, exits with its first line, and appends a "FAILED <sub>" marker so a
// test can prove the failing branch ran rather than trusting a silent no-match.
const fakeSystemctlScript = `#!/bin/sh
echo "$@" >> "$SYSTEMCTL_LOG"
sub="$1"
if [ "$sub" = "--user" ]; then
	sub="$2"
fi
rule="$SYSTEMCTL_RULES/$sub"
if [ -f "$rule" ]; then
	code=$(sed -n 1p "$rule")
	sed -n '2,$p' "$rule" >&2
	echo "FAILED $sub" >> "$SYSTEMCTL_LOG"
	exit "$code"
fi
exit 0
`

// fakeLoginctlScript stands in for loginctl so Install's linger handling never
// touches the real user manager. show-user reports $LOGINCTL_LINGER (default
// "yes"); every call is appended to $LOGINCTL_LOG.
const fakeLoginctlScript = `#!/bin/sh
echo "$@" >> "$LOGINCTL_LOG"
if [ "$1" = "show-user" ]; then
	echo "${LOGINCTL_LINGER:-yes}"
fi
exit 0
`

// newFakeSystemctl puts a fake systemctl first on PATH and returns the path of
// the call log.
func newFakeSystemctl(t *testing.T, failures map[string]fakeFailure) string {
	t.Helper()

	dir := t.TempDir()
	fake := filepath.Join(dir, "systemctl")
	if err := os.WriteFile(fake, []byte(fakeSystemctlScript), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "loginctl"), []byte(fakeLoginctlScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOGINCTL_LOG", filepath.Join(t.TempDir(), "loginctl.log"))

	logPath := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("SYSTEMCTL_LOG", logPath)

	rulesDir := t.TempDir()
	t.Setenv("SYSTEMCTL_RULES", rulesDir)
	for sub, failure := range failures {
		rule := fmt.Sprintf("%d\n%s\n", failure.exitCode, failure.message)
		if err := os.WriteFile(filepath.Join(rulesDir, sub), []byte(rule), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Keep the real PATH behind the fake: the script itself needs sh and sed.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return logPath
}

// parseSystemctlCalls returns the recorded invocations, without failure markers.
func parseSystemctlCalls(log string) []string {
	var calls []string
	for _, line := range strings.Split(log, "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "FAILED ") {
			continue
		}
		calls = append(calls, line)
	}
	return calls
}

// hasSystemctlFailure reports whether the fake rejected sub.
func hasSystemctlFailure(log, sub string) bool {
	for _, line := range strings.Split(log, "\n") {
		if line == "FAILED "+sub {
			return true
		}
	}
	return false
}

func systemctlCalls(t *testing.T, logPath string) []string {
	t.Helper()

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read systemctl call log: %v", err)
	}
	return parseSystemctlCalls(string(data))
}

// assertSystemctlFailed asserts the fake actually rejected sub — without it a
// mis-keyed rule would let a test pass while covering nothing.
func assertSystemctlFailed(t *testing.T, logPath, sub string) {
	t.Helper()

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read systemctl call log: %v", err)
	}
	if !hasSystemctlFailure(string(data), sub) {
		t.Fatalf("fake systemctl never failed %q; log:\n%s", sub, data)
	}
}

// TestParseSystemctlCallsIgnoresFailureMarkers pins the call-log format the
// linux tests reason about. The fixture is the log this fake really produces
// (see TestFakeSystemctlRecordsScriptedFailures).
func TestParseSystemctlCallsIgnoresFailureMarkers(t *testing.T) {
	log := `--user stop ferngeist-gateway.service
FAILED stop
--user daemon-reload
--user enable --now ferngeist-gateway.service
--user disable --now ferngeist-gateway.service
FAILED disable
--user show ferngeist-gateway.service --property=LoadState --value
`

	want := []string{
		"--user stop ferngeist-gateway.service",
		"--user daemon-reload",
		"--user enable --now ferngeist-gateway.service",
		"--user disable --now ferngeist-gateway.service",
		"--user show ferngeist-gateway.service --property=LoadState --value",
	}
	got := parseSystemctlCalls(log)
	if len(got) != len(want) {
		t.Fatalf("parseSystemctlCalls() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("parseSystemctlCalls()[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	if !hasSystemctlFailure(log, "stop") || !hasSystemctlFailure(log, "disable") {
		t.Fatalf("hasSystemctlFailure missed a recorded failure in:\n%s", log)
	}
	if hasSystemctlFailure(log, "enable") {
		t.Fatalf("hasSystemctlFailure reported a failure that never happened")
	}
}

// TestFakeSystemctlRecordsScriptedFailures is the meta-test the old suite
// lacked: it runs the fake for real and proves it can refuse a subcommand with
// systemd's exit code and message, while unscripted subcommands still succeed.
func TestFakeSystemctlRecordsScriptedFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake is a POSIX shell script; Windows cannot exec it without a .exe extension")
	}

	logPath := newFakeSystemctl(t, map[string]fakeFailure{
		"stop": {
			exitCode: 5,
			message:  "Failed to stop " + linuxUnitName + ": Unit " + linuxUnitName + " not loaded.",
		},
	})

	out, err := exec.Command("systemctl", "--user", "stop", linuxUnitName).CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("fake systemctl stop = %v (output %q), want a non-zero exit", err, out)
	}
	if exitErr.ExitCode() != 5 {
		t.Fatalf("fake systemctl stop exit code = %d, want 5", exitErr.ExitCode())
	}
	if !strings.Contains(string(out), "not loaded.") {
		t.Fatalf("fake systemctl stop output = %q, want systemd's message", out)
	}

	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		t.Fatalf("fake systemctl daemon-reload = %v (output %q), want success", err, out)
	}

	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !hasSystemctlFailure(string(log), "stop") {
		t.Fatalf("stop failure not recorded in log:\n%s", log)
	}
	if hasSystemctlFailure(string(log), "daemon-reload") {
		t.Fatalf("daemon-reload recorded as a failure:\n%s", log)
	}
	calls := parseSystemctlCalls(string(log))
	if len(calls) != 2 {
		t.Fatalf("parseSystemctlCalls() = %v, want the two invocations", calls)
	}
}

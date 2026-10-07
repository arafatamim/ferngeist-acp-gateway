package service

import (
	"errors"
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

// fakeLaunchctl puts a launchctl shell script on PATH that appends each
// invocation to a log file and reports the job as not loaded for `print`
// (exit 113 + "Could not find service"), so no real launchd is touched.
func fakeLaunchctl(t *testing.T) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$*\" >> '" + logPath + "'\n" +
		"if [ \"$1\" = \"print\" ] || [ \"$1\" = \"bootout\" ]; then echo 'Could not find service' >&2; exit 113; fi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "launchctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return logPath
}

func installDarwinForTest(t *testing.T, options InstallOptions) (string, darwinPaths, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("posix shell helper only")
	}
	t.Setenv("HOME", t.TempDir())
	calls := fakeLaunchctl(t)
	m := &darwinManager{}
	if err := m.Install(options); err != nil {
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
	return string(plist), paths, calls
}

// TestDarwinInstallWritesPlist verifies Install writes a plist containing the
// binary path, RunAtLoad, and KeepAlive, with stdout discarded, and loads it
// with bootout -> enable -> bootstrap -> kickstart. It uses a fake launchctl
// (a shell script) placed on PATH so no real launchd is touched.
func TestDarwinInstallWritesPlist(t *testing.T) {
	text, paths, calls := installDarwinForTest(t, InstallOptions{})
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
	if !strings.Contains(text, "<key>StandardOutPath</key>\n\t<string>/dev/null</string>") {
		t.Errorf("StandardOutPath is not /dev/null; plist:\n%s", text)
	}
	if !strings.Contains(text, paths.stderrLogPath) {
		t.Errorf("plist missing stderr path %q", paths.stderrLogPath)
	}

	log, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	var verbs []string
	for _, line := range strings.Split(strings.TrimSpace(string(log)), "\n") {
		verbs = append(verbs, strings.Fields(line)[0])
	}
	if got, want := strings.Join(verbs, " "), "bootout enable bootstrap kickstart"; got != want {
		t.Errorf("launchctl verbs = %q, want %q\n%s", got, want, log)
	}
}

func TestDarwinPlistCarriesRemoteSettings(t *testing.T) {
	text, _, _ := installDarwinForTest(t, InstallOptions{
		Host: "0.0.0.0", TailscaleMode: "auto", PublicURL: "https://gw.example.ts.net?a=1&b=2",
	})
	for _, want := range []string{
		"<key>FERNGEIST_GATEWAY_TAILSCALE_MODE</key>\n\t\t<string>auto</string>",
		"<key>FERNGEIST_GATEWAY_PUBLIC_BASE_URL</key>\n\t\t<string>https://gw.example.ts.net?a=1&amp;b=2</string>",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("plist missing %q; full plist:\n%s", want, text)
		}
	}
}

func TestDarwinPlistOmitsRemoteSettingsForLAN(t *testing.T) {
	text, _, _ := installDarwinForTest(t, InstallOptions{
		Host: "0.0.0.0", PublicURL: "https://stale.example.ts.net",
	})
	for _, bad := range []string{"TAILSCALE_MODE", "PUBLIC_BASE_URL"} {
		if strings.Contains(text, bad) {
			t.Errorf("LAN-only plist unexpectedly contains %q; full plist:\n%s", bad, text)
		}
	}
}

// TestDarwinStopBootsOutAndStaysInstalled verifies Stop uses bootout (not kill)
// and that a stopped service still reports Installed.
func TestDarwinStopBootsOutAndStaysInstalled(t *testing.T) {
	_, paths, calls := installDarwinForTest(t, InstallOptions{})
	m := &darwinManager{}
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	log, _ := os.ReadFile(calls)
	if strings.Contains(string(log), "kill") {
		t.Errorf("Stop used launchctl kill:\n%s", log)
	}
	st, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Installed || st.ActiveState != "inactive" || st.LoadState != "not-loaded" || st.UnitPath != paths.plistPath {
		t.Errorf("Status() after Stop = %+v, want installed+inactive+not-loaded", st)
	}
	// Start must bootstrap again because print reports not loaded.
	if err := m.Start(); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	log, _ = os.ReadFile(calls)
	if !strings.Contains(string(log), "bootstrap ") {
		t.Errorf("Start did not bootstrap:\n%s", log)
	}
	if err := os.Remove(paths.plistPath); err != nil {
		t.Fatal(err)
	}
	if st, _ := m.Status(); st.Installed {
		t.Errorf("Status() without plist = %+v, want not installed", st)
	}
}

func TestLaunchctlErrorClassifiers(t *testing.T) {
	for _, msg := range []string{"Boot-out failed: 3: No such process", "Could not find specified service", "Could not find service"} {
		if !isLaunchctlNotFound(errors.New(msg)) {
			t.Errorf("isLaunchctlNotFound(%q) = false", msg)
		}
	}
	if isLaunchctlPermissionDenied("Service denied by policy") {
		t.Error("bare 'denied' must not count as permission denied")
	}
	if !isLaunchctlPermissionDenied("Operation not permitted") {
		t.Error("operation not permitted should count")
	}
}

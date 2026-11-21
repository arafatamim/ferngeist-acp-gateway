//go:build linux && !android

package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenRCManagerSelectedByInitSystem verifies the runtime dispatch: Linux
// cannot split init systems by build tag, so detection at NewManager time is
// the contract. The seam is pinned, never the host.
func TestOpenRCManagerSelectedByInitSystem(t *testing.T) {
	prev := linuxInitSystem
	linuxInitSystem = func() string { return initOpenRC }
	t.Cleanup(func() { linuxInitSystem = prev })

	m := NewManager()
	if _, ok := m.(*openrcManager); !ok {
		t.Fatalf("NewManager() = %T, want *openrcManager", m)
	}
}

// TestOpenRCInstallRequiresRoot pins the unprivileged contract: the remedy
// rides in the error as ErrServiceUnsupportedConfig, deliberately NOT
// ErrServicePermissionDenied — the CLI's permission hint tells the operator
// not to use sudo, which is correct for per-user managers and the opposite of
// what an OpenRC system service needs.
func TestOpenRCInstallRequiresRoot(t *testing.T) {
	prev := effectiveUID
	effectiveUID = func() int { return 1000 }
	t.Cleanup(func() { effectiveUID = prev })

	err := (&openrcManager{}).Install(InstallOptions{Host: "127.0.0.1", Port: 5788})
	if !errors.Is(err, ErrServiceUnsupportedConfig) || !strings.Contains(err.Error(), "sudo") {
		t.Fatalf("Install() = %v, want an unsupported-config error carrying the sudo remedy", err)
	}
}

func TestOpenRCInstallWithoutSUDOUserFails(t *testing.T) {
	prev := effectiveUID
	effectiveUID = func() int { return 0 }
	t.Cleanup(func() { effectiveUID = prev })
	t.Setenv("SUDO_USER", "")

	err := (&openrcManager{}).Install(InstallOptions{Host: "127.0.0.1", Port: 5788})
	if !errors.Is(err, ErrServiceUnsupportedConfig) || !strings.Contains(err.Error(), "SUDO_USER") {
		t.Fatalf("Install() = %v, want an unsupported-config error naming SUDO_USER (root's own account is not a gateway home)", err)
	}
}

// seedOpenRCRoot pins the account/root seams the way a real `sudo daemon
// install` would look: root, a SUDO_USER, and an /etc redirected to a temp dir.
func seedOpenRCRoot(t *testing.T, home string) openrcAccount {
	t.Helper()

	prevUID, prevLookup, prevEtc := effectiveUID, openrcUserLookup, openrcEtcDir
	effectiveUID = func() int { return 0 }
	uid, gid := os.Getuid(), os.Getgid()
	openrcUserLookup = func(name string) (openrcAccount, error) {
		return openrcAccount{name: name, home: home, uid: uid, gid: gid}, nil
	}
	t.Cleanup(func() {
		effectiveUID, openrcUserLookup, openrcEtcDir = prevUID, prevLookup, prevEtc
	})

	openrcEtcDir = t.TempDir()
	t.Setenv("SUDO_USER", "gateway-user")

	seedServiceBinarySource(t)

	return openrcAccount{name: "gateway-user", home: home, uid: uid, gid: gid}
}

// TestOpenRCInstallWritesScriptAndStartsService pins the runscript's wire
// format — the file OpenRC actually executes — plus the lifecycle ordering:
// stop before the binary swap, rc-update add, start last.
func TestOpenRCInstallWritesScriptAndStartsService(t *testing.T) {
	home := t.TempDir()
	account := seedOpenRCRoot(t, home)
	serviceLog, updateLog := newFakeOpenRC(t, nil, nil)

	if err := (&openrcManager{}).Install(InstallOptions{Host: "127.0.0.1", Port: 5788}); err != nil {
		t.Fatalf("Install() = %v, want nil", err)
	}

	script, err := os.ReadFile(filepath.Join(openrcEtcDir, "init.d", openrcServiceName))
	if err != nil {
		t.Fatalf("init script not written: %v", err)
	}
	info, err := os.Stat(filepath.Join(openrcEtcDir, "init.d", openrcServiceName))
	if err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("init script not executable: %v %v", err, info)
	}

	paths := linuxPathsForHome(home)
	for _, want := range []string{
		"#!/sbin/openrc-run",
		`command="` + paths.execPath + `"`,
		`command_user="` + account.name + `"`,
		`directory="` + paths.rootDir + `"`,
		"supervisor=supervise-daemon",
		"respawn_delay=2",
		`. "` + paths.envPath + `"`,
		`export HOME="` + account.home + `"`,
		`PATH="$PATH:/sbin:/usr/sbin"`,
	} {
		if !strings.Contains(string(script), want) {
			t.Fatalf("init script missing %q:\n%s", want, script)
		}
	}

	env, err := os.ReadFile(paths.envPath)
	if err != nil {
		t.Fatalf("env file not written: %v", err)
	}
	if !strings.Contains(string(env), "FERNGEIST_GATEWAY_LISTEN_ADDR=127.0.0.1:5788") {
		t.Fatalf("env file = %q, want the listen address", env)
	}

	calls := rcCalls(t, serviceLog)
	want := []string{
		"--ifexists " + openrcServiceName + " stop",
		openrcServiceName + " start",
	}
	if len(calls) != len(want) {
		t.Fatalf("rc-service calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("rc-service call %d = %q, want %q (stop must precede the binary swap, start must be last)", i, calls[i], want[i])
		}
	}

	updates := rcCalls(t, updateLog)
	if len(updates) != 1 || updates[0] != "add "+openrcServiceName+" "+openrcRunlevel {
		t.Fatalf("rc-update calls = %v, want one add to the default runlevel", updates)
	}
}

// TestOpenRCStatusReportsCrashedDaemon pins the manager-level status wiring:
// rc-service reports with informational exit codes (crashed is 32), so a
// crash must come back as a state, not an error, and the runlevel symlink is
// what daemon status shows as boot persistence. The marker-to-state mapping
// itself is owned by TestOpenRCStatusFromMessage; this test owns only the
// exec boundary and the enabled detail.
func TestOpenRCStatusReportsCrashedDaemon(t *testing.T) {
	seedOpenRCRoot(t, t.TempDir())
	if err := os.MkdirAll(filepath.Join(openrcEtcDir, "init.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(openrcScriptPath(), []byte("#!/sbin/openrc-run\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Boot persistence: rc-update add is the runlevel symlink.
	if err := os.MkdirAll(filepath.Dir(openrcRunlevelPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(openrcScriptPath(), openrcRunlevelPath()); err != nil {
		t.Fatal(err)
	}

	_, _ = newFakeOpenRC(t, map[string]fakeRCRule{
		"status": {exitCode: 32, output: " * status: crashed"},
	}, nil)

	status, err := (&openrcManager{}).Status()
	if err != nil {
		t.Fatalf("Status() = %v, want nil (exit 32 is informational; the message carries the state)", err)
	}
	if !status.Installed {
		t.Fatal("Status().Installed = false with the script on disk")
	}
	if status.ActiveState == "" || status.SubState == "" {
		t.Fatalf("Status() = %+v, want the crashed marker mapped to a state (the specific values are owned by TestOpenRCStatusFromMessage)", status)
	}
	if status.UnitFileState != "enabled" {
		t.Fatalf("UnitFileState = %q, want enabled with the runlevel symlink present", status.UnitFileState)
	}
}

// TestOpenRCStatusWithoutInstallIsNotAnError guards the never-installed host:
// Status reports not-installed instead of surfacing rc-service's missing
// service prose. Whether rc-service is even consulted is not this test's
// business — an unscripted refusal (or a host without rc-service) already
// makes Status error, which this behavior assertion would catch.
func TestOpenRCStatusWithoutInstallIsNotAnError(t *testing.T) {
	seedOpenRCRoot(t, t.TempDir())

	status, err := (&openrcManager{}).Status()
	if err != nil {
		t.Fatalf("Status() = %v, want nil on a never-installed host", err)
	}
	if status.Installed {
		t.Fatalf("Status() = %+v, want not installed", status)
	}
}

// TestOpenRCInstallSurfacesRcServiceFailure pins the failure path: when
// rc-service refuses to start the service, the install reports the refusal
// instead of printing success.
func TestOpenRCInstallSurfacesRcServiceFailure(t *testing.T) {
	seedOpenRCRoot(t, t.TempDir())
	_, _ = newFakeOpenRC(t, map[string]fakeRCRule{
		"start": {exitCode: 1, output: "openrc: ferngeist-gateway failed to start"},
	}, nil)

	err := (&openrcManager{}).Install(InstallOptions{Host: "127.0.0.1", Port: 5788})
	if err == nil {
		t.Fatal("Install() = nil, want the rc-service refusal to surface")
	}
	if !strings.Contains(err.Error(), "ferngeist-gateway failed to start") {
		t.Fatalf("Install() = %v, want rc-service's message carried through", err)
	}
}

func TestOpenRCUninstallRemovesScriptAndRunlevel(t *testing.T) {
	home := t.TempDir()
	seedOpenRCRoot(t, home)
	if err := os.MkdirAll(filepath.Join(openrcEtcDir, "init.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(openrcScriptPath(), []byte("#!/sbin/openrc-run\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The never-added case exercises the benign classifier: the first
	// uninstall after a failed install must still complete.
	serviceLog, updateLog := newFakeOpenRC(t, nil, map[string]fakeRCRule{
		"del": {
			exitCode: 1,
			output:   "rc-update: service `" + openrcServiceName + "' is not in the runlevel `" + openrcRunlevel + "'",
		},
	})

	if err := (&openrcManager{}).Uninstall(true); err != nil {
		t.Fatalf("Uninstall() = %v, want nil (an absent runlevel entry is the desired end state)", err)
	}
	if _, err := os.Stat(openrcScriptPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("init script still present: %v", err)
	}
	if _, err := os.Stat(linuxPathsForHome(home).rootDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("purge left the state tree behind: %v", err)
	}

	calls := rcCalls(t, serviceLog)
	if len(calls) != 1 || calls[0] != "--ifexists "+openrcServiceName+" stop" {
		t.Fatalf("rc-service calls = %v, want one --ifexists stop", calls)
	}
	updates := rcCalls(t, updateLog)
	if len(updates) != 1 || updates[0] != "del "+openrcServiceName+" "+openrcRunlevel {
		t.Fatalf("rc-update calls = %v, want one del from the default runlevel", updates)
	}
}

// TestOpenRCUninstallWithoutSUDOUser pins the root-shell contract: plain
// uninstall touches only root-owned files, so `sudo -i` without SUDO_USER
// can still unregister the service. Only --purge needs the account, because
// only it touches the user's state tree.
func TestOpenRCUninstallWithoutSUDOUser(t *testing.T) {
	prev := effectiveUID
	effectiveUID = func() int { return 0 }
	t.Cleanup(func() { effectiveUID = prev })
	t.Setenv("SUDO_USER", "")

	prevEtc := openrcEtcDir
	openrcEtcDir = t.TempDir()
	t.Cleanup(func() { openrcEtcDir = prevEtc })

	if err := os.MkdirAll(filepath.Join(openrcEtcDir, "init.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(openrcScriptPath(), []byte("#!/sbin/openrc-run\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _ = newFakeOpenRC(t, nil, nil)

	if err := (&openrcManager{}).Uninstall(false); err != nil {
		t.Fatalf("Uninstall(false) = %v, want nil from a root shell without SUDO_USER", err)
	}
	if _, err := os.Stat(openrcScriptPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("init script still present: %v", err)
	}
}

// TestOpenRCStartRequiresInstalled guards the CLI contract shared with the
// systemd manager: lifecycle commands on an uninstalled service report
// ErrServiceNotInstalled, not rc-service's prose. The etc seam is pinned so a
// host that really has /etc/init.d/ferngeist-gateway never gets its real
// rc-service exec'd from a test.
func TestOpenRCStartRequiresInstalled(t *testing.T) {
	prev := effectiveUID
	effectiveUID = func() int { return 0 }
	t.Cleanup(func() { effectiveUID = prev })
	prevEtc := openrcEtcDir
	openrcEtcDir = t.TempDir()
	t.Cleanup(func() { openrcEtcDir = prevEtc })

	err := (&openrcManager{}).Start()
	if !errors.Is(err, ErrServiceNotInstalled) {
		t.Fatalf("Start() = %v, want ErrServiceNotInstalled", err)
	}
}

//go:build linux && !android

package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestMain(m *testing.M) {
	if handled, err := RunOpenRCStateWorker(os.Args[1:], os.Stdin, os.Stdout); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	os.Exit(m.Run())
}

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

	prevUID, prevLookup, prevEtc, prevState := effectiveUID, openrcUserLookup, openrcEtcDir, runOpenRCState
	effectiveUID = func() int { return 0 }
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		uid, gid = 65534, 65534
	}
	openrcUserLookup = func(name string) (openrcAccount, error) {
		return openrcAccount{name: name, home: home, uid: uid, gid: gid}, nil
	}
	t.Cleanup(func() {
		effectiveUID, openrcUserLookup, openrcEtcDir = prevUID, prevLookup, prevEtc
		runOpenRCState = prevState
	})

	openrcEtcDir = t.TempDir()
	t.Setenv("SUDO_USER", "gateway-user")

	seedServiceBinarySource(t)
	runOpenRCState = func(account openrcAccount, req openrcStateRequest) (openrcStateResult, error) {
		req.Home, req.UID = account.home, account.uid
		var binary *os.File
		if req.needsBinary() {
			path, err := installCopySource()
			if err != nil {
				return openrcStateResult{}, err
			}
			binary, err = os.Open(path)
			if err != nil {
				return openrcStateResult{}, err
			}
			defer binary.Close()
		}
		return applyOpenRCState(req, binary)
	}

	return openrcAccount{name: "gateway-user", home: home, uid: uid, gid: gid}
}

// TestOpenRCInstallWritesScriptAndStartsService pins the runscript's wire
// format — the file OpenRC actually executes — plus the lifecycle ordering:
// a first install publishes the runscript and rc-update adds it before start.
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
		`gateway_user="` + account.name + `"`,
		"supervisor=supervise-daemon",
		"respawn_delay=2",
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
		openrcServiceName + " start",
	}
	if len(calls) != len(want) {
		t.Fatalf("rc-service calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("rc-service call %d = %q, want %q", i, calls[i], want[i])
		}
	}

	updates := rcCalls(t, updateLog)
	if len(updates) != 1 || updates[0] != "add "+openrcServiceName+" "+openrcRunlevel {
		t.Fatalf("rc-update calls = %v, want one add to the default runlevel", updates)
	}
}

func TestOpenRCReinstallReplacesOldScriptAndLoadsSettingsOnlyInChild(t *testing.T) {
	if os.Geteuid() == 0 {
		runOpenRCRegressionAsServiceUser(t)
		return
	}
	account := seedOpenRCRoot(t, filepath.Join(t.TempDir(), "with  space"))
	paths := linuxPathsForHome(account.home)
	if err := mkdirLinuxState(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(openrcScriptPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	writeOpenRCFixture(t, openrcScriptPath(), "#!/sbin/openrc-run\nlegacy_runscript=loaded\n")
	writeOpenRCFixture(t, paths.overridePath, "FERNGEIST_GATEWAY_TEST_SETTING=from-override\nPATH='/custom path'\n")
	t.Setenv("OPENRC_TEST_SCRIPT", openrcScriptPath())
	result := filepath.Join(t.TempDir(), "result")
	t.Setenv("OPENRC_TEST_RESULT", result)
	binary := filepath.Join(t.TempDir(), "ferngeist-gateway")
	previous := installCopySource
	installCopySource = func() (string, error) { return binary, nil }
	t.Cleanup(func() { installCopySource = previous })
	writeOpenRCFixture(t, binary, `#!/bin/sh
printf 'child:%s:%s:%s:%s:%s:%s\n' "$FERNGEIST_GATEWAY_TEST_SETTING" "$FERNGEIST_GATEWAY_LISTEN_ADDR" \
	"$FERNGEIST_GATEWAY_STATE_DB" "$HOME" "$PWD" "$PATH" >> "$OPENRC_TEST_RESULT"
`)
	newFakeOpenRC(t, nil, nil)
	fakeRCService, _, _ := strings.Cut(os.Getenv("PATH"), ":")
	// Emulate the OpenRC shell's load and start phases. The real supervisor is
	// responsible for dropping privileges before invoking command.
	writeOpenRCFixture(t, filepath.Join(fakeRCService, "rc-service"), `#!/bin/sh
. "$OPENRC_TEST_SCRIPT"
case "$*" in
	*" stop") printf 'stop:%s\n' "${legacy_runscript-unset}" >> "$OPENRC_TEST_RESULT" ;;
	*" start")
		if command -v start_pre >/dev/null; then start_pre || exit; fi
		printf 'start:%s\n' "${FERNGEIST_GATEWAY_TEST_SETTING-unset}" >> "$OPENRC_TEST_RESULT"
		# Match OpenRC's two expansions, including word splitting before eval.
		launch() {
			printf 'account:%s\n' "$1" >> "$OPENRC_TEST_RESULT"
			shift
			"$@"
		}
		eval launch $command_user $command $command_args || exit
		gateway_uid=0
		if eval $command $command_args; then
			printf 'mismatched identity accepted\n' >> "$OPENRC_TEST_RESULT"
		fi
		;;
esac
`)
	if err := (&openrcManager{}).Install(InstallOptions{Host: "127.0.0.1", Port: 5788}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	want := "stop:unset\nstart:unset\naccount:gateway-user\nchild:from-override:127.0.0.1:5788:" +
		paths.dbPath + ":" + account.home + ":" + paths.rootDir + ":/custom path\n"
	if string(got) != want {
		t.Fatalf("lifecycle output = %q, want %q", got, want)
	}
	log, err := os.Stat(filepath.Join(paths.logDir, "supervisor.log"))
	if err != nil || log.Mode().Perm() != 0o600 {
		t.Fatalf("new supervisor log = %v, %v, want permissions 0600", log, err)
	}
}

func runOpenRCRegressionAsServiceUser(t *testing.T) {
	t.Helper()
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// The re-executed test must be able to traverse Go's top-level test dir.
	for _, path := range []string{filepath.Dir(dir), dir} {
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(dir, "service.test")
	if err := replaceFileAtomically(source, target, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(target, "-test.run=^"+t.Name()+"$")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: 65534, Gid: 65534},
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run unprivileged OpenRC regression: %v\n%s", err, out)
	}
}

func TestOpenRCFailedFirstInstallDoesNotPublishService(t *testing.T) {
	seedOpenRCRoot(t, t.TempDir())
	newFakeOpenRC(t, nil, nil)
	binary := filepath.Join(t.TempDir(), "missing-binary")
	previous := installCopySource
	installCopySource = func() (string, error) { return binary, nil }
	t.Cleanup(func() { installCopySource = previous })

	manager := &openrcManager{}
	if err := manager.Install(InstallOptions{}); err == nil {
		t.Fatal("Install succeeded with no source binary")
	}
	status, err := manager.Status()
	if err != nil || status.Installed {
		t.Fatalf("Status after failed first install = %+v, %v, want uninstalled", status, err)
	}
}

func TestOpenRCFailedInstallLeavesStateOwnedByServiceAccount(t *testing.T) {
	account := seedPrivilegedOpenRCStateTest(t)
	paths := linuxPathsForHome(account.home)
	if err := os.MkdirAll(paths.configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for dir := paths.configDir; dir != account.home; dir = filepath.Dir(dir) {
		if err := os.Chown(dir, account.uid, account.gid); err != nil {
			t.Fatal(err)
		}
	}
	optionsFile := filepath.Join(paths.configDir, installOptionsFile)
	writeOpenRCFixture(t, optionsFile, "invalid install options")
	if err := os.Chown(optionsFile, account.uid, account.gid); err != nil {
		t.Fatal(err)
	}

	err := (&openrcManager{}).Install(InstallOptions{})
	if err == nil || !strings.Contains(err.Error(), "decode install options") {
		t.Fatalf("Install() = %v, want the saved options error", err)
	}
	for _, path := range []string{paths.binDir, paths.binaryPath, paths.logDir} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if stat := info.Sys().(*syscall.Stat_t); stat.Uid != uint32(account.uid) || stat.Gid != uint32(account.gid) {
			t.Errorf("%s owner = %d:%d, want %d:%d", path, stat.Uid, stat.Gid, account.uid, account.gid)
		}
	}
}

func TestOpenRCStateWorkerRetainsSettingsAndPurgesAsServiceAccount(t *testing.T) {
	account := seedPrivilegedOpenRCStateTest(t)
	manager := &openrcManager{}
	want := InstallOptions{Host: "127.0.0.1", Port: 7001}
	if err := manager.Install(want); err != nil {
		t.Fatal(err)
	}
	if got, ok := manager.SavedInstallOptions(); !ok || got.Host != want.Host || got.Port != want.Port {
		t.Fatalf("saved options = %+v, %t, want host %s and port %d", got, ok, want.Host, want.Port)
	}
	rootDir := linuxPathsForHome(account.home).rootDir
	if err := manager.Uninstall(true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rootDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("purged state directory: %v, want not found", err)
	}
}

func seedPrivilegedOpenRCStateTest(t *testing.T) openrcAccount {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires a root test process with permission to switch users")
	}
	home := t.TempDir()
	account := seedOpenRCRoot(t, home)
	runOpenRCState = execOpenRCState
	newFakeOpenRC(t, nil, nil)
	if err := os.Chmod(filepath.Dir(home), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(home, account.uid, account.gid); err != nil {
		t.Fatal(err)
	}
	return account
}

func TestOpenRCFailedCopyLeavesExistingServiceRegistered(t *testing.T) {
	seedOpenRCRoot(t, t.TempDir())
	serviceLog, _ := newFakeOpenRC(t, nil, nil)
	if err := os.MkdirAll(filepath.Dir(openrcScriptPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	const installedScript = "#!/sbin/openrc-run\ncommand=/usr/bin/ferngeist-gateway\n"
	writeOpenRCFixture(t, openrcScriptPath(), installedScript)
	binary := filepath.Join(t.TempDir(), "missing-binary")
	previous := installCopySource
	installCopySource = func() (string, error) { return binary, nil }
	t.Cleanup(func() { installCopySource = previous })

	if err := (&openrcManager{}).Install(InstallOptions{}); err == nil {
		t.Fatal("Install succeeded with no source binary")
	}
	content, err := os.ReadFile(openrcScriptPath())
	if err != nil || string(content) != installedScript {
		t.Fatalf("registered script changed during failed copy: %v", err)
	}
	if calls := rcCalls(t, serviceLog); len(calls) != 0 {
		t.Fatalf("failed copy interrupted the existing service: %v", calls)
	}
}

func writeOpenRCFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
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

// TestInitSystemDetectionIgnoresCallerPath pins the fix for a real Alpine host:
// rc-service lives in /sbin, and installing with a minimal PATH made detection
// fall through to the systemd manager, which then reported a missing systemctl
// on a host that has OpenRC. Detection must find a standard-directory tool the
// caller's PATH does not contain, and exec the same absolute path it found.
func TestInitSystemDetectionIgnoresCallerPath(t *testing.T) {
	dir := t.TempDir()
	rcService := filepath.Join(dir, "rc-service")
	if err := os.WriteFile(rcService, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake rc-service: %v", err)
	}
	// The tool is not reachable by name: only the standard-directory search
	// can find it.
	t.Setenv("PATH", "")
	prev := initToolDirs
	initToolDirs = []string{dir}
	t.Cleanup(func() { initToolDirs = prev })

	if got := detectLinuxInitSystem(); got != initOpenRC {
		t.Fatalf("detectLinuxInitSystem() = %q, want %q when rc-service is off PATH", got, initOpenRC)
	}
	cmd, err := openrcCmd("rc-service", "status")
	if err != nil {
		t.Fatalf("openrcCmd(rc-service) = %v, want the resolved tool", err)
	}
	if cmd.Path != rcService {
		t.Fatalf("openrcCmd path = %q, want the resolved %q", cmd.Path, rcService)
	}
}

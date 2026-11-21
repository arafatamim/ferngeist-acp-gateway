//go:build linux && !android

package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// OpenRC services are system services: the runscript lives in /etc/init.d and
// rc-service requires root, unlike the per-user systemd manager. Install and
// uninstall therefore run under sudo, and the generated script drops the
// daemon back to the sudoing user's account (command_user) so state stays in
// that user's home and spawned agents never run as root.
//
// OpenRC 0.60+ does have user services (--user/-U, init scripts in
// ${XDG_CONFIG_HOME:-~/.config}/rc/init.d), but the gateway needs to be up
// without a login session, and OpenRC only starts a user session at boot
// through a *system* `user.$USER` service — root plus pam_openrc plus
// XDG_RUNTIME_DIR. The system service the same root buy, with fewer moving
// parts.

// openrcRunlevel is the boot runlevel the service is enabled in, OpenRC's
// counterpart of systemd's default.target.
const openrcRunlevel = "default"

// openrcEtcDir anchors the two OpenRC paths the manager touches (the init
// script and its runlevel symlink). A var so tests can redirect them off the
// real /etc, the same pattern as installCopySource.
var openrcEtcDir = "/etc"

// openrcScriptTemplate is the runscript Install regenerates on every install.
// Runtime settings never live here — they stay in the env file the systemd
// manager also loads, sourced in start_pre so supervise-daemon inherits them.
const openrcScriptTemplate = `#!/sbin/openrc-run

# Managed by ferngeist-gateway daemon install; regenerated on every install.

description="Ferngeist Gateway daemon"

command="%s"
command_args="daemon run"
command_user="%s"
directory="%s"
output_log="%s"
error_log="%s"

supervisor=supervise-daemon
# Mirrors the systemd unit's Restart=on-failure RestartSec=2, bounded the way
# systemd's start limit bounds it: give up after 5 respawns within 60s.
respawn_delay=2
respawn_max=5
respawn_period=60

depend() {
	need localmount
	after net
}

# supervise-daemon inherits this shell's exported environment, so set -a exports
# every FERNGEIST_GATEWAY_* assignment from the same env file the systemd unit
# loads via EnvironmentFile. HOME must be exported because OpenRC sets no login
# environment for command_user (systemd does for User= units) and the harness
# resolves agent config from it. The override file is optional.
#
# The env file also sets PATH to the installing user's, which must NOT stay in
# this shell: openrc-run resolves its own helpers through PATH, and with a
# replaced PATH it loses them mid-start. It then reports "failed to start" and
# never records the service as started, while supervise-daemon keeps running the
# daemon -- so status and stop both saw a stopped service and uninstall orphaned
# a live one. Keep openrc-run's PATH and hand the user's PATH to the child with
# supervise-daemon's own --env instead.
start_pre() {
	_rc_path="$PATH"
	set -a
	. "%s"
	if [ -f "%s" ]; then
		. "%s"
	fi
	set +a
	_gateway_path="$PATH"
	PATH="$_rc_path"
	export HOME="%s"
	supervise_daemon_args="--env PATH=$_gateway_path"
}
`

type openrcManager struct{}

// effectiveUID and openrcUserLookup are real work with test overrides (the
// same pattern as installCopySource): tests cannot drop privileges or conjure
// a passwd entry, so they replace both.
var (
	effectiveUID     = os.Geteuid
	openrcUserLookup = lookupSystemUser
)

type openrcAccount struct {
	name string
	home string
	uid  int
	gid  int
}

func lookupSystemUser(name string) (openrcAccount, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return openrcAccount{}, fmt.Errorf("look up user %q: %w", name, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return openrcAccount{}, fmt.Errorf("look up user %q: bad uid %q: %w", name, u.Uid, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return openrcAccount{}, fmt.Errorf("look up user %q: bad gid %q: %w", name, u.Gid, err)
	}
	return openrcAccount{name: u.Username, home: u.HomeDir, uid: uid, gid: gid}, nil
}

// openrcTargetAccount resolves the account the daemon runs as: the sudoing
// user. The runscript is a system service, so an install without a target
// account would run the gateway as root — every spawned agent with it. (OpenRC
// user services would carry their own uid, but they are login-bound; see the
// package comment.)
func openrcTargetAccount() (openrcAccount, error) {
	if err := requireOpenRCRoot(); err != nil {
		return openrcAccount{}, err
	}
	sudoUser := os.Getenv("SUDO_USER")
	if sudoUser == "" {
		return openrcAccount{}, fmt.Errorf(
			"%w: SUDO_USER is unset; run `sudo ferngeist-gateway daemon install` from the account the gateway should run as",
			ErrServiceUnsupportedConfig)
	}
	return openrcUserLookup(sudoUser)
}

// requireOpenRCRoot guards the rc-service mutations: OpenRC runscripts run
// as root, and the remedy travels in the error itself. This is
// ErrServiceUnsupportedConfig rather than ErrServicePermissionDenied on
// purpose: the CLI's permission hint tells the operator NOT to use sudo,
// which is correct for the per-user managers and the opposite of what an
// OpenRC system service needs.
func requireOpenRCRoot() error {
	if effectiveUID() == 0 {
		return nil
	}
	return fmt.Errorf("%w: OpenRC services are system services; rerun with sudo", ErrServiceUnsupportedConfig)
}

// openrcHome resolves the state home for read operations (status, logs,
// saved options): the sudoing user under sudo, the invoking user's home
// otherwise.
func openrcHome() (string, error) {
	sudoUser := os.Getenv("SUDO_USER")
	if sudoUser != "" && effectiveUID() == 0 {
		account, err := openrcUserLookup(sudoUser)
		if err != nil {
			return "", err
		}
		return account.home, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory: %w", err)
	}
	if strings.TrimSpace(home) == "" {
		return "", errors.New("resolve user home directory: home is empty")
	}
	return home, nil
}

func openrcScriptPath() string {
	return filepath.Join(openrcEtcDir, "init.d", openrcServiceName)
}

func openrcRunlevelPath() string {
	return filepath.Join(openrcEtcDir, "runlevels", openrcRunlevel, openrcServiceName)
}

func (m *openrcManager) Install(options InstallOptions) error {
	options = NormalizeInstallOptions(options)
	if err := ValidateInstallOptions(options); err != nil {
		return err
	}
	account, err := openrcTargetAccount()
	if err != nil {
		return err
	}
	paths := linuxPathsForHome(account.home)

	if err := mkdirLinuxState(paths); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(openrcScriptPath()), 0o755); err != nil {
		return fmt.Errorf("create init script directory: %w", err)
	}

	// A reinstall with a different binary must stop the running daemon
	// first: the old process holds the target open for execution and Linux
	// refuses to overwrite it (ETXTBSY). --ifexists makes the fresh-install
	// case a no-op; the rc-service start below brings the daemon back.
	if err := m.rcService("--ifexists", openrcServiceName, "stop"); err != nil {
		return err
	}

	if err := installOpenRCFiles(paths, options, account); err != nil {
		return m.restoreStoppedService(err)
	}

	// sudo created the whole state tree as root; the daemon runs as the
	// account and writes its db, logs and managed binaries there.
	if err := chownTree(paths.rootDir, account.uid, account.gid); err != nil {
		return m.restoreStoppedService(fmt.Errorf("chown service data to %s: %w", account.name, err))
	}

	if err := m.rcUpdate("add", openrcServiceName, openrcRunlevel); err != nil {
		return m.restoreStoppedService(err)
	}
	return m.rcService(openrcServiceName, "start")
}

// restoreStoppedService brings the daemon back after a post-stop install
// failure: the initial stop left a previously-running daemon down, and a
// best-effort start beats leaving it silently stopped. Safe at every call
// site — whatever script is on disk is the freshest complete install state.
func (m *openrcManager) restoreStoppedService(err error) error {
	_ = m.rcService("--ifexists", openrcServiceName, "start")
	return err
}

func installOpenRCFiles(paths linuxPaths, options InstallOptions, account openrcAccount) error {
	written, err := writeLinuxServiceFiles(paths, options)
	if err != nil {
		return err
	}
	return writeOpenRCScript(written, account)
}

func writeOpenRCScript(paths linuxPaths, account openrcAccount) error {
	logPath := filepath.Join(paths.logDir, "supervisor.log")
	script := fmt.Sprintf(openrcScriptTemplate,
		escapeShellQuoted(paths.execPath),
		escapeShellQuoted(account.name),
		escapeShellQuoted(paths.rootDir),
		escapeShellQuoted(logPath),
		escapeShellQuoted(logPath),
		escapeShellQuoted(paths.envPath),
		escapeShellQuoted(paths.overridePath),
		escapeShellQuoted(paths.overridePath),
		escapeShellQuoted(account.home),
	)
	if err := os.WriteFile(openrcScriptPath(), []byte(script), 0o755); err != nil {
		return fmt.Errorf("write OpenRC init script: %w", err)
	}
	return nil
}

func (m *openrcManager) Uninstall(purge bool) error {
	if err := requireOpenRCRoot(); err != nil {
		return err
	}

	// Only purge touches the user's state tree, so only it needs the account:
	// a root shell without SUDO_USER (sudo -i, su -) can still unregister the
	// service. Resolved before any mutation so a failed lookup purges nothing.
	var account openrcAccount
	if purge {
		resolved, err := openrcTargetAccount()
		if err != nil {
			return err
		}
		account = resolved
	}

	if err := m.rcService("--ifexists", openrcServiceName, "stop"); err != nil {
		return err
	}
	if err := m.rcUpdateDel(); err != nil {
		return err
	}
	if err := os.Remove(openrcScriptPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove OpenRC init script: %w", err)
	}

	if purge {
		paths := linuxPathsForHome(account.home)
		if err := os.RemoveAll(paths.rootDir); err != nil {
			return fmt.Errorf("purge service data: %w", err)
		}
	}
	return nil
}

func (m *openrcManager) Start() error {
	if err := m.ensureRootAndInstalled(); err != nil {
		return err
	}
	return m.rcService(openrcServiceName, "start")
}

func (m *openrcManager) Stop() error {
	if err := m.ensureRootAndInstalled(); err != nil {
		return err
	}
	return m.rcService(openrcServiceName, "stop")
}

func (m *openrcManager) Restart() error {
	if err := m.ensureRootAndInstalled(); err != nil {
		return err
	}
	return m.rcService(openrcServiceName, "restart")
}

// ensureRootAndInstalled is the shared guard for the lifecycle commands. The
// installed check comes first so an unprivileged caller on a host without an
// install learns that, not the sudo instruction — the same contract the
// systemd manager keeps.
func (m *openrcManager) ensureRootAndInstalled() error {
	if _, err := os.Stat(openrcScriptPath()); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrServiceNotInstalled
		}
		return fmt.Errorf("read init script: %w", err)
	}
	return requireOpenRCRoot()
}

func (m *openrcManager) Status() (Status, error) {
	scriptPath := openrcScriptPath()
	_, statErr := os.Stat(scriptPath)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return Status{Installed: false, UnitPath: scriptPath}, nil
		}
		return Status{}, fmt.Errorf("read init script: %w", statErr)
	}

	status := Status{Installed: true, UnitPath: scriptPath}
	if _, err := os.Stat(openrcRunlevelPath()); err == nil {
		status.UnitFileState = "enabled"
	}

	// rc-service status reports with informational exit codes (stopped is 3,
	// crashed is 32), so only the message text is a failure signal.
	cmd, err := openrcCmd("rc-service", openrcServiceName, "status")
	if err != nil {
		return Status{}, err
	}
	out, runErr := cmd.CombinedOutput()
	message := strings.TrimSpace(string(out))
	if runErr != nil && message == "" {
		message = runErr.Error()
	}
	active, sub, ok := openrcStatusFromMessage(message)
	if !ok {
		return Status{}, fmt.Errorf("rc-service %s status failed: %s", openrcServiceName, message)
	}
	status.ActiveState = active
	status.SubState = sub
	return status, nil
}

func (m *openrcManager) SavedInstallOptions() (InstallOptions, bool) {
	home, err := openrcHome()
	if err != nil {
		return InstallOptions{}, false
	}
	return loadInstallOptions(linuxPathsForHome(home).configDir)
}

func (m *openrcManager) LogDir() (string, error) {
	home, err := openrcHome()
	if err != nil {
		return "", err
	}
	return linuxPathsForHome(home).logDir, nil
}

// openrcCmd resolves an OpenRC tool to an absolute path before exec. Detection
// already resolves it, and the tool does not have to be on the caller's PATH:
// Alpine keeps rc-service and rc-update in /sbin.
func openrcCmd(tool string, args ...string) (*exec.Cmd, error) {
	path, ok := findInitTool(tool)
	if !ok {
		return nil, fmt.Errorf("%w: %s is not available", ErrServiceUnsupportedConfig, tool)
	}
	return exec.Command(path, args...), nil
}

func (m *openrcManager) rcService(args ...string) error {
	cmd, err := openrcCmd("rc-service", args...)
	if err != nil {
		return err
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(out))
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("rc-service %s failed: %s", strings.Join(args, " "), message)
	}
	return nil
}

func (m *openrcManager) rcUpdate(args ...string) error {
	cmd, err := openrcCmd("rc-update", args...)
	if err != nil {
		return err
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(out))
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("rc-update %s failed: %s", strings.Join(args, " "), message)
	}
	return nil
}

// rcUpdateDel removes the service from the boot runlevel. Absence is the
// desired end state, so rc-update's "not in the runlevel" refusal is a no-op —
// the same treatment systemd's "no such unit" gets.
func (m *openrcManager) rcUpdateDel() error {
	cmd, err := openrcCmd("rc-update", "del", openrcServiceName, openrcRunlevel)
	if err != nil {
		return err
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	message := strings.TrimSpace(string(out))
	if message == "" {
		message = err.Error()
	}
	if isRcUpdateNotInRunlevelMessage(openrcServiceName, message) {
		return nil
	}
	return fmt.Errorf("rc-update del %s failed: %s", openrcServiceName, message)
}

// escapeShellQuoted escapes a value for use inside a double-quoted string in
// the generated runscript.
func escapeShellQuoted(value string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`")
	return r.Replace(value)
}

// chownTree hands the sudo-created state tree to the account the daemon runs
// as, so its db, logs and managed binaries are writable without root.
func chownTree(root string, uid, gid int) error {
	return filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chown(path, uid, gid)
	})
}

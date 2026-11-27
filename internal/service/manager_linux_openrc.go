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
// manager also loads. OpenRC passes these trusted paths to a child shell
// after dropping privileges to the service account.
const openrcScriptTemplate = `#!/sbin/openrc-run

# Managed by ferngeist-gateway daemon install; regenerated on every install.

description="Ferngeist Gateway daemon"

gateway_user="%s"
gateway_uid="%d"
gateway_dir="%s"
gateway_log="%s"
gateway_env="%s"
gateway_override="%s"
gateway_home="%s"
gateway_bin="%s"

command="/bin/sh"
command_user='"$gateway_user"'
command_args="%s"

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
`

// OpenRC evaluates command_args once more when assembling the supervise-daemon
// invocation. Resolve paths only inside that evaluation, as quoted arguments.
// The child loads the optional override after the main file and keeps its PATH
// out of OpenRC's own shell. Set HOME for the agent harness without a login.
const openrcCommandArgs = `-c 'uid=$(command -p id -u) || exit; [ "$uid" = "$1" ] && [ "$uid" != 0 ] || exit 1; shift; ` +
	`cd "$1" || exit; previous_umask=$(umask); umask 077; exec >>"$2" 2>&1; umask "$previous_umask"; ` +
	`set -a; . "$3" || exit; if [ -f "$4" ]; then . "$4" || exit; fi; set +a; export HOME="$5"; exec "$6" daemon run' ` +
	`ferngeist "$gateway_uid" "$gateway_dir" "$gateway_log" "$gateway_env" "$gateway_override" "$gateway_home" "$gateway_bin"`

type openrcManager struct{}

// effectiveUID and openrcUserLookup are real work with test overrides (the
// same pattern as installCopySource): tests cannot drop privileges or conjure
// a passwd entry, so they replace both.
var (
	effectiveUID     = os.Geteuid
	openrcUserLookup = lookupSystemUser
)

type openrcAccount struct {
	name   string
	home   string
	uid    int
	gid    int
	groups []uint32
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
	groupIDs, err := u.GroupIds()
	if err != nil {
		return openrcAccount{}, fmt.Errorf("look up groups for %q: %w", name, err)
	}
	groups := make([]uint32, 0, len(groupIDs))
	for _, id := range groupIDs {
		group, err := strconv.ParseUint(id, 10, 32)
		if err != nil {
			return openrcAccount{}, fmt.Errorf("look up groups for %q: %w", name, err)
		}
		groups = append(groups, uint32(group))
	}
	return openrcAccount{name: u.Username, home: u.HomeDir, uid: uid, gid: gid, groups: groups}, nil
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
	account, err := openrcUserLookup(sudoUser)
	if err != nil {
		return openrcAccount{}, err
	}
	if account.uid <= 0 {
		return openrcAccount{}, fmt.Errorf("%w: the OpenRC service account must be non-root", ErrServiceUnsupportedConfig)
	}
	if numericUID, err := strconv.Atoi(account.name); err == nil && numericUID != account.uid {
		return openrcAccount{}, fmt.Errorf("%w: OpenRC interprets numeric account names as UIDs", ErrServiceUnsupportedConfig)
	}
	return account, nil
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

	source, err := installCopySource()
	if err != nil {
		return fmt.Errorf("resolve current binary: %w", err)
	}
	stable, managed := stableExecutable(source)
	if managed {
		paths.execPath = stable
	}
	req := openrcStateRequest{Action: openrcStateInstall, Options: options, ManagedBinary: managed}
	if err := os.MkdirAll(filepath.Dir(openrcScriptPath()), 0o755); err != nil {
		return fmt.Errorf("create init script directory: %w", err)
	}
	existing, err := m.stopForInstall(paths, account, &req)
	if err != nil {
		return err
	}

	if _, err := runOpenRCState(account, req); err != nil {
		return m.restoreStoppedService(existing, err)
	}
	if !existing {
		if err := writeOpenRCScript(paths, account); err != nil {
			return err
		}
	}

	if err := m.rcUpdate("add", openrcServiceName, openrcRunlevel); err != nil {
		return m.restoreStoppedService(existing, err)
	}
	return m.rcService(openrcServiceName, "start")
}

func (m *openrcManager) stopForInstall(paths linuxPaths, account openrcAccount, install *openrcStateRequest) (bool, error) {
	_, err := os.Lstat(openrcScriptPath())
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read init script: %w", err)
	}
	// A package-manager install may have no private binary. Prepare it as the
	// service account before a failed copy could strand the existing service.
	result, err := runOpenRCState(account, openrcStateRequest{
		Action: openrcStatePrepare, ManagedBinary: install.ManagedBinary,
	})
	if err != nil {
		return false, err
	}
	install.BinaryReady = result.BinaryReady

	// Replace the old runscript before rc-service can load it, then stop
	// the daemon before replacing the running binary (ETXTBSY).
	if err := writeOpenRCScript(paths, account); err != nil {
		return false, err
	}
	if err := m.rcService("--ifexists", openrcServiceName, "stop"); err != nil {
		return false, err
	}
	return true, nil
}

// restoreStoppedService brings the daemon back after a post-stop install
// failure: the initial stop left a previously-running daemon down, and a
// best-effort start beats leaving it silently stopped. Safe at every call
// site — whatever script is on disk is the freshest complete install state.
func (m *openrcManager) restoreStoppedService(reinstall bool, err error) error {
	if reinstall {
		_ = m.rcService("--ifexists", openrcServiceName, "start")
	}
	return err
}

func writeOpenRCScript(paths linuxPaths, account openrcAccount) error {
	logPath := filepath.Join(paths.logDir, "supervisor.log")
	script := fmt.Sprintf(openrcScriptTemplate,
		escapeShellQuoted(account.name),
		account.uid,
		escapeShellQuoted(paths.rootDir),
		escapeShellQuoted(logPath),
		escapeShellQuoted(paths.envPath),
		escapeShellQuoted(paths.overridePath),
		escapeShellQuoted(account.home),
		escapeShellQuoted(paths.execPath),
		escapeShellQuoted(openrcCommandArgs),
	)
	if err := writeFileAtomically(strings.NewReader(script), openrcScriptPath(), 0o755); err != nil {
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
		if _, err := runOpenRCState(account, openrcStateRequest{Action: openrcStatePurge}); err != nil {
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
	if effectiveUID() == 0 {
		account, err := openrcTargetAccount()
		if err != nil {
			return InstallOptions{}, false
		}
		result, err := runOpenRCState(account, openrcStateRequest{Action: openrcStateLoad})
		return result.Options, err == nil
	}
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

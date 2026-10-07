// This file holds the launchd manager logic that is compiled on every OS so
// the unit tests in manager_darwin_test.go can exercise the path/plist/env
// behavior on the Linux CI runner (TestDarwinPathsUsesHome and
// TestDarwinInstallWritesPlist run on any POSIX OS). All six Manager methods
// (Install/Uninstall/Start/Stop/Restart/Status) live here so *darwinManager
// satisfies Manager on every OS. The darwin-only pieces that need os.Getuid —
// darwinUID, newOSManager, and the init that points darwinServiceTarget at the
// real gui/<uid>/<label> launchd target — live in manager_darwin.go, which
// carries the //go:build darwin tag.

package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	darwinLabel        = "com.ferngeist.gateway"
	darwinPlistName    = darwinLabel + ".plist"
	darwinLaunchctlDir = "Library/LaunchAgents"

	// darwinStdoutPath discards stdout: the daemon already writes JSON logs to
	// gateway.log in LOG_DIR, so a captured copy would only grow unrotated.
	darwinStdoutPath = "/dev/null"
)

// darwinPlistTemplate is the launchd per-user LaunchAgent plist. It starts at
// login (RunAtLoad), keeps the daemon alive, and carries the runtime
// environment inline so launchd never needs editing. The environment entries
// come from darwinEnv, the same list daemon.plist.env is written from.
const darwinPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>daemon</string>
		<string>run</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
%s	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`

type darwinManager struct{}

// darwinServiceTarget resolves the launchd service target (gui/<uid>/<label>)
// used by the control methods. It is overridden on darwin via init in
// manager_darwin.go; on other OSes it stays a stub returning "" so the var is
// never nil and *darwinManager still satisfies Manager.
var darwinServiceTarget = func() string { return "" } // overridden on darwin

// darwinDomainTarget resolves the launchd domain (gui/<uid>) that bootstrap
// operates on. Overridden on darwin like darwinServiceTarget.
var darwinDomainTarget = func() string { return "" } // overridden on darwin

func (m *darwinManager) Install(options InstallOptions) error {
	options = NormalizeInstallOptions(options)
	if err := ValidateInstallOptions(options); err != nil {
		return err
	}
	if err := m.ensureLaunchctlAvailable(); err != nil {
		return err
	}

	paths, err := resolveDarwinPaths()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(paths.rootDir, 0o755); err != nil {
		return fmt.Errorf("create service root directory: %w", err)
	}
	if err := os.MkdirAll(paths.binDir, 0o755); err != nil {
		return fmt.Errorf("create service bin directory: %w", err)
	}
	if err := os.MkdirAll(paths.configDir, 0o755); err != nil {
		return fmt.Errorf("create service config directory: %w", err)
	}
	if err := os.MkdirAll(paths.logDir, 0o755); err != nil {
		return fmt.Errorf("create service log directory: %w", err)
	}
	if err := os.MkdirAll(paths.managedBinDir, 0o755); err != nil {
		return fmt.Errorf("create managed bin directory: %w", err)
	}
	if err := os.MkdirAll(paths.launchAgentsDir, 0o755); err != nil {
		return fmt.Errorf("create launch agents directory: %w", err)
	}

	// Stop the running instance first: a bootstrap over a loaded job keeps the
	// old process, and overwriting a running signed binary in place can crash it.
	if err := m.bootout(); err != nil {
		return err
	}
	if err := installCurrentBinary(paths.binaryPath); err != nil {
		return err
	}
	if err := writeDarwinEnvFile(paths, options); err != nil {
		return err
	}
	if err := writeDarwinPlist(paths, options); err != nil {
		return err
	}
	if err := saveInstallOptions(paths.configDir, options); err != nil {
		return err
	}

	return m.bootstrapAndKick()
}

// SavedInstallOptions returns the options persisted by the last Install.
func (m *darwinManager) SavedInstallOptions() (InstallOptions, bool) {
	paths, err := resolveDarwinPaths()
	if err != nil {
		return InstallOptions{}, false
	}
	return loadInstallOptions(paths.configDir)
}

// bootout unloads the job, treating "not loaded" as already stopped. Unlike
// `kill`, it stops KeepAlive from respawning the daemon.
func (m *darwinManager) bootout() error {
	if err := m.launchctl("bootout", darwinServiceTarget()); err != nil && !isLaunchctlNotFound(err) {
		return err
	}
	return nil
}

// bootstrap loads the job from the plist; enable first so a job disabled by an
// older `unload -w` can load.
func (m *darwinManager) bootstrap() error {
	paths, err := resolveDarwinPaths()
	if err != nil {
		return err
	}
	if err := m.launchctl("enable", darwinServiceTarget()); err != nil {
		return err
	}
	// bootout is asynchronous: a bootstrap issued while launchd is still
	// tearing the old job down fails with "Bootstrap failed: 5: Input/output
	// error". Retry briefly.
	// ponytail: fixed 5s budget; poll `launchctl print` if teardown is slower.
	for attempt := 0; ; attempt++ {
		err = m.launchctl("bootstrap", darwinDomainTarget(), paths.plistPath)
		if err == nil || attempt == 9 {
			return err
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (m *darwinManager) bootstrapAndKick() error {
	if err := m.bootstrap(); err != nil {
		return err
	}
	return m.launchctl("kickstart", darwinServiceTarget())
}

// loaded reports whether launchd currently has the job loaded.
func (m *darwinManager) loaded() (bool, error) {
	if _, err := m.launchctlOutput("print", darwinServiceTarget()); err != nil {
		if isLaunchctlNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (m *darwinManager) Uninstall(purge bool) error {
	if err := m.ensureLaunchctlAvailable(); err != nil {
		return err
	}

	paths, err := resolveDarwinPaths()
	if err != nil {
		return err
	}

	if err := m.bootout(); err != nil {
		return err
	}

	if err := os.Remove(paths.plistPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove launch agent plist: %w", err)
	}

	if purge {
		if err := os.RemoveAll(paths.rootDir); err != nil {
			return fmt.Errorf("purge service data: %w", err)
		}
	}

	return nil
}

func (m *darwinManager) Start() error {
	if err := m.ensureLaunchctlAvailable(); err != nil {
		return err
	}
	if err := m.ensureInstalled(); err != nil {
		return err
	}
	loaded, err := m.loaded()
	if err != nil {
		return err
	}
	if !loaded {
		return m.bootstrapAndKick()
	}
	return m.launchctl("kickstart", darwinServiceTarget())
}

// Stop boots the job out (launchctl kill would just be respawned by KeepAlive).
// The plist stays, so Status still reports Installed and Start can bootstrap.
func (m *darwinManager) Stop() error {
	if err := m.ensureLaunchctlAvailable(); err != nil {
		return err
	}
	if err := m.ensureInstalled(); err != nil {
		return err
	}
	return m.bootout()
}

func (m *darwinManager) Restart() error {
	if err := m.ensureLaunchctlAvailable(); err != nil {
		return err
	}
	if err := m.ensureInstalled(); err != nil {
		return err
	}
	loaded, err := m.loaded()
	if err != nil {
		return err
	}
	if !loaded {
		if err := m.bootstrap(); err != nil {
			return err
		}
	}
	return m.launchctl("kickstart", "-k", darwinServiceTarget())
}

func (m *darwinManager) Status() (Status, error) {
	if err := m.ensureLaunchctlAvailable(); err != nil {
		return Status{}, err
	}

	paths, err := resolveDarwinPaths()
	if err != nil {
		return Status{}, err
	}

	// Installed means the plist exists, not that launchd has it loaded: a
	// stopped (booted-out) service must still be startable.
	if _, err := os.Stat(paths.plistPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Status{Installed: false, UnitPath: paths.plistPath}, nil
		}
		return Status{}, fmt.Errorf("stat launch agent plist: %w", err)
	}

	out, err := m.launchctlOutput("print", darwinServiceTarget())
	if err != nil {
		if isLaunchctlNotFound(err) {
			return Status{
				Installed:     true,
				UnitPath:      paths.plistPath,
				UnitFileState: paths.plistPath,
				LoadState:     "not-loaded",
				ActiveState:   "inactive",
				SubState:      "stopped",
			}, nil
		}
		return Status{}, err
	}

	status := Status{
		Installed:   true,
		UnitPath:    paths.plistPath,
		LoadState:   "loaded",
		SubState:    "running",
		ActiveState: "active",
	}
	// launchctl print contains multiple "state =" lines: the job state at the
	// top and the spawned process's state inside its own block. Only the
	// FIRST one (the job state) determines whether the service is active;
	// the process state may legitimately differ (exited, waiting) between
	// KeepAlive restarts.
	for line := range strings.SplitSeq(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "state =") {
			value := strings.TrimSpace(strings.TrimPrefix(trimmed, "state ="))
			status.SubState = value
			if value == "running" {
				status.ActiveState = "active"
			} else {
				status.ActiveState = "inactive"
			}
			break
		}
	}
	status.UnitFileState = paths.plistPath

	return status, nil
}

func (m *darwinManager) ensureInstalled() error {
	status, err := m.Status()
	if err != nil {
		return err
	}
	if !status.Installed {
		return ErrServiceNotInstalled
	}
	return nil
}

func (m *darwinManager) ensureLaunchctlAvailable() error {
	if _, err := exec.LookPath("launchctl"); err != nil {
		return fmt.Errorf("%w: launchctl is not available", ErrServiceUnsupportedConfig)
	}
	return nil
}

func (m *darwinManager) launchctl(args ...string) error {
	_, err := m.launchctlOutput(args...)
	return err
}

func (m *darwinManager) launchctlOutput(args ...string) (string, error) {
	cmd := exec.Command("launchctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(out))
		if message == "" {
			message = err.Error()
		}
		if isLaunchctlPermissionDenied(message) {
			return "", fmt.Errorf("%w: launchctl access was denied for the current user", ErrServicePermissionDenied)
		}
		return "", fmt.Errorf("launchctl %s failed: %s", strings.Join(args, " "), message)
	}
	return string(out), nil
}

type darwinPaths struct {
	rootDir         string
	binDir          string
	configDir       string
	logDir          string
	managedBinDir   string
	dbPath          string
	binaryPath      string
	envPath         string
	launchAgentsDir string
	plistPath       string
	stderrLogPath   string
}

func resolveDarwinPaths() (darwinPaths, error) {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return darwinPaths{}, fmt.Errorf("resolve user home directory: %w", err)
	}

	rootDir := filepath.Join(home, "Library", "Application Support", "Ferngeist Gateway")
	launchAgentsDir := filepath.Join(home, darwinLaunchctlDir)

	return darwinPaths{
		rootDir:         rootDir,
		binDir:          filepath.Join(rootDir, "bin"),
		configDir:       filepath.Join(rootDir, "config"),
		logDir:          filepath.Join(rootDir, "logs"),
		managedBinDir:   filepath.Join(rootDir, "managed-bin"),
		dbPath:          filepath.Join(rootDir, "ferngeist-gateway.db"),
		binaryPath:      filepath.Join(rootDir, "bin", "ferngeist-gateway"),
		envPath:         filepath.Join(rootDir, "config", "daemon.plist.env"),
		launchAgentsDir: launchAgentsDir,
		plistPath:       filepath.Join(launchAgentsDir, darwinPlistName),
		stderrLogPath:   filepath.Join(rootDir, "logs", "daemon.err.log"),
	}, nil
}

type darwinEnvEntry struct{ key, value string }

// darwinEnv is the single ordered environment list shared by the plist (the
// source of truth, since launchd never reads daemon.plist.env) and the env file.
func darwinEnv(paths darwinPaths, options InstallOptions) []darwinEnvEntry {
	env := []darwinEnvEntry{
		{"FERNGEIST_GATEWAY_LISTEN_ADDR", ListenAddr(options)},
		{"FERNGEIST_GATEWAY_ENABLE_LAN", darwinBool(!isLoopbackHost(options.Host))},
		{"FERNGEIST_GATEWAY_STATE_DB", paths.dbPath},
		{"FERNGEIST_GATEWAY_LOG_DIR", paths.logDir},
		{"FERNGEIST_GATEWAY_MANAGED_BIN_DIR", paths.managedBinDir},
	}
	// A persisted remote URL must not survive into a LAN-only service: it
	// would otherwise keep advertising the old tailnet URL.
	if includePublicURL(options) {
		env = append(env, darwinEnvEntry{"FERNGEIST_GATEWAY_PUBLIC_BASE_URL", options.PublicURL})
	}
	if remoteModeRequested(options.TailscaleMode) {
		env = append(env, darwinEnvEntry{"FERNGEIST_GATEWAY_TAILSCALE_MODE", options.TailscaleMode})
	}
	return env
}

// writeDarwinEnvFile writes an informational copy of the environment.
func writeDarwinEnvFile(paths darwinPaths, options InstallOptions) error {
	options = NormalizeInstallOptions(options)
	var content strings.Builder
	for _, e := range darwinEnv(paths, options) {
		content.WriteString(e.key + "=" + e.value + "\n")
	}
	if err := os.WriteFile(paths.envPath, []byte(content.String()), 0o600); err != nil {
		return fmt.Errorf("write service environment file: %w", err)
	}
	return nil
}

func writeDarwinPlist(paths darwinPaths, options InstallOptions) error {
	options = NormalizeInstallOptions(options)

	var env strings.Builder
	for _, e := range darwinEnv(paths, options) {
		fmt.Fprintf(&env, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", escapePlist(e.key), escapePlist(e.value))
	}

	plistBody := fmt.Sprintf(
		darwinPlistTemplate,
		darwinLabel,
		escapePlist(paths.binaryPath),
		env.String(),
		darwinStdoutPath,
		escapePlist(paths.stderrLogPath),
	)

	if err := os.WriteFile(paths.plistPath, []byte(plistBody), 0o644); err != nil {
		return fmt.Errorf("write launch agent plist: %w", err)
	}
	return nil
}

func darwinBool(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func escapePlist(value string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	).Replace(value)
}

// isLaunchctlNotFound matches print/bootout errors for a job that is not
// loaded ("Could not find service", "Boot-out failed: 3: No such process", ...).
func isLaunchctlNotFound(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "could not find service") ||
		strings.Contains(message, "could not find specified service") ||
		strings.Contains(message, "no such process") ||
		strings.Contains(message, "not found")
}

func isLaunchctlPermissionDenied(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(lower, "permission denied") ||
		strings.Contains(lower, "operation not permitted")
}

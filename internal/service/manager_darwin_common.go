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
)

const (
	darwinLabel        = "com.ferngeist.gateway"
	darwinPlistName    = darwinLabel + ".plist"
	darwinLaunchctlDir = "Library/LaunchAgents"
)

// darwinPlistTemplate is the launchd per-user LaunchAgent plist. It starts at
// login (RunAtLoad), keeps the daemon alive, and carries the runtime
// environment inline so launchd never needs editing.
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
		<key>FERNGEIST_GATEWAY_LISTEN_ADDR</key>
		<string>%s</string>
		<key>FERNGEIST_GATEWAY_ENABLE_LAN</key>
		<string>%s</string>
		<key>FERNGEIST_GATEWAY_STATE_DB</key>
		<string>%s</string>
		<key>FERNGEIST_GATEWAY_LOG_DIR</key>
		<string>%s</string>
		<key>FERNGEIST_GATEWAY_MANAGED_BIN_DIR</key>
		<string>%s</string>
	</dict>
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

	if err := copyCurrentBinaryDarwin(paths.binaryPath); err != nil {
		return err
	}
	if err := writeDarwinEnvFile(paths, options); err != nil {
		return err
	}
	if err := writeDarwinPlist(paths, options); err != nil {
		return err
	}

	// Load replaces any existing instance; bootstrap is only for system agents.
	if err := m.launchctl("load", "-w", paths.plistPath); err != nil {
		return err
	}

	return nil
}

func (m *darwinManager) Uninstall(purge bool) error {
	if err := m.ensureLaunchctlAvailable(); err != nil {
		return err
	}

	paths, err := resolveDarwinPaths()
	if err != nil {
		return err
	}

	// Unload tolerates "no such label" (not currently loaded).
	if err := m.launchctl("unload", "-w", paths.plistPath); err != nil {
		if !isLaunchctlUnloadNotFound(err) {
			return err
		}
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
	return m.launchctl("kickstart", "-k", darwinServiceTarget())
}

func (m *darwinManager) Stop() error {
	if err := m.ensureLaunchctlAvailable(); err != nil {
		return err
	}
	if err := m.ensureInstalled(); err != nil {
		return err
	}
	return m.launchctl("kill", "SIGTERM", darwinServiceTarget())
}

func (m *darwinManager) Restart() error {
	if err := m.ensureLaunchctlAvailable(); err != nil {
		return err
	}
	if err := m.ensureInstalled(); err != nil {
		return err
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

	out, err := m.launchctlOutput("print", darwinServiceTarget())
	if err != nil {
		if isLaunchctlPrintNotFound(err) {
			return Status{Installed: false, UnitPath: paths.plistPath}, nil
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
	stdoutLogPath   string
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
		stdoutLogPath:   filepath.Join(rootDir, "logs", "daemon.log"),
		stderrLogPath:   filepath.Join(rootDir, "logs", "daemon.err.log"),
	}, nil
}

// copyCurrentBinaryDarwin mirrors copyCurrentBinary in manager_linux.go, which
// is hidden from darwin builds by its //go:build linux tag.
func copyCurrentBinaryDarwin(targetPath string) error {
	currentBinaryPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current binary: %w", err)
	}

	contents, err := os.ReadFile(currentBinaryPath)
	if err != nil {
		return fmt.Errorf("read current binary: %w", err)
	}
	if err := os.WriteFile(targetPath, contents, 0o755); err != nil {
		return fmt.Errorf("write service binary: %w", err)
	}

	return nil
}

func writeDarwinEnvFile(paths darwinPaths, options InstallOptions) error {
	options = NormalizeInstallOptions(options)
	lines := []string{
		"FERNGEIST_GATEWAY_LISTEN_ADDR=" + ListenAddr(options),
		"FERNGEIST_GATEWAY_ENABLE_LAN=" + darwinBool(!isLoopbackHost(options.Host)),
		"FERNGEIST_GATEWAY_STATE_DB=" + paths.dbPath,
		"FERNGEIST_GATEWAY_LOG_DIR=" + paths.logDir,
		"FERNGEIST_GATEWAY_MANAGED_BIN_DIR=" + paths.managedBinDir,
	}
	if options.PublicURL != "" {
		lines = append(lines, "FERNGEIST_GATEWAY_PUBLIC_BASE_URL="+options.PublicURL)
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(paths.envPath, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write service environment file: %w", err)
	}
	return nil
}

func writeDarwinPlist(paths darwinPaths, options InstallOptions) error {
	options = NormalizeInstallOptions(options)
	listenAddr := ListenAddr(options)
	enableLAN := "0"
	if !isLoopbackHost(options.Host) {
		enableLAN = "1"
	}

	plistBody := fmt.Sprintf(
		darwinPlistTemplate,
		darwinLabel,
		paths.binaryPath,
		escapePlist(listenAddr),
		enableLAN,
		escapePlist(paths.dbPath),
		escapePlist(paths.logDir),
		escapePlist(paths.managedBinDir),
		escapePlist(paths.stdoutLogPath),
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

func isLaunchctlPrintNotFound(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "could not find service") ||
		strings.Contains(message, "no such process") ||
		strings.Contains(message, "not found")
}

func isLaunchctlUnloadNotFound(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "could not find service") ||
		strings.Contains(message, "no such process") ||
		strings.Contains(message, "not found")
}

func isLaunchctlPermissionDenied(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(lower, "permission denied") ||
		strings.Contains(lower, "operation not permitted") ||
		strings.Contains(lower, "denied")
}

//go:build windows

package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	windowsTaskName = "FerngeistGateway"
)

type windowsManager struct{}

func newOSManager() Manager {
	return &windowsManager{}
}

func (m *windowsManager) Install(options InstallOptions) error {
	options = NormalizeInstallOptions(options)
	if err := ValidateInstallOptions(options); err != nil {
		return err
	}
	if err := m.ensureTaskSchedulerAvailable(); err != nil {
		return err
	}

	paths, err := resolveWindowsPaths()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(paths.serviceBinDir, 0o755); err != nil {
		return fmt.Errorf("create service bin directory: %w", err)
	}
	if err := os.MkdirAll(paths.serviceScriptsDir, 0o755); err != nil {
		return fmt.Errorf("create service scripts directory: %w", err)
	}
	if err := os.MkdirAll(paths.serviceConfigDir, 0o755); err != nil {
		return fmt.Errorf("create service config directory: %w", err)
	}
	if err := os.MkdirAll(paths.dataLogsDir, 0o755); err != nil {
		return fmt.Errorf("create daemon log directory: %w", err)
	}
	if err := os.MkdirAll(paths.dataManagedBinDir, 0o755); err != nil {
		return fmt.Errorf("create managed bin directory: %w", err)
	}

	// Stop any running daemon before replacing the binary: a running image is
	// write-locked on Windows. The scheduled task is recreated below, so this
	// does not lose the service registration.
	if err := killFerngeistProcess(); err != nil {
		return err
	}

	if err := copyCurrentBinaryWindows(paths.binaryPath); err != nil {
		return err
	}
	if err := writeWindowsWrapperScript(paths, options); err != nil {
		return err
	}
	if err := writeWindowsLauncherScript(paths); err != nil {
		return err
	}
	if err := writeWindowsOverridesTemplate(paths); err != nil {
		return err
	}

	// Launch the wrapper via wscript.exe: it has no console of its own, so
	// the wrapper gets no console window even though Windows Terminal is the
	// default terminal host (which ignores -WindowStyle Hidden for new
	// consoles).
	action := fmt.Sprintf("wscript.exe \"%s\"", paths.launcherScriptPath)

	if err := m.schtasks("/Create", "/TN", windowsTaskName, "/SC", "ONLOGON", "/TR", action, "/RL", "LIMITED", "/F"); err != nil {
		return err
	}

	// schtasks /Run returns success as soon as the scheduler accepts the
	// request; right after a /Create /F the run can be dropped while the
	// scheduler reconciles the recreated task. Retry until the task is
	// actually Running (or already running), so install leaves a live daemon
	// instead of a Ready task.
	for range 10 {
		err := m.schtasks("/Run", "/TN", windowsTaskName)
		if err == nil || isTaskAlreadyRunning(err) {
			if m.isTaskRunning() {
				return nil
			}
		} else {
			return err
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("daemon task did not enter running state after install")
}

func (m *windowsManager) Uninstall(purge bool) error {
	paths, err := resolveWindowsPaths()
	if err != nil {
		return err
	}

	// Stop the running daemon process FIRST. On --purge a running exe is
	// locked on Windows and cannot be deleted; on a plain uninstall we stop
	// the service the same way `systemctl disable --now` does on Linux.
	// taskkill on the same-user process needs no elevation.
	if err := killFerngeistProcess(); err != nil {
		return err
	}

	// End the task if running (best-effort: "not running" is fine), then
	// delete it. The delete is the core of a plain uninstall — if the task
	// survives, the daemon comes back at next logon — so a denied delete
	// must fail loudly, not be swallowed. Deleting a task requires
	// elevation, which is why uninstall needs an admin token.
	_ = m.schtasks("/End", "/TN", windowsTaskName)
	if err := m.schtasks("/Delete", "/TN", windowsTaskName, "/F"); err != nil {
		if isTaskNotFound(err) {
			return nil // already unregistered; nothing left to do
		}
		return err
	}

	// Plain uninstall unregisters the task and stops the daemon but keeps
	// the installed files (binary, wrapper, config) and data — the same
	// semantics as macOS/Linux. Only --purge removes the files themselves.
	if !purge {
		return nil
	}

	// If this uninstaller IS the installed binary (e.g. invoked as
	// `ferngeist-gateway daemon uninstall --purge` from the service bin),
	// Windows cannot delete a running image — the file is locked by this
	// very process. Renaming a running image is allowed, so move it out of
	// the tree, then hand the real delete to a detached helper that runs
	// after this process has exited and released the lock.
	if self, err := os.Executable(); err == nil &&
		strings.EqualFold(filepath.Clean(self), filepath.Clean(paths.binaryPath)) {
		keepAside := filepath.Join(os.TempDir(),
			fmt.Sprintf("ferngeist-gateway-uninstall-%d.exe", os.Getpid()))
		if renameErr := os.Rename(paths.binaryPath, keepAside); renameErr == nil {
			_ = scheduleDelayedDelete(keepAside)
		}
	}

	// The only locked file (the running image) was renamed out of the tree
	// above, so a single RemoveAll suffices.
	if err := os.RemoveAll(paths.rootDir); err != nil {
		return fmt.Errorf("purge daemon service data: %w", err)
	}

	return nil
}

// killFerngeistProcess terminates any running ferngeist-gateway.exe owned by
// this user, excluding the current process. The install/uninstall command
// itself is a ferngeist-gateway.exe when invoked from the service bin, so a
// blanket /IM taskkill would kill the caller before it can do anything. The
// /FI "PID ne <self>" filter excludes it; taskkill exits 0 when nothing
// matches (no daemon running) and /T tree-kills the daemon's agent children.
func killFerngeistProcess() error {
	cmd := exec.Command("taskkill", "/F", "/T", "/IM", "ferngeist-gateway.exe",
		"/FI", fmt.Sprintf("PID ne %d", os.Getpid()))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("stop daemon process: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// scheduleDelayedDelete spawns a detached, windowless helper that deletes
// path after a short delay. Used for the service binary when the uninstaller
// is that binary itself: the file cannot be deleted while this process is
// still running (Windows locks a running image), but renames are allowed, so
// we rename it aside and let the helper clean it up after we exit. The helper
// is a different process, so it is not subject to the same lock.
func scheduleDelayedDelete(path string) error {
	// PowerShell handles arbitrary paths cleanly via -LiteralPath (cmd /C
	// breaks on nested quotes). Start-Sleep gives this process time to exit
	// and release the image lock before the delete runs.
	escaped := strings.ReplaceAll(path, "'", "''")
	script := "Start-Sleep -Seconds 5; Remove-Item -LiteralPath '" + escaped + "' -Force"
	cmd := exec.Command("powershell", "-NoProfile", "-Command", script)
	// CREATE_NO_WINDOW = 0x08000000: keep the helper console-less so no
	// window flashes. (Go's syscall package does not export the constant.)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000}
	return cmd.Start()
}

func (m *windowsManager) Start() error {
	if err := m.ensureTaskSchedulerAvailable(); err != nil {
		return err
	}
	if err := m.ensureInstalled(); err != nil {
		return err
	}
	if err := m.schtasks("/Run", "/TN", windowsTaskName); err != nil {
		if !isTaskAlreadyRunning(err) {
			return err
		}
	}
	return nil
}

func (m *windowsManager) Stop() error {
	if err := m.ensureTaskSchedulerAvailable(); err != nil {
		return err
	}
	if err := m.ensureInstalled(); err != nil {
		return err
	}
	// /End ends the task instance, which is the launcher/wrapper — NOT the
	// daemon, which runs detached since the console-less spawn. Kill the
	// daemon directly first; then end the wrapper best-effort.
	if err := killFerngeistProcess(); err != nil {
		return err
	}
	if err := m.schtasks("/End", "/TN", windowsTaskName); err != nil {
		if !isTaskNotRunning(err) {
			return err
		}
	}
	return nil
}

func (m *windowsManager) Restart() error {
	if err := m.ensureTaskSchedulerAvailable(); err != nil {
		return err
	}
	if err := m.ensureInstalled(); err != nil {
		return err
	}
	// Same as Stop: the daemon runs detached from the task instance, so
	// /End alone leaves it running. Kill the daemon, end the wrapper, then
	// start again.
	if err := killFerngeistProcess(); err != nil {
		return err
	}
	if err := m.schtasks("/End", "/TN", windowsTaskName); err != nil {
		if !isTaskNotRunning(err) {
			return err
		}
	}
	if err := m.schtasks("/Run", "/TN", windowsTaskName); err != nil {
		if !isTaskAlreadyRunning(err) {
			return err
		}
	}
	return nil
}

func (m *windowsManager) Status() (Status, error) {
	if err := m.ensureTaskSchedulerAvailable(); err != nil {
		return Status{}, err
	}

	paths, err := resolveWindowsPaths()
	if err != nil {
		return Status{}, err
	}

	out, err := m.schtasksOutput("/Query", "/TN", windowsTaskName, "/FO", "LIST", "/V")
	if err != nil {
		if isTaskNotFound(err) {
			return Status{Installed: false, UnitPath: windowsTaskName}, nil
		}
		return Status{}, err
	}

	status := Status{
		Installed: true,
		UnitPath:  windowsTaskName,
		LoadState: "loaded",
	}

	for line := range strings.SplitSeq(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(trimmed), "status:") {
			value := strings.TrimSpace(strings.TrimPrefix(trimmed, "Status:"))
			if value != "" {
				status.ActiveState = strings.ToLower(value)
			}
		}
		if strings.HasPrefix(strings.ToLower(trimmed), "scheduled task state:") {
			value := strings.TrimSpace(strings.TrimPrefix(trimmed, "Scheduled Task State:"))
			if value != "" {
				status.SubState = strings.ToLower(value)
			}
		}
	}

	if status.ActiveState == "" {
		status.ActiveState = "unknown"
	}
	if status.SubState == "" {
		status.SubState = "unknown"
	}
	status.UnitFileState = paths.wrapperScriptPath

	return status, nil
}

func (m *windowsManager) ensureTaskSchedulerAvailable() error {
	if _, err := exec.LookPath("schtasks"); err != nil {
		return fmt.Errorf("%w: schtasks is not available", ErrServiceUnsupportedConfig)
	}
	if _, err := exec.LookPath("powershell"); err != nil {
		return fmt.Errorf("%w: powershell is not available", ErrServiceUnsupportedConfig)
	}
	return nil
}

func (m *windowsManager) ensureInstalled() error {
	status, err := m.Status()
	if err != nil {
		return err
	}
	if !status.Installed {
		return ErrServiceNotInstalled
	}
	return nil
}

// isTaskRunning reports whether the scheduled task is currently in the
// Running state (the daemon wrapper is up). Used by Install to confirm the
// task actually started after schtasks /Run.
func (m *windowsManager) isTaskRunning() bool {
	status, err := m.Status()
	if err != nil {
		return false
	}
	return status.Installed && status.ActiveState == "running"
}

func (m *windowsManager) schtasks(args ...string) error {
	_, err := m.schtasksOutput(args...)
	return err
}

func (m *windowsManager) schtasksOutput(args ...string) (string, error) {
	cmd := exec.Command("schtasks", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(out))
		if message == "" {
			message = err.Error()
		}
		if isTaskAccessDeniedMessage(message) {
			return "", fmt.Errorf("%w: task scheduler access was denied for the current user", ErrServicePermissionDenied)
		}
		return "", fmt.Errorf("schtasks %s failed: %s", strings.Join(args, " "), message)
	}
	return string(out), nil
}

type windowsPaths struct {
	rootDir            string
	serviceDir         string
	serviceBinDir      string
	serviceScriptsDir  string
	serviceConfigDir   string
	dataDir            string
	dataLogsDir        string
	dataManagedBinDir  string
	binaryPath         string
	wrapperScriptPath  string
	launcherScriptPath string
	overrideScriptPath string
	daemonLogPath      string
	stateDBPath        string
}

func resolveWindowsPaths() (windowsPaths, error) {
	localAppData := strings.TrimSpace(os.Getenv("LocalAppData"))
	if localAppData == "" {
		home, err := os.UserHomeDir()
		if err != nil || strings.TrimSpace(home) == "" {
			return windowsPaths{}, fmt.Errorf("%w: LocalAppData and home directory are unavailable", ErrServiceUnsupportedConfig)
		}
		localAppData = filepath.Join(home, "AppData", "Local")
	}

	rootDir := filepath.Join(localAppData, "FerngeistGateway")
	serviceDir := filepath.Join(rootDir, "service")
	dataDir := filepath.Join(rootDir, "data")

	return windowsPaths{
		rootDir:            rootDir,
		serviceDir:         serviceDir,
		serviceBinDir:      filepath.Join(serviceDir, "bin"),
		serviceScriptsDir:  filepath.Join(serviceDir, "scripts"),
		serviceConfigDir:   filepath.Join(serviceDir, "config"),
		dataDir:            dataDir,
		dataLogsDir:        filepath.Join(dataDir, "logs"),
		dataManagedBinDir:  filepath.Join(dataDir, "managed-bin"),
		binaryPath:         filepath.Join(serviceDir, "bin", "ferngeist-gateway.exe"),
		wrapperScriptPath:  filepath.Join(serviceDir, "scripts", "run-ferngeist-gateway-daemon.ps1"),
		launcherScriptPath: filepath.Join(serviceDir, "scripts", "run-ferngeist-gateway-daemon.vbs"),
		overrideScriptPath: filepath.Join(serviceDir, "config", "daemon-overrides.ps1"),
		daemonLogPath:      filepath.Join(dataDir, "logs", "daemon.log"),
		stateDBPath:        filepath.Join(dataDir, "ferngeist-gateway.db"),
	}, nil
}

func copyCurrentBinaryWindows(targetPath string) error {
	currentBinaryPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current binary: %w", err)
	}

	// Installing from the service directory itself: the running image cannot
	// be overwritten on Windows, and it is already the correct binary. Skip
	// the self-copy so `install` is idempotent when invoked via the
	// service-bin path.
	if strings.EqualFold(filepath.Clean(currentBinaryPath), filepath.Clean(targetPath)) {
		return nil
	}

	contents, err := os.ReadFile(currentBinaryPath)
	if err != nil {
		return fmt.Errorf("read current binary: %w", err)
	}

	// The daemon was killed moments ago; Windows can hold the file lock a
	// beat past process exit. Retry briefly before giving up.
	var lastErr error
	for range 5 {
		if err := os.WriteFile(targetPath, contents, 0o755); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("write service binary: %w", lastErr)
}

func writeWindowsWrapperScript(paths windowsPaths, options InstallOptions) error {
	options = NormalizeInstallOptions(options)
	listenAddr := ListenAddr(options)
	enableLAN := "0"
	if !isLoopbackHost(options.Host) {
		enableLAN = "1"
	}
	publicURLLine := ""
	// A persisted remote URL must not survive into a LAN-only service: it
	// would otherwise keep advertising the old tailnet URL.
	if includePublicURL(options) {
		publicURLLine = "$env:FERNGEIST_GATEWAY_PUBLIC_BASE_URL = '" + escapePowerShellSingleQuoted(options.PublicURL) + "'"
	}
	tailscaleModeLine := ""
	if remoteModeRequested(options.TailscaleMode) {
		tailscaleModeLine = "$env:FERNGEIST_GATEWAY_TAILSCALE_MODE = '" + escapePowerShellSingleQuoted(options.TailscaleMode) + "'"
	}

	content := fmt.Sprintf(
		`$ErrorActionPreference = "Stop"

$binaryPath = '%s'
$overrideScriptPath = '%s'
$daemonLogPath = '%s'
$stateDBPath = '%s'
$logDir = '%s'
$managedBinDir = '%s'

New-Item -ItemType Directory -Force -Path $logDir | Out-Null
New-Item -ItemType Directory -Force -Path $managedBinDir | Out-Null

$env:FERNGEIST_GATEWAY_STATE_DB = $stateDBPath
$env:FERNGEIST_GATEWAY_LOG_DIR = $logDir
$env:FERNGEIST_GATEWAY_MANAGED_BIN_DIR = $managedBinDir
$env:FERNGEIST_GATEWAY_LISTEN_ADDR = '%s'
$env:FERNGEIST_GATEWAY_ENABLE_LAN = '%s'
%s
%s

if (Test-Path $overrideScriptPath) {
    . $overrideScriptPath
}

# Launch the daemon hidden (CreateNoWindow). Note: CreateNoWindow suppresses
# a NEW console window, it does NOT detach the child from the parent console —
# the daemon still inherits the wrapper's console and would receive CTRL_CLOSE
# when that console closes. Two layers protect it: the launcher runs this
# wrapper under conhost.exe --headless (no visible console to close), and the
# daemon registers a SetConsoleCtrlHandler that ignores CTRL_CLOSE. stdout/
# stderr are redirected through pipes so the daemon's std handles stay valid
# and its output is captured to $daemonLogPath; the daemon also writes its own
# structured log to $logDir (gateway.log).
$psi = New-Object System.Diagnostics.ProcessStartInfo
$psi.FileName = $binaryPath
$psi.Arguments = 'daemon run'
$psi.UseShellExecute = $false
$psi.CreateNoWindow = $true
$psi.RedirectStandardOutput = $true
$psi.RedirectStandardError = $true

$daemon = New-Object System.Diagnostics.Process
$daemon.StartInfo = $psi
if (-not $daemon.Start()) {
    throw "failed to start gateway daemon (.NET Process.Start)"
}

# Pump stdout/stderr into the log file. Async reads keep the pipes draining so
# a full pipe can never block the daemon; WaitForExit keeps the wrapper alive
# as the task's lifecycle owner until the daemon exits.
$stdoutTask = $daemon.StandardOutput.ReadToEndAsync()
$stderrTask = $daemon.StandardError.ReadToEndAsync()
$daemon.WaitForExit()
$logEntry = $stdoutTask.GetAwaiter().GetResult() + $stderrTask.GetAwaiter().GetResult()
if ($logEntry) {
    [System.IO.File]::AppendAllText($daemonLogPath, $logEntry, [System.Text.Encoding]::UTF8)
}
`,
		escapePowerShellSingleQuoted(paths.binaryPath),
		escapePowerShellSingleQuoted(paths.overrideScriptPath),
		escapePowerShellSingleQuoted(paths.daemonLogPath),
		escapePowerShellSingleQuoted(paths.stateDBPath),
		escapePowerShellSingleQuoted(paths.dataLogsDir),
		escapePowerShellSingleQuoted(paths.dataManagedBinDir),
		escapePowerShellSingleQuoted(listenAddr),
		escapePowerShellSingleQuoted(enableLAN),
		publicURLLine,
		tailscaleModeLine,
	)

	if err := os.WriteFile(paths.wrapperScriptPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write daemon wrapper script: %w", err)
	}
	return nil
}

func writeWindowsLauncherScript(paths windowsPaths) error {
	// wscript.exe is a GUI-subsystem binary: it has no console of its own,
	// and window style 0 (SW_HIDE) in WScript.Shell.Run hides the child's.
	// The wrapper therefore never gets a console window, regardless of
	// Windows Terminal being the default terminal host (which ignores the
	// -WindowStyle Hidden SW_HIDE hint for new consoles). bWaitOnReturn
	// keeps wscript as the task's lifecycle owner, same as the wrapper was.
	//
	// conhost.exe --headless creates a detached console with no visible
	// window, so even a fresh Windows Terminal (which would otherwise
	// materialize a window for a new console) stays out of the picture. The
	// wrapper gets a real console it can read/write (daemon pipes) without
	// any surface the user could close; the daemon child then inherits that
	// console and is shielded from CTRL_CLOSE. See writeWindowsWrapperScript
	// for the daemon-side CTRL_CLOSE handler.
	content := fmt.Sprintf(
		`' Ferngeist gateway daemon launcher (windowless).
' wscript.exe has no console; style 0 hides the powershell child.
Set shell = CreateObject("WScript.Shell")
shell.Run "conhost.exe --headless powershell.exe -NoProfile -ExecutionPolicy Bypass -File ""%s""", 0, True
`,
		paths.wrapperScriptPath,
	)
	if err := os.WriteFile(paths.launcherScriptPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write daemon launcher script: %w", err)
	}
	return nil
}

func writeWindowsOverridesTemplate(paths windowsPaths) error {
	_, err := os.Stat(paths.overrideScriptPath)
	if err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read daemon override script: %w", err)
	}

	content := []byte(`# Optional daemon runtime overrides.
# Uncomment and edit as needed.
# $env:FERNGEIST_GATEWAY_ENABLE_LAN = "1"
# $env:FERNGEIST_GATEWAY_LISTEN_ADDR = "0.0.0.0:5788"
# $env:FERNGEIST_GATEWAY_PUBLIC_BASE_URL = "https://example.com"
# $env:FERNGEIST_GATEWAY_TAILSCALE_MODE = "auto"
`)

	if err := os.WriteFile(paths.overrideScriptPath, content, 0o644); err != nil {
		return fmt.Errorf("write daemon override script: %w", err)
	}
	return nil
}

func escapePowerShellSingleQuoted(value string) string {
	return strings.ReplaceAll(value, "'", "''")
}

func isTaskNotFound(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "cannot find the file specified") ||
		strings.Contains(message, "cannot find the task") ||
		strings.Contains(message, "task does not exist")
}

func isTaskNotRunning(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "not currently running")
}

func isTaskAlreadyRunning(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "already running")
}

func isTaskAccessDeniedMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(lower, "access is denied")
}

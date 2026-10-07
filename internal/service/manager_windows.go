//go:build windows

package service

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
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

	if err := writeWindowsWrapperScript(paths, options); err != nil {
		return err
	}
	if err := writeWindowsLauncherScript(paths); err != nil {
		return err
	}
	if err := writeWindowsOverridesTemplate(paths); err != nil {
		return err
	}
	if err := saveInstallOptions(paths.serviceConfigDir, options); err != nil {
		return err
	}

	// Register the task BEFORE touching the running daemon: the task's action
	// does not depend on the binary contents, and a failed registration (e.g.
	// access denied) must not leave the user with a dead daemon.
	if err := m.registerTask(paths); err != nil {
		return err
	}

	// Stop any running daemon before replacing the binary: a running image is
	// write-locked on Windows.
	if err := m.stopDaemon(paths); err != nil {
		return err
	}

	// Windows can hold the image lock a beat past process exit. Retry briefly.
	var copyErr error
	for range 5 {
		if copyErr = installCurrentBinary(paths.binaryPath); copyErr == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if copyErr != nil {
		return copyErr
	}

	// schtasks /Run returns success as soon as the scheduler accepts the
	// request; right after a re-registration the run can be dropped while the
	// scheduler reconciles the task. Retry until the task is actually Running,
	// so install leaves a live daemon instead of a Ready task.
	for range 10 {
		state, _, err := m.taskState()
		if err != nil {
			return err
		}
		if state == "running" {
			return nil
		}
		if err := m.schtasks("/Run", "/TN", windowsTaskName); err != nil {
			return err
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("daemon task did not enter running state after install")
}

func (m *windowsManager) SavedInstallOptions() (InstallOptions, bool) {
	paths, err := resolveWindowsPaths()
	if err != nil {
		return InstallOptions{}, false
	}
	return loadInstallOptions(paths.serviceConfigDir)
}

// registerTask (re)creates the scheduled task from a generated Task XML. The
// XML (rather than `schtasks /Create /SC ONLOGON`) is needed to override the
// defaults that keep laptops from running the daemon: not starting on
// battery, stopping when going on battery, a 72h execution limit, and no
// restart on failure. The trigger and principal are scoped to the current
// user, so registration needs no elevation.
func (m *windowsManager) registerTask(paths windowsPaths) error {
	current, err := user.Current()
	if err != nil {
		return fmt.Errorf("resolve current user: %w", err)
	}
	xmlPath := filepath.Join(paths.serviceConfigDir, "task.xml")
	if err := os.WriteFile(xmlPath, encodeTaskXML(windowsTaskXML(current.Username, paths.launcherScriptPath)), 0o644); err != nil {
		return fmt.Errorf("write scheduled task definition: %w", err)
	}
	return m.schtasks("/Create", "/TN", windowsTaskName, "/XML", xmlPath, "/F")
}

// windowsTaskXML builds the Task Scheduler definition. Launching goes through
// wscript.exe: it has no console of its own, so the wrapper gets no console
// window even though Windows Terminal is the default terminal host (which
// ignores -WindowStyle Hidden for new consoles). RestartOnFailure only fires
// when the action exits non-zero; the launcher and wrapper propagate the
// daemon's exit code for that reason.
func windowsTaskXML(userID, launcherPath string) string {
	esc := func(v string) string {
		var b bytes.Buffer
		_ = xml.EscapeText(&b, []byte(v))
		return b.String()
	}
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>` + esc(userID) + `</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>` + esc(userID) + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <StartWhenAvailable>true</StartWhenAvailable>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>999</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>wscript.exe</Command>
      <Arguments>` + esc(`"`+launcherPath+`"`) + `</Arguments>
    </Exec>
  </Actions>
</Task>
`
}

// encodeTaskXML encodes s as UTF-16 LE with a BOM, which schtasks /XML expects.
func encodeTaskXML(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 2, 2+2*len(units))
	out[0], out[1] = 0xFF, 0xFE
	for _, u := range units {
		out = binary.LittleEndian.AppendUint16(out, u)
	}
	return out
}

func (m *windowsManager) Uninstall(purge bool) error {
	paths, err := resolveWindowsPaths()
	if err != nil {
		return err
	}

	// Stop the running daemon process FIRST. On --purge a running exe is
	// locked on Windows and cannot be deleted; on a plain uninstall we stop
	// the service the same way `systemctl disable --now` does on Linux.
	// Killing a same-user process needs no elevation.
	if err := m.stopDaemon(paths); err != nil {
		return err
	}

	// Delete the task if registered. A task that is already gone is not an
	// error, and must not skip the purge below. A denied delete must fail
	// loudly: if the task survives, the daemon comes back at next logon.
	if _, found, err := m.taskState(); err != nil {
		return err
	} else if found {
		if err := m.schtasks("/Delete", "/TN", windowsTaskName, "/F"); err != nil {
			return err
		}
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

// stopDaemon stops the service: it ends the task instance (the launcher/
// wrapper) first, so the wrapper cannot see the daemon die and let
// RestartOnFailure bring it back, then kills the daemon itself, which runs
// detached from the task instance since the console-less spawn.
func (m *windowsManager) stopDaemon(paths windowsPaths) error {
	if state, _, err := m.taskState(); err != nil {
		return err
	} else if state == "running" {
		if err := m.schtasks("/End", "/TN", windowsTaskName); err != nil {
			return err
		}
	}
	return killFerngeistProcess(paths.binaryPath)
}

// ferngeistProcessListScript prints the PIDs of processes running binaryPath,
// excluding self. Matching on the full image path (not just the name) leaves
// other users' daemons and a foreground `daemon run`/`pair` from another
// install location alone. The install/uninstall command itself may be the
// service binary, so it must be excluded or it would kill the caller.
func ferngeistProcessListScript(binaryPath string, self int) string {
	return "Get-CimInstance Win32_Process -Filter \"Name='ferngeist-gateway.exe'\" | " +
		"Where-Object { $_.ExecutablePath -eq '" + escapePowerShellSingleQuoted(binaryPath) + "' -and $_.ProcessId -ne " + strconv.Itoa(self) + " } | " +
		"ForEach-Object { $_.ProcessId }"
}

// killFerngeistProcess tree-kills (taskkill /T, so agent children die too)
// every service-binary process except the current one.
func killFerngeistProcess(binaryPath string) error {
	out, err := runPowerShell(ferngeistProcessListScript(binaryPath, os.Getpid()))
	if err != nil {
		return fmt.Errorf("list daemon processes: %w", err)
	}
	for pid := range strings.FieldsSeq(out) {
		if _, err := strconv.Atoi(pid); err != nil {
			continue
		}
		if out, err := exec.Command("taskkill", "/F", "/T", "/PID", pid).CombinedOutput(); err != nil {
			// The process may have exited on its own since the listing.
			if alive, _ := runPowerShell("if (Get-Process -Id " + pid + " -ErrorAction SilentlyContinue) { 'alive' }"); strings.Contains(alive, "alive") {
				return fmt.Errorf("stop daemon process: %s", strings.TrimSpace(string(out)))
			}
		}
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
	state, found, err := m.taskState()
	if err != nil {
		return err
	}
	if !found {
		return ErrServiceNotInstalled
	}
	if state == "running" {
		return nil
	}
	return m.schtasks("/Run", "/TN", windowsTaskName)
}

func (m *windowsManager) Stop() error {
	if err := m.ensureTaskSchedulerAvailable(); err != nil {
		return err
	}
	if err := m.ensureInstalled(); err != nil {
		return err
	}
	paths, err := resolveWindowsPaths()
	if err != nil {
		return err
	}
	return m.stopDaemon(paths)
}

func (m *windowsManager) Restart() error {
	if err := m.Stop(); err != nil {
		return err
	}
	return m.Start()
}

func (m *windowsManager) Status() (Status, error) {
	if err := m.ensureTaskSchedulerAvailable(); err != nil {
		return Status{}, err
	}

	paths, err := resolveWindowsPaths()
	if err != nil {
		return Status{}, err
	}

	state, found, err := m.taskState()
	if err != nil {
		return Status{}, err
	}
	if !found {
		return Status{Installed: false, UnitPath: windowsTaskName}, nil
	}

	status := Status{
		Installed:   true,
		UnitPath:    windowsTaskName,
		LoadState:   "loaded",
		ActiveState: state,
		SubState:    state,
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

// taskState reports the scheduled task's State (lowercased enum name: ready,
// running, disabled, queued). Querying through Get-ScheduledTask rather than
// parsing schtasks output keeps this independent of the Windows display
// language: enum names and HRESULTs are never localized.
func (m *windowsManager) taskState() (state string, found bool, err error) {
	out, err := runPowerShell("try { $t = Get-ScheduledTask -TaskName '" + windowsTaskName + "' -ErrorAction SilentlyContinue; " +
		"if ($null -eq $t) { 'FG_NOTFOUND' } else { 'FG_STATE=' + $t.State } } catch { 'FG_ERR=' + $_.Exception.HResult }")
	if err != nil {
		return "", false, fmt.Errorf("query scheduled task: %w", err)
	}
	return parseTaskState(out)
}

// parseTaskState decodes the sentinel lines printed by taskState's script.
func parseTaskState(out string) (string, bool, error) {
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "FG_NOTFOUND":
			return "", false, nil
		case strings.HasPrefix(line, "FG_STATE="):
			return strings.ToLower(strings.TrimPrefix(line, "FG_STATE=")), true, nil
		case strings.HasPrefix(line, "FG_ERR="):
			code := strings.TrimPrefix(line, "FG_ERR=")
			// 0x80070005 (E_ACCESSDENIED) as a signed 32-bit value.
			if code == "-2147024891" {
				return "", false, fmt.Errorf("%w: task scheduler access was denied for the current user", ErrServicePermissionDenied)
			}
			return "", false, fmt.Errorf("query scheduled task failed (HRESULT %s)", code)
		}
	}
	return "", false, fmt.Errorf("query scheduled task: unexpected output %q", strings.TrimSpace(out))
}

func runPowerShell(script string) (string, error) {
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
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
# stderr are redirected through pipes so the daemon's std handles stay valid.
# The daemon's stdout duplicates its own structured log in $logDir
# (gateway.log), so it is drained and discarded; stderr (crash output) is
# appended to $daemonLogPath as it arrives, never buffered in memory.
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

# Async copies keep the pipes draining so a full pipe can never block the
# daemon; WaitForExit keeps the wrapper alive as the task's lifecycle owner
# until the daemon exits.
$logStream = New-Object System.IO.FileStream($daemonLogPath, [System.IO.FileMode]::Append, [System.IO.FileAccess]::Write, [System.IO.FileShare]::ReadWrite, 1)
$stdoutTask = $daemon.StandardOutput.BaseStream.CopyToAsync([System.IO.Stream]::Null)
$stderrTask = $daemon.StandardError.BaseStream.CopyToAsync($logStream)
$daemon.WaitForExit()
$stdoutTask.GetAwaiter().GetResult()
$stderrTask.GetAwaiter().GetResult()
$logStream.Dispose()

# Propagate the daemon's exit code so the task's RestartOnFailure fires on a
# crash (it only triggers on a non-zero action exit code).
exit $daemon.ExitCode
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
WScript.Quit shell.Run("conhost.exe --headless powershell.exe -NoProfile -ExecutionPolicy Bypass -File ""%s""", 0, True)
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

func isTaskAccessDeniedMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(lower, "access is denied")
}

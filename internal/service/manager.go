package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var (
	ErrServiceUnsupportedOS     = errors.New("daemon service management is unsupported on this operating system")
	ErrServiceUnsupportedConfig = errors.New("daemon service management is unsupported in this environment")
	ErrServicePermissionDenied  = errors.New("insufficient permissions to manage daemon service")
	ErrInvalidInstallOptions    = errors.New("invalid daemon install options")
	ErrServiceNotInstalled      = errors.New("daemon service is not installed")
)

const (
	defaultInstallHost = "127.0.0.1"
	defaultInstallPort = 5788
)

const linuxUnitName = "ferngeist-gateway.service"

// remoteModeRequested reports whether the operator asked for remote access
// (any valid Tailscale mode). An empty string is NOT remote — it is the
// LAN/localhost default.
func remoteModeRequested(mode string) bool {
	return mode != "" && mode != "off"
}

// includePublicURL decides whether an explicit PublicURL is written into the
// service environment. It is dropped only for a LAN-only install (non-loopback
// host, no remote requested): a URL persisted by a previous --remote run would
// otherwise keep advertising the old tailnet URL. It is kept for loopback
// installs (reverse-proxy setups) and whenever remote access is requested.
func includePublicURL(options InstallOptions) bool {
	if options.PublicURL == "" {
		return false
	}
	return remoteModeRequested(options.TailscaleMode) || isLoopbackHost(options.Host)
}

type Status struct {
	Installed     bool
	LoadState     string
	ActiveState   string
	SubState      string
	UnitFileState string
	UnitPath      string
}

type InstallOptions struct {
	Host      string
	Port      int
	PublicURL string
	// TailscaleMode is written into the service environment; "off" writes
	// nothing. Valid: "off", "auto", "cli", "tsnet".
	TailscaleMode string
}

type Manager interface {
	Install(options InstallOptions) error
	// SavedInstallOptions returns the options the current install was made
	// with, if any were recorded.
	SavedInstallOptions() (InstallOptions, bool)
	Uninstall(purge bool) error
	Start() error
	Stop() error
	Restart() error
	Status() (Status, error)
}

func NewManager() Manager {
	return newOSManager()
}

type unsupportedManager struct {
	err error
}

func (m unsupportedManager) Install(_ InstallOptions) error {
	return m.err
}

func (m unsupportedManager) SavedInstallOptions() (InstallOptions, bool) {
	return InstallOptions{}, false
}

func (m unsupportedManager) Uninstall(_ bool) error {
	return m.err
}

func (m unsupportedManager) Start() error {
	return m.err
}

func (m unsupportedManager) Stop() error {
	return m.err
}

func (m unsupportedManager) Restart() error {
	return m.err
}

func (m unsupportedManager) Status() (Status, error) {
	return Status{}, m.err
}

func ValidateInstallOptions(options InstallOptions) error {
	normalized := NormalizeInstallOptions(options)
	host := strings.TrimSpace(normalized.Host)
	if host == "" {
		return fmt.Errorf("%w: host is required", ErrInvalidInstallOptions)
	}
	if normalized.Port < 1 || normalized.Port > 65535 {
		return fmt.Errorf("%w: port must be between 1 and 65535", ErrInvalidInstallOptions)
	}
	switch normalized.TailscaleMode {
	case "", "off", "auto", "cli", "tsnet":
	default:
		return fmt.Errorf("%w: invalid tailscale mode %q", ErrInvalidInstallOptions, normalized.TailscaleMode)
	}
	return nil
}

func ListenAddr(options InstallOptions) string {
	normalized := NormalizeInstallOptions(options)
	return net.JoinHostPort(strings.TrimSpace(normalized.Host), strconv.Itoa(normalized.Port))
}

func NormalizeInstallOptions(options InstallOptions) InstallOptions {
	host := strings.TrimSpace(options.Host)
	if host == "" {
		host = defaultInstallHost
	}
	port := options.Port
	if port == 0 {
		port = defaultInstallPort
	}

	return InstallOptions{
		Host:          host,
		Port:          port,
		PublicURL:     strings.TrimSpace(options.PublicURL),
		TailscaleMode: strings.TrimSpace(options.TailscaleMode),
	}
}

func isLoopbackHost(host string) bool {
	trimmed := strings.TrimSpace(strings.Trim(host, "[]"))
	if strings.EqualFold(trimmed, "localhost") {
		return true
	}
	ip := net.ParseIP(trimmed)
	return ip != nil && ip.IsLoopback()
}

// installCopySource is the binary copied into the service bin dir on Install.
// Defaults to the running executable with symlinks resolved (darwin's
// os.Executable reports the symlink a process was started through, which
// would defeat the self-copy check). Tests override it.
var installCopySource = func() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}

// sameFile reports whether a and b name the same file on disk.
func sameFile(a, b string) bool {
	ai, aerr := os.Stat(a)
	bi, berr := os.Stat(b)
	return aerr == nil && berr == nil && os.SameFile(ai, bi)
}

// replaceFileAtomically copies src to dst through a temp file in dst's
// directory and renames it into place, so dst gets a fresh inode. Never
// truncate a binary in place: Linux refuses (ETXTBSY) while it executes,
// and macOS kills processes whose signed image changes under them.
// On Windows a running dst cannot be replaced; callers stop it first.
func replaceFileAtomically(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".install-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// installCurrentBinary copies the install source into targetPath unless it
// already is targetPath (installing from the service bin dir).
func installCurrentBinary(targetPath string) error {
	src, err := installCopySource()
	if err != nil {
		return fmt.Errorf("resolve current binary: %w", err)
	}
	if sameFile(src, targetPath) {
		return nil
	}
	if err := replaceFileAtomically(src, targetPath, 0o755); err != nil {
		return fmt.Errorf("write service binary: %w", err)
	}
	return nil
}

// installOptionsFile is where Install records the options it was given, so
// `daemon install --keep-settings` (used by package upgrades) can reapply
// them instead of resetting the service to package defaults.
const installOptionsFile = "install-options.json"

func saveInstallOptions(configDir string, options InstallOptions) error {
	data, err := json.MarshalIndent(NormalizeInstallOptions(options), "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(configDir, installOptionsFile), data, 0o600); err != nil {
		return fmt.Errorf("record install options: %w", err)
	}
	return nil
}

func loadInstallOptions(configDir string) (InstallOptions, bool) {
	data, err := os.ReadFile(filepath.Join(configDir, installOptionsFile))
	if err != nil {
		return InstallOptions{}, false
	}
	var options InstallOptions
	if json.Unmarshal(data, &options) != nil {
		return InstallOptions{}, false
	}
	return options, true
}

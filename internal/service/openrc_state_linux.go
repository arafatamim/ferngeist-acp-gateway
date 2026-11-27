//go:build linux && !android

package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	openrcStateWorkerArg = "--openrc-state-worker"
	openrcStatePrepare   = "prepare"
	openrcStateInstall   = "install"
	openrcStateLoad      = "load"
	openrcStatePurge     = "purge"
)

type openrcStateRequest struct {
	Action        string
	Home          string
	UID           uint32
	Options       InstallOptions
	ManagedBinary bool
	BinaryReady   bool
}

type openrcStateResult struct {
	Options     InstallOptions
	BinaryReady bool
}

func (r openrcStateRequest) needsBinary() bool {
	return !r.ManagedBinary && !r.BinaryReady && (r.Action == openrcStatePrepare || r.Action == openrcStateInstall)
}

var runOpenRCState = execOpenRCState

func execOpenRCState(account openrcAccount, req openrcStateRequest) (openrcStateResult, error) {
	req.Home, req.UID = account.home, account.uid
	data, err := json.Marshal(req)
	if err != nil {
		return openrcStateResult{}, fmt.Errorf("encode OpenRC state request: %w", err)
	}
	// Re-exec the running binary without resolving a command in the user's home
	// or on PATH. Pass a descriptor to that same image for private installs.
	cmd := exec.Command("/proc/self/exe", openrcStateWorkerArg)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
		Uid: account.uid, Gid: account.gid, Groups: account.groups,
	}}
	cmd.Dir = "/"
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"FERNGEIST_GATEWAY_PRESERVE_INSTALL_PATH=" + os.Getenv("FERNGEIST_GATEWAY_PRESERVE_INSTALL_PATH"),
	}
	cmd.Stdin = bytes.NewReader(data)
	if req.needsBinary() {
		binary, err := os.Open("/proc/self/exe")
		if err != nil {
			return openrcStateResult{}, fmt.Errorf("open running installer: %w", err)
		}
		defer binary.Close()
		cmd.ExtraFiles = []*os.File{binary}
	}
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return openrcStateResult{}, fmt.Errorf("OpenRC state %s as %s: %w: %s", req.Action, account.name, err, strings.TrimSpace(string(exit.Stderr)))
		}
		return openrcStateResult{}, fmt.Errorf("run OpenRC state worker as %s: %w", account.name, err)
	}
	var result openrcStateResult
	if err := json.Unmarshal(out, &result); err != nil {
		return openrcStateResult{}, fmt.Errorf("decode OpenRC state result: %w", err)
	}
	return result, nil
}

// RunOpenRCStateWorker handles the private subprocess invocation used to
// operate on OpenRC's user-owned state after dropping privileges.
func RunOpenRCStateWorker(args []string, in io.Reader, out io.Writer) (bool, error) {
	if len(args) != 1 || args[0] != openrcStateWorkerArg {
		return false, nil
	}
	var req openrcStateRequest
	if err := json.NewDecoder(in).Decode(&req); err != nil {
		return true, fmt.Errorf("decode OpenRC state request: %w", err)
	}
	// Convert the kernel IDs back to unsigned values because Go exposes them as int.
	if req.UID == 0 || req.UID != uint32(os.Getuid()) || req.UID != uint32(os.Geteuid()) || !filepath.IsAbs(req.Home) {
		return true, errors.New("OpenRC state worker requires the non-root service account and its absolute home directory")
	}
	var binary *os.File
	if req.needsBinary() {
		binary = os.NewFile(3, "running installer")
		defer binary.Close()
	}
	result, err := applyOpenRCState(req, binary)
	if err != nil {
		return true, err
	}
	return true, json.NewEncoder(out).Encode(result)
}

func applyOpenRCState(req openrcStateRequest, binary *os.File) (openrcStateResult, error) {
	paths := linuxPathsForHome(req.Home)
	switch req.Action {
	case openrcStateLoad:
		options, err := readInstallOptions(paths.configDir)
		return openrcStateResult{Options: options}, err
	case openrcStatePurge:
		return openrcStateResult{}, os.RemoveAll(paths.rootDir)
	case openrcStatePrepare, openrcStateInstall:
	default:
		return openrcStateResult{}, fmt.Errorf("unknown OpenRC state action %q", req.Action)
	}
	if err := mkdirLinuxState(paths); err != nil {
		return openrcStateResult{}, err
	}
	if req.Action == openrcStatePrepare {
		if !req.needsBinary() {
			return openrcStateResult{}, nil
		}
		ready, err := prepareOpenRCBinary(binary, paths.binaryPath)
		return openrcStateResult{BinaryReady: ready}, err
	}
	if err := installOpenRCBinary(req, binary, paths.binaryPath); err != nil {
		return openrcStateResult{}, err
	}
	return openrcStateResult{}, writeLinuxServiceSettings(paths, req.Options)
}

func prepareOpenRCBinary(binary *os.File, target string) (bool, error) {
	_, err := os.Lstat(target)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("read service binary: %w", err)
	}
	if err := writeFileAtomically(binary, target, 0o755); err != nil {
		return false, fmt.Errorf("write service binary: %w", err)
	}
	return true, nil
}

func installOpenRCBinary(req openrcStateRequest, binary *os.File, target string) error {
	if req.ManagedBinary {
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove old service binary: %w", err)
		}
		return nil
	}
	if req.BinaryReady {
		return nil
	}
	sourceInfo, err := binary.Stat()
	if err != nil {
		return fmt.Errorf("stat installer binary: %w", err)
	}
	if targetInfo, err := os.Stat(target); err == nil && os.SameFile(sourceInfo, targetInfo) {
		return nil
	}
	if err := writeFileAtomically(binary, target, 0o755); err != nil {
		return fmt.Errorf("write service binary: %w", err)
	}
	return nil
}

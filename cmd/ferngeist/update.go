package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/service"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/update"
)

// runUpdate implements `ferngeist-gateway update`. Package-manager-installed
// builds (updateChannel != "" && != "self") refuse to self-update.
func runUpdate() error {
	if updateChannel != "" && updateChannel != "self" {
		return fmt.Errorf("this build was installed via %s; update it with your package manager instead", updateChannel)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Remove .old binaries left by a previous Windows swap (best-effort).
	if p, err := serviceBinaryPath(); err == nil {
		update.CleanupOld(p)
	}
	if exe, err := runningExecutable(); err == nil {
		update.CleanupOld(exe)
	}

	checker := update.NewChecker("arafatamim/ferngeist-acp-gateway")
	release, err := checker.LatestStable(ctx)
	if err != nil {
		return fmt.Errorf("check for updates: %w", err)
	}

	latest := strings.TrimPrefix(release.TagName, "v")
	current := strings.TrimPrefix(buildVersion, "v")
	// An unparseable current version (e.g. "dev") is always updatable.
	if update.ValidVersion(current) && !update.IsNewer(latest, current) {
		if latest == current {
			fmt.Println("Already up to date (" + current + ").")
		} else {
			fmt.Printf("Installed version %s is not older than latest stable %s; skipping.\n", current, release.TagName)
		}
		return nil
	}

	asset, err := checker.AssetFor(release, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}

	client := checker.Client
	if client == nil {
		client = update.DefaultClient()
	}

	// Fetch SHA256SUMS from the same release download base.
	baseURL := checker.AssetBaseURL(release)
	sumsResp, err := client.Get(baseURL + "/" + update.ChecksumFileName)
	if err != nil {
		return fmt.Errorf("fetch checksums: %w", err)
	}
	defer sumsResp.Body.Close()
	if sumsResp.StatusCode != 200 {
		return fmt.Errorf("fetch checksums: status %d", sumsResp.StatusCode)
	}
	sumsData, err := io.ReadAll(sumsResp.Body)
	if err != nil {
		return fmt.Errorf("read checksums: %w", err)
	}

	// Locate the expected checksum for this asset.
	wantHex, err := update.ChecksumFor(sumsData, asset.Name)
	if err != nil {
		return err
	}

	// Decide what to update. The service binary is updated when the service is
	// installed; the running executable is updated too when it is a different
	// file (otherwise a later `daemon install` from it would downgrade the
	// service). Without a usable service manager (no systemd, android, or not
	// installed) only the running executable is updated.
	manager := service.NewManager()
	serviceManaged := false
	status, err := manager.Status()
	switch {
	case err == nil:
		serviceManaged = status.Installed
	case errors.Is(err, service.ErrServiceUnsupportedOS), errors.Is(err, service.ErrServiceUnsupportedConfig):
	default:
		return fmt.Errorf("read service status: %w", err)
	}
	var targets []string
	if serviceManaged {
		binaryPath, err := serviceBinaryPath()
		if err != nil {
			return err
		}
		targets = append(targets, binaryPath)
	}
	if exe, err := runningExecutable(); err == nil {
		if !sameFileAsAny(exe, targets) {
			targets = append(targets, exe)
		}
	} else if len(targets) == 0 {
		return fmt.Errorf("locate running executable: %w", err)
	}

	fmt.Printf("Downloading %s\n", asset.BrowserDownloadURL)

	// The raw archive is staged in the system temp dir; each binary is then
	// staged next to its target so the final swap is a same-filesystem rename.
	tmpArchive, err := os.CreateTemp("", "ferngeist-update-*")
	if err != nil {
		return fmt.Errorf("stage update archive: %w", err)
	}
	tmpArchiveName := tmpArchive.Name()
	_ = tmpArchive.Close()
	defer os.Remove(tmpArchiveName)

	if err := update.DownloadAndVerify(ctx, client, asset.BrowserDownloadURL, wantHex, tmpArchiveName); err != nil {
		return fmt.Errorf("download and verify update: %w", err)
	}

	// Stage every new binary before touching the service, so an extraction
	// failure leaves the running daemon alone. The archive entry is named after
	// the platform binary (ferngeist-gateway[.exe]); base(target) is the exact
	// name the extractor must match.
	staged := make([]string, len(targets))
	defer func() {
		for _, s := range staged {
			if s != "" {
				_ = os.Remove(s) // already gone once swapped
			}
		}
	}()
	for i, target := range targets {
		staged[i], err = update.StageArchiveFromFile(tmpArchiveName, filepath.Base(target), target)
		if err != nil {
			return fmt.Errorf("extract update: %w", err)
		}
	}

	// Stop the service so its binary can be replaced and re-launched.
	if serviceManaged {
		if err := manager.Stop(); err != nil {
			return fmt.Errorf("stop daemon service: %w", err)
		}
	}

	var swapErr error
	for i, target := range targets {
		if err := update.SwapBinary(staged[i], target); err != nil {
			swapErr = fmt.Errorf("replace %s: %w", target, err)
			break
		}
	}

	if serviceManaged {
		// Restart even after a failed swap so the old binary keeps running.
		if swapErr == nil {
			fmt.Printf("Updated to %s; restarting the daemon service.\n", release.TagName)
		}
		if err := manager.Restart(); err != nil && swapErr == nil {
			return fmt.Errorf("restart daemon service: %w", err)
		}
	}
	if swapErr != nil {
		return swapErr
	}
	if !serviceManaged {
		fmt.Printf("Updated to %s at %s. Restart it to pick up the new version.\n", release.TagName, strings.Join(targets, ", "))
	}
	return nil
}

// runningExecutable returns the symlink-resolved path of the current process.
func runningExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// sameFileAsAny reports whether path is the same file as any of others.
func sameFileAsAny(path string, others []string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	for _, o := range others {
		if oi, err := os.Stat(o); err == nil && os.SameFile(fi, oi) {
			return true
		}
	}
	return false
}

// serviceBinaryPath returns the installed service binary path for this OS,
// mirroring the path layout in internal/service/{manager_linux,manager_darwin,
// manager_windows}.go.
func serviceBinaryPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory: %w", err)
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Ferngeist Gateway", "bin", "ferngeist-gateway"), nil
	case "linux":
		return filepath.Join(home, ".local", "share", "ferngeist-gateway", "bin", "ferngeist-gateway"), nil
	case "windows":
		base := os.Getenv("LocalAppData")
		if base == "" {
			base = filepath.Join(home, "AppData", "Local")
		}
		return filepath.Join(base, "FerngeistGateway", "service", "bin", "ferngeist-gateway.exe"), nil
	case "android":
		// Termux: the installer persists the binary to ~/.local/bin.
		return filepath.Join(home, ".local", "bin", "ferngeist-gateway"), nil
	default:
		return "", fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

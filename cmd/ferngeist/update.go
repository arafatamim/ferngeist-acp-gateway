package main

import (
	"context"
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

	checker := update.NewChecker("arafatamim/ferngeist-acp-gateway")
	release, err := checker.LatestStable(ctx)
	if err != nil {
		return fmt.Errorf("check for updates: %w", err)
	}

	latest := strings.TrimPrefix(release.TagName, "v")
	current := strings.TrimPrefix(buildVersion, "v")
	if latest == current {
		fmt.Println("Already up to date (" + current + ").")
		return nil
	}
	if !strings.HasPrefix(latest, current+".") && !isNewerVersionTag(latest, current) {
		fmt.Printf("Installed version %s is newer than latest stable %s; skipping.\n", current, release.TagName)
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

	// Resolve the installed service binary path so the verified binary can be
	// swapped in place. Status.UnitPath is the unit/task path, not the binary.
	manager := service.NewManager()
	status, err := manager.Status()
	if err != nil {
		return fmt.Errorf("read service status: %w", err)
	}
	if !status.Installed {
		return fmt.Errorf("daemon service is not installed; run `ferngeist-gateway daemon install` first")
	}
	binaryPath, err := serviceBinaryPath()
	if err != nil {
		return err
	}

	fmt.Printf("Downloading %s\n", asset.BrowserDownloadURL)

	// Stage the archive in a temporary file next to the binary path so
	// DownloadAndVerify (which writes the raw asset and verifies its
	// checksum) and ExtractArchive (which pulls the binary out) share the
	// same destination directory and filesystem.
	tmpArchive, err := os.CreateTemp(filepath.Dir(binaryPath), ".update-*")
	if err != nil {
		return fmt.Errorf("stage update archive: %w", err)
	}
	tmpArchiveName := tmpArchive.Name()
	_ = tmpArchive.Close()
	defer os.Remove(tmpArchiveName)

	if err := update.DownloadAndVerify(ctx, client, asset.BrowserDownloadURL, wantHex, tmpArchiveName); err != nil {
		return fmt.Errorf("download and verify update: %w", err)
	}

	// Stop the service before replacing the running binary. On Windows a
	// running executable is locked and cannot be renamed over; on all
	// platforms the process must be restarted to pick up the new binary.
	if err := manager.Stop(); err != nil {
		return fmt.Errorf("stop daemon service: %w", err)
	}

	if err := update.ExtractArchiveFromFile(tmpArchiveName, "ferngeist-gateway", binaryPath); err != nil {
		return fmt.Errorf("extract update: %w", err)
	}

	fmt.Printf("Updated to %s; restarting the daemon service.\n", release.TagName)
	if err := manager.Restart(); err != nil {
		return fmt.Errorf("restart daemon service: %w", err)
	}
	return nil
}

// isNewerVersionTag compares two version strings numerically (dot-separated).
func isNewerVersionTag(a, b string) bool {
	ap := parseTagParts(a)
	bp := parseTagParts(b)
	for i := 0; i < len(ap) || i < len(bp); i++ {
		av, bv := 0, 0
		if i < len(ap) {
			av = ap[i]
		}
		if i < len(bp) {
			bv = bp[i]
		}
		if av != bv {
			return av > bv
		}
	}
	return false
}

func parseTagParts(v string) []int {
	var parts []int
	for _, p := range strings.Split(v, ".") {
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				return parts
			}
			n = n*10 + int(c-'0')
		}
		parts = append(parts, n)
	}
	return parts
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
	default:
		return "", fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

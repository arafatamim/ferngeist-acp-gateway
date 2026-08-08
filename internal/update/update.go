// Package update implements release discovery and checksum-verified
// downloads for self-updating the gateway binary.
package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	// LatestReleasePath is the GitHub API path suffix for the newest
	// stable (non-prerelease, non-draft) release of a repository.
	LatestReleasePath = "/releases/latest"
	// ChecksumFileName is the name of the release asset holding the
	// SHA-256 digest of every other release asset.
	ChecksumFileName = "SHA256SUMS"
)

// Release is the subset of a GitHub release payload the updater needs.
type Release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

// Asset is a downloadable release artifact.
type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// Checker resolves the latest stable release for a GitHub repository.
type Checker struct {
	Repo    string
	BaseURL string // test seam; defaults to https://api.github.com/repos/<Repo>
	Client  *http.Client
}

// NewChecker returns a Checker for the given owner/repo pair.
func NewChecker(repo string) *Checker {
	return &Checker{Repo: repo}
}

// client returns c.Client, falling back to the process-wide default.
func (c *Checker) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return http.DefaultClient
}

// baseURL returns the API root for the repository, honoring the test seam.
func (c *Checker) baseURL() string {
	if c.BaseURL != "" {
		return strings.TrimSuffix(c.BaseURL, "/")
	}
	return "https://api.github.com/repos/" + c.Repo
}

// LatestStable fetches the newest stable release of the repository.
func (c *Checker) LatestStable(ctx context.Context) (Release, error) {
	var release Release
	url := c.baseURL() + LatestReleasePath

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return release, fmt.Errorf("fetch latest release: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.client().Do(req)
	if err != nil {
		return release, fmt.Errorf("fetch latest release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return release, fmt.Errorf("fetch latest release: unexpected status %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return release, fmt.Errorf("fetch latest release: %w", err)
	}
	if release.TagName == "" {
		return release, errors.New("fetch latest release: empty tag_name")
	}
	return release, nil
}

// AssetFor returns the first asset whose name contains "<goos>_<goarch>".
func (c *Checker) AssetFor(r Release, goos, goarch string) (Asset, error) {
	needle := goos + "_" + goarch
	for _, asset := range r.Assets {
		if strings.Contains(asset.Name, needle) {
			return asset, nil
		}
	}
	return Asset{}, fmt.Errorf("no asset for %s/%s", goos, goarch)
}

// AssetBaseURL returns the directory containing the release assets,
// derived from the first asset that has a download URL. The URL's scheme
// and authority are preserved (path.Dir on the full URL would collapse
// "https://" into "https:/").
func (c *Checker) AssetBaseURL(r Release) string {
	for _, asset := range r.Assets {
		if asset.BrowserDownloadURL == "" {
			continue
		}
		u, err := url.Parse(asset.BrowserDownloadURL)
		if err != nil {
			continue
		}
		u.Path = path.Dir(u.Path)
		return u.String()
	}
	return ""
}

// DefaultClient returns the client used when none is configured.
func DefaultClient() *http.Client {
	return http.DefaultClient
}

// ChecksumFor parses SHA256SUMS data and returns the hex-decoded digest
// for the named file. Entries are "<hexdigest>  <name>" lines; a missing
// entry, a non-hex digest, a digest of the wrong length, or an all-zero
// digest are all errors.
func ChecksumFor(data []byte, name string) ([]byte, error) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] != name {
			continue
		}
		digest, err := hex.DecodeString(fields[0])
		if err != nil {
			return nil, fmt.Errorf("invalid checksum for %s: %w", name, err)
		}
		if len(digest) != sha256.Size {
			return nil, fmt.Errorf("invalid checksum for %s: %w", name, errors.New("expected a 32-byte sha256 digest"))
		}
		var zero [sha256.Size]byte
		if bytes.Equal(digest, zero[:]) {
			return nil, fmt.Errorf("invalid checksum for %s: %w", name, errors.New("all-zero sha256 digest"))
		}
		return digest, nil
	}
	return nil, fmt.Errorf("no checksum entry for %s", name)
}

// DownloadAndVerify streams url into dest, verifying its SHA-256 digest
// against want before the file is made visible at dest. The download is
// staged in a temporary file next to dest and only renamed into place
// once the checksum matches; any failure removes the temporary file.
func DownloadAndVerify(ctx context.Context, client *http.Client, url string, want []byte, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: unexpected status %s", url, resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".update-*")
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	tmpName := tmp.Name()
	removeTmp := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hasher), resp.Body); err != nil {
		removeTmp()
		return fmt.Errorf("download %s: %w", url, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("download %s: %w", url, err)
	}
	if !bytes.Equal(hasher.Sum(nil), want) {
		_ = os.Remove(tmpName)
		return fmt.Errorf("checksum mismatch for %s", url)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("download %s: %w", url, err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("download %s: %w", url, err)
	}
	return nil
}

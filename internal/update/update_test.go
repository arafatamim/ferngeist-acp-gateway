package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCheckerLatestStable(t *testing.T) {
	var baseURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/ferngeist/releases/latest" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		body := fmt.Sprintf(`{"tag_name":"v1.2.3","assets":[
			{"name":"ferngeist-gateway_1.2.3_linux_amd64.tar.gz","browser_download_url":"%[1]s/releases/download/v1.2.3/ferngeist-gateway_1.2.3_linux_amd64.tar.gz"},
			{"name":"ferngeist-gateway_1.2.3_darwin_arm64.tar.gz","browser_download_url":"%[1]s/releases/download/v1.2.3/ferngeist-gateway_1.2.3_darwin_arm64.tar.gz"}
		]}`, baseURL)
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	baseURL = server.URL

	checker := NewChecker("acme/ferngeist")
	checker.BaseURL = server.URL + "/repos/acme/ferngeist"

	release, err := checker.LatestStable(context.Background())
	if err != nil {
		t.Fatalf("LatestStable() error = %v", err)
	}
	if release.TagName != "v1.2.3" {
		t.Fatalf("TagName = %q, want %q", release.TagName, "v1.2.3")
	}
	if len(release.Assets) != 2 {
		t.Fatalf("len(Assets) = %d, want 2", len(release.Assets))
	}
	if !strings.Contains(release.Assets[0].Name, "linux_amd64") {
		t.Fatalf("first asset = %q, want linux_amd64 asset first", release.Assets[0].Name)
	}
}

func TestCheckerAssetFor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()

	release := Release{
		TagName: "v1.2.3",
		Assets: []Asset{
			{Name: "ferngeist-gateway_1.2.3_linux_amd64.tar.gz", BrowserDownloadURL: server.URL + "/releases/download/v1.2.3/ferngeist-gateway_1.2.3_linux_amd64.tar.gz"},
			{Name: "ferngeist-gateway_1.2.3_darwin_arm64.tar.gz", BrowserDownloadURL: server.URL + "/releases/download/v1.2.3/ferngeist-gateway_1.2.3_darwin_arm64.tar.gz"},
		},
	}

	checker := NewChecker("acme/ferngeist")

	asset, err := checker.AssetFor(release, "linux", "amd64")
	if err != nil {
		t.Fatalf("AssetFor(linux/amd64) error = %v", err)
	}
	if asset.Name != "ferngeist-gateway_1.2.3_linux_amd64.tar.gz" {
		t.Fatalf("asset.Name = %q, want linux amd64 asset", asset.Name)
	}

	if _, err := checker.AssetFor(release, "plan9", "amd64"); err == nil {
		t.Fatal("AssetFor(plan9/amd64) = nil error, want error")
	}
}

func TestChecksumFor(t *testing.T) {
	digest := sha256.Sum256([]byte("hello world"))

	got, err := ChecksumFor([]byte(hex.EncodeToString(digest[:])+"  asset.bin\n"), "asset.bin")
	if err != nil {
		t.Fatalf("ChecksumFor(asset.bin) error = %v", err)
	}
	if !bytes.Equal(got, digest[:]) {
		t.Fatalf("ChecksumFor() = %x, want %x", got, digest[:])
	}

	zeros := strings.Repeat("0", sha256.Size*2)
	if _, err := ChecksumFor([]byte(zeros+"  asset.bin\n"), "asset.bin"); err == nil {
		t.Fatal("ChecksumFor(all-zero digest) = nil error, want error")
	}

	if _, err := ChecksumFor([]byte(hex.EncodeToString(digest[:])+"  asset.bin\n"), "other.bin"); err == nil {
		t.Fatal("ChecksumFor(missing name) = nil error, want error")
	} else if !strings.Contains(err.Error(), "no checksum entry") {
		t.Fatalf("ChecksumFor(missing name) error = %v, want contains %q", err, "no checksum entry")
	}
}

func TestDownloadAndVerify(t *testing.T) {
	payload := []byte("ferngeist-gateway binary payload")
	goodSum := sha256.Sum256(payload)
	badSum := sha256.Sum256([]byte("not the payload"))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer server.Close()

	dest := filepath.Join(t.TempDir(), "ferngeist-gateway")

	if err := DownloadAndVerify(context.Background(), server.Client(), server.URL, goodSum[:], dest); err != nil {
		t.Fatalf("DownloadAndVerify(correct hash) error = %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("os.ReadFile(dest) error = %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatalf("dest contents = %q, want %q", data, payload)
	}
	// os.Chmod is a no-op on Windows (executability is by extension there),
	// so the permission assertion is POSIX-only.
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(dest); err != nil {
			t.Fatalf("os.Stat(dest) error = %v", err)
		} else if info.Mode().Perm() != 0o755 {
			t.Fatalf("dest mode = %v, want 0o755", info.Mode().Perm())
		}
	}

	dest2 := filepath.Join(t.TempDir(), "ferngeist-gateway")
	err = DownloadAndVerify(context.Background(), server.Client(), server.URL, badSum[:], dest2)
	if err == nil {
		t.Fatal("DownloadAndVerify(wrong hash) = nil error, want error")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("DownloadAndVerify(wrong hash) error = %v, want contains %q", err, "checksum mismatch")
	}
	if _, statErr := os.Stat(dest2); !os.IsNotExist(statErr) {
		t.Fatalf("dest exists after checksum mismatch: stat error = %v", statErr)
	}

	entries, err := os.ReadDir(filepath.Dir(dest2))
	if err != nil {
		t.Fatalf("os.ReadDir(tempdir) error = %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".update-") {
			t.Fatalf("temp file %q left behind after checksum mismatch", entry.Name())
		}
	}
}

func TestAssetBaseURL(t *testing.T) {
	release := Release{
		Assets: []Asset{
			{Name: "ferngeist-gateway_1.2.3_linux_amd64.tar.gz", BrowserDownloadURL: "https://host/releases/download/v1.2.3/ferngeist-gateway_1.2.3_linux_amd64.tar.gz"},
		},
	}
	if got := NewChecker("acme/ferngeist").AssetBaseURL(release); got != "https://host/releases/download/v1.2.3" {
		t.Fatalf("AssetBaseURL() = %q, want %q", got, "https://host/releases/download/v1.2.3")
	}
}

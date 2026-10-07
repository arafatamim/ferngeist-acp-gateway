package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
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

	// An all-zero digest is valid (astronomically unlikely in practice, but
	// not a format error — length and hex checks are what matter).
	zeros := strings.Repeat("0", sha256.Size*2)
	gotZero, err := ChecksumFor([]byte(zeros+"  asset.bin\n"), "asset.bin")
	if err != nil {
		t.Fatalf("ChecksumFor(all-zero digest) error = %v, want nil", err)
	}
	if len(gotZero) != sha256.Size {
		t.Fatalf("ChecksumFor(all-zero digest) len = %d, want %d", len(gotZero), sha256.Size)
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
		if _, err := w.Write(payload); err != nil {
			t.Fatalf("w.Write() error = %v", err)
		}
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

// buildTarGz builds a gzipped tarball containing binName at the goreleaser
// layout "<dir>/<binary>". Archive member names always use forward slashes,
// regardless of host OS.
func buildTarGz(t *testing.T, binName string, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: "ferngeist-gateway/" + binName,
		Mode: 0o755,
		Size: int64(len(payload)),
	}); err != nil {
		t.Fatalf("tar WriteHeader: %v", err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatalf("tar Write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}
	return buf.Bytes()
}

// buildZip builds a zip archive containing binName.
func buildZip(t *testing.T, binName string, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("ferngeist-gateway/" + binName)
	if err != nil {
		t.Fatalf("zip Create: %v", err)
	}
	if _, err := f.Write(payload); err != nil {
		t.Fatalf("zip Write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip Close: %v", err)
	}
	return buf.Bytes()
}

func TestExtractArchive(t *testing.T) {
	const binName = "ferngeist-gateway"
	payload := []byte("ferngeist-gateway binary payload")
	dest := filepath.Join(t.TempDir(), "ferngeist-gateway")

	tarGz := buildTarGz(t, binName, payload)
	if err := ExtractArchive(tarGz, binName, dest); err != nil {
		t.Fatalf("ExtractArchive(tar.gz) error = %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("os.ReadFile(tar.gz dest) error = %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatalf("tar.gz dest contents = %q, want %q", data, payload)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(dest); err != nil {
			t.Fatalf("os.Stat(tar.gz dest) error = %v", err)
		} else if info.Mode().Perm() != 0o755 {
			t.Fatalf("tar.gz dest mode = %v, want 0o755", info.Mode().Perm())
		}
	}

	zipDest := filepath.Join(t.TempDir(), "ferngeist-gateway")
	if err := ExtractArchive(buildZip(t, binName, payload), binName, zipDest); err != nil {
		t.Fatalf("ExtractArchive(zip) error = %v", err)
	}
	zipData, err := os.ReadFile(zipDest)
	if err != nil {
		t.Fatalf("os.ReadFile(zip dest) error = %v", err)
	}
	if !bytes.Equal(zipData, payload) {
		t.Fatalf("zip dest contents = %q, want %q", zipData, payload)
	}
}

// TestExtractArchiveWindowsExe covers the goreleaser windows archive layout:
// the entry is ferngeist-gateway.exe (the .exe suffix is kept). runUpdate
// passes filepath.Base(binaryPath) as the name, which is ferngeist-gateway.exe
// on Windows — ExtractArchive must match it. Regression test for the
// "ferngeist-gateway not found in archive" update failure on Windows.
func TestExtractArchiveWindowsExe(t *testing.T) {
	const binName = "ferngeist-gateway.exe"
	payload := []byte("windows binary payload")
	dest := filepath.Join(t.TempDir(), binName)

	if err := ExtractArchive(buildZip(t, binName, payload), binName, dest); err != nil {
		t.Fatalf("ExtractArchive(zip, .exe) error = %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("os.ReadFile(dest) error = %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatalf("dest contents = %q, want %q", data, payload)
	}
}

func TestExtractArchiveMissingBinary(t *testing.T) {
	const binName = "ferngeist-gateway"
	payload := []byte("ferngeist-gateway binary payload")
	dest := filepath.Join(t.TempDir(), "ferngeist-gateway")

	// A tarball that does not contain the binary must error and leave no
	// file behind.
	archive := buildTarGz(t, "other-binary", payload)
	if err := ExtractArchive(archive, binName, dest); err == nil {
		t.Fatal("ExtractArchive(missing binary) = nil error, want error")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("dest exists after failed extract: stat error = %v", statErr)
	}
}

func TestAssetForExactNameIgnoresBrewAndPrefixes(t *testing.T) {
	r := Release{
		TagName: "v1.2.3",
		Assets: []Asset{
			{Name: "ferngeist-gateway_1.2.3_linux_amd64_brew.tar.gz"},
			{Name: "ferngeist-gateway_1.2.3_linux_arm64.tar.gz"},
			{Name: "ferngeist-gateway_1.2.3_linux_amd64.tar.gz"},
			{Name: "ferngeist-gateway_1.2.3_windows_amd64_brew.zip"},
			{Name: "ferngeist-gateway_1.2.3_windows_amd64.zip"},
		},
	}
	c := NewChecker("acme/ferngeist")
	for goos, want := range map[string]string{
		"linux":   "ferngeist-gateway_1.2.3_linux_amd64.tar.gz",
		"windows": "ferngeist-gateway_1.2.3_windows_amd64.zip",
	} {
		got, err := c.AssetFor(r, goos, "amd64")
		if err != nil || got.Name != want {
			t.Errorf("AssetFor(%s/amd64) = %q, %v; want %q", goos, got.Name, err, want)
		}
	}
	if a, err := c.AssetFor(r, "linux", "arm"); err == nil {
		t.Errorf("AssetFor(linux/arm) = %q, want error (must not prefix-match arm64)", a.Name)
	}
}

func TestSwapBinaryAside(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "gw")
	staged := filepath.Join(dir, "staged")
	write := func(p, s string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(s), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	read := func(p string) string {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	// Success, with a stale .old that must be replaced.
	write(dest, "old")
	write(dest+".old", "stale")
	write(staged, "new")
	if err := swapBinary(staged, dest, true); err != nil {
		t.Fatal(err)
	}
	if read(dest) != "new" || read(dest+".old") != "old" {
		t.Fatalf("dest=%q old=%q, want new/old", read(dest), read(dest+".old"))
	}
	CleanupOld(dest)
	if _, err := os.Stat(dest + ".old"); !os.IsNotExist(err) {
		t.Fatalf(".old not cleaned: %v", err)
	}

	// Failed final rename (staged missing) rolls dest back.
	if err := swapBinary(filepath.Join(dir, "nope"), dest, true); err == nil {
		t.Fatal("swap with missing staged file = nil error")
	}
	if read(dest) != "new" {
		t.Fatalf("dest after rollback = %q, want new", read(dest))
	}

	// No existing dest: plain rename.
	fresh := filepath.Join(dir, "fresh")
	write(staged, "x")
	if err := swapBinary(staged, fresh, true); err != nil || read(fresh) != "x" {
		t.Fatalf("fresh swap err=%v", err)
	}

	// Plain (non-aside) mode.
	write(staged, "y")
	if err := swapBinary(staged, fresh, false); err != nil || read(fresh) != "y" {
		t.Fatalf("plain swap err=%v", err)
	}
}

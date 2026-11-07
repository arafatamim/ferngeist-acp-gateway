package acquire

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/catalog"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

// Installer acquires trusted registry-backed binaries into a gateway-managed
// install root so launch stays uniform after the first install.
type Installer struct {
	logger     *slog.Logger
	httpClient *http.Client
	managedDir string
	store      *storage.SQLiteStore

	// mu guards the npm agent fields (see npm.go).
	mu            sync.Mutex
	jsRuntime     string
	npmInstalling map[string]bool // install dir -> background install running
}

func New(logger *slog.Logger, managedDir string, store *storage.SQLiteStore) *Installer {
	return &Installer{
		logger: logger.With("component", "acquire"),
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
		managedDir: managedDir,
		store:      store,
	}
}

func (i *Installer) Ensure(ctx context.Context, agent catalog.Agent) (catalog.Agent, bool, error) {
	if agent.Launch.Mode != "external" {
		return agent, false, nil
	}
	if strings.TrimSpace(agent.Registry.CurrentBinaryArchiveURL) == "" || strings.TrimSpace(agent.Registry.CurrentBinaryPath) == "" {
		return agent, false, nil
	}

	targetPath := filepath.Join(i.managedDir, agent.ID, filepath.Base(strings.ReplaceAll(agent.Registry.CurrentBinaryPath, "\\", "/")))
	archiveName := agent.Registry.CurrentBinaryArchiveURL
	if parsed, err := url.Parse(archiveName); err == nil {
		archiveName = parsed.Path
	}
	installed, cached := i.cachedBinary(ctx, agent, targetPath, archiveName)
	if cached {
		return useBinary(agent, installed), false, nil
	}

	installedPath, err := i.download(ctx, agent, targetPath)
	if err != nil {
		if installed == "" {
			return agent, false, err
		}
		i.logger.Warn("update registry binary failed; keeping installed version", "agent_id", agent.ID, "version", agent.Registry.Version, "error", err)
		return useBinary(agent, installed), false, nil
	}

	targetPath = installedPath
	i.logger.Info("acquired registry binary", "agent_id", agent.ID, "version", agent.Registry.Version, "target_path", targetPath)
	if i.store != nil {
		if err := i.store.SaveAcquiredBinary(ctx, storage.AcquiredBinaryRecord{
			AgentID:     agent.ID,
			Version:     agent.Registry.Version,
			Path:        targetPath,
			ArchiveURL:  agent.Registry.CurrentBinaryArchiveURL,
			InstalledAt: time.Now().UTC(),
		}); err != nil {
			i.logger.Warn("persist acquired binary failed", "agent_id", agent.ID, "error", err)
		}
	}
	return useBinary(agent, targetPath), true, nil
}

func (i *Installer) cachedBinary(ctx context.Context, agent catalog.Agent, targetPath, archiveName string) (string, bool) {
	archive := isArchive(archiveName)
	// installed is a previous version kept as a fallback when the update fails.
	installed := ""
	if i.store != nil {
		if record, err := i.store.GetAcquiredBinary(ctx, agent.ID); err == nil && isFile(record.Path) {
			bundle := bundleDir(filepath.Dir(targetPath), agent.Registry.Version, archiveName) + string(os.PathSeparator)
			if record.Version == agent.Registry.Version && (!archive || strings.HasPrefix(record.Path, bundle)) {
				return record.Path, true
			}
			installed = record.Path
		}
	}
	if installed != "" {
		return installed, false
	}
	entry := filepath.FromSlash(normalizeArchiveEntry(agent.Registry.CurrentBinaryPath))
	if archive && filepath.IsLocal(entry) {
		cached := filepath.Join(bundleDir(filepath.Dir(targetPath), agent.Registry.Version, archiveName), entry)
		if isFile(cached) {
			return cached, true
		}
	}
	if !archive && isFile(targetPath) {
		// No record to compare versions against (no store, or a pre-record install).
		return targetPath, true
	}
	return "", false
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func useBinary(agent catalog.Agent, path string) catalog.Agent {
	agent.Launch.Command = path
	agent.Detection = catalog.DetectionConfig{Mode: "local_file", Path: path}
	agent.Detected = true
	return agent
}

// download fetches the agent's registry artifact and installs its binary at
// targetPath.
func (i *Installer) download(ctx context.Context, agent catalog.Agent, targetPath string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return "", err
	}

	tmpFile, err := os.CreateTemp("", "ferngeist-gateway-download-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmpFile.Name()
	_ = tmpFile.Close()
	defer os.Remove(tmpPath)

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, agent.Registry.CurrentBinaryArchiveURL, nil)
	if err != nil {
		return "", err
	}
	response, err := i.httpClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("binary download returned status %d", response.StatusCode)
	}

	downloadFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(downloadFile, response.Body); err != nil {
		_ = downloadFile.Close()
		return "", err
	}
	if err := downloadFile.Close(); err != nil {
		return "", err
	}

	// The temp file has no extension, so the archive format comes from the URL path.
	archiveName := agent.Registry.CurrentBinaryArchiveURL
	if parsed, err := url.Parse(archiveName); err == nil {
		archiveName = parsed.Path
	}
	return installDownloadedArtifact(tmpPath, archiveName, targetPath, agent.Registry.CurrentBinaryPath, agent.Registry.Version)
}

func installDownloadedArtifact(sourcePath, archiveName, targetPath, archiveEntry, version string) (string, error) {
	lower := strings.ToLower(archiveName)
	if !isArchive(lower) {
		return targetPath, copyFile(sourcePath, targetPath, 0o755)
	}
	parent := filepath.Dir(targetPath)
	stage, err := os.MkdirTemp(parent, ".archive-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	root, err := os.OpenRoot(stage)
	if err != nil {
		return "", err
	}
	var entries []string
	if strings.HasSuffix(lower, ".zip") {
		entries, err = extractZip(sourcePath, root)
	} else {
		entries, err = extractTarGz(sourcePath, root)
	}
	closeErr := root.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	needle := normalizeArchiveEntry(archiveEntry)
	launch := ""
	ambiguous := false
	for _, name := range entries {
		if name == needle {
			launch = name
			ambiguous = false
			break
		}
		if filepath.Base(name) == filepath.Base(needle) {
			if launch != "" {
				ambiguous = true
			}
			launch = name
		}
	}
	if ambiguous {
		return "", fmt.Errorf("ambiguous archive entry %q", archiveEntry)
	}
	if launch == "" {
		return "", fmt.Errorf("archive entry %q not found", archiveEntry)
	}
	info, err := os.Stat(filepath.Join(stage, filepath.FromSlash(launch)))
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("archive entry %q is not a regular file", archiveEntry)
	}
	if err := os.Chmod(filepath.Join(stage, filepath.FromSlash(launch)), info.Mode().Perm()|0o111); err != nil {
		return "", err
	}
	// Versioned directories let a completed install appear in one rename while the old version remains usable.
	final := bundleDir(parent, version, archiveName)
	if err := os.Rename(stage, final); err != nil {
		if !isFile(filepath.Join(final, filepath.FromSlash(launch))) {
			return "", err
		}
	}
	return filepath.Join(final, filepath.FromSlash(launch)), nil
}

func isArchive(name string) bool {
	name = strings.ToLower(name)
	return strings.HasSuffix(name, ".zip") || strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz")
}

func bundleDir(parent, version, archiveName string) string {
	hash := sha256.Sum256([]byte(version + "\x00" + archiveName))
	return filepath.Join(parent, fmt.Sprintf("bundle-%x", hash[:12]))
}

func extractZip(sourcePath string, root *os.Root) ([]string, error) {
	reader, err := zip.OpenReader(sourcePath)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	var entries []string
	for _, file := range reader.File {
		src, err := file.Open()
		if err != nil {
			return nil, err
		}
		err = extractEntry(root, file.Name, file.Mode(), src, "")
		closeErr := src.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if !file.FileInfo().IsDir() {
			entries = append(entries, normalizeArchiveEntry(file.Name))
		}
	}
	return entries, nil
}

func extractTarGz(sourcePath string, root *os.Root) ([]string, error) {
	file, err := os.Open(sourcePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	var entries []string
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		mode := header.FileInfo().Mode()
		if header.Typeflag != tar.TypeReg && header.Typeflag != 0 && header.Typeflag != tar.TypeDir && header.Typeflag != tar.TypeSymlink {
			return nil, fmt.Errorf("unsupported archive entry %q", header.Name)
		}
		if err := extractEntry(root, header.Name, mode, reader, header.Linkname); err != nil {
			return nil, err
		}
		if !mode.IsDir() {
			entries = append(entries, normalizeArchiveEntry(header.Name))
		}
	}
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return nil, err
	}
	return entries, nil
}

func extractEntry(root *os.Root, name string, mode os.FileMode, src io.Reader, link string) error {
	name = strings.ReplaceAll(name, "\\", "/")
	name = strings.TrimPrefix(name, "./")
	if name == "." || name == "" {
		return nil
	}
	if !filepath.IsLocal(filepath.FromSlash(name)) {
		return fmt.Errorf("unsafe archive path %q", name)
	}
	name = filepath.FromSlash(name)
	if mode.IsDir() {
		return root.MkdirAll(name, 0o755)
	}
	if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}
	if mode&os.ModeSymlink != 0 {
		if link == "" {
			data, err := io.ReadAll(io.LimitReader(src, 4097))
			if err != nil {
				return err
			}
			link = string(data)
		}
		link = strings.ReplaceAll(link, "\\", "/")
		if len(link) > 4096 || !filepath.IsLocal(filepath.Join(filepath.Dir(name), filepath.FromSlash(link))) || filepath.IsAbs(filepath.FromSlash(link)) {
			return fmt.Errorf("unsafe archive symlink %q", name)
		}
		return root.Symlink(filepath.FromSlash(link), name)
	}
	if !mode.IsRegular() {
		return fmt.Errorf("unsupported archive entry %q", name)
	}
	out, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode.Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, src)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	return root.Chmod(name, mode.Perm())
}

func copyFile(sourcePath, targetPath string, mode os.FileMode) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	return writeReaderToFile(targetPath, source, mode)
}

func writeReaderToFile(targetPath string, source io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return err
	}
	// Write beside the target and rename: overwriting in place fails while the
	// old version is running (ETXTBSY) and leaves a truncated binary on error.
	tmpPath := targetPath + ".download"
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer os.Remove(tmpPath)
	if _, err := io.Copy(file, source); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, targetPath)
}

func normalizeArchiveEntry(path string) string {
	return strings.TrimPrefix(strings.ReplaceAll(path, "\\", "/"), "./")
}

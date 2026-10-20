package acquire

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
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
	// installed is a previous version kept as a fallback when the update fails.
	installed := ""
	recorded := false
	if i.store != nil {
		if record, err := i.store.GetAcquiredBinary(ctx, agent.ID); err == nil && isFile(record.Path) {
			if record.Version == agent.Registry.Version {
				return useBinary(agent, record.Path), false, nil
			}
			installed, recorded = record.Path, true
		}
	}
	if !recorded && isFile(targetPath) {
		// No record to compare versions against (no store, or a pre-record install).
		return useBinary(agent, targetPath), false, nil
	}

	if err := i.download(ctx, agent, targetPath); err != nil {
		if installed == "" {
			return agent, false, err
		}
		i.logger.Warn("update registry binary failed; keeping installed version", "agent_id", agent.ID, "version", agent.Registry.Version, "error", err)
		return useBinary(agent, installed), false, nil
	}

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
func (i *Installer) download(ctx context.Context, agent catalog.Agent, targetPath string) error {
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return err
	}

	tmpFile, err := os.CreateTemp("", "ferngeist-gateway-download-*")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	_ = tmpFile.Close()
	defer os.Remove(tmpPath)

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, agent.Registry.CurrentBinaryArchiveURL, nil)
	if err != nil {
		return err
	}
	response, err := i.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("binary download returned status %d", response.StatusCode)
	}

	downloadFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(downloadFile, response.Body); err != nil {
		_ = downloadFile.Close()
		return err
	}
	if err := downloadFile.Close(); err != nil {
		return err
	}

	// The temp file has no extension, so the archive format comes from the URL path.
	archiveName := agent.Registry.CurrentBinaryArchiveURL
	if parsed, err := url.Parse(archiveName); err == nil {
		archiveName = parsed.Path
	}
	return installDownloadedArtifact(tmpPath, archiveName, targetPath, agent.Registry.CurrentBinaryPath)
}

func installDownloadedArtifact(sourcePath, archiveName, targetPath, archiveEntry string) error {
	lower := strings.ToLower(archiveName)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return extractZipEntry(sourcePath, targetPath, archiveEntry)
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return extractTarGzEntry(sourcePath, targetPath, archiveEntry)
	default:
		return copyFile(sourcePath, targetPath, 0o755)
	}
}

func extractZipEntry(sourcePath, targetPath, archiveEntry string) error {
	reader, err := zip.OpenReader(sourcePath)
	if err != nil {
		return err
	}
	defer reader.Close()

	needle := normalizeArchiveEntry(archiveEntry)
	for _, file := range reader.File {
		if normalizeArchiveEntry(file.Name) != needle && filepath.Base(file.Name) != filepath.Base(needle) {
			continue
		}
		src, err := file.Open()
		if err != nil {
			return err
		}
		defer src.Close()
		return writeReaderToFile(targetPath, src, 0o755)
	}
	return fmt.Errorf("archive entry %q not found in zip", archiveEntry)
}

func extractTarGzEntry(sourcePath, targetPath, archiveEntry string) error {
	file, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer file.Close()

	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzipReader.Close()

	tarReader := tar.NewReader(gzipReader)
	needle := normalizeArchiveEntry(archiveEntry)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if header.FileInfo().IsDir() {
			continue
		}
		if normalizeArchiveEntry(header.Name) != needle && filepath.Base(header.Name) != filepath.Base(needle) {
			continue
		}
		return writeReaderToFile(targetPath, tarReader, 0o755)
	}
	return fmt.Errorf("archive entry %q not found in tar.gz", archiveEntry)
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
	return strings.TrimLeft(strings.ReplaceAll(path, "\\", "/"), "./")
}

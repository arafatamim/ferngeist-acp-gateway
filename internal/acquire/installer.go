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

	// mu guards install jobs and the npm agent fields (see npm.go).
	mu            sync.Mutex
	jsRuntime     string
	npmInstalling map[string]bool // install dir -> background install running
	npmFailed     map[string]bool // install dir -> last install failed; launch through npm exec

	binaryCtx  context.Context
	stopBinary context.CancelFunc
	binaryWG   sync.WaitGroup
	binaryJobs map[string]*binaryJob
}

const binaryDownloadIdleTimeout = 30 * time.Second

// New creates an installer. Close cancels and joins its registry binary jobs.
func New(logger *slog.Logger, managedDir string, store *storage.SQLiteStore) *Installer {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = binaryDownloadIdleTimeout
	ctx, cancel := context.WithCancel(context.Background())
	return &Installer{
		logger:     logger.With("component", "acquire"),
		httpClient: &http.Client{Transport: transport},
		managedDir: managedDir,
		store:      store,
		binaryCtx:  ctx,
		stopBinary: cancel,
		binaryJobs: make(map[string]*binaryJob),
	}
}

// Close stops registry binary installs before the daemon closes their store.
func (i *Installer) Close() {
	i.mu.Lock()
	i.stopBinary()
	i.mu.Unlock()
	i.binaryWG.Wait()
	i.httpClient.CloseIdleConnections()
}

func (i *Installer) installBinary(ctx context.Context, agent catalog.Agent, targetPath, archiveName string, job *binaryJob) (string, error) {
	installedPath, err := i.download(ctx, agent, targetPath, archiveName, job)
	if err != nil {
		return "", fmt.Errorf("acquire registry binary: %w", err)
	}

	i.logger.Info("acquired registry binary", "agent_id", agent.ID, "version", agent.Registry.Version, "target_path", installedPath)
	if i.store != nil {
		if err := i.store.SaveAcquiredBinary(ctx, storage.AcquiredBinaryRecord{
			AgentID:     agent.ID,
			Version:     agent.Registry.Version,
			Path:        installedPath,
			ArchiveURL:  agent.Registry.CurrentBinaryArchiveURL,
			InstalledAt: time.Now().UTC(),
		}); err != nil {
			i.logger.Warn("persist acquired binary failed", "agent_id", agent.ID, "error", err)
		}
	}
	return installedPath, nil
}

func (i *Installer) cachedBinary(ctx context.Context, agent catalog.Agent, targetPath, archiveName string) (string, bool) {
	archive := isArchive(archiveName)
	// Keep the previous version usable by listings and as a fallback when an update fails.
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
	entry := filepath.FromSlash(normalizeArchiveEntry(agent.Registry.CurrentBinaryPath))
	if archive && filepath.IsLocal(entry) {
		cached := filepath.Join(bundleDir(filepath.Dir(targetPath), agent.Registry.Version, archiveName), entry)
		if isFile(cached) {
			return cached, true
		}
	}
	if !archive {
		if cached := rawBinaryPath(targetPath, agent.Registry.Version, archiveName); isFile(cached) {
			return cached, true
		}
		if installed == "" && isFile(targetPath) {
			// No record to compare versions against (no store, or a pre-record install).
			return targetPath, true
		}
	}
	return installed, false
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
func (i *Installer) download(ctx context.Context, agent catalog.Agent, targetPath, archiveName string, job *binaryJob) (string, error) {
	if !isArchive(archiveName) {
		targetPath = rawBinaryPath(targetPath, agent.Registry.Version, archiveName)
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(filepath.Dir(targetPath), ".download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(file.Name())

	copyErr := i.downloadTo(ctx, agent.Registry.CurrentBinaryArchiveURL, file, job)
	closeErr := file.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return installDownloadedArtifact(ctx, file.Name(), archiveName, targetPath, agent.Registry.CurrentBinaryPath, agent.Registry.Version)
}

func (i *Installer) downloadTo(ctx context.Context, url string, dst io.Writer, job *binaryJob) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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

	job.total.Store(max(response.ContentLength, 0))
	reader := binaryDownloadReader{Reader: response.Body, cancel: cancel, idle: binaryDownloadIdleTimeout, job: job}
	if _, err := io.Copy(dst, reader); err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		return err
	}
	return nil
}

type binaryDownloadReader struct {
	io.Reader
	cancel context.CancelCauseFunc
	idle   time.Duration
	job    *binaryJob
}

func (r binaryDownloadReader) Read(p []byte) (int, error) {
	timer := time.AfterFunc(r.idle, func() { r.cancel(errBinaryDownloadIdle) })
	defer timer.Stop()
	n, err := r.Reader.Read(p)
	r.job.received.Add(int64(n))
	return n, err
}

type installReader struct {
	ctx context.Context
	io.Reader
}

func (r installReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

func installDownloadedArtifact(ctx context.Context, sourcePath, archiveName, targetPath, archiveEntry, version string) (string, error) {
	lower := strings.ToLower(archiveName)
	if !isArchive(lower) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := os.Chmod(sourcePath, 0o755); err != nil {
			return "", err
		}
		return targetPath, os.Rename(sourcePath, targetPath)
	}
	parent := filepath.Dir(targetPath)
	stage, err := os.MkdirTemp(parent, ".archive-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	entries, err := extractArchive(ctx, sourcePath, lower, stage)
	if err != nil {
		return "", err
	}
	launch, err := findArchiveBinary(stage, archiveEntry, entries)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
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

func extractArchive(ctx context.Context, sourcePath, lower, stage string) ([]string, error) {
	root, err := os.OpenRoot(stage)
	if err != nil {
		return nil, err
	}
	var entries []string
	if strings.HasSuffix(lower, ".zip") {
		entries, err = extractZip(ctx, sourcePath, root)
	} else {
		entries, err = extractTarGz(ctx, sourcePath, root)
	}
	closeErr := root.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return entries, nil
}

func findArchiveBinary(stage, archiveEntry string, entries []string) (string, error) {
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
	return launch, nil
}

func isArchive(name string) bool {
	name = strings.ToLower(name)
	return strings.HasSuffix(name, ".zip") || strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz")
}

func rawBinaryPath(targetPath, version, archiveName string) string {
	return filepath.Join(bundleDir(filepath.Dir(targetPath), version, archiveName), filepath.Base(targetPath))
}

func bundleDir(parent, version, archiveName string) string {
	hash := sha256.Sum256([]byte(version + "\x00" + archiveName))
	return filepath.Join(parent, fmt.Sprintf("bundle-%x", hash[:12]))
}

func extractZip(ctx context.Context, sourcePath string, root *os.Root) ([]string, error) {
	reader, err := zip.OpenReader(sourcePath)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	var entries []string
	for _, file := range reader.File {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		src, err := file.Open()
		if err != nil {
			return nil, err
		}
		err = extractEntry(root, file.Name, file.Mode(), installReader{ctx: ctx, Reader: src}, "")
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

func extractTarGz(ctx context.Context, sourcePath string, root *os.Root) ([]string, error) {
	file, err := os.Open(sourcePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	gz, err := gzip.NewReader(installReader{ctx: ctx, Reader: file})
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	source := installReader{ctx: ctx, Reader: gz}
	reader := tar.NewReader(source)
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
	if _, err := io.Copy(io.Discard, source); err != nil {
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

func normalizeArchiveEntry(path string) string {
	return strings.TrimPrefix(strings.ReplaceAll(path, "\\", "/"), "./")
}

package acquire

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/catalog"
)

const binaryInstallRetryDelay = 30 * time.Second

var errBinaryDownloadIdle = errors.New("registry binary download stopped making progress")

// ErrBinaryInstallRequired indicates a cold registry binary on a listing request.
var ErrBinaryInstallRequired = errors.New("connect to install the agent's registry binary")

// AgentInstalling reports an agent install in progress. Registry binaries
// report download progress; npm installs and unknown lengths report zero.
type AgentInstalling struct {
	BytesReceived int64
	BytesTotal    int64
}

func (*AgentInstalling) Error() string { return "agent is installing" }

type binaryJob struct {
	version  string
	path     string
	received atomic.Int64
	total    atomic.Int64
	err      error
	retryAt  time.Time
}

// ResolveBinary selects a completed binary or starts acquiring the current
// version. A lookup without installation may use an older completed version.
func (i *Installer) ResolveBinary(ctx context.Context, agent catalog.Agent, install bool) (catalog.Agent, error) {
	registry := agent.Registry
	if agent.Launch.Mode != "external" || strings.TrimSpace(registry.CurrentBinaryArchiveURL) == "" ||
		strings.TrimSpace(registry.CurrentBinaryPath) == "" {
		return agent, nil
	}

	target := filepath.Join(i.managedDir, agent.ID, filepath.Base(strings.ReplaceAll(registry.CurrentBinaryPath, "\\", "/")))
	archiveName := artifactName(registry.CurrentBinaryArchiveURL)
	bundle := bundleDir(filepath.Dir(target), registry.Version, archiveName)

	installed, current := i.cachedBinary(ctx, agent, target, archiveName)
	i.mu.Lock()
	defer i.mu.Unlock()
	job := i.binaryJobs[agent.ID]
	running := job != nil && job.err == nil && job.path == ""
	sameVersion := job != nil && job.version == registry.Version
	switch {
	case install && running && sameVersion:
		return agent, &AgentInstalling{BytesReceived: job.received.Load(), BytesTotal: job.total.Load()}
	case sameVersion && job.path != "" && isBundleFile(bundle, job.path):
		// An archive may have matched by basename and its stored path may not be available.
		return useBinary(agent, job.path), nil
	case current || (!install && installed != ""):
		return useBinary(agent, installed), nil
	case !install:
		return agent, ErrBinaryInstallRequired
	case ctx.Err() != nil:
		return agent, ctx.Err()
	case i.binaryCtx.Err() != nil:
		return agent, i.binaryCtx.Err()
	case running:
		// The installed record is shared across versions.
		return agent, &AgentInstalling{}
	case sameVersion && time.Now().Before(job.retryAt):
		if installed != "" {
			return useBinary(agent, installed), nil
		}
		return agent, job.err
	}

	job = &binaryJob{version: registry.Version}
	i.binaryJobs[agent.ID] = job
	i.binaryWG.Add(1)
	go i.runBinaryJob(agent, target, archiveName, job)
	return agent, &AgentInstalling{}
}

func (i *Installer) runBinaryJob(agent catalog.Agent, target, archiveName string, job *binaryJob) {
	defer i.binaryWG.Done()
	path, err := i.installBinary(i.binaryCtx, agent, target, archiveName, job)
	i.mu.Lock()
	defer i.mu.Unlock()
	if err == nil {
		job.path = path
		return
	}
	// Polling clients should see the failure before another attempt starts.
	job.err = err
	job.retryAt = time.Now().Add(binaryInstallRetryDelay)
}

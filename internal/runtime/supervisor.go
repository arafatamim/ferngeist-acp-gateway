// Package runtime launches gateway-managed agent processes and keeps a
// diagnostic record for each one: its identity and status, a bounded in-memory
// log ring, and persisted failure records. A launched process is owned by its
// caller (the session layer), which drains its stdout, writes its stdin, and
// reports the exit back with RecordExit. The supervisor never restarts a
// process.
package runtime

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/acquire"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/catalog"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

// stoppedRuntimeRetention keeps recently stopped runtimes visible long enough
// for clients and diagnostics to confirm that a stop completed successfully.
const stoppedRuntimeRetention = 10 * time.Minute

var (
	// ErrRuntimeNotFound is returned when a requested runtime ID does not exist
	// in the supervisor's registry or has been pruned.
	ErrRuntimeNotFound = errors.New("runtime not found")

	// ErrAgentNotDetected is returned when the agent binary cannot be located
	// on the current host and automatic installation is not available.
	ErrAgentNotDetected = errors.New("agent is not detected on this host")

	// ErrRemoteStartNotAllowed is returned when the agent's security policy
	// explicitly forbids the gateway from launching it remotely.
	ErrRemoteStartNotAllowed = errors.New("agent does not allow gateway-managed remote start")

	// ErrUnsupportedLaunch is returned when the launch mode (e.g., container, VM)
	// is not yet implemented by the supervisor.
	ErrUnsupportedLaunch = errors.New("agent launch mode is not supported yet")

	// ErrExecutableNotFound is returned when the agent binary specified in the
	// launch configuration cannot be found on disk or in PATH.
	ErrExecutableNotFound = errors.New("agent executable not found")

	// ErrRuntimeNotRunning is returned when attempting to operate on a runtime
	// that is not in the "running" state.
	ErrRuntimeNotRunning = errors.New("runtime is not running")
)

// Status constants represent the possible lifecycle states of a runtime.
const (
	// StatusStarting indicates the runtime process has been launched but has not
	// yet been recorded as running.
	StatusStarting = "starting"

	// StatusRunning indicates the runtime is active and ready to accept connections.
	StatusRunning = "running"

	// StatusStopped indicates the runtime has terminated successfully or was
	// intentionally stopped. This state is retained for pruning.
	StatusStopped = "stopped"

	// StatusFailed indicates the runtime terminated unexpectedly.
	StatusFailed = "failed"
)

// Runtime is the gateway-owned view of a launched agent process plus the
// metadata needed to hand an ACP session back to Ferngeist.
type Runtime struct {
	ID            string    `json:"id"`
	AgentID       string    `json:"agentId"`
	AgentName     string    `json:"agentName"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"createdAt"`
	PID           int       `json:"pid"`
	Command       string    `json:"command"`
	LastError     string    `json:"lastError,omitempty"`
	Detected      bool      `json:"detected"`
	LaunchMode    string    `json:"launchMode"`
	LastFailureAt time.Time `json:"lastFailureAt,omitempty"`
	// StoppedAt marks when a runtime entered the terminal stopped state so the
	// supervisor can prune it after a short retention window.
	StoppedAt time.Time `json:"stoppedAt,omitempty"`
}

// LogEntry represents a single line of output from a runtime process,
// tagged with a timestamp and stream type (stdout, stderr, or gateway).
type LogEntry struct {
	// Timestamp is when this log entry was captured in UTC.
	Timestamp time.Time `json:"timestamp"`

	// Stream identifies the source of the log line (e.g., "stdout", "stderr", "gateway").
	Stream string `json:"stream"`

	// Message is the actual log line content.
	Message string `json:"message"`
}

// Summary provides an aggregate view of all runtimes managed by the supervisor,
// including counts by status and recent failure details for diagnostics.
type Summary struct {
	// Total is the total number of runtimes currently tracked.
	Total int `json:"total"`

	// Starting is the count of runtimes that are launching but not yet ready.
	Starting int `json:"starting"`

	// Running is the count of runtimes actively serving ACP sessions.
	Running int `json:"running"`

	// Stopped is the count of intentionally terminated runtimes still in retention.
	Stopped int `json:"stopped"`

	// Failed is the count of runtimes that crashed or failed to start.
	Failed int `json:"failed"`

	// RecentFailures contains the most recent failure summaries, sorted by time.
	RecentFailures []FailureSummary `json:"recentFailures"`
}

// FailureSummary captures diagnostic information about a runtime failure,
// including the error context and recent log lines for troubleshooting.
type FailureSummary struct {
	// RuntimeID is the unique identifier of the failed runtime.
	RuntimeID string `json:"runtimeId"`

	// AgentID identifies which agent failed.
	AgentID string `json:"agentId"`

	// AgentName is the human-readable agent display name.
	AgentName string `json:"agentName"`

	// LastError contains the error message from the process exit or startup failure.
	LastError string `json:"lastError"`

	// CreatedAt is when the runtime was originally launched.
	CreatedAt time.Time `json:"createdAt"`

	// FailedAt is when the failure occurred, if available.
	FailedAt time.Time `json:"failedAt,omitempty"`

	// RecentLogLines contains the last few log entries before the failure for debugging.
	RecentLogLines []LogEntry `json:"recentLogLines"`

	// CleanExit marks a summary built from a clean (exit code 0) process exit
	// record rather than a crash. Such records are persisted for diagnostics
	// but excluded from the RecentFailures view.
	CleanExit bool `json:"cleanExit,omitempty"`
}

// ConnectDescriptor contains the connection parameters needed to establish
// an ACP session with a running runtime.
type ConnectDescriptor struct {
	// RuntimeID is the runtime this descriptor connects to.
	RuntimeID string `json:"runtimeId"`

	// AgentID identifies which agent the runtime is running.
	AgentID string `json:"agentId"`

	// Protocol is the connection protocol (currently "acp").
	Protocol string `json:"protocol"`

	// WebSocketPath is the URL path for WebSocket connections.
	WebSocketPath string `json:"websocketPath"`
}

// Process is a launched agent process. The caller that launched it owns it: it
// drains Stdout, writes Stdin, and calls Supervisor.RecordExit once Done is
// closed. Stop is the graceful-termination path used on session close.
type Process struct {
	// Runtime is the diagnostic record for this process.
	Runtime Runtime

	// Cmd is the underlying OS process.
	Cmd *exec.Cmd

	// Stdin is the process's standard input pipe for ACP communication.
	Stdin io.WriteCloser

	// Stdout is the process's standard output pipe for ACP communication.
	Stdout io.ReadCloser

	done chan struct{}
	err  error
	// logsDone is closed once the stderr capture has drained, and launchKey
	// is what the process was started as (see spareKey).
	logsDone  chan struct{}
	launchKey string
}

// Done is closed when the process has exited.
func (p *Process) Done() <-chan struct{} { return p.done }

// Err returns the error from waiting on the process. It is valid only after
// Done is closed (a nil error means a clean exit).
func (p *Process) Err() error { return p.err }

// Stop terminates the process, waiting up to timeout for a graceful exit before
// escalating to a kill. It never calls Wait itself — the owner's wait goroutine
// closes Done.
func (p *Process) Stop(timeout time.Duration) error {
	if p == nil || p.Cmd == nil || p.Cmd.Process == nil {
		return nil
	}
	select {
	case <-p.done:
		return nil
	default:
	}

	// Stage 1: close stdin to signal EOF to the process.
	if p.Stdin != nil {
		_ = p.Stdin.Close()
	}

	// Stage 2: send SIGINT for a graceful shutdown, escalating immediately when
	// the platform cannot deliver it (e.g. Windows).
	if err := p.Cmd.Process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
		if killErr := p.Cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			return killErr
		}
	}

	// Stage 3: wait for the graceful exit or the timeout.
	select {
	case <-p.done:
		return nil
	case <-time.After(timeout):
	}

	// Stage 4: force kill and wait briefly for it to take effect.
	if err := p.Cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	select {
	case <-p.done:
		return nil
	case <-time.After(250 * time.Millisecond):
		return context.DeadlineExceeded
	}
}

// Supervisor owns the launch of gateway-managed agent processes and the
// diagnostic state that outlives them: a runtime record per launch, a bounded
// log ring, and persisted failure records. All state is guarded by one mutex.
type Supervisor struct {
	// logger is the structured logger with component context pre-applied.
	logger *slog.Logger

	// mu protects all mutable state in the supervisor.
	mu sync.Mutex

	// now is a time provider function for testability.
	now func() time.Time

	// runtimes maps runtime ID to its current state and metadata.
	runtimes map[string]Runtime

	// logs stores bounded in-memory log buffers per runtime.
	logs map[string][]LogEntry

	// baseDir is the root directory for resolving relative agent paths.
	baseDir string

	// store is the optional SQLite persistence layer for runtime state.
	store *storage.SQLiteStore

	// installer handles automatic agent acquisition if not present.
	installer *acquire.Installer

	// spares holds one pre-booted process per agent ID; nil until
	// EnableSpares. closed stops new spares after Close.
	spares   map[string]*spare
	sparesWG sync.WaitGroup
	closed   bool
}

// NewSupervisor creates a supervisor rooted at baseDir with optional SQLite
// persistence and an optional installer for automatic agent acquisition.
func NewSupervisor(logger *slog.Logger, baseDir string, store *storage.SQLiteStore, installer *acquire.Installer) *Supervisor {
	return &Supervisor{
		logger:    logger.With("component", "runtime"),
		now:       time.Now,
		runtimes:  make(map[string]Runtime),
		logs:      make(map[string][]LogEntry),
		baseDir:   baseDir,
		store:     store,
		installer: installer,
	}
}

// Launch validates the agent, resolves its executable, starts a fresh process,
// and records it as running. Every call produces a distinct runtime; the caller
// owns the returned process. envOverrides are merged over the daemon
// environment.
func (s *Supervisor) Launch(agent catalog.Agent, envOverrides map[string]string) (*Process, error) {
	prepared, err := s.prepareAgent(agent)
	if err != nil {
		return nil, err
	}
	// A spare runs on the daemon environment, so env overrides launch cold.
	if len(envOverrides) > 0 {
		return s.launchProcess(prepared, envOverrides)
	}
	proc := s.claimSpare(prepared)
	if proc == nil {
		if proc, err = s.launchProcess(prepared, nil); err != nil {
			return nil, err
		}
	}
	s.refillSpare(prepared)
	return proc, nil
}

// Spawn starts a fresh process of agent on the daemon environment that is never
// tracked as a runtime and never claims or refills a spare, for short-lived
// callers outside the session lifecycle. The caller stops it with Discard.
func (s *Supervisor) Spawn(agent catalog.Agent) (*Process, error) {
	prepared, err := s.prepareAgent(agent)
	if err != nil {
		return nil, err
	}
	return s.spawn(prepared, nil)
}

// prepareAgent applies auto-acquisition and validates launch prerequisites,
// returning the agent ready to launch.
func (s *Supervisor) prepareAgent(agent catalog.Agent) (catalog.Agent, error) {
	// Ensure agent is available - try auto-install if installer is configured
	if !agent.Detected {
		if s.installer != nil {
			acquiredAgent, _, err := s.installer.Ensure(context.Background(), agent)
			if err != nil {
				return catalog.Agent{}, err
			}
			agent = acquiredAgent
		}
	}
	if s.installer != nil {
		agent = s.installer.EnsureNpm(agent)
	}

	// Validate all prerequisites before attempting launch
	if !agent.Detected {
		return catalog.Agent{}, ErrAgentNotDetected
	}
	if !agent.Security.AllowsRemoteStart {
		return catalog.Agent{}, ErrRemoteStartNotAllowed
	}
	if agent.Launch.Mode != "process" && agent.Launch.Mode != "external" {
		return catalog.Agent{}, ErrUnsupportedLaunch
	}
	return agent, nil
}

// launchProcess is the core transition from a validated catalog agent to a
// tracked, running process.
func (s *Supervisor) launchProcess(agent catalog.Agent, envOverrides map[string]string) (*Process, error) {
	proc, err := s.spawn(agent, envOverrides)
	if err != nil {
		return nil, err
	}
	s.register(proc)
	return proc, nil
}

// spawn starts the agent's process without tracking it as a runtime, so a
// spare stays out of listings until register.
func (s *Supervisor) spawn(agent catalog.Agent, envOverrides map[string]string) (*Process, error) {
	commandPath, workingDir, err := s.resolveLaunch(agent.Launch)
	if err != nil {
		return nil, err
	}

	runtimeInfo := Runtime{
		ID:         randomToken(18),
		AgentID:    agent.ID,
		AgentName:  agent.DisplayName,
		Status:     StatusStarting,
		Command:    commandPath,
		Detected:   agent.Detected,
		LaunchMode: agent.Launch.Mode,
	}

	cmd := exec.Command(commandPath, agent.Launch.Args...)
	cmd.Dir = workingDir
	cmd.Env = mergeProcessEnv(envOverrides)
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	runtimeInfo.PID = cmd.Process.Pid

	proc := &Process{
		Runtime:   runtimeInfo,
		Cmd:       cmd,
		Stdin:     stdinPipe,
		Stdout:    stdoutPipe,
		done:      make(chan struct{}),
		logsDone:  make(chan struct{}),
		launchKey: spareKey(commandPath, workingDir, agent.Launch.Args),
	}
	// The wait goroutine reaps the process and signals Done. The owner calls
	// RecordExit when Done fires; nothing else waits on the process.
	go func() {
		proc.err = cmd.Wait()
		close(proc.done)
	}()
	go func() {
		defer close(proc.logsDone)
		s.captureLogs(runtimeInfo.ID, "stderr", stderrPipe, os.Stderr)
	}()
	return proc, nil
}

// register records a spawned process as a running runtime.
func (s *Supervisor) register(proc *Process) {
	runtimeInfo := proc.Runtime
	runtimeInfo.Status = StatusStarting
	runtimeInfo.CreatedAt = s.now().UTC()
	s.mu.Lock()
	s.runtimes[runtimeInfo.ID] = runtimeInfo
	s.mu.Unlock()
	s.persistRuntime(runtimeInfo)

	s.mu.Lock()
	runtimeInfo = s.runtimes[runtimeInfo.ID]
	runtimeInfo.Status = StatusRunning
	s.runtimes[runtimeInfo.ID] = runtimeInfo
	s.mu.Unlock()
	s.persistRuntime(runtimeInfo)
	proc.Runtime = runtimeInfo
}

// RecordExit normalizes a process exit into the runtime record: an intentional
// stop lands in StatusStopped, a clean exit is recorded and persisted for
// diagnostics, and an unexpected exit lands in StatusFailed with a persisted
// failure record. The process's owner calls this exactly once per launch.
func (s *Supervisor) RecordExit(runtimeID string, waitErr error, intentional bool) {
	s.mu.Lock()
	runtimeInfo, ok := s.runtimes[runtimeID]
	if !ok {
		s.mu.Unlock()
		return
	}

	switch {
	case intentional:
		runtimeInfo.Status = StatusStopped
		runtimeInfo.PID = 0
		runtimeInfo.StoppedAt = s.now().UTC()
		s.runtimes[runtimeID] = runtimeInfo
		s.mu.Unlock()
		s.persistRuntime(runtimeInfo)

	case waitErr != nil:
		runtimeInfo.Status = StatusFailed
		runtimeInfo.LastError = waitErr.Error()
		runtimeInfo.PID = 0
		runtimeInfo.StoppedAt = time.Time{}
		runtimeInfo.LastFailureAt = s.now().UTC()
		s.runtimes[runtimeID] = runtimeInfo
		s.mu.Unlock()
		s.persistRuntime(runtimeInfo)
		s.logger.Error("agent process exited unexpectedly",
			"runtime_id", runtimeID, "agent_id", runtimeInfo.AgentID,
			"error", waitErr.Error())
		s.appendLog(runtimeID, LogEntry{
			Timestamp: s.now().UTC(),
			Stream:    "gateway",
			Message:   fmt.Sprintf("agent process exited unexpectedly: %s", waitErr.Error()),
		})
		s.persistFailure(runtimeID, runtimeInfo, s.recentLogs(runtimeID, 5), runtimeInfo.LastFailureAt)

	default:
		runtimeInfo.Status = StatusStopped
		runtimeInfo.PID = 0
		runtimeInfo.StoppedAt = s.now().UTC()
		s.runtimes[runtimeID] = runtimeInfo
		s.mu.Unlock()
		s.persistRuntime(runtimeInfo)
		s.appendLog(runtimeID, LogEntry{
			Timestamp: s.now().UTC(),
			Stream:    "gateway",
			Message:   "agent process exited cleanly (exit code 0)",
		})
		s.persistCleanExit(runtimeID, runtimeInfo, s.recentLogs(runtimeID, 5), runtimeInfo.StoppedAt)
	}
}

// List returns all runtimes currently managed by the supervisor, sorted by
// creation time in descending order (newest first). Stopped runtimes outside
// the retention window are pruned before returning.
func (s *Supervisor) List() []Runtime {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneStoppedRuntimesLocked(s.now().UTC())

	out := make([]Runtime, 0, len(s.runtimes))
	for _, runtimeInfo := range s.runtimes {
		out = append(out, runtimeInfo)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// Get returns the current record for a runtime.
//
// Returns ErrRuntimeNotFound if the runtime is not tracked.
func (s *Supervisor) Get(runtimeID string) (Runtime, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneStoppedRuntimesLocked(s.now().UTC())

	runtimeInfo, ok := s.runtimes[runtimeID]
	if !ok {
		return Runtime{}, ErrRuntimeNotFound
	}
	return runtimeInfo, nil
}

// Connect builds a connection descriptor for a running runtime. The descriptor
// contains everything needed to establish an ACP session; the WebSocket
// endpoint authenticates with the client's attach token.
//
// Returns ErrRuntimeNotFound if the runtime doesn't exist or ErrRuntimeNotRunning
// if it's not in running state.
func (s *Supervisor) Connect(runtimeID string) (ConnectDescriptor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneStoppedRuntimesLocked(s.now().UTC())

	runtimeInfo, ok := s.runtimes[runtimeID]
	if !ok {
		return ConnectDescriptor{}, ErrRuntimeNotFound
	}
	if runtimeInfo.Status != StatusRunning {
		return ConnectDescriptor{}, ErrRuntimeNotRunning
	}

	return ConnectDescriptor{
		RuntimeID:     runtimeInfo.ID,
		AgentID:       runtimeInfo.AgentID,
		Protocol:      "acp",
		WebSocketPath: fmt.Sprintf("/v1/acp/%s", runtimeInfo.ID),
	}, nil
}

// Logs retrieves the buffered log entries for a specific runtime. The buffer
// is bounded to maxEntries (200) and contains the most recent entries.
//
// Returns nil slice if the runtime exists but has no logs, or
// ErrRuntimeNotFound if the runtime is not tracked.
func (s *Supervisor) Logs(runtimeID string) ([]LogEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneStoppedRuntimesLocked(s.now().UTC())

	entries, ok := s.logs[runtimeID]
	if !ok {
		if _, exists := s.runtimes[runtimeID]; exists {
			return nil, nil
		}
		return nil, ErrRuntimeNotFound
	}

	out := make([]LogEntry, len(entries))
	copy(out, entries)
	return out, nil
}

// AppendLog records gateway-observed ACP traffic or lifecycle messages using the
// same bounded in-memory buffer returned by the runtime log endpoints.
func (s *Supervisor) AppendLog(runtimeID, stream, message string) {
	if strings.TrimSpace(runtimeID) == "" {
		return
	}
	s.appendLog(runtimeID, LogEntry{
		Timestamp: s.now().UTC(),
		Stream:    stream,
		Message:   message,
	})
}

// Summary aggregates the current state of all runtimes into a summary view.
// It includes counts by status and the most recent failures (up to 5) with
// diagnostic context. Failures are sourced from both in-memory state and
// persisted records if a store is configured.
func (s *Supervisor) Summary() Summary {
	// Snapshot under lock, SQLite after unlock: holding s.mu across
	// ListRecentRuntimeFailures serializes every per-frame appendLog behind
	// disk I/O. Prune + copy here, merge persisted rows below lock-free.
	s.mu.Lock()
	s.pruneStoppedRuntimesLocked(s.now().UTC())

	summary := Summary{}
	// Use a map to deduplicate failures by runtime ID
	failuresByRuntimeID := make(map[string]FailureSummary)

	for runtimeID, runtimeInfo := range s.runtimes {
		summary.Total++
		switch runtimeInfo.Status {
		case StatusStarting:
			summary.Starting++
		case StatusRunning:
			summary.Running++
		case StatusFailed:
			summary.Failed++
			failuresByRuntimeID[runtimeID] = FailureSummary{
				RuntimeID:      runtimeID,
				AgentID:        runtimeInfo.AgentID,
				AgentName:      runtimeInfo.AgentName,
				LastError:      runtimeInfo.LastError,
				CreatedAt:      runtimeInfo.CreatedAt,
				FailedAt:       runtimeInfo.CreatedAt,
				RecentLogLines: lastLogEntries(s.logs[runtimeID], 5),
			}
		default:
			// Stopped (or any unknown state) is a terminal, non-failure state.
			summary.Stopped++
		}
	}
	store := s.store
	s.mu.Unlock()

	// Merge persisted failures from SQLite store (for runtimes that were pruned)
	if store != nil {
		persistedFailures, err := store.ListRecentRuntimeFailures(context.Background(), 5)
		if err != nil {
			s.logger.Warn("load runtime failures failed", "error", err)
		} else {
			for _, record := range persistedFailures {
				// Clean exits are diagnostics, not failures: they must not
				// surface in the failure view.
				if record.CleanExit {
					continue
				}
				// Skip if we already have a more recent in-memory failure
				if _, exists := failuresByRuntimeID[record.RuntimeID]; exists {
					continue
				}
				failuresByRuntimeID[record.RuntimeID] = failureSummaryFromRecord(record)
			}
		}
	}

	// Sort failures by time (most recent first) and take top 5
	failures := make([]FailureSummary, 0, len(failuresByRuntimeID))
	for _, failure := range failuresByRuntimeID {
		failures = append(failures, failure)
	}
	sort.Slice(failures, func(i, j int) bool {
		return failureSortTime(failures[i]).After(failureSortTime(failures[j]))
	})
	if len(failures) > 5 {
		failures = failures[:5]
	}
	summary.RecentFailures = failures
	return summary
}

// resolveCommandPath converts a command string to an absolute path.
// If the command is already absolute, it's returned as-is. Otherwise,
// it's resolved relative to the supervisor's base directory and made absolute.
func (s *Supervisor) resolveCommandPath(command string) string {
	if filepath.IsAbs(command) {
		return command
	}
	resolved := filepath.Join(s.baseDir, command)
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return resolved
	}
	return abs
}

// resolveLaunch determines the executable path and working directory for an
// agent based on its launch configuration. For "process" mode, it resolves
// the command relative to the base directory. For "external" mode, it searches
// the system PATH.
//
// Returns the absolute command path, working directory, and any error.
func (s *Supervisor) resolveLaunch(launch catalog.LaunchConfig) (string, string, error) {
	switch launch.Mode {
	case "process":
		commandPath := s.resolveCommandPath(launch.Command)
		if _, err := os.Stat(commandPath); err != nil {
			return "", "", fmt.Errorf("%w: %s", ErrExecutableNotFound, commandPath)
		}
		return commandPath, filepath.Dir(commandPath), nil
	case "external":
		commandPath, err := exec.LookPath(launch.Command)
		if err != nil {
			return "", "", fmt.Errorf("%w: %s", ErrExecutableNotFound, launch.Command)
		}
		workingDir, dirErr := os.Getwd()
		if dirErr != nil {
			workingDir = "."
		}
		return commandPath, workingDir, nil
	default:
		return "", "", ErrUnsupportedLaunch
	}
}

// pruneStoppedRuntimesLocked removes terminal runtimes after the retention
// window so live runtime listings do not grow without bound. Both clean stops
// and failures are pruned: failure diagnostics survive in the persisted store
// (merged into Summary), so there is no need to keep them in memory forever.
// Failed entries are aged by LastFailureAt (falling back to CreatedAt when the
// failure timestamp is missing); each eviction also drops its log ring.
func (s *Supervisor) pruneStoppedRuntimesLocked(now time.Time) {
	cutoff := now.Add(-stoppedRuntimeRetention)
	for runtimeID, runtimeInfo := range s.runtimes {
		switch runtimeInfo.Status {
		case StatusStopped:
			if runtimeInfo.StoppedAt.IsZero() || runtimeInfo.StoppedAt.After(cutoff) {
				continue
			}
		case StatusFailed:
			failedAt := runtimeInfo.LastFailureAt
			if failedAt.IsZero() {
				failedAt = runtimeInfo.CreatedAt
			}
			if failedAt.IsZero() || failedAt.After(cutoff) {
				continue
			}
		default:
			continue
		}
		delete(s.runtimes, runtimeID)
		delete(s.logs, runtimeID)
	}
}

// captureLogs copies process output into the in-memory ring buffer while also
// mirroring it to the optional sink for local developer visibility.
func (s *Supervisor) captureLogs(runtimeID, stream string, source io.Reader, sink io.Writer) {
	scanner := bufio.NewScanner(source)
	buffer := make([]byte, 0, 64*1024)
	scanner.Buffer(buffer, 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if sink != nil {
			_, _ = fmt.Fprintln(sink, line)
		}
		s.appendLog(runtimeID, LogEntry{
			Timestamp: s.now().UTC(),
			Stream:    stream,
			Message:   line,
		})
	}
	if err := scanner.Err(); err != nil {
		s.appendLog(runtimeID, LogEntry{
			Timestamp: s.now().UTC(),
			Stream:    stream,
			Message:   "log stream error: " + err.Error(),
		})
	}
}

func (s *Supervisor) appendLog(runtimeID string, entry LogEntry) {
	// Byte-cap: a single 64MiB agent frame must not land verbatim in the
	// ring (200 x 64MiB = 12GiB/runtime pathological). Truncate to 8KiB;
	// full frames remain in the frame log / SQLite audit path.
	const maxLogMessageBytes = 8 * 1024
	if len(entry.Message) > maxLogMessageBytes {
		entry.Message = entry.Message[:maxLogMessageBytes] + "…[truncated]"
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Maintain a bounded ring buffer of 200 entries per runtime
	// to prevent unbounded memory growth
	const maxEntries = 200
	entries := append(s.logs[runtimeID], entry)
	if len(entries) > maxEntries {
		// Drop oldest entries when buffer exceeds limit
		entries = entries[len(entries)-maxEntries:]
	}
	s.logs[runtimeID] = entries
}

// recentLogs returns a copy of the last `limit` log entries for a runtime,
// taking s.mu so it is safe to call from paths that have already released the
// lock. lastLogEntries copies the tail, so the returned slice never aliases the
// live buffer.
func (s *Supervisor) recentLogs(runtimeID string, limit int) []LogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return lastLogEntries(s.logs[runtimeID], limit)
}

// mergeProcessEnv merges environment variable overrides with the current
// process environment. Base environment variables are loaded first, then
// overrides are applied on top. The result is sorted alphabetically.
func mergeProcessEnv(overrides map[string]string) []string {
	// Start with the current process environment
	base := os.Environ()
	if len(overrides) == 0 {
		return base
	}

	// Parse base env into a map for easy merging
	merged := make(map[string]string, len(base)+len(overrides))
	for _, entry := range base {
		if key, value, ok := strings.Cut(entry, "="); ok {
			merged[key] = value
		}
	}

	// Apply overrides (they take precedence)
	for key, value := range overrides {
		merged[key] = value
	}

	// Convert back to KEY=VALUE slice and sort for deterministic ordering
	out := make([]string, 0, len(merged))
	for key, value := range merged {
		out = append(out, key+"="+value)
	}
	sort.Strings(out)
	return out
}

// lastLogEntries returns the last N entries from a log buffer, or the entire
// buffer if it contains fewer than limit entries. Returns nil for invalid limits.
func lastLogEntries(entries []LogEntry, limit int) []LogEntry {
	if limit <= 0 || len(entries) == 0 {
		return nil
	}
	// Extract the tail of the log buffer
	if len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	// Return a copy to prevent holding references to the full buffer
	out := make([]LogEntry, len(entries))
	copy(out, entries)
	return out
}

// failureSortTime returns the appropriate timestamp for sorting failures.
// It prefers FailedAt if available, otherwise falls back to CreatedAt.
func failureSortTime(failure FailureSummary) time.Time {
	if !failure.FailedAt.IsZero() {
		return failure.FailedAt
	}
	return failure.CreatedAt
}

// failureSummaryFromRecord converts a persisted SQLite failure record into
// a FailureSummary struct. It attempts to decode the JSON log preview,
// falling back to an error message if deserialization fails.
func failureSummaryFromRecord(record storage.RuntimeFailureRecord) FailureSummary {
	var logLines []LogEntry
	if strings.TrimSpace(record.LogPreview) != "" {
		// Attempt to deserialize the persisted log preview
		if err := json.Unmarshal([]byte(record.LogPreview), &logLines); err != nil {
			// Provide a sentinel error if deserialization fails
			logLines = []LogEntry{{
				Timestamp: record.FailedAt,
				Stream:    "gateway",
				Message:   "failed to decode persisted log preview",
			}}
		}
	}

	return FailureSummary{
		RuntimeID:      record.RuntimeID,
		AgentID:        record.AgentID,
		AgentName:      record.AgentName,
		LastError:      record.LastError,
		CreatedAt:      record.CreatedAt,
		FailedAt:       record.FailedAt,
		RecentLogLines: logLines,
		CleanExit:      record.CleanExit,
	}
}

// persistFailure stores a small failure summary instead of raw transcript or
// full logs. That keeps the gateway useful for debugging without turning SQLite
// into a session store.
func (s *Supervisor) persistFailure(runtimeID string, runtimeInfo Runtime, logLines []LogEntry, failedAt time.Time) {
	if s.store == nil {
		return
	}

	// Serialize log preview for storage
	logPreview, err := json.Marshal(logLines)
	if err != nil {
		s.logger.Warn("serialize runtime failure log preview failed", "runtime_id", runtimeID, "error", err)
		return
	}

	// Store failure record in SQLite (best-effort)
	if err := s.store.SaveRuntimeFailure(context.Background(), storage.RuntimeFailureRecord{
		RuntimeID:  runtimeID,
		AgentID:    runtimeInfo.AgentID,
		AgentName:  runtimeInfo.AgentName,
		LastError:  runtimeInfo.LastError,
		CreatedAt:  runtimeInfo.CreatedAt,
		FailedAt:   failedAt,
		LogPreview: string(logPreview),
	}); err != nil {
		s.logger.Error("persist runtime failure failed", "runtime_id", runtimeID, "error", err)
	}
}

// persistCleanExit records a clean (exit code 0) process exit so every agent
// death leaves a diagnostic trace, not just crashes. The record is stored in
// the same table as failures but flagged with CleanExit so the failure view
// can filter it out. "Clean" here means the supervisor saw no wait error —
// the process may still have been killed externally in a way Windows reports
// as a normal exit.
func (s *Supervisor) persistCleanExit(runtimeID string, runtimeInfo Runtime, logLines []LogEntry, exitedAt time.Time) {
	if s.store == nil {
		return
	}

	logPreview, err := json.Marshal(logLines)
	if err != nil {
		s.logger.Warn("serialize runtime clean exit log preview failed", "runtime_id", runtimeID, "error", err)
		return
	}

	if err := s.store.SaveRuntimeFailure(context.Background(), storage.RuntimeFailureRecord{
		RuntimeID:  runtimeID,
		AgentID:    runtimeInfo.AgentID,
		AgentName:  runtimeInfo.AgentName,
		LastError:  "exit status 0 (clean)",
		CreatedAt:  runtimeInfo.CreatedAt,
		FailedAt:   exitedAt,
		LogPreview: string(logPreview),
		CleanExit:  true,
	}); err != nil {
		s.logger.Error("persist runtime clean exit failed", "runtime_id", runtimeID, "error", err)
	}
}

// randomToken generates a URL-safe base64-encoded random token of the specified
// byte length. It panics if the system random source fails, as this indicates
// a critical system issue.
func randomToken(byteLen int) string {
	buf := make([]byte, byteLen)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Errorf("runtime token generation failed: %w", err))
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// persistRuntime saves the current runtime state to the SQLite store if one
// is configured. Errors are logged but not propagated to callers since
// persistence is best-effort for diagnostics.
func (s *Supervisor) persistRuntime(runtimeInfo Runtime) {
	if s.store == nil {
		return
	}

	if err := s.store.SaveRuntime(context.Background(), storage.RuntimeRecord{
		RuntimeID: runtimeInfo.ID,
		AgentID:   runtimeInfo.AgentID,
		AgentName: runtimeInfo.AgentName,
		Status:    runtimeInfo.Status,
		Command:   runtimeInfo.Command,
		PID:       runtimeInfo.PID,
		LastError: runtimeInfo.LastError,
		CreatedAt: runtimeInfo.CreatedAt,
	}); err != nil {
		s.logger.Error("persist runtime failed", "runtime_id", runtimeInfo.ID, "error", err)
	}
}

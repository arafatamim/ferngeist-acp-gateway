// Package runtime contains unit and integration tests for the runtime
// supervisor. The supervisor launches agent processes, owns their diagnostic
// log ring, and records their exits; these tests cover launching, connect
// descriptors, exit recording, log capture, summary reporting, pruning, and
// persistence via the storage backend. Most tests use a compiled mock stdio
// agent launched as a subprocess.
package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/acquire"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/catalog"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

// --- Launch / Connect lifecycle ---

func mockAgent() catalog.Agent {
	return catalog.Agent{
		ID:          "mock-acp",
		DisplayName: "Mock ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:    "process",
			Command: filepath.Join("bin", mockAgentBinaryName()),
		},
	}
}

// launchMock launches the mock agent and registers cleanup that stops it.
func launchMock(t *testing.T, supervisor *Supervisor) *Process {
	t.Helper()
	proc, err := supervisor.Launch(mockAgent(), nil)
	if err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	t.Cleanup(func() { _ = proc.Stop(2 * time.Second) })
	return proc
}

func TestLaunchRecordsRunningRuntime(t *testing.T) {
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil, nil)
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	proc := launchMock(t, supervisor)
	if proc.Runtime.Status != StatusRunning {
		t.Fatalf("Status = %q, want %q", proc.Runtime.Status, StatusRunning)
	}
	if proc.Runtime.PID == 0 {
		t.Fatal("PID = 0, want the launched process pid")
	}

	runtimes := supervisor.List()
	if len(runtimes) != 1 || runtimes[0].ID != proc.Runtime.ID {
		t.Fatalf("List() = %+v, want the launched runtime", runtimes)
	}
	if got, err := supervisor.Get(proc.Runtime.ID); err != nil || got.ID != proc.Runtime.ID {
		t.Fatalf("Get() = %+v, %v; want the launched runtime", got, err)
	}
}

func TestLaunchProducesDistinctRuntimes(t *testing.T) {
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil, nil)
	first := launchMock(t, supervisor)
	second := launchMock(t, supervisor)

	if first.Runtime.ID == second.Runtime.ID {
		t.Fatalf("Launch must produce distinct runtimes, both = %q", first.Runtime.ID)
	}
	if got := len(supervisor.List()); got != 2 {
		t.Fatalf("len(List()) = %d, want 2", got)
	}
}

func TestConnectReturnsDescriptorForRunningRuntime(t *testing.T) {
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil, nil)
	proc := launchMock(t, supervisor)

	descriptor, err := supervisor.Connect(proc.Runtime.ID)
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	if descriptor.Protocol != "acp" {
		t.Fatalf("Protocol = %q, want %q", descriptor.Protocol, "acp")
	}
	if descriptor.WebSocketPath != "/v1/acp/"+proc.Runtime.ID {
		t.Fatalf("WebSocketPath = %q", descriptor.WebSocketPath)
	}
	if descriptor.AgentID != "mock-acp" {
		t.Fatalf("AgentID = %q, want %q", descriptor.AgentID, "mock-acp")
	}
}

func TestRecordExitCleanMarksStopped(t *testing.T) {
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), ".", nil, nil)
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	supervisor.now = func() time.Time { return now }
	supervisor.runtimes["run-1"] = Runtime{
		ID: "run-1", AgentID: "mock-acp", AgentName: "Mock", Status: StatusRunning, PID: 99, CreatedAt: now,
	}

	supervisor.RecordExit("run-1", nil, false)

	got := supervisor.runtimes["run-1"]
	if got.Status != StatusStopped {
		t.Fatalf("Status = %q, want %q", got.Status, StatusStopped)
	}
	if got.PID != 0 {
		t.Fatalf("PID = %d, want 0", got.PID)
	}
	if got.StoppedAt.IsZero() {
		t.Fatal("StoppedAt is zero, want the exit time")
	}
}

func TestRecordExitCrashMarksFailedAndPersists(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), ".", store, nil)
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	supervisor.now = func() time.Time { return now }
	supervisor.runtimes["run-1"] = Runtime{
		ID: "run-1", AgentID: "mock-acp", AgentName: "Mock", Status: StatusRunning, CreatedAt: now,
	}

	supervisor.RecordExit("run-1", errors.New("exit status 1"), false)

	got := supervisor.runtimes["run-1"]
	if got.Status != StatusFailed {
		t.Fatalf("Status = %q, want %q", got.Status, StatusFailed)
	}
	if got.LastError != "exit status 1" {
		t.Fatalf("LastError = %q", got.LastError)
	}
	failures, err := store.ListRecentRuntimeFailures(context.Background(), 5)
	if err != nil {
		t.Fatalf("ListRecentRuntimeFailures() error = %v", err)
	}
	found := false
	for _, f := range failures {
		if f.RuntimeID == "run-1" && f.LastError == "exit status 1" {
			found = true
		}
	}
	if !found {
		t.Fatal("crash record not persisted")
	}
}

func TestRecordExitIntentionalMarksStoppedWithoutFailure(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), ".", store, nil)
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	supervisor.now = func() time.Time { return now }
	supervisor.runtimes["run-1"] = Runtime{
		ID: "run-1", AgentID: "mock-acp", AgentName: "Mock", Status: StatusRunning, CreatedAt: now,
	}

	// A killed process reports a non-nil wait error; an intentional stop must
	// still land in StatusStopped and persist no failure record.
	supervisor.RecordExit("run-1", errors.New("signal: killed"), true)

	got := supervisor.runtimes["run-1"]
	if got.Status != StatusStopped {
		t.Fatalf("Status = %q, want %q", got.Status, StatusStopped)
	}
	failures, err := store.ListRecentRuntimeFailures(context.Background(), 5)
	if err != nil {
		t.Fatalf("ListRecentRuntimeFailures() error = %v", err)
	}
	if len(failures) != 0 {
		t.Fatalf("len(failures) = %d, want 0 for an intentional exit", len(failures))
	}
}

func TestRecordExitUnknownRuntimeNoop(t *testing.T) {
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), ".", nil, nil)
	supervisor.RecordExit("nope", errors.New("boom"), false)
	if len(supervisor.runtimes) != 0 {
		t.Fatalf("runtimes = %d, want 0", len(supervisor.runtimes))
	}
}

func TestConnectNotRunningAfterExit(t *testing.T) {
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), ".", nil, nil)
	runtimeInfo := Runtime{
		ID: "run-1", AgentID: "mock-acp", AgentName: "Mock", Status: StatusStopped, CreatedAt: time.Now(),
	}
	supervisor.runtimes[runtimeInfo.ID] = runtimeInfo

	if _, err := supervisor.Connect(runtimeInfo.ID); err != ErrRuntimeNotRunning {
		t.Fatalf("Connect() error = %v, want %v", err, ErrRuntimeNotRunning)
	}
}

// --- Input validation ---

func TestLaunchRejectsUndetectedAgent(t *testing.T) {
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), ".", nil, nil)

	_, err := supervisor.Launch(catalog.Agent{
		ID:          "codex-acp",
		DisplayName: "Codex ACP",
		Detected:    false,
		Launch:      catalog.LaunchConfig{Mode: "external"},
	}, nil)
	if err != ErrAgentNotDetected {
		t.Fatalf("Launch() error = %v, want %v", err, ErrAgentNotDetected)
	}
}

func TestLaunchRejectsUnsupportedLaunch(t *testing.T) {
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), ".", nil, nil)

	_, err := supervisor.Launch(catalog.Agent{
		ID:          "container-agent",
		DisplayName: "Container Agent",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch:      catalog.LaunchConfig{Mode: "container"},
	}, nil)
	if err != ErrUnsupportedLaunch {
		t.Fatalf("Launch() error = %v, want %v", err, ErrUnsupportedLaunch)
	}
}

func TestLaunchRejectsRemoteStartDisabled(t *testing.T) {
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), ".", nil, nil)

	_, err := supervisor.Launch(catalog.Agent{
		ID:          "no-remote",
		DisplayName: "No Remote",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: false},
		Launch:      catalog.LaunchConfig{Mode: "process"},
	}, nil)
	if err != ErrRemoteStartNotAllowed {
		t.Fatalf("Launch() error = %v, want %v", err, ErrRemoteStartNotAllowed)
	}
}

func TestLaunchDoesNotPanicWithEmptyAgent(t *testing.T) {
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), ".", nil, nil)

	if _, err := supervisor.Launch(catalog.Agent{}, nil); err == nil {
		t.Fatal("expected error for empty agent")
	}
}

func TestLaunchSupportsExternalStdioRuntime(t *testing.T) {
	baseDir := t.TempDir()
	binDir := filepath.Join(baseDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	buildMockStdioAgent(t, filepath.Join(binDir, namedBinary("codex-acp")))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil, nil)
	proc, err := supervisor.Launch(catalog.Agent{
		ID:          "codex-acp",
		DisplayName: "Codex ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:    "external",
			Command: "codex-acp",
		},
	}, nil)
	if err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	defer func() { _ = proc.Stop(2 * time.Second) }()

	if proc.Runtime.LaunchMode != "external" {
		t.Fatalf("LaunchMode = %q, want %q", proc.Runtime.LaunchMode, "external")
	}

	scanner := bufio.NewScanner(proc.Stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	if _, err := io.WriteString(proc.Stdin, "{\"jsonrpc\":\"2.0\",\"id\":\"1\",\"method\":\"initialize\",\"params\":{\"protocolVersion\":1,\"capabilities\":{},\"clientInfo\":{\"name\":\"test\",\"version\":\"0\"}}}\n"); err != nil {
		t.Fatalf("WriteString() error = %v", err)
	}
	if !scanner.Scan() {
		t.Fatal("expected JSON-RPC response from stdio runtime")
	}
	var resp map[string]json.RawMessage
	if err := json.Unmarshal([]byte(scanner.Text()), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if _, ok := resp["result"]; !ok {
		t.Fatal("expected result field in JSON-RPC response")
	}
}

func TestLaunchAutoAcquiresMissingExternalBinary(t *testing.T) {
	sourceDir := t.TempDir()
	sourcePath := filepath.Join(sourceDir, namedBinary("mock-stdio-agent"))
	buildMockStdioAgent(t, sourcePath)

	server := httptest.NewServer(http.FileServer(http.Dir(sourceDir)))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	installer := acquire.New(logger, t.TempDir(), nil)
	supervisor := NewSupervisor(logger, t.TempDir(), nil, installer)

	proc, err := supervisor.Launch(catalog.Agent{
		ID:          "codex-acp",
		DisplayName: "Codex ACP",
		Detected:    false,
		Protocol:    "acp",
		Security:    catalog.SecurityConfig{CuratedLaunch: true, AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:    "external",
			Command: "codex-acp",
		},
		Registry: catalog.RegistryInfo{
			CurrentBinaryPath:       namedBinary("mock-stdio-agent"),
			CurrentBinaryArchiveURL: server.URL + "/" + namedBinary("mock-stdio-agent"),
		},
	}, nil)
	if err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	defer func() { _ = proc.Stop(2 * time.Second) }()

	if proc.Runtime.Status != StatusRunning {
		t.Fatalf("Status = %q, want %q", proc.Runtime.Status, StatusRunning)
	}
}

// --- Process.Stop ---

func TestProcessStopTerminatesLiveProcess(t *testing.T) {
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil, nil)
	proc, err := supervisor.Launch(mockAgent(), nil)
	if err != nil {
		t.Fatalf("Launch() error = %v", err)
	}

	select {
	case <-proc.Done():
		t.Fatal("process exited before Stop")
	default:
	}

	if err := proc.Stop(2 * time.Second); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	select {
	case <-proc.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done not closed after Stop")
	}
	// Idempotent: stopping an already-exited process succeeds.
	if err := proc.Stop(time.Second); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
}

func TestProcessStopNilSafe(t *testing.T) {
	var proc *Process
	if err := proc.Stop(time.Second); err != nil {
		t.Fatalf("Stop() on nil process = %v, want nil", err)
	}
}

// --- Logs ---

func TestAppendLog(t *testing.T) {
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), ".", nil, nil)
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }
	supervisor.runtimes["rt-1"] = Runtime{ID: "rt-1", AgentID: "mock-acp", Status: StatusRunning, CreatedAt: time.Now()}

	supervisor.AppendLog("rt-1", "gateway", "hello")

	entries, err := supervisor.Logs("rt-1")
	if err != nil {
		t.Fatalf("Logs() error = %v", err)
	}
	if len(entries) != 1 || entries[0].Message != "hello" || entries[0].Stream != "gateway" {
		t.Fatalf("Logs() = %+v", entries)
	}
}

func TestAppendLogEmptyID(t *testing.T) {
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), ".", nil, nil)
	supervisor.AppendLog("", "gateway", "ignored")
	if len(supervisor.logs) != 0 {
		t.Fatalf("logs = %d entries, want 0", len(supervisor.logs))
	}
}

func TestAppendLogOverflow(t *testing.T) {
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), ".", nil, nil)
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	supervisor.now = func() time.Time { return now }

	for i := range 250 {
		supervisor.appendLog("test-id", LogEntry{
			Timestamp: now,
			Stream:    "stdout",
			Message:   fmt.Sprintf("line %d", i),
		})
	}

	entries, err := supervisor.Logs("test-id")
	if err != nil {
		t.Fatalf("Logs() error = %v", err)
	}
	if len(entries) != 200 {
		t.Fatalf("len(Logs) = %d, want 200", len(entries))
	}
	if entries[0].Message != "line 50" {
		t.Fatalf("first entry message = %q, want %q", entries[0].Message, "line 50")
	}
}

func TestLogsEmptyForExistingRuntime(t *testing.T) {
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), ".", nil, nil)
	supervisor.runtimes["rt-1"] = Runtime{ID: "rt-1", AgentID: "mock-acp", Status: StatusRunning, CreatedAt: time.Now()}

	entries, err := supervisor.Logs("rt-1")
	if err != nil {
		t.Fatalf("Logs() error = %v", err)
	}
	if entries != nil {
		t.Fatalf("Logs() = %v, want nil", entries)
	}
}

func TestLogsNotFound(t *testing.T) {
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), ".", nil, nil)
	if _, err := supervisor.Logs("nonexistent"); err != ErrRuntimeNotFound {
		t.Fatalf("Logs() error = %v, want %v", err, ErrRuntimeNotFound)
	}
	if _, err := supervisor.Get("nonexistent"); err != ErrRuntimeNotFound {
		t.Fatalf("Get() error = %v, want %v", err, ErrRuntimeNotFound)
	}
}

type errorReader struct{ err error }

func (r *errorReader) Read(p []byte) (int, error) { return 0, r.err }

func TestCaptureLogsScannerError(t *testing.T) {
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), ".", nil, nil)
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	supervisor.captureLogs("test-id", "stderr", &errorReader{err: fmt.Errorf("simulated read failure")}, io.Discard)

	entries, err := supervisor.Logs("test-id")
	if err != nil {
		t.Fatalf("Logs() error = %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least one log entry from scanner error")
	}
	if !strings.Contains(entries[0].Message, "simulated read failure") {
		t.Fatalf("Message = %q, want it to contain 'simulated read failure'", entries[0].Message)
	}
}

// --- Summary / pruning ---

func TestSummaryIncludesRecentFailures(t *testing.T) {
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), ".", nil, nil)
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	supervisor.now = func() time.Time { return now }

	supervisor.runtimes["run-failed"] = Runtime{
		ID: "run-failed", AgentID: "mock-acp", AgentName: "Mock ACP",
		Status: StatusFailed, LastError: "process exited with status 1", CreatedAt: now, LastFailureAt: now,
	}
	supervisor.runtimes["run-running"] = Runtime{
		ID: "run-running", AgentID: "mock-acp", AgentName: "Mock ACP",
		Status: StatusRunning, CreatedAt: now.Add(-time.Minute),
	}
	supervisor.logs["run-failed"] = []LogEntry{
		{Timestamp: now, Stream: "stderr", Message: "boom"},
		{Timestamp: now.Add(time.Second), Stream: "stderr", Message: "crash"},
	}

	summary := supervisor.Summary()
	if summary.Total != 2 || summary.Running != 1 || summary.Failed != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if len(summary.RecentFailures) != 1 {
		t.Fatalf("len(RecentFailures) = %d, want 1", len(summary.RecentFailures))
	}
	if summary.RecentFailures[0].LastError != "process exited with status 1" {
		t.Fatalf("LastError = %q", summary.RecentFailures[0].LastError)
	}
	if len(summary.RecentFailures[0].RecentLogLines) != 2 {
		t.Fatalf("len(RecentLogLines) = %d, want 2", len(summary.RecentFailures[0].RecentLogLines))
	}
}

func TestSummaryCountsLifecycleTransitions(t *testing.T) {
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), ".", nil, nil)
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	supervisor.now = func() time.Time { return now }

	supervisor.runtimes["run-starting"] = Runtime{ID: "run-starting", AgentID: "a", AgentName: "A", Status: StatusStarting, CreatedAt: now}
	supervisor.runtimes["run-running"] = Runtime{ID: "run-running", AgentID: "b", AgentName: "B", Status: StatusRunning, CreatedAt: now}
	supervisor.runtimes["run-stopped"] = Runtime{ID: "run-stopped", AgentID: "d", AgentName: "D", Status: StatusStopped, CreatedAt: now, StoppedAt: now}
	supervisor.runtimes["run-failed"] = Runtime{ID: "run-failed", AgentID: "e", AgentName: "E", Status: StatusFailed, LastError: "boom", CreatedAt: now, LastFailureAt: now}

	summary := supervisor.Summary()
	if summary.Total != 4 {
		t.Fatalf("Total = %d, want 4", summary.Total)
	}
	if summary.Starting != 1 || summary.Running != 1 || summary.Stopped != 1 || summary.Failed != 1 {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestStoppedRuntimeCountedInSummary(t *testing.T) {
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil, nil)
	proc := launchMock(t, supervisor)

	// A clean exit lands the runtime in StatusStopped.
	supervisor.RecordExit(proc.Runtime.ID, nil, false)

	summary := supervisor.Summary()
	if summary.Stopped != 1 || summary.Total != 1 {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestStoppedRuntimeIsPrunedAfterRetentionWindow(t *testing.T) {
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), ".", nil, nil)
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	supervisor.now = func() time.Time { return now }

	runtimeInfo := Runtime{
		ID: "run-stopped", AgentID: "mock-acp", AgentName: "Mock ACP",
		Status: StatusStopped, CreatedAt: now.Add(-time.Hour),
		StoppedAt: now.Add(-stoppedRuntimeRetention).Add(-time.Second),
	}
	supervisor.runtimes[runtimeInfo.ID] = runtimeInfo
	supervisor.logs[runtimeInfo.ID] = []LogEntry{{Timestamp: now, Stream: "gateway", Message: "stopped"}}

	if got := len(supervisor.List()); got != 0 {
		t.Fatalf("len(List()) = %d, want 0 after retention pruning", got)
	}
	if _, err := supervisor.Logs(runtimeInfo.ID); err != ErrRuntimeNotFound {
		t.Fatalf("Logs() error = %v, want %v", err, ErrRuntimeNotFound)
	}
	if summary := supervisor.Summary(); summary.Total != 0 {
		t.Fatalf("Summary().Total = %d, want 0 after retention pruning", summary.Total)
	}
}

func TestFailedRuntimesPrunedAfterRetention(t *testing.T) {
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), ".", nil, nil)
	now := time.Now().UTC()
	supervisor.now = func() time.Time { return now }

	seed := func(id, status string, lastFailure, stoppedAt time.Time) {
		supervisor.runtimes[id] = Runtime{
			ID: id, AgentID: "mock-acp", AgentName: "Mock",
			Status: status, CreatedAt: now.Add(-2 * time.Hour),
			LastFailureAt: lastFailure, StoppedAt: stoppedAt,
		}
		supervisor.logs[id] = []LogEntry{{Timestamp: now, Stream: "stderr", Message: "boom"}}
	}
	seed("rt-old-failed", StatusFailed, now.Add(-time.Hour), time.Time{})
	seed("rt-fresh-failed", StatusFailed, now, time.Time{})
	seed("rt-old-stopped", StatusStopped, time.Time{}, now.Add(-time.Hour))

	got := supervisor.List()
	if len(got) != 1 || got[0].ID != "rt-fresh-failed" {
		ids := make([]string, 0, len(got))
		for _, r := range got {
			ids = append(ids, r.ID)
		}
		t.Fatalf("List() = %v, want [rt-fresh-failed]", ids)
	}
	for _, id := range []string{"rt-old-failed", "rt-old-stopped"} {
		if _, ok := supervisor.logs[id]; ok {
			t.Errorf("logs[%q] survives prune, want evicted", id)
		}
	}
	if _, ok := supervisor.logs["rt-fresh-failed"]; !ok {
		t.Error("logs[rt-fresh-failed] pruned too early, want retained")
	}
}

func TestListSortsByCreatedAtThenID(t *testing.T) {
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), ".", nil, nil)
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)

	supervisor.runtimes["b"] = Runtime{ID: "b", AgentID: "b", AgentName: "B", Status: StatusRunning, CreatedAt: now}
	supervisor.runtimes["a"] = Runtime{ID: "a", AgentID: "a", AgentName: "A", Status: StatusRunning, CreatedAt: now}

	runtimes := supervisor.List()
	if len(runtimes) != 2 {
		t.Fatalf("len = %d, want 2", len(runtimes))
	}
	if runtimes[0].ID != "a" || runtimes[1].ID != "b" {
		t.Fatalf("order = %s, %s; want a, b", runtimes[0].ID, runtimes[1].ID)
	}
}

// --- Persistence ---

func TestLaunchPersistsRuntimeWithStore(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, store, nil)
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }
	proc := launchMock(t, supervisor)

	// The runtime record is written to the configured store on launch and on
	// exit; persisting must not fail (the store has no runtime reader).
	supervisor.RecordExit(proc.Runtime.ID, nil, false)
	if got, err := supervisor.Get(proc.Runtime.ID); err != nil || got.Status != StatusStopped {
		t.Fatalf("Get() = %+v, %v; want stopped", got, err)
	}
}

func TestPersistFailureWithStore(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), ".", nil, nil)
	supervisor.store = store
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	supervisor.now = func() time.Time { return now }

	runtimeInfo := Runtime{
		ID: "run-failed", AgentID: "mock-acp", AgentName: "Mock ACP",
		Status: StatusFailed, LastError: "process exited with status 1", CreatedAt: now,
	}
	supervisor.runtimes[runtimeInfo.ID] = runtimeInfo

	supervisor.persistFailure(runtimeInfo.ID, runtimeInfo, []LogEntry{
		{Timestamp: now, Stream: "stderr", Message: "boom"},
	}, now)

	failures, err := store.ListRecentRuntimeFailures(context.Background(), 5)
	if err != nil {
		t.Fatalf("ListRecentRuntimeFailures() error = %v", err)
	}
	found := false
	for _, f := range failures {
		if f.RuntimeID == runtimeInfo.ID {
			found = true
			if f.LastError != "process exited with status 1" {
				t.Fatalf("LastError = %q", f.LastError)
			}
		}
	}
	if !found {
		t.Fatal("failure record not found in persisted store")
	}
}

func TestPersistCleanExitWithStore(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), ".", nil, nil)
	supervisor.store = store
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	supervisor.now = func() time.Time { return now }

	runtimeInfo := Runtime{
		ID: "run-clean", AgentID: "mock-acp", AgentName: "Mock ACP", Status: StatusStopped, CreatedAt: now,
	}
	supervisor.runtimes[runtimeInfo.ID] = runtimeInfo

	supervisor.persistCleanExit(runtimeInfo.ID, runtimeInfo, []LogEntry{
		{Timestamp: now, Stream: "stderr", Message: "bye"},
	}, now)

	records, err := store.ListRuntimeFailureRecords(context.Background(), 5)
	if err != nil {
		t.Fatalf("ListRuntimeFailureRecords() error = %v", err)
	}
	found := false
	for _, f := range records {
		if f.RuntimeID == runtimeInfo.ID {
			found = true
			if !f.CleanExit {
				t.Fatal("CleanExit = false, want true")
			}
			if f.LastError != "exit status 0 (clean)" {
				t.Fatalf("LastError = %q", f.LastError)
			}
		}
	}
	if !found {
		t.Fatal("clean-exit record not found in persisted store")
	}
}

func TestPersistRuntimeWithClosedStore(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	store.Close()

	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), ".", nil, nil)
	supervisor.store = store
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	supervisor.persistRuntime(Runtime{
		ID: "test-id", AgentID: "test-agent", AgentName: "Test Agent", Status: StatusRunning, CreatedAt: time.Now(),
	}) // must not panic on closed store
}

func TestPersistFailureWithClosedStore(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	store.Close()

	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), ".", nil, nil)
	supervisor.store = store
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)

	supervisor.persistFailure("test-id", Runtime{
		ID: "test-id", AgentID: "test-agent", AgentName: "Test Agent", Status: StatusFailed, LastError: "boom", CreatedAt: now,
	}, []LogEntry{{Timestamp: now, Stream: "stderr", Message: "error"}}, now)

	if _, err := store.ListRecentRuntimeFailures(context.Background(), 5); err == nil {
		t.Fatal("expected error querying closed store")
	}
}

func TestSummaryIncludesPersistedFailures(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	failedAt := time.Date(2026, 3, 25, 10, 5, 0, 0, time.UTC)
	if err := store.SaveRuntimeFailure(context.Background(), storage.RuntimeFailureRecord{
		RuntimeID:  "run-failed",
		AgentID:    "mock-acp",
		AgentName:  "Mock ACP",
		LastError:  "process exited with status 1",
		CreatedAt:  time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC),
		FailedAt:   failedAt,
		LogPreview: `[{"timestamp":"2026-03-25T10:04:59Z","stream":"stderr","message":"boom"}]`,
	}); err != nil {
		t.Fatalf("SaveRuntimeFailure() error = %v", err)
	}

	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir(), store, nil)
	summary := supervisor.Summary()

	if len(summary.RecentFailures) != 1 {
		t.Fatalf("len(RecentFailures) = %d, want 1", len(summary.RecentFailures))
	}
	if summary.RecentFailures[0].FailedAt != failedAt {
		t.Fatalf("FailedAt = %v, want %v", summary.RecentFailures[0].FailedAt, failedAt)
	}
	if len(summary.RecentFailures[0].RecentLogLines) != 1 {
		t.Fatalf("len(RecentLogLines) = %d, want 1", len(summary.RecentFailures[0].RecentLogLines))
	}
}

func TestSummaryExcludesCleanExitRecords(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	if err := store.SaveRuntimeFailure(context.Background(), storage.RuntimeFailureRecord{
		RuntimeID:  "run-clean",
		AgentID:    "mock-acp",
		AgentName:  "Mock ACP",
		LastError:  "exit status 0 (clean)",
		CreatedAt:  now,
		FailedAt:   now,
		LogPreview: "[]",
		CleanExit:  true,
	}); err != nil {
		t.Fatalf("SaveRuntimeFailure() error = %v", err)
	}

	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir(), store, nil)
	supervisor.now = func() time.Time { return now }

	summary := supervisor.Summary()
	if len(summary.RecentFailures) != 0 {
		t.Fatalf("len(RecentFailures) = %d, want 0 (clean exits must not surface as failures)", len(summary.RecentFailures))
	}
}

func TestSummaryWithClosedStore(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	store.Close()

	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir(), store, nil)
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }
	supervisor.runtimes["run-failed"] = Runtime{
		ID: "run-failed", AgentID: "mock-acp", AgentName: "Mock ACP",
		Status: StatusFailed, LastError: "process exited with status 1", CreatedAt: supervisor.now(),
	}

	summary := supervisor.Summary()
	if summary.Total != 1 || summary.Failed != 1 {
		t.Fatalf("summary = %+v", summary)
	}
}

// --- Pure helpers ---

func TestResolveCommandPath(t *testing.T) {
	supervisor := &Supervisor{baseDir: t.TempDir()}
	abs := filepath.Join(supervisor.baseDir, "agents", "bin")
	if got := supervisor.resolveCommandPath(abs); got != abs {
		t.Fatalf("resolveCommandPath(abs) = %q, want %q", got, abs)
	}
	resolved := supervisor.resolveCommandPath(filepath.Join("agents", "bin"))
	if !filepath.IsAbs(resolved) {
		t.Fatalf("resolveCommandPath(rel) = %q, want absolute", resolved)
	}
	if !strings.HasPrefix(resolved, supervisor.baseDir) {
		t.Fatalf("resolveCommandPath(rel) = %q, want under %q", resolved, supervisor.baseDir)
	}
}

func TestResolveLaunchExternalNotFound(t *testing.T) {
	supervisor := &Supervisor{baseDir: t.TempDir()}
	_, _, err := supervisor.resolveLaunch(catalog.LaunchConfig{
		Mode:    "external",
		Command: "definitely-not-a-real-binary-ferngeist",
	})
	if !errors.Is(err, ErrExecutableNotFound) {
		t.Fatalf("resolveLaunch() error = %v, want %v", err, ErrExecutableNotFound)
	}
}

func TestResolveLaunchRejectsUnknownMode(t *testing.T) {
	supervisor := &Supervisor{baseDir: t.TempDir()}
	_, _, err := supervisor.resolveLaunch(catalog.LaunchConfig{Mode: "container"})
	if !errors.Is(err, ErrUnsupportedLaunch) {
		t.Fatalf("resolveLaunch() error = %v, want %v", err, ErrUnsupportedLaunch)
	}
}

func TestFailureSortTime(t *testing.T) {
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	created := now.Add(-time.Hour)
	if got := failureSortTime(FailureSummary{FailedAt: now, CreatedAt: created}); got != now {
		t.Fatalf("failureSortTime() = %v, want FailedAt", got)
	}
	if got := failureSortTime(FailureSummary{CreatedAt: created}); got != created {
		t.Fatalf("failureSortTime() = %v, want CreatedAt fallback", got)
	}
}

func TestFailureSummaryFromRecord(t *testing.T) {
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	got := failureSummaryFromRecord(storage.RuntimeFailureRecord{
		RuntimeID:  "rt-1",
		AgentID:    "mock-acp",
		AgentName:  "Mock ACP",
		LastError:  "boom",
		CreatedAt:  now,
		FailedAt:   now,
		LogPreview: `[{"timestamp":"2026-03-25T10:00:00Z","stream":"stderr","message":"line"}]`,
	})
	if got.LastError != "boom" || len(got.RecentLogLines) != 1 {
		t.Fatalf("failureSummaryFromRecord() = %+v", got)
	}

	empty := failureSummaryFromRecord(storage.RuntimeFailureRecord{RuntimeID: "rt-2"})
	if empty.RecentLogLines != nil {
		t.Fatalf("empty LogPreview should yield no log lines, got %+v", empty.RecentLogLines)
	}

	invalid := failureSummaryFromRecord(storage.RuntimeFailureRecord{RuntimeID: "rt-3", LogPreview: "not json"})
	if len(invalid.RecentLogLines) != 1 || !strings.Contains(invalid.RecentLogLines[0].Message, "decode") {
		t.Fatalf("invalid LogPreview = %+v", invalid.RecentLogLines)
	}
}

// --- Optional installed-agent smoke test ---

func TestOptionalInstalledOpenCodeACPSmoke(t *testing.T) {
	if os.Getenv("FERNGEIST_RUN_REAL_AGENT_TESTS") != "1" {
		t.Skip("set FERNGEIST_RUN_REAL_AGENT_TESTS=1 to run installed-agent smoke tests")
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode is not installed on PATH")
	}

	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir(), nil, nil)
	proc, err := supervisor.Launch(catalog.Agent{
		ID:          "opencode",
		DisplayName: "OpenCode",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:    "external",
			Command: "opencode",
			Args:    []string{"acp"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	defer func() { _ = proc.Stop(2 * time.Second) }()

	request := "{\"jsonrpc\":\"2.0\",\"id\":\"1\",\"method\":\"initialize\",\"params\":{\"protocolVersion\":1,\"capabilities\":{},\"clientInfo\":{\"name\":\"ferngeist-smoke\",\"version\":\"dev\"}}}\n"
	if _, err := io.WriteString(proc.Stdin, request); err != nil {
		t.Fatalf("WriteString() error = %v", err)
	}

	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		scanner := bufio.NewScanner(proc.Stdout)
		if scanner.Scan() {
			done <- result{line: scanner.Text()}
			return
		}
		done <- result{err: scanner.Err()}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("scanner error = %v", res.err)
		}
		if strings.TrimSpace(res.line) == "" {
			t.Fatal("expected non-empty ACP response line from installed opencode")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for ACP response from installed opencode")
	}
}

// --- Test helpers ---

func buildMockAgent(t *testing.T, baseDir string) {
	t.Helper()

	binDir := filepath.Join(baseDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	outputPath := filepath.Join(binDir, mockAgentBinaryName())
	command := exec.Command("go", "build", "-o", outputPath, "./cmd/mock-stdio-agent")
	command.Dir = filepath.Join("..", "..")
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		t.Fatalf("go build mock agent error = %v", err)
	}
}

func buildMockStdioAgent(t *testing.T, outputPath string) {
	t.Helper()

	command := exec.Command("go", "build", "-o", outputPath, "./cmd/mock-stdio-agent")
	command.Dir = filepath.Join("..", "..")
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		t.Fatalf("go build mock stdio agent error = %v", err)
	}
}

func mockAgentBinaryName() string {
	if os.PathSeparator == '\\' {
		return "mock-stdio-agent.exe"
	}
	return "mock-stdio-agent"
}

func namedBinary(name string) string {
	if os.PathSeparator == '\\' {
		return name + ".exe"
	}
	return name
}

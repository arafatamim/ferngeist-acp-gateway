// Package runtime_test contains unit and integration tests for the runtime supervisor.
// Tests cover the full lifecycle: starting, connecting, restarting, stopping, and
// pruning runtimes; lease acquisition and release; process exit handling and circuit
// breakers; log capture and overflow; summary reporting; and persistence via the
// storage backend. Most tests use a compiled mock stdio agent launched as a subprocess.
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

// --- Start / Connect / Restart lifecycle ---

func TestStartIsIdempotentPerAgent(t *testing.T) { // TestStartIsIdempotentPerAgent verifies that calling Start twice for the same agent returns the same runtime ID.
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	agent := catalog.Agent{
		ID:          "mock-acp",
		DisplayName: "Mock ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", mockAgentBinaryName()),
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{
				Mode: "immediate",
			},
		},
		HealthCheck: catalog.HealthCheckConfig{
			Mode: "none",
		},
	}

	first, err := supervisor.Start(agent)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	second, err := supervisor.Start(agent)
	if err != nil {
		t.Fatalf("Start() second error = %v", err)
	}

	if first.ID != second.ID {
		t.Fatalf("runtime IDs differ: %q vs %q", first.ID, second.ID)
	}

	_, _ = supervisor.StopByAgentID(agent.ID)
}

func TestStartReplacesAttachedRuntimeForReconnect(t *testing.T) { // TestStartReplacesAttachedRuntimeForReconnect verifies that Start creates a new runtime when the previous one has an attached legacy client.
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	agent := catalog.Agent{
		ID:          "mock-acp",
		DisplayName: "Mock ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", mockAgentBinaryName()),
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{
				Mode: "immediate",
			},
		},
		HealthCheck: catalog.HealthCheckConfig{
			Mode: "none",
		},
	}

	first, err := supervisor.Start(agent)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	_, _, release, err := supervisor.AttachStdio(first.ID)
	if err != nil {
		t.Fatalf("AttachStdio() error = %v", err)
	}
	defer release()

	second, err := supervisor.Start(agent)
	if err != nil {
		t.Fatalf("Start() reconnect error = %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("runtime IDs match after reconnect: %q", first.ID)
	}

	_, _ = supervisor.StopByAgentID(agent.ID)
}

func TestConnectReturnsDescriptorForRunningRuntime(t *testing.T) { // TestConnectReturnsDescriptorForRunningRuntime verifies that Connect returns a WebSocket path and bearer token for a running runtime.
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	runtime, err := supervisor.Start(catalog.Agent{
		ID:          "mock-acp",
		DisplayName: "Mock ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", mockAgentBinaryName()),
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{
				Mode: "immediate",
			},
		},
		HealthCheck: catalog.HealthCheckConfig{
			Mode: "none",
		},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	descriptor, err := supervisor.Connect(runtime.ID)
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}

	if descriptor.WebSocketPath == "" {
		t.Fatal("WebSocketPath should not be empty")
	}
	if descriptor.BearerToken == "" {
		t.Fatal("BearerToken should not be empty")
	}

	_, _ = supervisor.StopByAgentID("mock-acp")
}

func TestRestartLaunchesNewRuntimeWithMergedEnv(t *testing.T) { // TestRestartLaunchesNewRuntimeWithMergedEnv verifies that Restart creates a new runtime ID and passes merged environment variables to the agent.
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)
	runtimeInfo, err := supervisor.Start(catalog.Agent{
		ID:          "mock-acp",
		DisplayName: "Mock ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", mockAgentBinaryName()),
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{Mode: "immediate"},
		},
		HealthCheck: catalog.HealthCheckConfig{Mode: "none"},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	restarted, err := supervisor.Restart(runtimeInfo.ID, map[string]string{"FERNGEIST_TEST_ENV": "after-restart"})
	if err != nil {
		t.Fatalf("Restart() error = %v", err)
	}
	if restarted.ID == runtimeInfo.ID {
		t.Fatal("Restart() should create a new runtime id")
	}

	stdin, stdout, release, err := supervisor.AttachStdio(restarted.ID)
	if err != nil {
		t.Fatalf("AttachStdio() error = %v", err)
	}
	defer release()
	defer stdin.Close()

	if _, err := io.WriteString(stdin, "{\"jsonrpc\":\"2.0\",\"id\":\"1\",\"method\":\"initialize\",\"params\":{\"protocolVersion\":1,\"capabilities\":{},\"clientInfo\":{\"name\":\"test\",\"version\":\"0\"}}}\n"); err != nil {
		t.Fatalf("WriteString() error = %v", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !scanner.Scan() {
		t.Fatalf("expected JSON-RPC initialize response: %v", scanner.Err())
	}
	var initResp struct {
		Result struct {
			AgentInfo struct {
				Env string `json:"env"`
			} `json:"agentInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(scanner.Text()), &initResp); err != nil {
		t.Fatalf("unmarshal initialize response: %v", err)
	}
	if initResp.Result.AgentInfo.Env != "after-restart" {
		t.Fatalf("agentInfo.env = %q, want %q", initResp.Result.AgentInfo.Env, "after-restart")
	}

	runtimes := supervisor.List()
	oldStatus := ""
	for _, listed := range runtimes {
		if listed.ID == runtimeInfo.ID {
			oldStatus = listed.Status
			break
		}
	}
	if oldStatus != StatusStopped {
		t.Fatalf("old runtime status = %q, want %q", oldStatus, StatusStopped)
	}

	_, _ = supervisor.StopByRuntimeID(restarted.ID)
}

// --- Stop lifecycle ---

func TestStoppedRuntimeAppearsInList(t *testing.T) { // TestStoppedRuntimeAppearsInList verifies that a stopped runtime remains in List and cannot be connected to.
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)
	runtimeInfo, err := supervisor.Start(catalog.Agent{
		ID:          "mock-acp",
		DisplayName: "Mock ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", mockAgentBinaryName()),
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{
				Mode: "immediate",
			},
		},
		HealthCheck: catalog.HealthCheckConfig{
			Mode: "none",
		},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	stopped, err := supervisor.StopByRuntimeID(runtimeInfo.ID)
	if err != nil {
		t.Fatalf("StopByRuntimeID() error = %v", err)
	}
	if stopped.Status != StatusStopped {
		t.Fatalf("stopped.Status = %q, want %q", stopped.Status, StatusStopped)
	}

	runtimes := supervisor.List()
	if len(runtimes) != 1 {
		t.Fatalf("len(List()) = %d, want 1", len(runtimes))
	}
	if runtimes[0].ID != runtimeInfo.ID {
		t.Fatalf("List()[0].ID = %q, want %q", runtimes[0].ID, runtimeInfo.ID)
	}
	if runtimes[0].Status != StatusStopped {
		t.Fatalf("List()[0].Status = %q, want %q", runtimes[0].Status, StatusStopped)
	}

	if _, err := supervisor.Connect(runtimeInfo.ID); err != ErrRuntimeNotRunning {
		t.Fatalf("Connect() error = %v, want %v", err, ErrRuntimeNotRunning)
	}
}

func TestStoppedRuntimeCountedInSummary(t *testing.T) { // TestStoppedRuntimeCountedInSummary verifies that Summary includes stopped runtimes in the count.
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)
	runtimeInfo, err := supervisor.Start(catalog.Agent{
		ID:          "mock-acp",
		DisplayName: "Mock ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", mockAgentBinaryName()),
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{
				Mode: "immediate",
			},
		},
		HealthCheck: catalog.HealthCheckConfig{
			Mode: "none",
		},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	stopped, err := supervisor.StopByRuntimeID(runtimeInfo.ID)
	if err != nil {
		t.Fatalf("StopByRuntimeID() error = %v", err)
	}
	if stopped.Status != StatusStopped {
		t.Fatalf("stopped.Status = %q, want %q", stopped.Status, StatusStopped)
	}

	summary := supervisor.Summary()
	if summary.Stopped != 1 {
		t.Fatalf("Summary().Stopped = %d, want 1", summary.Stopped)
	}
	if summary.Total != 1 {
		t.Fatalf("Summary().Total = %d, want 1", summary.Total)
	}
}

func TestStoppedRuntimeIsPrunedAfterRetentionWindow(t *testing.T) { // TestStoppedRuntimeIsPrunedAfterRetentionWindow verifies that stopped runtimes are removed from List, Logs, and Summary after the retention period.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	supervisor.now = func() time.Time { return now }

	runtimeInfo := Runtime{
		ID:        "run-stopped",
		AgentID:   "mock-acp",
		AgentName: "Mock ACP",
		Status:    StatusStopped,
		CreatedAt: now.Add(-time.Hour),
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

// --- Input validation ---

func TestStartRejectsUndetectedAgent(t *testing.T) { // TestStartRejectsUndetectedAgent verifies that Start returns ErrAgentNotDetected for agents not marked as detected.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))

	_, err := supervisor.Start(catalog.Agent{
		ID:          "codex-acp",
		DisplayName: "Codex ACP",
		Detected:    false,
		Launch:      catalog.LaunchConfig{Mode: "external"},
		HealthCheck: catalog.HealthCheckConfig{Mode: "none"},
	})
	if err != ErrAgentNotDetected {
		t.Fatalf("Start() error = %v, want %v", err, ErrAgentNotDetected)
	}
}

func TestStartSupportsExternalStdioRuntime(t *testing.T) { // TestStartSupportsExternalStdioRuntime verifies that Start launches an external stdio agent from PATH and supports JSON-RPC over stdin/stdout.
	baseDir := t.TempDir()
	binDir := filepath.Join(baseDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	buildMockStdioAgent(t, filepath.Join(binDir, namedBinary("codex-acp")))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)
	runtimeInfo, err := supervisor.Start(catalog.Agent{
		ID:          "codex-acp",
		DisplayName: "Codex ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "external",
			Command:   "codex-acp",
			Args:      []string{},
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{
				Mode: "immediate",
			},
			Restart: catalog.RestartConfig{
				Mode: "never",
			},
		},
		HealthCheck: catalog.HealthCheckConfig{
			Mode: "none",
		},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if runtimeInfo.Transport != "stdio" {
		t.Fatalf("Transport = %q, want %q", runtimeInfo.Transport, "stdio")
	}

	stdin, stdout, release, err := supervisor.AttachStdio(runtimeInfo.ID)
	if err != nil {
		t.Fatalf("AttachStdio() error = %v", err)
	}
	defer release()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	if _, err := io.WriteString(stdin, "{\"jsonrpc\":\"2.0\",\"id\":\"1\",\"method\":\"initialize\",\"params\":{\"protocolVersion\":1,\"capabilities\":{},\"clientInfo\":{\"name\":\"test\",\"version\":\"0\"}}}\n"); err != nil {
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

	_, _ = supervisor.StopByRuntimeID(runtimeInfo.ID)
}

// --- Restart / Circuit breaker ---

func TestRuntimeRestartsAfterFailureWhenPolicyEnabled(t *testing.T) { // TestRuntimeRestartsAfterFailureWhenPolicyEnabled verifies that a process started with on_failure restart policy is automatically restarted after crash, keeping the same runtime ID.
	baseDir := t.TempDir()
	binDir := filepath.Join(baseDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	stateFile := filepath.Join(baseDir, "restart-state.txt")
	flakySource := filepath.Join(baseDir, "flaky.go")
	source := fmt.Sprintf(`package main
import (
	"os"
	"time"
)
func main() {
	stateFile := %q
	if _, err := os.Stat(stateFile); err != nil {
		_ = os.WriteFile(stateFile, []byte("1"), 0o644)
		time.Sleep(100 * time.Millisecond)
		os.Exit(1)
	}
	for {
		time.Sleep(time.Hour)
	}
}
`, stateFile)
	if err := os.WriteFile(flakySource, []byte(source), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	outputPath := filepath.Join(binDir, namedBinary("flaky-acp-agent"))
	command := exec.Command("go", "build", "-o", outputPath, flakySource)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		t.Fatalf("go build flaky agent error = %v", err)
	}

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)
	runtimeInfo, err := supervisor.Start(catalog.Agent{
		ID:          "flaky-acp",
		DisplayName: "Flaky ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", namedBinary("flaky-acp-agent")),
			Transport: "stdio",
			Args:      []string{},
			Readiness: catalog.ReadinessConfig{
				Mode: "immediate",
			},
			Restart: catalog.RestartConfig{
				Mode:       "on_failure",
				MaxRetries: 1,
			},
		},
		HealthCheck: catalog.HealthCheckConfig{
			Mode: "none",
		},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		_, _ = supervisor.StopByRuntimeID(runtimeInfo.ID)
	})

	var restarted Runtime
	found := false
	for range 40 {
		runtimes := supervisor.List()
		if len(runtimes) == 1 && runtimes[0].Status == StatusRunning && runtimes[0].RestartAttempts == 1 {
			restarted = runtimes[0]
			found = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !found {
		t.Fatal("expected runtime to restart and return to running state")
	}
	if restarted.ID != runtimeInfo.ID {
		t.Fatalf("runtime ID changed across restart: %q vs %q", restarted.ID, runtimeInfo.ID)
	}
	if restarted.CircuitOpen {
		t.Fatal("CircuitOpen should be false after successful restart")
	}
}

func TestRuntimeOpensCircuitAfterRestartLimitExceeded(t *testing.T) { // TestRuntimeOpensCircuitAfterRestartLimitExceeded verifies that after exhausting MaxRetries, the runtime enters StatusFailed with CircuitOpen set.
	baseDir := t.TempDir()
	binDir := filepath.Join(baseDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	flakySource := filepath.Join(baseDir, "always-fail.go")
	source := `package main
import (
	"time"
)
func main() {
	time.Sleep(100 * time.Millisecond)
	panic("boom")
}
`
	if err := os.WriteFile(flakySource, []byte(source), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	outputPath := filepath.Join(binDir, namedBinary("always-fail-acp-agent"))
	command := exec.Command("go", "build", "-o", outputPath, flakySource)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		t.Fatalf("go build always-fail agent error = %v", err)
	}

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)
	runtimeInfo, err := supervisor.Start(catalog.Agent{
		ID:          "always-fail-acp",
		DisplayName: "Always Fail ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", namedBinary("always-fail-acp-agent")),
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{
				Mode: "immediate",
			},
			Restart: catalog.RestartConfig{
				Mode:       "on_failure",
				MaxRetries: 1,
			},
		},
		HealthCheck: catalog.HealthCheckConfig{
			Mode: "none",
		},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		_, _ = supervisor.StopByRuntimeID(runtimeInfo.ID)
	})

	var failed Runtime
	found := false
	for range 50 {
		runtimes := supervisor.List()
		if len(runtimes) == 1 && runtimes[0].Status == StatusFailed && runtimes[0].CircuitOpen {
			failed = runtimes[0]
			found = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !found {
		t.Fatal("expected runtime to fail with circuit open after restart limit")
	}
	if failed.RestartAttempts != 1 {
		t.Fatalf("RestartAttempts = %d, want 1", failed.RestartAttempts)
	}
	if failed.FailureStreak < 2 {
		t.Fatalf("FailureStreak = %d, want at least 2", failed.FailureStreak)
	}
}

// --- Smoke test ---

func TestOptionalInstalledOpenCodeACPSmoke(t *testing.T) { // TestOptionalInstalledOpenCodeACPSmoke is a smoke test (opt-in via FERNGEIST_RUN_REAL_AGENT_TESTS=1) that launches the installed opencode binary over ACP.
	if os.Getenv("FERNGEIST_RUN_REAL_AGENT_TESTS") != "1" {
		t.Skip("set FERNGEIST_RUN_REAL_AGENT_TESTS=1 to run installed-agent smoke tests")
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode is not installed on PATH")
	}

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir(), nil)
	runtimeInfo, err := supervisor.Start(catalog.Agent{
		ID:          "opencode",
		DisplayName: "OpenCode",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "external",
			Command:   "opencode",
			Args:      []string{"acp"},
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{
				Mode: "immediate",
			},
			Restart: catalog.RestartConfig{
				Mode: "never",
			},
		},
		HealthCheck: catalog.HealthCheckConfig{
			Mode: "none",
		},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		_, _ = supervisor.StopByRuntimeID(runtimeInfo.ID)
	})

	stdin, stdout, release, err := supervisor.AttachStdio(runtimeInfo.ID)
	if err != nil {
		t.Fatalf("AttachStdio() error = %v", err)
	}
	defer release()

	request := "{\"jsonrpc\":\"2.0\",\"id\":\"1\",\"method\":\"initialize\",\"params\":{\"protocolVersion\":1,\"capabilities\":{},\"clientInfo\":{\"name\":\"ferngeist-smoke\",\"version\":\"dev\"}}}\n"
	if _, err := io.WriteString(stdin, request); err != nil {
		t.Fatalf("WriteString() error = %v", err)
	}

	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
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

// --- Summary ---

func TestSummaryIncludesRecentFailures(t *testing.T) { // TestSummaryIncludesRecentFailures verifies that Summary exposes RecentFailures with LastError and log lines for failed runtimes.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	supervisor.now = func() time.Time { return now }

	supervisor.runtimes["run-failed"] = Runtime{
		ID:        "run-failed",
		AgentID:   "mock-acp",
		AgentName: "Mock ACP",
		Status:    StatusFailed,
		LastError: "process exited with status 1",
		CreatedAt: now,
	}
	supervisor.runtimes["run-running"] = Runtime{
		ID:        "run-running",
		AgentID:   "mock-acp",
		AgentName: "Mock ACP",
		Status:    StatusRunning,
		CreatedAt: now.Add(-time.Minute),
	}
	supervisor.logs["run-failed"] = []LogEntry{
		{Timestamp: now, Stream: "stderr", Message: "boom"},
		{Timestamp: now.Add(time.Second), Stream: "stderr", Message: "crash"},
	}

	summary := supervisor.Summary()
	if summary.Total != 2 {
		t.Fatalf("Total = %d, want 2", summary.Total)
	}
	if summary.Running != 1 {
		t.Fatalf("Running = %d, want 1", summary.Running)
	}
	if summary.Failed != 1 {
		t.Fatalf("Failed = %d, want 1", summary.Failed)
	}
	if summary.CircuitOpen != 0 {
		t.Fatalf("CircuitOpen = %d, want 0", summary.CircuitOpen)
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

func TestSummaryCountsLifecycleTransitions(t *testing.T) { // TestSummaryCountsLifecycleTransitions verifies that Summary counts each lifecycle status (Starting, Running, Stopping, Stopped, Failed, CircuitOpen) correctly.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)

	supervisor.runtimes["run-starting"] = Runtime{ID: "run-starting", AgentID: "a", AgentName: "A", Status: StatusStarting, CreatedAt: now}
	supervisor.runtimes["run-running"] = Runtime{ID: "run-running", AgentID: "b", AgentName: "B", Status: StatusRunning, CreatedAt: now}
	supervisor.runtimes["run-stopping"] = Runtime{ID: "run-stopping", AgentID: "c", AgentName: "C", Status: StatusStopping, CreatedAt: now}
	supervisor.runtimes["run-stopped"] = Runtime{ID: "run-stopped", AgentID: "d", AgentName: "D", Status: StatusStopped, CreatedAt: now}
	supervisor.runtimes["run-failed"] = Runtime{ID: "run-failed", AgentID: "e", AgentName: "E", Status: StatusFailed, LastError: "boom", CreatedAt: now}
	supervisor.runtimes["run-open"] = Runtime{ID: "run-open", AgentID: "f", AgentName: "F", Status: StatusFailed, LastError: "loop", CircuitOpen: true, CreatedAt: now}

	summary := supervisor.Summary()
	if summary.Total != 6 {
		t.Fatalf("Total = %d, want 6", summary.Total)
	}
	if summary.Starting != 1 {
		t.Fatalf("Starting = %d, want 1", summary.Starting)
	}
	if summary.Running != 1 {
		t.Fatalf("Running = %d, want 1", summary.Running)
	}
	if summary.Stopping != 1 {
		t.Fatalf("Stopping = %d, want 1", summary.Stopping)
	}
	if summary.Stopped != 1 {
		t.Fatalf("Stopped = %d, want 1", summary.Stopped)
	}
	if summary.Failed != 2 {
		t.Fatalf("Failed = %d, want 2", summary.Failed)
	}
	if summary.CircuitOpen != 1 {
		t.Fatalf("CircuitOpen = %d, want 1", summary.CircuitOpen)
	}
}

func TestSummaryIncludesPersistedFailures(t *testing.T) { // TestSummaryIncludesPersistedFailures verifies that Summary includes failures persisted in the storage backend.
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

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir(), store)
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

// --- Shutdown ---

func TestShutdownStopsRunningProcess(t *testing.T) { // TestShutdownStopsRunningProcess verifies that Shutdown stops all running processes and removes them from the runtime list.
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)
	runtimeInfo, err := supervisor.Start(catalog.Agent{
		ID:          "mock-acp",
		DisplayName: "Mock ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", mockAgentBinaryName()),
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{
				Mode: "immediate",
			},
		},
		HealthCheck: catalog.HealthCheckConfig{
			Mode: "none",
		},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	if err := supervisor.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if got := len(supervisor.List()); got != 0 {
		t.Fatalf("len(List()) = %d, want 0", got)
	}
	if _, err := supervisor.Connect(runtimeInfo.ID); err != ErrRuntimeNotFound {
		t.Fatalf("Connect() error = %v, want %v", err, ErrRuntimeNotFound)
	}
}

// --- Auto-acquire ---

func TestStartAutoAcquiresMissingExternalBinary(t *testing.T) { // TestStartAutoAcquiresMissingExternalBinary verifies that Start downloads a missing external binary via the acquire system when CuratedLaunch is enabled.
	sourceDir := t.TempDir()
	sourcePath := filepath.Join(sourceDir, namedBinary("mock-stdio-agent"))
	buildMockStdioAgent(t, sourcePath)

	server := httptest.NewServer(http.FileServer(http.Dir(sourceDir)))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	installer := acquire.New(logger, t.TempDir(), nil)
	supervisor := NewSupervisorWithBaseDirAndInstaller(logger, t.TempDir(), nil, installer)

	runtimeInfo, err := supervisor.Start(catalog.Agent{
		ID:          "codex-acp",
		DisplayName: "Codex ACP",
		Detected:    false,
		Protocol:    "acp",
		Security:    catalog.SecurityConfig{CuratedLaunch: true, AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "external",
			Command:   "codex-acp",
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{Mode: "immediate"},
			Restart:   catalog.RestartConfig{Mode: "never"},
		},
		HealthCheck: catalog.HealthCheckConfig{Mode: "none"},
		Registry: catalog.RegistryInfo{
			CurrentBinaryPath:       namedBinary("mock-stdio-agent"),
			CurrentBinaryArchiveURL: server.URL + "/" + namedBinary("mock-stdio-agent"),
		},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if runtimeInfo.Status != StatusRunning {
		t.Fatalf("Status = %q, want %q", runtimeInfo.Status, StatusRunning)
	}
	if _, _, release, err := supervisor.AttachStdio(runtimeInfo.ID); err != nil {
		t.Fatalf("AttachStdio() error = %v", err)
	} else {
		release()
	}
	if _, err := supervisor.StopByRuntimeID(runtimeInfo.ID); err != nil {
		t.Fatalf("StopByRuntimeID() error = %v", err)
	}
}

// --- Lease lifecycle ---

func TestAcquireLeaseReturnsPipesRunningRuntime(t *testing.T) { // TestAcquireLeaseReturnsPipesRunningRuntime verifies that AcquireLease returns valid pipes for a running runtime.
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	agent := catalog.Agent{
		ID:          "mock-acp",
		DisplayName: "Mock ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", mockAgentBinaryName()),
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{Mode: "immediate"},
		},
		HealthCheck: catalog.HealthCheckConfig{Mode: "none"},
	}

	rt, err := supervisor.Start(agent)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	pipes, err := supervisor.AcquireLease(rt.ID, "my-session-id")
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	if pipes == nil {
		t.Fatal("AcquireLease() returned nil pipes")
	}
	pipes.Release()

	_, _ = supervisor.StopByAgentID(agent.ID)
}

func TestReleaseLeaseFreesLeaseForReAcquire(t *testing.T) { // TestReleaseLeaseFreesLeaseForReAcquire verifies that ReleaseLease clears the leaseholder, allowing a different session to acquire the lease.
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	agent := catalog.Agent{
		ID:          "mock-acp",
		DisplayName: "Mock ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", mockAgentBinaryName()),
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{Mode: "immediate"},
		},
		HealthCheck: catalog.HealthCheckConfig{Mode: "none"},
	}

	rt, err := supervisor.Start(agent)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	pipes, err := supervisor.AcquireLease(rt.ID, "my-session-id")
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	if pipes == nil {
		t.Fatal("AcquireLease() returned nil pipes")
	}

	if err := supervisor.ReleaseLease(rt.ID, "my-session-id"); err != nil {
		t.Fatalf("ReleaseLease() error = %v", err)
	}

	pipes2, err := supervisor.AcquireLease(rt.ID, "my-session-id-2")
	if err != nil {
		t.Fatalf("AcquireLease() after ReleaseLease error = %v", err)
	}
	if pipes2 == nil {
		t.Fatal("AcquireLease() after ReleaseLease returned nil pipes")
	}
	pipes2.Release()

	_, _ = supervisor.StopByAgentID(agent.ID)
}

func TestAcquireLeaseDoubleAcquireError(t *testing.T) { // TestAcquireLeaseDoubleAcquireError verifies that a second AcquireLease on the same runtime returns ErrRuntimeLeaseHeld.
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	agent := catalog.Agent{
		ID:          "mock-acp",
		DisplayName: "Mock ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", mockAgentBinaryName()),
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{Mode: "immediate"},
		},
		HealthCheck: catalog.HealthCheckConfig{Mode: "none"},
	}

	rt, err := supervisor.Start(agent)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	pipes, err := supervisor.AcquireLease(rt.ID, "session-A")
	if err != nil {
		t.Fatalf("First AcquireLease() error = %v", err)
	}
	defer pipes.Release()

	_, err = supervisor.AcquireLease(rt.ID, "session-B")
	if err != ErrRuntimeLeaseHeld {
		t.Fatalf("Second AcquireLease() error = %v, want %v", err, ErrRuntimeLeaseHeld)
	}

	_, _ = supervisor.StopByAgentID(agent.ID)
}

func TestReleaseLeaseWrongLeaseholder(t *testing.T) { // TestReleaseLeaseWrongLeaseholder verifies that ReleaseLease returns ErrRuntimeLeaseHeld when called with a leaseholder that does not match.
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	agent := catalog.Agent{
		ID:          "mock-acp",
		DisplayName: "Mock ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", mockAgentBinaryName()),
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{Mode: "immediate"},
		},
		HealthCheck: catalog.HealthCheckConfig{Mode: "none"},
	}

	rt, err := supervisor.Start(agent)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	pipes, err := supervisor.AcquireLease(rt.ID, "session-A")
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	defer pipes.Release()

	err = supervisor.ReleaseLease(rt.ID, "session-B")
	if err != ErrRuntimeLeaseHeld {
		t.Fatalf("ReleaseLease() with wrong holder error = %v, want %v", err, ErrRuntimeLeaseHeld)
	}

	_, _ = supervisor.StopByAgentID(agent.ID)
}

// --- OnProcessExit ---

func TestOnProcessExitCallback(t *testing.T) { // TestOnProcessExitCallback verifies that OnProcessExit fires the registered callback with the correct runtime ID when a process stops.
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	agent := catalog.Agent{
		ID:          "mock-acp",
		DisplayName: "Mock ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", mockAgentBinaryName()),
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{Mode: "immediate"},
		},
		HealthCheck: catalog.HealthCheckConfig{Mode: "none"},
	}

	rt, err := supervisor.Start(agent)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	callbackCh := make(chan string, 1)
	supervisor.OnProcessExit(rt.ID, func(runtimeID string) {
		callbackCh <- runtimeID
	})

	_, err = supervisor.StopByRuntimeID(rt.ID)
	if err != nil {
		t.Fatalf("StopByRuntimeID() error = %v", err)
	}

	select {
	case got := <-callbackCh:
		if got != rt.ID {
			t.Fatalf("callback received runtimeID = %q, want %q", got, rt.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for OnProcessExit callback")
	}
}

// --- AppendLog ---

func TestAppendLog(t *testing.T) { // TestAppendLog verifies that AppendLog writes an entry retrievable via Logs.
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	agent := catalog.Agent{
		ID:          "mock-acp",
		DisplayName: "Mock ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", mockAgentBinaryName()),
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{Mode: "immediate"},
		},
		HealthCheck: catalog.HealthCheckConfig{Mode: "none"},
	}

	rt, err := supervisor.Start(agent)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	supervisor.AppendLog(rt.ID, "gateway", "test log message")

	entries, err := supervisor.Logs(rt.ID)
	if err != nil {
		t.Fatalf("Logs() error = %v", err)
	}
	found := false
	for _, entry := range entries {
		if entry.Stream == "gateway" && entry.Message == "test log message" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("AppendLog entry not found in Logs()")
	}

	_, _ = supervisor.StopByAgentID(agent.ID)
}

// --- WriteToAgent / LeasedPipes ---

func TestWriteToAgent(t *testing.T) { // TestWriteToAgent verifies that WriteToAgent sends data to the agent's stdin through an acquired lease.
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	agent := catalog.Agent{
		ID:          "mock-acp",
		DisplayName: "Mock ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", mockAgentBinaryName()),
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{Mode: "immediate"},
		},
		HealthCheck: catalog.HealthCheckConfig{Mode: "none"},
	}

	rt, err := supervisor.Start(agent)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	pipes, err := supervisor.AcquireLease(rt.ID, "test-writer")
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	defer pipes.Release()

	if err := pipes.WriteToAgent([]byte(`{"jsonrpc":"2.0","id":"1","method":"initialize","params":{}}`)); err != nil {
		t.Fatalf("WriteToAgent() error = %v", err)
	}

	_, _ = supervisor.StopByAgentID(agent.ID)
}

// --- Start with session lease ---

func TestStartDoesNotKillSessionLeasedRuntime(t *testing.T) { // TestStartDoesNotKillSessionLeasedRuntime verifies that Start returns the same runtime ID when a session lease is held (unlike legacy attach where it replaces the runtime).
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	agent := catalog.Agent{
		ID:          "mock-acp",
		DisplayName: "Mock ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", mockAgentBinaryName()),
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{Mode: "immediate"},
		},
		HealthCheck: catalog.HealthCheckConfig{Mode: "none"},
	}

	first, err := supervisor.Start(agent)
	if err != nil {
		t.Fatalf("First Start() error = %v", err)
	}

	pipes, err := supervisor.AcquireLease(first.ID, "my-session-id")
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	defer pipes.Release()

	second, err := supervisor.Start(agent)
	if err != nil {
		t.Fatalf("Second Start() error = %v", err)
	}

	if first.ID != second.ID {
		t.Fatalf("runtime IDs differ after re-Start with session lease: %q vs %q", first.ID, second.ID)
	}

	_, _ = supervisor.StopByAgentID(agent.ID)
}

// --- StopByRuntimeID edge cases ---

func TestStopByRuntimeID_ReleasesPipes(t *testing.T) { // TestStopByRuntimeID_ReleasesPipes verifies that StopByRuntimeID releases the process pipes, making AcquireLease return ErrRuntimeNotRunning.
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	agent := catalog.Agent{
		ID:          "mock-acp",
		DisplayName: "Mock ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", mockAgentBinaryName()),
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{Mode: "immediate"},
		},
		HealthCheck: catalog.HealthCheckConfig{Mode: "none"},
	}

	rt, err := supervisor.Start(agent)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	_, err = supervisor.StopByRuntimeID(rt.ID)
	if err != nil {
		t.Fatalf("StopByRuntimeID() error = %v", err)
	}

	runtimes := supervisor.List()
	if len(runtimes) != 1 {
		t.Fatalf("len(runtimes) = %d, want 1", len(runtimes))
	}
	if runtimes[0].Status != StatusStopped {
		t.Fatalf("runtime status = %q, want %q", runtimes[0].Status, StatusStopped)
	}

	_, err = supervisor.AcquireLease(rt.ID, "post-stop")
	if err != ErrRuntimeNotRunning {
		t.Fatalf("AcquireLease() after StopByRuntimeID error = %v, want %v (lease should be released)", err, ErrRuntimeNotRunning)
	}
}

// --- LeasedPipes Release behavior ---

func TestLeasedPipesRelease(t *testing.T) { // TestLeasedPipesRelease verifies that a legacy ("legacy") lease Release closes stdin and frees the lease for re-acquire.
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	agent := catalog.Agent{
		ID:          "mock-acp",
		DisplayName: "Mock ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", mockAgentBinaryName()),
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{Mode: "immediate"},
		},
		HealthCheck: catalog.HealthCheckConfig{Mode: "none"},
	}

	rt, err := supervisor.Start(agent)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	pipes, err := supervisor.AcquireLease(rt.ID, "legacy")
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}

	if err := pipes.Release(); err != nil {
		t.Fatalf("Release() error = %v", err)
	}

	pipes2, err := supervisor.AcquireLease(rt.ID, "new-holder")
	if err != nil {
		t.Fatalf("AcquireLease() after release error = %v", err)
	}
	pipes2.Release()

	if _, err := pipes.(*LeasedPipes).Stdin.Write([]byte("test")); err == nil {
		t.Fatal("expected error writing to stdin after legacy Release()")
	}

	_, _ = supervisor.StopByAgentID(agent.ID)
}

func TestLeasedPipesRelease_SessionLease(t *testing.T) { // TestLeasedPipesRelease_SessionLease verifies that a session lease Release does not close stdin and allows subsequent writes and re-acquire.
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	agent := catalog.Agent{
		ID:          "mock-acp",
		DisplayName: "Mock ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", mockAgentBinaryName()),
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{Mode: "immediate"},
		},
		HealthCheck: catalog.HealthCheckConfig{Mode: "none"},
	}

	rt, err := supervisor.Start(agent)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	pipes, err := supervisor.AcquireLease(rt.ID, "sess_abc123")
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}

	if err := pipes.Release(); err != nil {
		t.Fatalf("Release() error = %v", err)
	}

	if err := pipes.WriteToAgent([]byte("test")); err != nil {
		t.Fatalf("WriteToAgent() after session Release() should succeed, got: %v", err)
	}

	pipes2, err := supervisor.AcquireLease(rt.ID, "sess_xyz789")
	if err != nil {
		t.Fatalf("AcquireLease() after session Release() error = %v", err)
	}
	pipes2.Release()

	_, _ = supervisor.StopByAgentID(agent.ID)
}

// --- More input validation ---

func TestStartRejectsUnsupportedLaunch(t *testing.T) { // TestStartRejectsUnsupportedLaunch verifies that Start returns ErrUnsupportedLaunch for an unknown launch mode.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))

	_, err := supervisor.Start(catalog.Agent{
		ID:          "bad-agent",
		DisplayName: "Bad Agent",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "unsupported",
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{Mode: "immediate"},
		},
		HealthCheck: catalog.HealthCheckConfig{Mode: "none"},
	})
	if err != ErrUnsupportedLaunch {
		t.Fatalf("Start() error = %v, want %v", err, ErrUnsupportedLaunch)
	}
}

func TestStartRejectsNoTransport(t *testing.T) { // TestStartRejectsNoTransport verifies that Start returns ErrRuntimeNotConnectable for non-stdio transports.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))

	_, err := supervisor.Start(catalog.Agent{
		ID:          "http-agent",
		DisplayName: "HTTP Agent",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Transport: "http",
			Readiness: catalog.ReadinessConfig{Mode: "immediate"},
		},
		HealthCheck: catalog.HealthCheckConfig{Mode: "none"},
	})
	if err != ErrRuntimeNotConnectable {
		t.Fatalf("Start() error = %v, want %v", err, ErrRuntimeNotConnectable)
	}
}

func TestStartRejectsNotDetectedNoInstaller(t *testing.T) { // TestStartRejectsNotDetectedNoInstaller verifies that Start returns ErrAgentNotDetected for undetected agents without an installer.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))

	_, err := supervisor.Start(catalog.Agent{
		ID:          "missing-agent",
		DisplayName: "Missing Agent",
		Detected:    false,
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{Mode: "immediate"},
		},
		HealthCheck: catalog.HealthCheckConfig{Mode: "none"},
	})
	if err != ErrAgentNotDetected {
		t.Fatalf("Start() error = %v, want %v", err, ErrAgentNotDetected)
	}
}

func TestStartFailsOnUnsupportedReadinessMode(t *testing.T) { // TestStartFailsOnUnsupportedReadinessMode verifies that Start returns an error for invalid readiness mode.
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)

	_, err := supervisor.Start(catalog.Agent{
		ID:          "mock-acp",
		DisplayName: "Mock ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", mockAgentBinaryName()),
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{Mode: "invalid"},
		},
		HealthCheck: catalog.HealthCheckConfig{Mode: "none"},
	})
	if err == nil {
		t.Fatal("expected error for unsupported readiness mode")
	}
	if !strings.Contains(err.Error(), "unsupported readiness mode") {
		t.Fatalf("error = %v, want error containing 'unsupported readiness mode'", err)
	}
}

// --- Logging edge cases ---

func TestAppendLogEmptyID(t *testing.T) { // TestAppendLogEmptyID verifies that AppendLog with an empty runtime ID does not create log entries.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	supervisor.AppendLog("", "gateway", "test")

	_, err := supervisor.Logs("")
	if err != ErrRuntimeNotFound {
		t.Fatalf("Logs(\"\") error = %v, want %v", err, ErrRuntimeNotFound)
	}
}

func TestLogsNotFound(t *testing.T) { // TestLogsNotFound verifies that Logs returns ErrRuntimeNotFound for a nonexistent ID.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := supervisor.Logs("nonexistent")
	if err != ErrRuntimeNotFound {
		t.Fatalf("Logs() error = %v, want %v", err, ErrRuntimeNotFound)
	}
}

// --- Helper function tests ---

func TestReadinessMode(t *testing.T) { // TestReadinessMode verifies that readinessMode returns the expected defaults for stdio and non-stdio transports.
	if mode := readinessMode(catalog.LaunchConfig{Readiness: catalog.ReadinessConfig{Mode: "immediate"}}); mode != "immediate" {
		t.Fatalf("readinessMode(explicit) = %q, want %q", mode, "immediate")
	}
	if mode := readinessMode(catalog.LaunchConfig{Transport: "stdio"}); mode != "immediate" {
		t.Fatalf("readinessMode(stdio default) = %q, want %q", mode, "immediate")
	}
	if mode := readinessMode(catalog.LaunchConfig{Transport: "http"}); mode != "" {
		t.Fatalf("readinessMode(non-stdio) = %q, want empty", mode)
	}
}

func TestRestartMaxRetriesNegative(t *testing.T) { // TestRestartMaxRetriesNegative verifies that restartMaxRetries clamps negative values to zero.
	if n := restartMaxRetries(catalog.RestartConfig{MaxRetries: -1}); n != 0 {
		t.Fatalf("restartMaxRetries(-1) = %d, want 0", n)
	}
	if n := restartMaxRetries(catalog.RestartConfig{MaxRetries: 3}); n != 3 {
		t.Fatalf("restartMaxRetries(3) = %d, want 3", n)
	}
}

func TestRestartBackoffConfig(t *testing.T) { // TestRestartBackoffConfig verifies that restartBackoff returns the correct duration for zero, negative, and positive BackoffSeconds.
	if d := restartBackoff(catalog.RestartConfig{BackoffSeconds: 0}); d != 0 {
		t.Fatalf("restartBackoff(0) = %v, want 0", d)
	}
	if d := restartBackoff(catalog.RestartConfig{BackoffSeconds: -1}); d != 0 {
		t.Fatalf("restartBackoff(-1) = %v, want 0", d)
	}
	if d := restartBackoff(catalog.RestartConfig{BackoffSeconds: 5}); d != 5*time.Second {
		t.Fatalf("restartBackoff(5) = %v, want 5s", d)
	}
}

// --- Health check ---

func TestHealthCheckModeNever(t *testing.T) { // TestHealthCheckModeNever verifies that healthCheckMode returns "none" for both the "none" and empty modes.
	if mode := healthCheckMode(catalog.HealthCheckConfig{Mode: "none"}); mode != "none" {
		t.Fatalf("healthCheckMode(none) = %q, want %q", mode, "none")
	}
	if mode := healthCheckMode(catalog.HealthCheckConfig{Mode: ""}); mode != "none" {
		t.Fatalf("healthCheckMode(empty) = %q, want %q", mode, "none")
	}
}

func TestRunHealthCheckUnsupported(t *testing.T) { // TestRunHealthCheckUnsupported verifies that runHealthCheck returns nil for "none" mode and an error for unsupported modes.
	if err := runHealthCheck(catalog.LaunchConfig{}, catalog.HealthCheckConfig{Mode: "none"}); err != nil {
		t.Fatalf("runHealthCheck(none) = %v, want nil", err)
	}
	if err := runHealthCheck(catalog.LaunchConfig{}, catalog.HealthCheckConfig{Mode: "http"}); err == nil {
		t.Fatal("runHealthCheck(http) should return error")
	}
}

// --- resolveLaunch ---

func TestResolveCommandPath(t *testing.T) { // TestResolveCommandPath verifies that resolveCommandPath returns absolute paths unchanged and resolves relative paths against baseDir.
	baseDir := t.TempDir()
	s := &Supervisor{baseDir: baseDir}

	absPath := filepath.Join(baseDir, "some", "binary")
	if got := s.resolveCommandPath(absPath); got != absPath {
		t.Fatalf("resolveCommandPath(abs) = %q, want %q", got, absPath)
	}

	relPath := filepath.Join("bin", "agent")
	got := s.resolveCommandPath(relPath)
	expected, _ := filepath.Abs(filepath.Join(baseDir, relPath))
	if got != expected {
		t.Fatalf("resolveCommandPath(rel) = %q, want %q", got, expected)
	}
}

// --- Failure helpers ---

func TestFailureSortTime(t *testing.T) { // TestFailureSortTime verifies that failureSortTime prefers FailedAt over CreatedAt.
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)

	f1 := FailureSummary{FailedAt: now, CreatedAt: now.Add(-time.Hour)}
	if got := failureSortTime(f1); got != now {
		t.Fatalf("failureSortTime(with FailedAt) = %v, want %v", got, now)
	}

	f2 := FailureSummary{CreatedAt: now}
	if got := failureSortTime(f2); got != now {
		t.Fatalf("failureSortTime(zero FailedAt) = %v, want %v", got, now)
	}
}

func TestFailureSummaryFromRecord(t *testing.T) { // TestFailureSummaryFromRecord verifies that failureSummaryFromRecord correctly converts a storage record, handling empty and invalid LogPreview.
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)

	record := storage.RuntimeFailureRecord{
		RuntimeID:  "test-id",
		AgentID:    "test-agent",
		AgentName:  "Test Agent",
		LastError:  "boom",
		CreatedAt:  now,
		FailedAt:   now,
		LogPreview: "",
	}
	fs := failureSummaryFromRecord(record)
	if fs.RuntimeID != "test-id" {
		t.Fatalf("RuntimeID = %q, want %q", fs.RuntimeID, "test-id")
	}
	if len(fs.RecentLogLines) != 0 {
		t.Fatalf("RecentLogLines = %d, want 0 for empty preview", len(fs.RecentLogLines))
	}

	record.LogPreview = "{invalid json}"
	fs = failureSummaryFromRecord(record)
	if len(fs.RecentLogLines) != 1 {
		t.Fatalf("RecentLogLines = %d, want 1 for invalid preview", len(fs.RecentLogLines))
	}
	if fs.RecentLogLines[0].Message != "failed to decode persisted log preview" {
		t.Fatalf("Message = %q, want %q", fs.RecentLogLines[0].Message, "failed to decode persisted log preview")
	}
}

func TestShouldRestartEdgeCases(t *testing.T) { // TestShouldRestartEdgeCases verifies that shouldRestart returns false for "never" mode, non-stdio transports, exceeded retries, and true for valid restart config.
	s := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))

	if s.shouldRestart(catalog.RestartConfig{Mode: "never"}, "stdio", 0) {
		t.Fatal("shouldRestart(never) should be false")
	}
	if s.shouldRestart(catalog.RestartConfig{Mode: "on_failure"}, "http", 0) {
		t.Fatal("shouldRestart(non-stdio) should be false")
	}
	if s.shouldRestart(catalog.RestartConfig{Mode: "on_failure", MaxRetries: 1}, "stdio", 1) {
		t.Fatal("shouldRestart(exceeded) should be false")
	}
	if !s.shouldRestart(catalog.RestartConfig{Mode: "on_failure", MaxRetries: 3}, "stdio", 0) {
		t.Fatal("shouldRestart(valid) should be true")
	}
}

// --- stopProcessWithContext ---

func TestStopProcessWithContextNilHandle(t *testing.T) { // TestStopProcessWithContextNilHandle verifies that stopProcessWithContext does not panic on a nil process handle.
	if err := stopProcessWithContext(context.Background(), nil); err != nil {
		t.Fatalf("stopProcessWithContext(nil) = %v, want nil", err)
	}
}

func TestStopProcessWithContextAlreadyDone(t *testing.T) { // TestStopProcessWithContextAlreadyDone verifies that stopProcessWithContext handles an already-completed process without error.
	done := make(chan struct{})
	close(done)
	handle := &processHandle{done: done}
	if err := stopProcessWithContext(context.Background(), handle); err != nil {
		t.Fatalf("stopProcessWithContext(done) = %v, want nil", err)
	}
}

// --- Persistence ---

func TestPersistRuntimeWithStore(t *testing.T) { // TestPersistRuntimeWithStore verifies that starting a runtime persists it in the storage backend.
	store, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, store)
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	agent := catalog.Agent{
		ID:          "mock-acp",
		DisplayName: "Mock ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", mockAgentBinaryName()),
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{Mode: "immediate"},
		},
		HealthCheck: catalog.HealthCheckConfig{Mode: "none"},
	}

	rt, err := supervisor.Start(agent)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	_, _ = supervisor.StopByRuntimeID(rt.ID)
}

func TestPersistFailureWithStore(t *testing.T) { // TestPersistFailureWithStore verifies that persistFailure writes a failure record to the storage backend.
	store, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	supervisor.store = store
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	supervisor.now = func() time.Time { return now }

	runtime := Runtime{
		ID:        "run-failed",
		AgentID:   "mock-acp",
		AgentName: "Mock ACP",
		Status:    StatusFailed,
		LastError: "process exited with status 1",
		CreatedAt: now,
	}
	supervisor.runtimes[runtime.ID] = runtime

	supervisor.persistFailure(runtime.ID, runtime, []LogEntry{
		{Timestamp: now, Stream: "stderr", Message: "boom"},
	}, now)

	failures, err := store.ListRecentRuntimeFailures(context.Background(), 5)
	if err != nil {
		t.Fatalf("ListRecentRuntimeFailures() error = %v", err)
	}
	found := false
	for _, f := range failures {
		if f.RuntimeID == runtime.ID {
			found = true
			if f.LastError != "process exited with status 1" {
				t.Fatalf("LastError = %q, want %q", f.LastError, "process exited with status 1")
			}
			break
		}
	}
	if !found {
		t.Fatal("failure record not found in persisted store")
	}
}

// --- restartAfterBackoff ---

func TestRestartAfterBackoffLaunchFails(t *testing.T) { // TestRestartAfterBackoffLaunchFails verifies that restartAfterBackoff marks the runtime as failed when the launch itself fails.
	baseDir := t.TempDir()

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	supervisor.now = func() time.Time { return now }

	runtimeInfo := Runtime{
		ID:        "run-restart-fail",
		AgentID:   "mock-acp",
		AgentName: "Mock ACP",
		Status:    StatusStarting,
		CreatedAt: now,
	}
	supervisor.runtimes[runtimeInfo.ID] = runtimeInfo
	supervisor.runtimeByAgent[runtimeInfo.AgentID] = runtimeInfo.ID

	agent := catalog.Agent{
		ID:          "mock-acp",
		DisplayName: "Mock ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", mockAgentBinaryName()),
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{Mode: "immediate"},
		},
		HealthCheck: catalog.HealthCheckConfig{Mode: "none"},
	}

	supervisor.restartAfterBackoff(runtimeInfo, agent, nil)

	supervisor.mu.Lock()
	updated, ok := supervisor.runtimes[runtimeInfo.ID]
	supervisor.mu.Unlock()
	if !ok {
		t.Fatal("runtime should still exist after failed restart")
	}
	if updated.Status != StatusFailed {
		t.Fatalf("Status = %q, want %q after failed restart",
			updated.Status, StatusFailed)
	}
	if updated.LastError == "" {
		t.Fatal("LastError should be set after failed restart")
	}
}

func TestRestartAfterBackoffRuntimeRemoved(t *testing.T) { // TestRestartAfterBackoffRuntimeRemoved verifies that restartAfterBackoff does not recreate a runtime that was already removed from the map.
	baseDir := t.TempDir()

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	supervisor.now = func() time.Time { return now }

	runtimeInfo := Runtime{
		ID:        "run-gone",
		AgentID:   "mock-acp",
		AgentName: "Mock ACP",
		Status:    StatusStarting,
		CreatedAt: now,
	}

	agent := catalog.Agent{
		ID:          "mock-acp",
		DisplayName: "Mock ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", mockAgentBinaryName()),
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{Mode: "immediate"},
		},
		HealthCheck: catalog.HealthCheckConfig{Mode: "none"},
	}

	supervisor.restartAfterBackoff(runtimeInfo, agent, nil)

	supervisor.mu.Lock()
	if _, exists := supervisor.runtimes[runtimeInfo.ID]; exists {
		t.Fatal("runtime should have been removed if it never existed")
	}
	supervisor.mu.Unlock()
}

func TestStopProcessWithContextNilCmd(t *testing.T) { // TestStopProcessWithContextNilCmd verifies that stopProcessWithContext handles a processHandle with a nil cmd field.
	handle := &processHandle{cmd: nil, done: make(chan struct{})}
	if err := stopProcessWithContext(context.Background(), handle); err != nil {
		t.Fatalf("stopProcessWithContext(nil cmd) = %v, want nil", err)
	}
}

func TestStartRejectsRemoteStartDisabled(t *testing.T) { // TestStartRejectsRemoteStartDisabled verifies that Start returns ErrRemoteStartNotAllowed when AllowsRemoteStart is false.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))

	_, err := supervisor.Start(catalog.Agent{
		ID:          "no-remote",
		DisplayName: "No Remote",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: false},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{Mode: "immediate"},
		},
		HealthCheck: catalog.HealthCheckConfig{Mode: "none"},
	})
	if err != ErrRemoteStartNotAllowed {
		t.Fatalf("Start() error = %v, want %v", err, ErrRemoteStartNotAllowed)
	}
}

func TestConnectNonStdioTransport(t *testing.T) { // TestConnectNonStdioTransport verifies that Connect returns ErrRuntimeNotConnectable for non-stdio transports.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	supervisor.runtimes["run-http"] = Runtime{
		ID:        "run-http",
		AgentID:   "http-agent",
		AgentName: "HTTP Agent",
		Status:    StatusRunning,
		Transport: "http",
		CreatedAt: time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC),
	}

	_, err := supervisor.Connect("run-http")
	if err != ErrRuntimeNotConnectable {
		t.Fatalf("Connect(http) error = %v, want %v", err, ErrRuntimeNotConnectable)
	}
}

func TestAcquireLeaseNonStdioTransport(t *testing.T) { // TestAcquireLeaseNonStdioTransport verifies that AcquireLease returns ErrRuntimeNotConnectable for non-stdio transports.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	supervisor.runtimes["run-http"] = Runtime{
		ID:        "run-http",
		AgentID:   "http-agent",
		AgentName: "HTTP Agent",
		Status:    StatusRunning,
		Transport: "http",
		CreatedAt: time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC),
	}

	_, err := supervisor.AcquireLease("run-http", "test")
	if err != ErrRuntimeNotConnectable {
		t.Fatalf("AcquireLease(http) error = %v, want %v", err, ErrRuntimeNotConnectable)
	}
}

func TestAcquireLeaseMissingStdio(t *testing.T) { // TestAcquireLeaseMissingStdio verifies that AcquireLease returns ErrRuntimeNotConnectable when the runtime has no stdio pipes allocated.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	supervisor.runtimes["run-no-stdio"] = Runtime{
		ID:        "run-no-stdio",
		AgentID:   "test-agent",
		AgentName: "Test Agent",
		Status:    StatusRunning,
		Transport: "stdio",
		CreatedAt: time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC),
	}

	_, err := supervisor.AcquireLease("run-no-stdio", "test")
	if err != ErrRuntimeNotConnectable {
		t.Fatalf("AcquireLease(no pipes) error = %v, want %v", err, ErrRuntimeNotConnectable)
	}
}

func TestReleaseLeaseMissingProcess(t *testing.T) { // TestReleaseLeaseMissingProcess verifies that ReleaseLease returns ErrRuntimeNotFound for a nonexistent runtime ID.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := supervisor.ReleaseLease("nonexistent", "test")
	if err != ErrRuntimeNotFound {
		t.Fatalf("ReleaseLease() error = %v, want %v", err, ErrRuntimeNotFound)
	}
}

func TestWriteToAgentOnClosedPipe(t *testing.T) { // TestWriteToAgentOnClosedPipe verifies that WriteToAgent returns an error after a legacy lease Release has closed stdin.
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	agent := catalog.Agent{
		ID:          "mock-acp",
		DisplayName: "Mock ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", mockAgentBinaryName()),
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{Mode: "immediate"},
		},
		HealthCheck: catalog.HealthCheckConfig{Mode: "none"},
	}

	rt, err := supervisor.Start(agent)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	pipes, err := supervisor.AcquireLease(rt.ID, "legacy")
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}

	if err := pipes.Release(); err != nil {
		t.Fatalf("Release() error = %v", err)
	}

	if err := pipes.WriteToAgent([]byte("test")); err == nil {
		t.Fatal("expected error writing to stdin after legacy Release()")
	}

	_, _ = supervisor.StopByAgentID(agent.ID)
}

func TestAppendLogOverflow(t *testing.T) { // TestAppendLogOverflow verifies that the internal log buffer caps at 200 entries, discarding the oldest.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
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

func TestMergeEnvOverridesNilCurrent(t *testing.T) { // TestMergeEnvOverridesNilCurrent verifies that mergeEnvOverrides creates a new map when current is nil and returns nil when both are nil.
	updates := map[string]string{"KEY": "val"}
	result := mergeEnvOverrides(nil, updates)
	if result == nil {
		t.Fatal("mergeEnvOverrides(nil, updates) should not be nil")
	}
	if result["KEY"] != "val" {
		t.Fatalf("result[KEY] = %q, want %q", result["KEY"], "val")
	}

	bothNil := mergeEnvOverrides(nil, nil)
	if bothNil != nil {
		t.Fatal("mergeEnvOverrides(nil, nil) should be nil")
	}
}

func TestPersistRuntimeWithClosedStore(t *testing.T) { // TestPersistRuntimeWithClosedStore verifies that persistRuntime does not panic when the store is closed.
	store, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	store.Close()

	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	supervisor.store = store
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	runtime := Runtime{
		ID:        "test-id",
		AgentID:   "test-agent",
		AgentName: "Test Agent",
		Status:    StatusRunning,
		CreatedAt: time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC),
	}
	supervisor.persistRuntime(runtime) // must not panic on closed store
}

func TestPersistFailureWithClosedStore(t *testing.T) { // TestPersistFailureWithClosedStore verifies that persistFailure does not panic when the store is closed.
	store, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	store.Close()

	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	supervisor.store = store
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)

	runtime := Runtime{
		ID:        "test-id",
		AgentID:   "test-agent",
		AgentName: "Test Agent",
		Status:    StatusFailed,
		LastError: "boom",
		CreatedAt: now,
	}
	supervisor.persistFailure(runtime.ID, runtime, []LogEntry{
		{Timestamp: now, Stream: "stderr", Message: "error"},
	}, now)

	_, err = store.ListRecentRuntimeFailures(context.Background(), 5)
	if err == nil {
		t.Fatal("expected error querying closed store")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Fatalf("error = %v, want 'closed' in error message", err)
	}
}

// --- handleProcessExit ---

func TestHandleProcessExitNoRuntimeInMap(t *testing.T) { // TestHandleProcessExitNoRuntimeInMap verifies that handleProcessExit does nothing for a runtime not in the map.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	supervisor.handleProcessExit("nonexistent", "test", &processHandle{})

	if got := len(supervisor.List()); got != 0 {
		t.Fatalf("len(List()) = %d, want 0 (handleProcessExit should not modify empty state)", got)
	}
}

func TestHandleProcessExitCleanExit(t *testing.T) { // TestHandleProcessExitCleanExit verifies that handleProcessExit transitions a running runtime to StatusStopped on clean exit.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	supervisor.now = func() time.Time { return now }

	done := make(chan struct{})
	close(done)
	handle := &processHandle{
		done:    done,
		waitErr: nil,
		agent: catalog.Agent{
			Launch: catalog.LaunchConfig{
				Restart: catalog.RestartConfig{Mode: "never"},
			},
		},
	}

	supervisor.runtimes["test-id"] = Runtime{
		ID: "test-id", AgentID: "test", AgentName: "Test", Status: StatusRunning, CreatedAt: now,
	}
	supervisor.runtimeByAgent["test"] = "test-id"
	supervisor.logs["test-id"] = []LogEntry{}

	supervisor.handleProcessExit("test-id", "test", handle)

	supervisor.mu.Lock()
	rt, ok := supervisor.runtimes["test-id"]
	supervisor.mu.Unlock()
	if !ok {
		t.Fatal("runtime should still exist after clean exit")
	}
	if rt.Status != StatusStopped {
		t.Fatalf("Status = %q, want %q after clean exit", rt.Status, StatusStopped)
	}
}

func TestHandleProcessExitRuntimeAlreadyRemoved(t *testing.T) { // TestHandleProcessExitRuntimeAlreadyRemoved verifies that a runtime removed by Shutdown does not reappear after process exit.
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	agent := catalog.Agent{
		ID:          "mock-acp",
		DisplayName: "Mock ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", mockAgentBinaryName()),
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{Mode: "immediate"},
		},
		HealthCheck: catalog.HealthCheckConfig{Mode: "none"},
	}

	_, err := supervisor.Start(agent)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	if err := supervisor.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	runtimesAfter := supervisor.List()
	if len(runtimesAfter) != 0 {
		t.Fatalf("len(List()) = %d, want 0 after shutdown", len(runtimesAfter))
	}
}

func TestAttachStdioOnNonRunningRuntime(t *testing.T) { // TestAttachStdioOnNonRunningRuntime verifies that AttachStdio returns ErrRuntimeNotRunning for stopped runtimes.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	supervisor.runtimes["run-stopped"] = Runtime{
		ID:        "run-stopped",
		AgentID:   "test",
		AgentName: "Test",
		Status:    StatusStopped,
		CreatedAt: time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC),
	}

	_, _, _, err := supervisor.AttachStdio("run-stopped")
	if err != ErrRuntimeNotRunning {
		t.Fatalf("AttachStdio() error = %v, want %v", err, ErrRuntimeNotRunning)
	}
}

func TestCleanupFailedLaunchNonexistent(t *testing.T) { // TestCleanupFailedLaunchNonexistent verifies that cleanupFailedLaunch does not create entries for nonexistent runtimes.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	supervisor.cleanupFailedLaunch("nonexistent", "test", fmt.Errorf("test error"))

	runtimes := supervisor.List()
	if len(runtimes) != 0 {
		t.Fatalf("len(List()) = %d, want 0 (cleanupFailedLaunch should not create entries)", len(runtimes))
	}
}

func TestLogsEmptyForExistingRuntime(t *testing.T) { // TestLogsEmptyForExistingRuntime verifies that Logs returns nil (not an error) for a runtime with no log entries.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	supervisor.runtimes["run-empty"] = Runtime{
		ID:        "run-empty",
		AgentID:   "test",
		AgentName: "Test",
		Status:    StatusRunning,
		CreatedAt: time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC),
	}

	entries, err := supervisor.Logs("run-empty")
	if err != nil {
		t.Fatalf("Logs() error = %v, want nil", err)
	}
	if entries != nil {
		t.Fatalf("Logs() = %v, want nil", entries)
	}
}

func TestListSortsByCreatedAtThenID(t *testing.T) { // TestListSortsByCreatedAtThenID verifies that List returns runtimes sorted by CreatedAt then ID.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
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

func TestStopByRuntimeIDNotFound(t *testing.T) { // TestStopByRuntimeIDNotFound verifies that StopByRuntimeID returns ErrRuntimeNotFound for nonexistent IDs.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := supervisor.StopByRuntimeID("nonexistent")
	if err != ErrRuntimeNotFound {
		t.Fatalf("StopByRuntimeID() error = %v, want %v", err, ErrRuntimeNotFound)
	}
}

type errorReader struct{ err error }

func (r *errorReader) Read(p []byte) (int, error) { return 0, r.err }

func TestCaptureLogsScannerError(t *testing.T) { // TestCaptureLogsScannerError verifies that captureLogs records scanner errors as log entries.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	supervisor.now = func() time.Time { return now }

	errReader := &errorReader{err: fmt.Errorf("simulated read failure")}
	supervisor.captureLogs("test-id", "stderr", errReader, io.Discard)

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

func TestResolveLaunchExternalNotFound(t *testing.T) { // TestResolveLaunchExternalNotFound verifies that resolveLaunch returns ErrExecutableNotFound for an unresolvable external command.
	s := &Supervisor{baseDir: t.TempDir()}
	_, _, err := s.resolveLaunch(catalog.LaunchConfig{
		Mode:    "external",
		Command: "nonexistent-utility-that-does-not-exist",
	})
	if !errors.Is(err, ErrExecutableNotFound) {
		t.Fatalf("resolveLaunch() error = %v, want %v", err, ErrExecutableNotFound)
	}
}

func TestStopByAgentIDAlreadyStopped(t *testing.T) { // TestStopByAgentIDAlreadyStopped verifies that StopByAgentID is a no-op for already-stopped runtimes.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	supervisor.runtimes["test-id"] = Runtime{
		ID: "test-id", AgentID: "test", AgentName: "Test",
		Status: StatusStopped, CreatedAt: time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC),
	}
	supervisor.runtimeByAgent["test"] = "test-id"

	result, err := supervisor.StopByAgentID("test")
	if err != nil {
		t.Fatalf("StopByAgentID() error = %v, want nil", err)
	}
	if result.Status != StatusStopped {
		t.Fatalf("Status = %q, want %q", result.Status, StatusStopped)
	}
}

func TestStopByRuntimeIDAlreadyStopped(t *testing.T) { // TestStopByRuntimeIDAlreadyStopped verifies that StopByRuntimeID is a no-op for already-stopped runtimes.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	supervisor.runtimes["test-id"] = Runtime{
		ID: "test-id", AgentID: "test", AgentName: "Test",
		Status: StatusStopped, CreatedAt: time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC),
	}

	result, err := supervisor.StopByRuntimeID("test-id")
	if err != nil {
		t.Fatalf("StopByRuntimeID() error = %v, want nil", err)
	}
	if result.Status != StatusStopped {
		t.Fatalf("Status = %q, want %q", result.Status, StatusStopped)
	}
}

func TestStopByAgentIDOrphanedMapping(t *testing.T) { // TestStopByAgentIDOrphanedMapping verifies that StopByAgentID returns ErrRuntimeNotFound when the agent-to-runtime mapping points to a nonexistent ID.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	supervisor.runtimeByAgent["orphaned"] = "nonexistent-id"

	_, err := supervisor.StopByAgentID("orphaned")
	if err != ErrRuntimeNotFound {
		t.Fatalf("StopByAgentID() error = %v, want %v", err, ErrRuntimeNotFound)
	}
}

func TestAcquireLeaseNotFound(t *testing.T) { // TestAcquireLeaseNotFound verifies that AcquireLease returns ErrRuntimeNotFound for a nonexistent runtime ID.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := supervisor.AcquireLease("nonexistent", "test")
	if err != ErrRuntimeNotFound {
		t.Fatalf("AcquireLease() error = %v, want %v", err, ErrRuntimeNotFound)
	}
}

func TestSummaryWithClosedStore(t *testing.T) { // TestSummaryWithClosedStore verifies that Summary works with memory-only state even when the store is closed.
	store, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	store.Close()

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir(), store)
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	supervisor.runtimes["run-failed"] = Runtime{
		ID:          "run-failed",
		AgentID:     "mock-acp",
		AgentName:   "Mock ACP",
		Status:      StatusFailed,
		LastError:   "process exited with status 1",
		CreatedAt:   time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC),
		CircuitOpen: true,
	}

	summary := supervisor.Summary()
	if summary.Total != 1 {
		t.Fatalf("Total = %d, want 1", summary.Total)
	}
	if summary.CircuitOpen != 1 {
		t.Fatalf("CircuitOpen = %d, want 1", summary.CircuitOpen)
	}
}

func TestStopByAgentIDNotFound(t *testing.T) { // TestStopByAgentIDNotFound verifies that StopByAgentID returns ErrRuntimeNotFound for an unmapped agent ID.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := supervisor.StopByAgentID("nonexistent")
	if err != ErrRuntimeNotFound {
		t.Fatalf("StopByAgentID() error = %v, want %v", err, ErrRuntimeNotFound)
	}
}

func TestReleaseLeaseAfterProcessRemoved(t *testing.T) { // TestReleaseLeaseAfterProcessRemoved verifies that Release on a lease acquired before StopByRuntimeID does not panic after the process is gone.
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	agent := catalog.Agent{
		ID:          "mock-acp",
		DisplayName: "Mock ACP",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", mockAgentBinaryName()),
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{Mode: "immediate"},
		},
		HealthCheck: catalog.HealthCheckConfig{Mode: "none"},
	}

	rt, err := supervisor.Start(agent)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	pipes, err := supervisor.AcquireLease(rt.ID, "legacy")
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}

	_, err = supervisor.StopByRuntimeID(rt.ID)
	if err != nil {
		t.Fatalf("StopByRuntimeID() error = %v", err)
	}

	if err := pipes.Release(); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
}

func TestHandleProcessExitCircuitOpenBlock(t *testing.T) { // TestHandleProcessExitCircuitOpenBlock verifies that a repeatedly-failing runtime opens the circuit breaker and enters StatusFailed with CircuitOpen.
	baseDir := t.TempDir()
	binDir := filepath.Join(baseDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	flakySource := filepath.Join(baseDir, "circuit-source.go")
	source := `package main
import ("time")
func main() { time.Sleep(50 * time.Millisecond); panic("boom") }`
	if err := os.WriteFile(flakySource, []byte(source), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	outputPath := filepath.Join(binDir, namedBinary("circuit-agent"))
	build := exec.Command("go", "build", "-o", outputPath, flakySource)
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("go build circuit agent error = %v", err)
	}

	supervisor := NewSupervisorWithBaseDir(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil)
	supervisor.now = func() time.Time { return time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) }

	rt, err := supervisor.Start(catalog.Agent{
		ID:          "circuit-test",
		DisplayName: "Circuit Test",
		Detected:    true,
		Security:    catalog.SecurityConfig{AllowsRemoteStart: true},
		Launch: catalog.LaunchConfig{
			Mode:      "process",
			Command:   filepath.Join("bin", namedBinary("circuit-agent")),
			Transport: "stdio",
			Readiness: catalog.ReadinessConfig{Mode: "immediate"},
			Restart: catalog.RestartConfig{
				Mode:           "on_failure",
				MaxRetries:     1,
				BackoffSeconds: 0,
			},
		},
		HealthCheck: catalog.HealthCheckConfig{Mode: "none"},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		_, _ = supervisor.StopByRuntimeID(rt.ID)
	})

	gotCircuitOpen := false
	for range 50 {
		runtimes := supervisor.List()
		for _, r := range runtimes {
			if r.ID == rt.ID && r.CircuitOpen {
				gotCircuitOpen = true
				break
			}
		}
		if gotCircuitOpen {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !gotCircuitOpen {
		t.Fatal("expected circuit to open after exceeding max retries")
	}
}

// --- Lease edge cases (empty IDs) ---

func TestAcquireLeaseEmptyRuntimeID(t *testing.T) { // TestAcquireLeaseEmptyRuntimeID verifies that AcquireLease returns ErrRuntimeNotFound for an empty runtime ID.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := supervisor.AcquireLease("", "test")
	if err != ErrRuntimeNotFound {
		t.Fatalf("AcquireLease(\"\") error = %v, want %v", err, ErrRuntimeNotFound)
	}
}

func TestReleaseLeaseEmptyRuntimeID(t *testing.T) { // TestReleaseLeaseEmptyRuntimeID verifies that ReleaseLease returns ErrRuntimeNotFound for an empty runtime ID.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := supervisor.ReleaseLease("", "test")
	if err != ErrRuntimeNotFound {
		t.Fatalf("ReleaseLease(\"\") error = %v, want %v", err, ErrRuntimeNotFound)
	}
}

func TestStartDoesNotPanicWithEmptyAgent(t *testing.T) { // TestStartDoesNotPanicWithEmptyAgent verifies that Start returns an error (not a panic) for a zero-value agent.
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))

	_, err := supervisor.Start(catalog.Agent{})
	if err == nil {
		t.Fatal("expected error for empty agent")
	}
}

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

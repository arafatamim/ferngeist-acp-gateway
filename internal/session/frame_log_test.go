package session

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
)

// TestFrameLogManagerAppends verifies that inbound and outbound frames are
// written to the per-agent file as newline-delimited JSON with direction
// metadata.
func TestFrameLogManagerAppends(t *testing.T) {
	dir := t.TempDir()
	m, err := newFrameLogManager(true, dir, 1<<20, 3)
	if err != nil {
		t.Fatalf("newFrameLogManager: %v", err)
	}
	defer m.close()

	m.append("claude", "rt-1", "sess-1", "out", []byte(`{"jsonrpc":"2.0","id":"1","result":{}}`))
	m.append("claude", "rt-1", "sess-1", "in", []byte(`{"jsonrpc":"2.0","id":"2","method":"session/prompt"}`))

	raw, err := os.ReadFile(filepath.Join(dir, "claude-agent.log"))
	if err != nil {
		t.Fatalf("read frame log: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "claude-agent.log"))
	if err != nil {
		t.Fatalf("stat frame log: %v", err)
	}
	// POSIX applies the requested 0600; Windows ignores the mode bits on
	// regular files and inherits the parent directory's ACL instead.
	if goruntime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("frame log mode = %o, want 0600", perm)
		}
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(lines), raw)
	}
	if !strings.Contains(lines[0], `"dir":"out"`) || !strings.Contains(lines[0], `"frame":"{\"jsonrpc\":\"2.0\",\"id\":\"1\"`) {
		t.Errorf("outbound line missing direction/frame: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"dir":"in"`) || !strings.Contains(lines[1], `"session_id":"sess-1"`) {
		t.Errorf("inbound line missing direction/session: %s", lines[1])
	}
}

// TestFrameLogManagerNilIsNoop verifies a nil manager never panics.
func TestFrameLogManagerNilIsNoop(t *testing.T) {
	var m *frameLogManager
	m.append("claude", "rt", "sess", "in", []byte(`{}`)) // must not panic
	m.close()                                            // must not panic
}

// TestFrameLogManagerDisabled verifies the toggle-off case returns no manager.
func TestFrameLogManagerDisabled(t *testing.T) {
	m, err := newFrameLogManager(false, t.TempDir(), 1<<20, 3)
	if err != nil {
		t.Fatalf("newFrameLogManager(false): %v", err)
	}
	if m != nil {
		t.Fatal("expected nil manager when disabled")
	}
}

// TestFrameLogManagerPerAgentFiles verifies different agents get different
// files and that reusing the same agent shares one writer (no duplicate opens
// fighting over rotation).
func TestFrameLogManagerPerAgentFiles(t *testing.T) {
	dir := t.TempDir()
	m, err := newFrameLogManager(true, dir, 1<<20, 3)
	if err != nil {
		t.Fatalf("newFrameLogManager: %v", err)
	}
	defer m.close()

	m.append("claude", "rt-1", "sess-1", "out", []byte(`{"a":1}`))
	m.append("copilot", "rt-2", "sess-2", "out", []byte(`{"b":2}`))

	if _, err := os.Stat(filepath.Join(dir, "claude-agent.log")); err != nil {
		t.Errorf("claude-agent.log missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "copilot-agent.log")); err != nil {
		t.Errorf("copilot-agent.log missing: %v", err)
	}

	// Same agent reuses the shared writer.
	w1, _ := m.writerFor("claude")
	w2, _ := m.writerFor("claude")
	if w1 != w2 {
		t.Error("expected the same writer for the same agent")
	}
}

// TestFrameLogManagerRotates verifies the rolling writer rotates once the
// configured size is exceeded.
func TestFrameLogManagerRotates(t *testing.T) {
	dir := t.TempDir()
	// Tiny max size forces rotation after a few frames.
	m, err := newFrameLogManager(true, dir, 64, 2)
	if err != nil {
		t.Fatalf("newFrameLogManager: %v", err)
	}
	defer m.close()

	frame := []byte(`{"jsonrpc":"2.0","id":"1","result":{"data":"` + strings.Repeat("x", 128) + `"}}`)
	for i := 0; i < 10; i++ {
		m.append("claude", "rt", "sess", "out", frame)
	}

	if _, err := os.Stat(filepath.Join(dir, "claude-agent.log")); err != nil {
		t.Fatalf("active frame log missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "claude-agent.log.1")); err != nil {
		t.Fatalf("expected a rotated backup, got: %v", err)
	}
}

// TestFrameLogPumpTaps verifies the pump records frames through both taps when
// a frame log manager is configured.
func TestFrameLogPumpTaps(t *testing.T) {
	dir := t.TempDir()
	m, err := newFrameLogManager(true, dir, 1<<20, 3)
	if err != nil {
		t.Fatalf("newFrameLogManager: %v", err)
	}
	defer m.close()

	pump := &StdioPump{
		pipes: &runtime.LeasedPipes{
			Stdin:  nopWriteCloser{},
			Stdout: nil,
		},
		runtimeID: "rt-tap",
		sessionID: "sess-tap",
		agentID:   "claude",
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		frameLog:  m,
	}
	// Outbound via the drain loop path (handleStdoutLine with no client).
	pump.handleStdoutLine(`{"jsonrpc":"2.0","id":"1","result":{"protocolVersion":1}}`)
	// Inbound via WriteToAgent.
	if err := pump.WriteToAgent([]byte(`{"jsonrpc":"2.0","id":"2","method":"session/new","params":{"cwd":"/proj"}}`)); err != nil {
		t.Fatalf("WriteToAgent: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "claude-agent.log"))
	if err != nil {
		t.Fatalf("read frame log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 frame lines, got %d: %q", len(lines), raw)
	}
	if !strings.Contains(lines[0], `"dir":"out"`) {
		t.Errorf("expected outbound frame first, got: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"dir":"in"`) {
		t.Errorf("expected inbound frame second, got: %s", lines[1])
	}
}

package session

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/logging"
)

// frameLogManager owns one rolling frame-log writer per agent, all rooted in
// the gateway's log directory. Enabling the toggle makes the pump append every
// raw ACP JSON-RPC frame (client->agent and agent->client) to
// <logdir>/<agent>-agent.log as newline-delimited JSON. Frames can contain
// project code and tool output, so files are created with 0600 permissions and
// should be treated as sensitive.
type frameLogManager struct {
	mu         sync.Mutex
	dir        string
	maxSize    int64
	maxBackups int
	writers    map[string]*logging.Service // agentID -> writer
	// queue decouples the agent stdout drain loop from disk: append()
	// enqueues non-blocking (drops on full) and a single background
	// goroutine performs the marshal + file write. A disk stall must not
	// backpressure the agent pipe.
	queue chan frameLogEntry
	once  sync.Once
}

type frameLogEntry struct {
	agentID   string
	runtimeID string
	sessionID string
	direction string
	payload   string
}

// newFrameLogManager returns a manager rooted at dir, or nil when the toggle
// is off. Sessions for the same agent share one writer: multiple sessions per
// agent are allowed (multi-session per agent), so concurrent pumps of the same
// agent do append to the same file — safe because logging.Service.Write is
// mutex-guarded and each line carries its runtime_id.
func newFrameLogManager(enabled bool, dir string, maxSize int64, maxBackups int) (*frameLogManager, error) {
	if !enabled {
		return nil, nil
	}
	m := &frameLogManager{
		dir:        dir,
		maxSize:    maxSize,
		maxBackups: maxBackups,
		writers:    make(map[string]*logging.Service),
		queue:      make(chan frameLogEntry, 512),
	}
	go m.loop()
	return m, nil
}

// writerFor returns the shared rolling writer for an agent, creating it on
// first use.
func (m *frameLogManager) writerFor(agentID string) (*logging.Service, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w, ok := m.writers[agentID]; ok {
		return w, nil
	}
	name := agentID + "-agent.log"
	w, err := logging.NewServiceWithMode(m.dir, name, m.maxSize, m.maxBackups, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open frame log for agent %s: %w", agentID, err)
	}
	m.writers[agentID] = w
	return w, nil
}

// append records one frame line. Direction is "in" (client->agent) or "out"
// (agent->client). It is best-effort and must not perturb the session hot
// path; failures are logged by the caller if it chooses to.
func (m *frameLogManager) append(agentID, runtimeID, sessionID, direction string, payload []byte) {
	if m == nil {
		return
	}
	// Safe against a concurrent close(): a send on a closed queue panics;
	// best-effort logging must never crash the drain loop.
	defer func() { _ = recover() }()
	select {
	case m.queue <- frameLogEntry{agentID: agentID, runtimeID: runtimeID, sessionID: sessionID, direction: direction, payload: string(payload)}:
	default:
		// Queue full (disk behind): drop rather than stall the agent.
	}
}

// loop performs the marshal + file write off the hot path.
func (m *frameLogManager) loop() {
	for e := range m.queue {
		w, err := m.writerFor(e.agentID)
		if err != nil {
			continue
		}
		line, err := json.Marshal(struct {
			Timestamp string `json:"ts"`
			RuntimeID string `json:"runtime_id,omitempty"`
			SessionID string `json:"session_id,omitempty"`
			Direction string `json:"dir"`
			Frame     string `json:"frame"`
		}{
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
			RuntimeID: e.runtimeID,
			SessionID: e.sessionID,
			Direction: e.direction,
			Frame:     e.payload,
		})
		if err != nil {
			continue
		}
		_, _ = w.Write(append(line, '\n'))
	}
}

// flushForTest waits until the background queue drains (or timeout). Tests
// use it because append() is intentionally async.
func (m *frameLogManager) flushForTest(timeout time.Duration) {
	if m == nil {
		return
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(m.queue) == 0 {
			// Give the loop a slice to finish the in-flight Write.
			time.Sleep(20 * time.Millisecond)
			if len(m.queue) == 0 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// close closes every writer.
func (m *frameLogManager) close() error {
	if m == nil {
		return nil
	}
	m.once.Do(func() { close(m.queue) })
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.writers {
		_ = w.Close()
	}
	m.writers = make(map[string]*logging.Service)
	return nil
}

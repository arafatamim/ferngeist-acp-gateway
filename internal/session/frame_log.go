package session

import (
	"encoding/json"
	"fmt"
	"path/filepath"
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
}

// newFrameLogManager returns a manager rooted at dir, or nil when the toggle
// is off. Sessions for the same agent share one writer: a session is superseded
// (not duplicated), so concurrent pumps never race on the same file.
func newFrameLogManager(enabled bool, dir string, maxSize int64, maxBackups int) (*frameLogManager, error) {
	if !enabled {
		return nil, nil
	}
	return &frameLogManager{
		dir:        dir,
		maxSize:    maxSize,
		maxBackups: maxBackups,
		writers:    make(map[string]*logging.Service),
	}, nil
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
	w, err := m.writerFor(agentID)
	if err != nil {
		return
	}
	line, err := json.Marshal(struct {
		Timestamp string `json:"ts"`
		RuntimeID string `json:"runtime_id,omitempty"`
		SessionID string `json:"session_id,omitempty"`
		Direction string `json:"dir"`
		Frame     string `json:"frame"`
	}{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		RuntimeID: runtimeID,
		SessionID: sessionID,
		Direction: direction,
		Frame:     string(payload),
	})
	if err != nil {
		return
	}
	_, _ = w.Write(append(line, '\n'))
}

// close closes every writer.
func (m *frameLogManager) close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.writers {
		_ = w.Close()
	}
	m.writers = make(map[string]*logging.Service)
	return nil
}

// frameLogPath is a test helper mirroring the file a manager would open for an
// agent.
func frameLogPath(dir, agentID string) string {
	return filepath.Join(dir, agentID+"-agent.log")
}

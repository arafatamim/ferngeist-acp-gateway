// Package turns holds the cross-device status of ACP prompt turns served by
// GET /v1/turns: the wire shape, the classification of a finished prompt reply,
// and the daemon-wide store of finished outcomes. Live state (running prompts,
// pending human requests) is not stored here; the session layer reads it from
// its pumps at snapshot time and merges it over the store.
package turns

import (
	"cmp"
	"slices"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// Turn states.
const (
	StateRunning   = "running"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
	StateUnknown   = "unknown"
)

// Attention kinds: a human request the agent is waiting on.
const (
	AttentionPermission  = "permission"
	AttentionElicitation = "elicitation"
)

// maxOutcomes caps the store daemon-wide; at the cap the oldest outcome goes.
const maxOutcomes = 4096

// Status is one turn as GET /v1/turns reports it.
type Status struct {
	AgentID          string `json:"agentId"`
	SessionID        string `json:"sessionId"`
	GatewaySessionID string `json:"gatewaySessionId"`
	RuntimeID        string `json:"runtimeId"`
	State            string `json:"state"`
	// Attention is sorted and distinct, and empty unless State is running.
	Attention  []string       `json:"attention"`
	StartedAt  time.Time      `json:"startedAt"`
	ChangedAt  time.Time      `json:"changedAt"`
	StopReason acp.StopReason `json:"stopReason,omitempty"`
}

type key struct{ agentID, gatewaySessionID, sessionID string }

func (s Status) key() key { return key{s.AgentID, s.GatewaySessionID, s.SessionID} }

// Outcome classifies a finished prompt reply: an error reply failed, and a
// result is judged by its stop reason. Only end_turn is success; a stop reason
// the SDK does not know, or none, is unknown rather than success.
func Outcome(errored bool, reason acp.StopReason) string {
	switch {
	case errored:
		return StateFailed
	case reason == acp.StopReasonEndTurn:
		return StateSucceeded
	case reason == acp.StopReasonCancelled:
		return StateCancelled
	case reason == acp.StopReasonMaxTokens, reason == acp.StopReasonMaxTurnRequests, reason == acp.StopReasonRefusal:
		return StateFailed
	default:
		return StateUnknown
	}
}

// Store keeps each chat's latest finished outcome, in memory for the daemon's
// lifetime. The zero value is not ready; use NewStore.
type Store struct {
	mu       sync.Mutex
	outcomes map[key]Status
}

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{outcomes: make(map[key]Status)}
}

// Record keeps a finished turn's outcome, replacing the chat's previous one
// unless that one started later: a late reply for an older prompt must not
// overwrite a newer outcome.
func (s *Store) Record(st Status) {
	st.Attention = []string{}
	k := st.key()
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.outcomes[k]; ok {
		if st.StartedAt.Before(old.StartedAt) {
			return
		}
	} else if len(s.outcomes) >= maxOutcomes {
		delete(s.outcomes, s.oldestLocked())
	}
	s.outcomes[k] = st
}

// oldestLocked returns the key of the outcome changed longest ago, ties broken
// by key so eviction is deterministic. shortcut: an O(n) scan per insert at the
// cap, replace with a heap if the cap grows by orders of magnitude.
func (s *Store) oldestLocked() key {
	var oldest Status
	first := true
	for _, st := range s.outcomes {
		if first || compareAge(st, oldest) < 0 {
			oldest, first = st, false
		}
	}
	return oldest.key()
}

func compareAge(a, b Status) int {
	return cmp.Or(a.ChangedAt.Compare(b.ChangedAt), cmp.Compare(a.AgentID, b.AgentID),
		cmp.Compare(a.SessionID, b.SessionID), cmp.Compare(a.GatewaySessionID, b.GatewaySessionID))
}

// Snapshot merges live running turns over the stored outcomes, a running turn
// hiding the stored outcome of its chat, sorted by agent ID, ACP session ID and
// gateway session ID. It never returns nil.
func (s *Store) Snapshot(live []Status) []Status {
	running := make(map[key]bool, len(live))
	out := make([]Status, 0, len(live))
	for _, st := range live {
		running[st.key()] = true
		out = append(out, st)
	}
	s.mu.Lock()
	for k, st := range s.outcomes {
		if !running[k] {
			out = append(out, st)
		}
	}
	s.mu.Unlock()
	slices.SortFunc(out, func(a, b Status) int {
		return cmp.Or(cmp.Compare(a.AgentID, b.AgentID), cmp.Compare(a.SessionID, b.SessionID),
			cmp.Compare(a.GatewaySessionID, b.GatewaySessionID))
	})
	return out
}

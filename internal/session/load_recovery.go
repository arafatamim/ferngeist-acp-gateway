package session

import (
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
)

// maxLoadHistoryBytes bounds the per-session session/update history buffered
// for re-load recovery. A very long conversation drops its oldest frames first;
// the recent tail (which matters most for context) is preserved.
const maxLoadHistoryBytes = 8 << 20 // 8 MiB

// LoadRecovery makes session re-load work for agents that keep the session
// loaded across a client disconnect. A reconnecting client re-issues
// session/load, but such an agent rejects the duplicate with "already loaded"
// and replays no history — which would strand the client with an unrecoverable
// error (Ferngeist keeps no local transcript).
//
// To keep re-load working, LoadRecovery buffers each session's session/update
// history as it streams, remembers in-flight load request ids, caches the first
// successful load response, and — when the agent rejects a duplicate load —
// replays the buffered history followed by a synthesized success in place of
// the error. Idempotent agents never reach the error path, so their behavior is
// unchanged.
type LoadRecovery struct {
	mu           sync.Mutex
	history      map[string][]string // acpSessionId -> ordered session/update frames
	histSize     map[string]int      // acpSessionId -> approximate buffered bytes
	response     map[string][]byte   // acpSessionId -> first successful session/load response
	pendingLoads map[string]string   // load request id -> acpSessionId

	logger *slog.Logger
}

// newLoadRecovery builds a LoadRecovery bound to the given logger.
func newLoadRecovery(logger *slog.Logger) *LoadRecovery {
	return &LoadRecovery{logger: logger}
}

// OnOutbound records an in-flight session/load request so the agent's later
// response (a success to cache, or an "already loaded" error to recover from)
// can be correlated to the session it targets. Called on the client->agent path
// before the frame is forwarded.
func (r *LoadRecovery) OnOutbound(payload []byte) {
	var req struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
		Params *struct {
			SessionID string `json:"sessionId"`
		} `json:"params"`
	}
	if err := json.Unmarshal(payload, &req); err != nil || req.Method != "session/load" {
		return
	}
	if req.Params == nil || req.Params.SessionID == "" || len(req.ID) == 0 {
		return
	}
	r.mu.Lock()
	if r.pendingLoads == nil {
		r.pendingLoads = make(map[string]string)
	}
	r.pendingLoads[responseIDKey(req.ID)] = req.Params.SessionID
	r.mu.Unlock()
}

// OnFrame inspects an agent->client frame and applies the re-load recovery
// policy. It first buffers session/update history for future re-loads, then
// handles recovery. When a duplicate session/load is rejected as "already
// loaded", it returns replacement frames — the buffered session/update history
// followed by a synthesized success — and reports handled=true so the caller
// suppresses the original error. Otherwise the frame passes through unchanged.
func (r *LoadRecovery) OnFrame(line string) ([]string, bool) {
	r.bufferHistory(line)

	r.mu.Lock()
	noPending := len(r.pendingLoads) == 0
	r.mu.Unlock()
	if noPending {
		return nil, false
	}

	var resp struct {
		ID    json.RawMessage `json:"id"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &resp); err != nil || len(resp.ID) == 0 {
		return nil, false
	}
	key := responseIDKey(resp.ID)

	r.mu.Lock()
	sid, pending := r.pendingLoads[key]
	if !pending {
		r.mu.Unlock()
		return nil, false
	}
	delete(r.pendingLoads, key)
	// The entry is deleted before building the replacement. If building fails
	// (only possible on json.Marshal error — effectively impossible with a map
	// of json.RawMessage), the original error passes through and tracking is
	// silently lost. This is intentional: re-tracking a doomed entry would just
	// delay the same failure to the next response.

	// Success: remember the response shape and let it reach the client unchanged.
	if resp.Error == nil {
		if r.response == nil {
			r.response = make(map[string][]byte)
		}
		r.response[sid] = append([]byte(nil), line...)
		r.mu.Unlock()
		return nil, false
	}

	// A load error unrelated to re-load (e.g. unknown session) is surfaced as-is.
	if !strings.Contains(strings.ToLower(resp.Error.Message), "already loaded") {
		r.mu.Unlock()
		return nil, false
	}

	frames := append([]string(nil), r.history[sid]...)
	cached := r.response[sid]
	r.mu.Unlock()

	success, ok := synthesizeLoadSuccess(cached, resp.ID)
	if !ok {
		return nil, false // could not build a safe success; surface the original error
	}
	r.logger.Info("recovered already-loaded session/load by replaying buffered history",
		"acpSessionId", sid, "historyFrames", len(frames))
	return append(frames, success), true
}

// bufferHistory appends a session/update frame to its session's replay buffer,
// evicting the oldest frames once the buffer exceeds maxLoadHistoryBytes. Only
// session/update notifications are buffered; live request/response frames
// (permission prompts, rpc results) are not, so a reconnecting client never
// replays a stale, since-resolved request.
func (r *LoadRecovery) bufferHistory(line string) {
	var probe struct {
		Method string `json:"method"`
		Params *struct {
			SessionID string `json:"sessionId"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(line), &probe); err != nil ||
		probe.Method != "session/update" || probe.Params == nil || probe.Params.SessionID == "" {
		return
	}
	sid := probe.Params.SessionID

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.history == nil {
		r.history = make(map[string][]string)
		r.histSize = make(map[string]int)
	}
	r.history[sid] = append(r.history[sid], line)
	r.histSize[sid] += len(line)
	// Note: a single frame exceeding maxLoadHistoryBytes is never evicted (the
	// loop requires len > 1). The effective per-session bound is
	// max(maxLoadHistoryBytes, single-frame-size), which is acceptable because
	// one frame is the minimum needed for replay.
	for r.histSize[sid] > maxLoadHistoryBytes && len(r.history[sid]) > 1 {
		dropped := r.history[sid][0]
		r.history[sid] = r.history[sid][1:]
		r.histSize[sid] -= len(dropped)
	}
}

// responseIDKey normalizes a JSON-RPC id (number or string) into a comparable
// map key.
func responseIDKey(id json.RawMessage) string {
	return strings.TrimSpace(string(id))
}

// synthesizeLoadSuccess builds the session/load success response a reconnecting
// client expects. It reuses the cached first-load response when available
// (preserving any modes/models the agent returned), rewriting only the id;
// otherwise it falls back to a null result, which the ACP client accepts.
func synthesizeLoadSuccess(cached []byte, id json.RawMessage) (string, bool) {
	if len(cached) > 0 {
		if out, ok := rewriteResponseID(cached, id); ok {
			return string(out), true
		}
	}
	out, err := json.Marshal(map[string]json.RawMessage{
		"jsonrpc": json.RawMessage(`"2.0"`),
		"id":      id,
		"result":  json.RawMessage(`null`),
	})
	if err != nil {
		return "", false
	}
	return string(out), true
}

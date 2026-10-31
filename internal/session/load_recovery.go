package session

import (
	"encoding/json"
	"log/slog"
	"strconv"
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
// the error. The replay is announced first with a `_ferngeist/resumed`
// notification so the client is never lied to about who restored the session.
// Idempotent agents never reach the error path, so their behavior is unchanged.
type LoadRecovery struct {
	mu           sync.Mutex
	history      map[string][]historyFrame // acpSessionId -> ordered session/update frames
	histSize     map[string]int            // acpSessionId -> approximate buffered bytes
	userFrames   map[string]int            // acpSessionId -> buffered user_message_chunk frames
	trimmed      map[string]bool           // acpSessionId -> the buffer evicted oldest frames
	response     map[string][]byte         // acpSessionId -> first successful session/load response
	pendingLoads map[string]string         // load request id -> acpSessionId

	// Catch-up numbering. Every buffered update carries its session's next
	// ferngeist/seq, so a reattaching client asks only for what it missed.
	// epoch names this buffer: it dies with the pump, and a seq from another
	// epoch means nothing here.
	epoch   string
	seq     map[string]int64 // acpSessionId -> last assigned seq
	evicted map[string]int64 // acpSessionId -> highest seq evicted from history

	logger *slog.Logger
}

// historyFrame is one buffered session/update; user marks a user_message_chunk
// and seq is its ferngeist/seq, 0 for a frame that could not be numbered.
type historyFrame struct {
	line string
	user bool
	seq  int64
}

// newLoadRecovery builds a LoadRecovery bound to the given logger.
func newLoadRecovery(logger *slog.Logger) *LoadRecovery {
	return &LoadRecovery{
		logger:  logger,
		epoch:   generateID(),
		seq:     make(map[string]int64),
		evicted: make(map[string]int64),
	}
}

// OnOutbound records an in-flight session/load request so the agent's later
// response (a success to cache, or an "already loaded" error to recover from)
// can be correlated to the session it targets. Called on the client->agent path
// before the frame is forwarded.
func (r *LoadRecovery) OnOutbound(f *clientFrame) {
	if f.Method == "session/prompt" && f.Params.SessionID != "" {
		r.bufferPrompt(f.Params.SessionID, f.Params.Prompt, f.Params.Meta.ClientID)
		return
	}
	if f.Method != "session/load" || f.Params.SessionID == "" || len(f.ID) == 0 {
		return
	}
	r.mu.Lock()
	if r.pendingLoads == nil {
		r.pendingLoads = make(map[string]string)
	}
	r.pendingLoads[responseIDKey(f.ID)] = f.Params.SessionID
	r.mu.Unlock()
}

// OnFrame inspects an agent->client frame and applies the re-load recovery
// policy (history is buffered separately, by record). When a duplicate session/load is rejected as "already
// loaded", it returns replacement frames — an explicit `_ferngeist/resumed`
// notification, the buffered session/update history, and a synthesized success
// — and reports handled=true so the caller suppresses the original error.
// Otherwise the frame passes through unchanged.
func (r *LoadRecovery) OnFrame(line string, probe frameProbe, ok bool) ([]string, bool) {
	r.mu.Lock()
	noPending := len(r.pendingLoads) == 0
	r.mu.Unlock()
	if noPending {
		return nil, false
	}

	if !ok || probe.ID == nil || len(*probe.ID) == 0 {
		return nil, false
	}
	key := responseIDKey(*probe.ID)

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
	if probe.Error == nil {
		if r.response == nil {
			r.response = make(map[string][]byte)
		}
		r.response[sid] = append([]byte(nil), line...)
		r.mu.Unlock()
		return nil, false
	}

	// A load error unrelated to re-load (e.g. unknown session) is surfaced as-is.
	if !strings.Contains(strings.ToLower(probe.Error.Message), "already loaded") {
		r.mu.Unlock()
		return nil, false
	}

	// The gateway restores the session itself here: it tells the client so
	// openly, then replays the buffered history and finally answers the load
	// with a synthesized success. The client can trust the replay instead of
	// defending against a fabricated agent response (see docs/api.md).
	frames := make([]string, 0, len(r.history[sid])+1)
	frames = append(frames, resumedFrame(sid, len(r.history[sid]), r.trimmed[sid]))
	for _, frame := range r.history[sid] {
		frames = append(frames, frame.line)
	}
	cached := r.response[sid]
	r.mu.Unlock()

	success, ok := synthesizeLoadSuccess(cached, *probe.ID)
	if !ok {
		return nil, false // could not build a safe success; surface the original error
	}
	r.logger.Info("recovered already-loaded session/load by replaying buffered history",
		"acpSessionId", sid, "historyFrames", len(frames)-1)
	return append(frames, success), true
}

// record numbers a session/update frame and appends it to its session's replay
// buffer, evicting the oldest frames once the buffer exceeds
// maxLoadHistoryBytes, and returns the numbered line to forward. Only
// session/update notifications are buffered; live request/response frames
// (permission prompts, rpc results) are not, so a reconnecting client never
// replays a stale, since-resolved request. The pump calls it under clientMu,
// so a catch-up replay can neither repeat nor reorder this frame's forward.
func (r *LoadRecovery) record(line string, probe frameProbe, ok bool) string {
	if !ok ||
		probe.Method != "session/update" || probe.Params == nil || probe.Params.SessionID == "" {
		return line
	}
	sid := probe.Params.SessionID
	user := probe.Params.Update != nil && probe.Params.Update.Discriminator == "user_message_chunk"

	r.mu.Lock()
	defer r.mu.Unlock()
	seq := r.seq[sid] + 1
	stamped, numbered := withUpdateMeta(line, map[string]json.RawMessage{
		seqMetaKey: json.RawMessage(strconv.FormatInt(seq, 10)),
	})
	if !numbered {
		// Not an object update: buffered for re-load, but never numbered, so
		// the session's seqs stay gap-free.
		r.appendHistoryLocked(sid, line, user, 0)
		return line
	}
	r.seq[sid] = seq
	r.appendHistoryLocked(sid, stamped, user, seq)
	return stamped
}

// bufferPrompt records a prompt the client sent as the user_message_chunk
// updates a load replay would carry. Agents do not echo a live prompt, so
// without this a re-load replays every reply since the gateway took the session
// with no user turn between them, and the client folds them into one message.
// clientID, the prompt's own id, rides along so a reattaching client can tell
// which of its prompts arrived.
func (r *LoadRecovery) bufferPrompt(sessionID string, blocks []json.RawMessage, clientID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, block := range blocks {
		seq := r.seq[sessionID] + 1
		meta := map[string]any{seqMetaKey: seq}
		if clientID != "" {
			meta[clientIDMetaKey] = clientID
		}
		frame, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": sessionID,
				"update": map[string]any{
					"sessionUpdate": "user_message_chunk",
					"content":       block,
					"_meta":         meta,
				},
			},
		})
		if err == nil {
			r.seq[sessionID] = seq
			r.appendHistoryLocked(sessionID, string(frame), true, seq)
		}
	}
}

// catchUp returns sid's buffered updates numbered after afterSeq, oldest
// first, and the session's head seq. gap reports that the buffer cannot fill
// the client's gap: another epoch (a restarted pump), updates it needs were
// evicted, or a seq this epoch never assigned.
func (r *LoadRecovery) catchUp(sid, epoch string, afterSeq int64) (frames []string, head int64, gap bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	head = r.seq[sid]
	if epoch != r.epoch || afterSeq < r.evicted[sid] || afterSeq > head {
		return nil, head, true
	}
	for _, f := range r.history[sid] {
		if f.seq > afterSeq {
			frames = append(frames, f.line)
		}
	}
	return frames, head, false
}

// appendHistoryLocked appends one frame, then evicts the oldest past
// maxLoadHistoryBytes. After an eviction it keeps dropping up to the next user
// message, so a replay never opens partway through a reply. Caller holds mu.
func (r *LoadRecovery) appendHistoryLocked(sid, line string, user bool, seq int64) {
	if r.history == nil {
		r.history = make(map[string][]historyFrame)
		r.histSize = make(map[string]int)
		r.userFrames = make(map[string]int)
	}
	r.history[sid] = append(r.history[sid], historyFrame{line: line, user: user, seq: seq})
	r.histSize[sid] += len(line)
	if user {
		r.userFrames[sid]++
	}
	// Note: a single frame exceeding maxLoadHistoryBytes is never evicted (the
	// loop requires len > 1). The effective per-session bound is
	// max(maxLoadHistoryBytes, single-frame-size), which is acceptable because
	// one frame is the minimum needed for replay.
	evicted := false
	for r.histSize[sid] > maxLoadHistoryBytes && len(r.history[sid]) > 1 {
		r.dropOldestLocked(sid)
		evicted = true
	}
	for evicted && r.userFrames[sid] > 0 && !r.history[sid][0].user {
		r.dropOldestLocked(sid)
	}
	if evicted {
		if r.trimmed == nil {
			r.trimmed = make(map[string]bool)
		}
		r.trimmed[sid] = true
	}
}

func (r *LoadRecovery) dropOldestLocked(sid string) {
	dropped := r.history[sid][0]
	r.history[sid] = r.history[sid][1:]
	r.histSize[sid] -= len(dropped.line)
	r.evicted[sid] = max(r.evicted[sid], dropped.seq)
	if dropped.user {
		r.userFrames[sid]--
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

// resumedMethod is the gateway's ACP extension notification announcing that a
// session was restored by the gateway rather than by the agent. Underscore-
// prefixed per ACP's extension rule, like turnEndedMethod.
const resumedMethod = "_ferngeist/resumed"

// resumedFrame tells the client, in band on the ACP stream, that everything
// that follows — buffered history and then a session/load success — came from
// the gateway, not from the agent. The gateway cannot make an agent re-emit a
// session it still holds, so it synthesizes the reply; saying so lets the
// client stop treating the synthesized response as an agent contract and rely
// on the gateway's resume instead (see docs/api.md). truncated reports that
// the buffer evicted the oldest frames, so the replay starts mid-conversation;
// it is omitted when the history is intact.
func resumedFrame(acpSessionID string, replayed int, truncated bool) string {
	params := map[string]any{
		"sessionId":      acpSessionID,
		"replayedFrames": replayed,
	}
	if truncated {
		params["truncated"] = true
	}
	out, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  resumedMethod,
		"params":  params,
	})
	return string(out)
}

// rewriteResponseID returns the cached JSON-RPC response with its `id` replaced
// by id (a reconnecting client's request id), so the client correlates the
// replayed response with its own request. Returns false if the cache is not a
// valid JSON object.
func rewriteResponseID(cached []byte, id json.RawMessage) ([]byte, bool) {
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(cached, &resp); err != nil {
		return nil, false
	}
	if len(id) > 0 {
		resp["id"] = id
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return nil, false
	}
	return out, true
}

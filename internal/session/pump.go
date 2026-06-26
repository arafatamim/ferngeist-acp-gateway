package session

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/push"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
	"github.com/coder/acp-go-sdk"
	"github.com/coder/websocket"
)

// acpWebSocketWriteTimeout is the write deadline per WebSocket frame — keep in
// sync with api/server.go:acpWebSocketWriteTimeout (same value, separate context
// ownership: API package uses its own context for handler frames; pump creates
// its own for live stdout writes).
const acpWebSocketWriteTimeout = 30 * time.Second

// maxLoadHistoryBytes bounds the per-session session/update history the pump
// buffers for re-load recovery. A very long conversation drops its oldest
// frames first; the recent tail (which matters most for context) is preserved.
const maxLoadHistoryBytes = 8 << 20 // 8 MiB

// StdioPump owns the agent's stdout drain loop and provides stdin write access
// for the session. It runs independently of any WebSocket client — agent output
// is forwarded to the WebSocket when attached or discarded when no client is
// connected. Turn-complete, permission-request, and error detection fire push
// notifications regardless of client attachment: the gateway cannot tell whether
// the app is foregrounded or backgrounded (only whether a socket is attached, a
// poor proxy), so it always emits and lets the client decide whether to display.
// PushEvent carries all context for a push notification fired by the pump.
// The client decides whether to display it based on its own foreground state.
type PushEvent struct {
	SessionID    string
	AcpSessionID string
	Category     string
	Title        string
	Body         string
}

type StdioPump struct {
	pipes     *runtime.LeasedPipes
	runtimeID string
	sessionID string
	logger    *slog.Logger
	appendLog func(string, string, string)

	onPushNotification func(PushEvent)

	clientMu      sync.Mutex
	client        *websocket.Conn // current connected WebSocket, or nil
	supportsClose atomic.Bool     // set to true when agent advertises sessionCapabilities.close

	// Cached agent `initialize` response. A reconnecting client re-runs the ACP
	// handshake, but the agent process is already initialized — forwarding a
	// second `initialize` can make a strict agent error out and exit. Instead we
	// replay this cached response (with the new request's id) and never forward
	// the duplicate to the agent.
	initMu       sync.Mutex
	initResponse []byte

	// Cached ACP session id ("ses_…"), snooped from the agent's session/new
	// response. Sent as the push SessionID so it matches the id the client
	// navigates and keys ActiveChat by — the gateway's own resilient session id
	// (sessionID above) is a different namespace and never matches the client.
	acpMu        sync.Mutex
	acpSessionID string

	// Resilient re-load support. A reconnecting client re-issues session/load,
	// but an agent that keeps the session loaded across the disconnect rejects
	// the duplicate with "already loaded" and replays no history — which would
	// strand the client with an unrecoverable error (Ferngeist keeps no local
	// transcript). To keep re-load working for such agents, the pump buffers each
	// session's session/update history as it streams, remembers in-flight load
	// request ids, caches the first successful load response, and — when the
	// agent rejects a duplicate load — replays the buffered history followed by a
	// synthesized success in place of the error. Idempotent agents never reach
	// the error path, so their behavior is unchanged.
	loadMu       sync.Mutex
	loadHistory  map[string][]string // acpSessionId -> ordered session/update frames
	loadHistSize map[string]int      // acpSessionId -> approximate buffered bytes
	loadResponse map[string][]byte   // acpSessionId -> first successful session/load response
	pendingLoads map[string]string   // load request id -> acpSessionId

	lastStdoutAt time.Time // updated on each agent stdout line; used by reaper to avoid killing active agents
	lastStdoutMu sync.Mutex
}

// StdoutDrainLoop continuously reads from agent stdout and forwards frames
// to an attached WebSocket client. When no client is connected, frames are
// discarded after notable-event detection and log append. Push notifications
// fire on notable events regardless of whether a client is attached. The loop
// stops when the context is cancelled.
func (p *StdioPump) StdoutDrainLoop(ctx context.Context) {
	scanner := bufio.NewScanner(p.pipes.Stdout)

	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		line := scanner.Text()

		// Track when the agent last produced output, so the reaper can
		// distinguish abandoned sessions from actively-streaming ones.
		p.lastStdoutMu.Lock()
		p.lastStdoutAt = time.Now()
		p.lastStdoutMu.Unlock()

		if p.appendLog != nil {
			p.appendLog(p.runtimeID, "acp.stdout", line)
		}

		p.snoopInitialize(line)
		p.snoopSessionID(line)

		// Fire a push on notable events regardless of client attachment — the
		// client suppresses it when foregrounded and shows it when backgrounded
		// or killed, which is a distinction the gateway cannot make itself.
		p.checkAndNotify(line)

		// Buffer conversation history so a reconnecting client can be re-hydrated
		// even when the agent rejects a duplicate session/load as "already loaded".
		p.bufferLoadHistory(line)

		// Normally the frame is forwarded as-is. A rejected duplicate session/load
		// is replaced with the buffered history followed by a synthesized success,
		// so the client restores context instead of seeing an unrecoverable error.
		// If the recovery multi-frame write fails partway through, the client
		// sees history frames but no terminal success. The connection closes,
		// triggering the detach flow; the client's reconnect logic handles it.
		outFrames := []string{line}
		if replacements, handled := p.maybeRecoverLoad(line); handled {
			outFrames = replacements
		}

		p.clientMu.Lock()
		if p.client != nil {
			var werr error
			for _, frame := range outFrames {
				writeCtx, cancel := context.WithTimeout(context.Background(), acpWebSocketWriteTimeout)
				werr = p.client.Write(writeCtx, websocket.MessageText, []byte(frame))
				cancel()
				if werr != nil {
					break
				}
			}
			if werr != nil {
				p.logger.Warn("write to client failed", "error", werr)
				failed := p.client
				p.client = nil
				p.clientMu.Unlock()
				// Close the dead conn so the handler's read loop unblocks and runs
				// its (generation-fenced) DetachClient. Session state is owned by the
				// handler, not the pump, so there is a single detach path.
				failed.CloseNow()
				continue
			}
		}
		p.clientMu.Unlock()
	}

	// After the scanner exits (ctx cancelled, agent stdout closed, or scan error),
	// close any attached client WebSocket. This unblocks proxyWebSocketToStdio's
	// read loop so handleSessionWebSocket can clean up the connection. Without
	// this, a dead agent — whose stdout pipe has closed — leaves the WebSocket
	// open and the client waiting forever.
	p.clientMu.Lock()
	if p.client != nil {
		failed := p.client
		p.client = nil
		p.clientMu.Unlock()
		failed.CloseNow()
	} else {
		p.clientMu.Unlock()
	}
}

// bufferLoadHistory appends a session/update frame to its session's replay
// buffer, evicting the oldest frames once the buffer exceeds maxLoadHistoryBytes.
// Only session/update notifications are buffered; live request/response frames
// (permission prompts, rpc results) are not, so a reconnecting client never
// replays a stale, since-resolved request.
func (p *StdioPump) bufferLoadHistory(line string) {
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

	p.loadMu.Lock()
	defer p.loadMu.Unlock()
	if p.loadHistory == nil {
		p.loadHistory = make(map[string][]string)
		p.loadHistSize = make(map[string]int)
	}
	p.loadHistory[sid] = append(p.loadHistory[sid], line)
	p.loadHistSize[sid] += len(line)
	// Note: a single frame exceeding maxLoadHistoryBytes is never evicted
	// (the loop requires len > 1). The effective per-session bound is
	// max(maxLoadHistoryBytes, single-frame-size), which is acceptable
	// because one frame is the minimum needed for replay.
	for p.loadHistSize[sid] > maxLoadHistoryBytes && len(p.loadHistory[sid]) > 1 {
		dropped := p.loadHistory[sid][0]
		p.loadHistory[sid] = p.loadHistory[sid][1:]
		p.loadHistSize[sid] -= len(dropped)
	}
}

// maybeRecoverLoad inspects an agent->client frame correlated to an in-flight
// session/load. On a successful response it caches the response shape (so a
// future synthesized success can preserve the agent's modes/models) and lets the
// frame flow unchanged. On an "already loaded" error it returns replacement
// frames — the buffered session/update history followed by a synthesized
// success — and reports true so the caller suppresses the original error.
func (p *StdioPump) maybeRecoverLoad(line string) ([]string, bool) {
	p.loadMu.Lock()
	noPending := len(p.pendingLoads) == 0
	p.loadMu.Unlock()
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
	key := idKey(resp.ID)

	p.loadMu.Lock()
	sid, pending := p.pendingLoads[key]
	if !pending {
		p.loadMu.Unlock()
		return nil, false
	}
	delete(p.pendingLoads, key)
	// The entry is deleted before synthesizeLoadSuccess. If synthesis fails
	// (only possible on json.Marshal error — effectively impossible with a
	// map of json.RawMessage), the original error passes through and tracking
	// is silently lost. This is intentional: re-tracking a doomed entry would
	// just delay the same failure to the next response.

	// Success: remember the response shape and let it reach the client unchanged.
	if resp.Error == nil {
		if p.loadResponse == nil {
			p.loadResponse = make(map[string][]byte)
		}
		p.loadResponse[sid] = append([]byte(nil), line...)
		p.loadMu.Unlock()
		return nil, false
	}

	// A load error unrelated to re-load (e.g. unknown session) is surfaced as-is.
	if !strings.Contains(strings.ToLower(resp.Error.Message), "already loaded") {
		p.loadMu.Unlock()
		return nil, false
	}

	frames := append([]string(nil), p.loadHistory[sid]...)
	cached := p.loadResponse[sid]
	p.loadMu.Unlock()

	success, ok := synthesizeLoadSuccess(cached, resp.ID)
	if !ok {
		return nil, false // could not build a safe success; surface the original error
	}
	p.logger.Info("recovered already-loaded session/load by replaying buffered history",
		"acpSessionId", sid, "historyFrames", len(frames))
	return append(frames, success), true
}

// noteOutboundLoad records an in-flight session/load request so the agent's
// later response (a success to cache, or an "already loaded" error to recover
// from) can be correlated to the session it targets. Called on the client->agent
// path before the frame is forwarded.
func (p *StdioPump) noteOutboundLoad(payload []byte) {
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
	p.loadMu.Lock()
	if p.pendingLoads == nil {
		p.pendingLoads = make(map[string]string)
	}
	p.pendingLoads[idKey(req.ID)] = req.Params.SessionID
	p.loadMu.Unlock()
}

// idKey normalizes a JSON-RPC id (number or string) into a comparable map key.
func idKey(id json.RawMessage) string {
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

// checkAndNotify fires a push notification when the agent emits a notable
// event (turn complete, permission request, or error). The client decides
// whether to display it based on its own foreground/background state.
func (p *StdioPump) checkAndNotify(line string) {
	if p.onPushNotification == nil {
		return
	}
	switch {
	case isTurnComplete([]byte(line)):
		p.onPushNotification(PushEvent{SessionID: p.sessionID, AcpSessionID: p.AcpSessionID(), Category: push.CategoryTurnComplete, Title: "Turn Complete", Body: "Your agent has finished processing."})
	case isPermissionRequest([]byte(line)):
		p.onPushNotification(PushEvent{SessionID: p.sessionID, AcpSessionID: p.AcpSessionID(), Category: push.CategoryPermissionRequest, Title: "Permission Required", Body: "Your agent needs approval to run a tool."})
	case isJSONRPCError([]byte(line)):
		p.onPushNotification(PushEvent{SessionID: p.sessionID, AcpSessionID: p.AcpSessionID(), Category: push.CategoryError, Title: "Agent Error", Body: "Your agent encountered an unexpected error."})
	}
}

// snoopInitialize inspects an outbound frame for the agent's `initialize`
// response. When found it caches the raw response (for replay to reconnecting
// clients) and records whether the agent advertises sessionCapabilities.close.
// A response is identified by the presence of result.protocolVersion, which only
// initialize responses carry.
func (p *StdioPump) snoopInitialize(line string) {
	if p.initResponseCached() {
		return
	}
	var probe struct {
		Result *struct {
			ProtocolVersion *int `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(line), &probe); err != nil ||
		probe.Result == nil || probe.Result.ProtocolVersion == nil {
		return
	}

	p.initMu.Lock()
	p.initResponse = append([]byte(nil), line...)
	p.initMu.Unlock()

	var typed struct {
		Result *acp.InitializeResponse `json:"result"`
	}
	if err := json.Unmarshal([]byte(line), &typed); err == nil &&
		typed.Result != nil &&
		typed.Result.AgentCapabilities.SessionCapabilities.Close != nil {
		p.supportsClose.Store(true)
	}
}

func (p *StdioPump) initResponseCached() bool {
	p.initMu.Lock()
	defer p.initMu.Unlock()
	return p.initResponse != nil
}

// snoopSessionID inspects an outbound frame for the agent's `session/new`
// response and caches the ACP session id it returns. Only the session/new
// response carries a top-level result.sessionId, so this never misfires on
// other frames. The id is captured once and reused for the session's lifetime;
// it is sent as the push SessionID so notifications match the id the client
// navigates and suppresses by.
func (p *StdioPump) snoopSessionID(line string) {
	p.acpMu.Lock()
	cached := p.acpSessionID != ""
	p.acpMu.Unlock()
	if cached {
		return
	}
	var probe struct {
		Result *struct {
			SessionID string `json:"sessionId"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(line), &probe); err != nil ||
		probe.Result == nil || probe.Result.SessionID == "" {
		return
	}
	p.acpMu.Lock()
	p.acpSessionID = probe.Result.SessionID
	p.acpMu.Unlock()
}

// AcpSessionID returns the snooped ACP session id, or "" if no session/new
// response has been observed yet.
func (p *StdioPump) AcpSessionID() string {
	p.acpMu.Lock()
	defer p.acpMu.Unlock()
	return p.acpSessionID
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

// MaybeReplayInitialize intercepts a client `initialize` request. If the agent
// has already been initialized (its response is cached), it answers the client
// directly with that cached response — rewritten to carry the request's id — and
// returns true so the caller does not forward a duplicate `initialize` to the
// agent. It returns false for non-initialize frames, or for the first
// `initialize` (no cache yet), which must reach the agent normally.
func (p *StdioPump) MaybeReplayInitialize(payload []byte) bool {
	var req struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(payload, &req); err != nil || req.Method != "initialize" {
		return false
	}

	p.initMu.Lock()
	cached := p.initResponse
	p.initMu.Unlock()
	if cached == nil {
		return false
	}

	out, ok := rewriteResponseID(cached, req.ID)
	if !ok {
		return false // malformed cache: fall back to forwarding to the agent
	}

	p.clientMu.Lock()
	client := p.client
	p.clientMu.Unlock()
	if client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), acpWebSocketWriteTimeout)
		err := client.Write(ctx, websocket.MessageText, out)
		cancel()
		if err != nil {
			p.logger.Warn("replay initialize write failed", "error", err)
		}
	}
	return true
}

// isTurnComplete checks if a stdout line is a JSON-RPC response with a non-empty
// stopReason. Any terminal stop reason (end_turn, stop, error, etc.) triggers
// the push notification callback. Uses acp.PromptResponse for typed access to
// the StopReason field.
func isTurnComplete(data []byte) bool {
	var msg struct {
		Result *acp.PromptResponse `json:"result"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return false
	}
	return msg.Result != nil && msg.Result.StopReason != ""
}

// isPermissionRequest checks if a stdout line is a session/request_permission
// notification. The agent sends this during a turn when it needs user approval
// before executing a tool. Detected here so a push notification can be fired.
// Uses acp.AgentNotification for typed method name access.
func isPermissionRequest(data []byte) bool {
	var n acp.AgentNotification
	if err := json.Unmarshal(data, &n); err != nil {
		return false
	}
	return n.Method == "session/request_permission"
}

// isJSONRPCError checks if a stdout line is a JSON-RPC error response (top-level
// error field instead of result). This catches agent-side failures that aren't
// represented as a stopReason — e.g. uncaught exceptions, protocol violations.
// Uses acp.AgentResponse which validates id+error presence per JSON-RPC 2.0.
func isJSONRPCError(data []byte) bool {
	var resp acp.AgentResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return false
	}
	return resp.Error != nil
}

func (p *StdioPump) WriteToAgent(payload []byte) error {
	p.snoopInboundSessionID(payload)
	// Record session/load requests so the pump can recover from an "already
	// loaded" rejection by replaying buffered history (see maybeRecoverLoad).
	p.noteOutboundLoad(payload)
	return p.pipes.WriteToAgent(payload)
}

// snoopInboundSessionID captures the ACP session id from a client→agent frame's
// params.sessionId (session/prompt, session/load, session/cancel, …). The
// outbound snoop only sees session/new responses, so on a resilient reconnect —
// where the client restores context via session/load and the agent never
// re-emits session/new — it would never observe the id and the push SessionID
// would be empty. The inbound frames always carry it, so this fills the gap.
// Only client frames reach this path; the gateway's own session/close is written
// via the leased pipes directly, so it never contaminates the cache with the
// resilient (non-ACP) session id.
func (p *StdioPump) snoopInboundSessionID(payload []byte) {
	p.acpMu.Lock()
	cached := p.acpSessionID != ""
	p.acpMu.Unlock()
	if cached {
		return
	}
	var probe struct {
		Params *struct {
			SessionID string `json:"sessionId"`
		} `json:"params"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil ||
		probe.Params == nil || probe.Params.SessionID == "" {
		return
	}
	p.acpMu.Lock()
	p.acpSessionID = probe.Params.SessionID
	p.acpMu.Unlock()
}

func (p *StdioPump) SetClient(conn *websocket.Conn) {
	p.clientMu.Lock()
	defer p.clientMu.Unlock()
	p.client = conn
}

func (p *StdioPump) ClearClient() {
	p.clientMu.Lock()
	defer p.clientMu.Unlock()
	p.client = nil
}

func (p *StdioPump) SupportsClose() bool {
	return p.supportsClose.Load()
}

// LastStdoutAt returns the timestamp of the agent's most recent stdout line.
// Zero time means the pump has never received any output — the reaper falls
// back to DisconnectedAt in that case.
func (p *StdioPump) LastStdoutAt() time.Time {
	p.lastStdoutMu.Lock()
	defer p.lastStdoutMu.Unlock()
	return p.lastStdoutAt
}

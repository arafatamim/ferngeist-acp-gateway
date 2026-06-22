package session

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
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
	SessionID   string
	AcpSessionID string
	Category    string
	Title       string
	Body        string
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

		p.clientMu.Lock()
		if p.client != nil {
			writeCtx, cancel := context.WithTimeout(context.Background(), acpWebSocketWriteTimeout)
			err := p.client.Write(writeCtx, websocket.MessageText, []byte(line))
			cancel()
			if err != nil {
				p.logger.Warn("write to client failed", "error", err)
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

package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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

// maxAgentFrameBytes is the pathological-agent safety bound on a single agent
// stdout frame. Real ACP frames are far smaller; this only guards against a
// misbehaving agent emitting an unbounded line (which would otherwise grow the
// accumulation buffer without limit). It is deliberately much larger than the
// old scanner cap so legitimate large frames (chat history, tool output) pass.
const maxAgentFrameBytes = 64 << 20 // 64 MiB

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
	agentID   string
	logger    *slog.Logger
	appendLog func(string, string, string)

	onPushNotification func(PushEvent)

	clientMu      sync.Mutex
	client        *websocket.Conn // current connected WebSocket, or nil
	connGen       int64           // bumped on every Attach; fences stale Bind/Detach from evicted conns
	supportsClose atomic.Bool     // set when agent advertises sessionCapabilities.close

	// Per-client write queue. The drain loop enqueues outbound frames and
	// returns immediately — it must never block on a client write, or a slow
	// client would stall agent stdout draining and the session would look
	// dead. A dedicated writer goroutine (owned by the bound client) drains
	// the queue with per-frame timeouts. Guarded by clientMu; writerCtx is
	// cancelled on takeover/detach to stop the goroutine, and writerDone is
	// closed when the goroutine exits (after flushing queued frames).
	writerCh     chan string
	writerCtx    context.Context
	writerCancel context.CancelFunc
	writerDone   chan struct{}
	closed       bool // set when the drain loop exits; no new Bind may attach

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

	// ACP session working directory ("/abs/project/path"), snooped from the
	// client's session/new request params.cwd. This is the project directory the
	// user opened in the agent — the anchor for the gateway's file/diff/status
	// workspace endpoints. Empty until the client issues session/new.
	acpCwd string

	// Resilient re-load support: buffers session/update history and recovers
	// "already loaded" rejections from agents that keep the session loaded
	// across a disconnect. See LoadRecovery. Nil when disabled.
	loadRecovery *LoadRecovery

	lastStdoutAt time.Time // updated on each agent stdout line; used by reaper to avoid killing active agents
	lastStdoutMu sync.Mutex

	// ProgressInterval is the minimum time between non-terminal progress pushes.
	// 0 means push every new/different tool with no throttle.
	ProgressInterval time.Duration

	// Throttle/dedupe state for live progress pushes. Guarded by progressMu.
	progressMu           sync.Mutex
	lastProgressPush     time.Time
	lastProgressToolCall string
	lastProgressSummary  string

	// frameLog optionally records every raw ACP frame (in + out) as
	// newline-delimited JSON in per-agent files. nil when disabled.
	frameLog *frameLogManager
}

// StdoutDrainLoop continuously reads from agent stdout and forwards frames
// to an attached WebSocket client. When no client is connected, frames are
// discarded after notable-event detection and log append. Push notifications
// fire on notable events regardless of whether a client is attached. The loop
// stops when the context is cancelled.
func (p *StdioPump) StdoutDrainLoop(ctx context.Context) {
	// Streaming newline reader instead of bufio.Scanner. A Scanner has a hard
	// per-line cap (default 64 KiB; 1 MiB here) and silently exits with
	// ErrTooLong when an agent emits a larger single line — a big session/update
	// or session/load response with chat history / tool output. That silent
	// exit closed the client WebSocket with no log, making the agent look dead
	// ("connection dropped while loading a session"). A Reader accumulates
	// lines across reads, so any practical frame passes through; the only cap
	// is a pathological-agent safety bound far above real traffic.
	reader := bufio.NewReader(p.pipes.Stdout)
	var line strings.Builder
	// overCap marks that the current line exceeded the safety cap; the rest of
	// the line is discarded until its terminating newline so an oversized line
	// is never split into a bogus partial frame.
	overCap := false

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		fragment, err := reader.ReadString('\n')
		if fragment != "" {
			line.WriteString(fragment)
		}
		if err != nil {
			// io.EOF with a non-empty buffer means the agent closed stdout
			// mid-line (or exited); process the tail as a final frame.
			if line.Len() > 0 {
				p.handleStdoutLine(strings.TrimSuffix(line.String(), "\n"))
				line.Reset()
			}
			if !errors.Is(err, io.EOF) {
				p.logger.Warn("agent stdout read failed",
					"runtime_id", p.runtimeID, "error", err)
			}
			break
		}
		if line.Len() > maxAgentFrameBytes && !overCap {
			p.logger.Warn("agent stdout frame exceeds safety cap; dropping",
				"runtime_id", p.runtimeID, "bytes", line.Len())
			overCap = true
		}
		if strings.HasSuffix(fragment, "\n") {
			// Strip the delimiter so frames match the old scanner's Text()
			// semantics (no trailing newline in log/history/snoop paths).
			frame := strings.TrimSuffix(line.String(), "\n")
			if !overCap {
				p.handleStdoutLine(frame)
			}
			line.Reset()
			overCap = false
		}
	}

	// After the loop exits (ctx cancelled, agent stdout closed, or read error),
	// stop the writer and wait for it to flush its queue, then close the
	// attached client WebSocket. This unblocks proxyWebSocketToStdio's read
	// loop so handleSessionWebSocket can clean up the connection. Without this,
	// a dead agent — whose stdout pipe has closed — leaves the WebSocket open
	// and the client waiting forever.
	p.clientMu.Lock()
	conn := p.client
	done := p.writerDone
	p.closed = true
	p.stopWriterLocked()
	p.clientMu.Unlock()
	if done != nil {
		// Bound the wait: a stuck client must not hang shutdown, but normally
		// the flush completes in milliseconds.
		select {
		case <-done:
		case <-time.After(acpWebSocketWriteTimeout + time.Second):
			p.logger.Warn("timed out waiting for client write flush")
		}
	}
	if conn != nil {
		conn.CloseNow()
	}
}

func (p *StdioPump) stopWriterLocked() {
	if p.writerCancel != nil {
		p.writerCancel()
		p.writerCancel = nil
	}
	p.writerCh = nil
	p.writerCtx = nil
	p.writerDone = nil
}

// handleStdoutLine processes a single complete agent stdout frame: log append,
// session snooping, notification, history buffering, and forwarding to the
// attached WebSocket (with load-recovery replacement).
func (p *StdioPump) handleStdoutLine(line string) {
	// Track when the agent last produced output, so the reaper can
	// distinguish abandoned sessions from actively-streaming ones.
	p.lastStdoutMu.Lock()
	p.lastStdoutAt = time.Now()
	p.lastStdoutMu.Unlock()

	if p.appendLog != nil {
		p.appendLog(p.runtimeID, "acp.stdout", line)
	}
	if p.frameLog != nil {
		p.frameLog.append(p.agentID, p.runtimeID, p.sessionID, "out", []byte(line))
	}

	p.snoopInitialize(line)
	p.snoopSessionID(line)

	// Fire a push on notable events regardless of client attachment — the
	// client suppresses it when foregrounded and shows it when backgrounded
	// or killed, which is a distinction the gateway cannot make itself.
	p.checkAndNotify(line)

	// Buffer conversation history so a reconnecting client can be re-hydrated
	// even when the agent rejects a duplicate session/load as "already loaded".
	// Normally the frame is forwarded as-is. A rejected duplicate session/load
	// is replaced with the buffered history followed by a synthesized success,
	// so the client restores context instead of seeing an unrecoverable error.
	// If the recovery multi-frame write fails partway through, the client
	// sees history frames but no terminal success. The connection closes,
	// triggering the detach flow; the client's reconnect logic handles it.
	outFrames := []string{line}
	if p.loadRecovery != nil {
		if replacements, handled := p.loadRecovery.OnFrame(line); handled {
			outFrames = replacements
		}
	}

	// Enqueue the frames to the bound client's writer goroutine and return
	// immediately — the drain loop must never block on a client write, or a
	// slow client would stall agent stdout draining (agent pipe fills, agent
	// stalls, session looks dead) and takeover would be blocked. The writer
	// goroutine applies the per-frame timeout and closes the conn on failure;
	// if the queue is full (client far behind), frames are dropped — a slow
	// client must not backpressure the agent.
	p.clientMu.Lock()
	if p.client != nil && p.writerCh != nil {
		for _, frame := range outFrames {
			select {
			case p.writerCh <- frame:
			default:
				// Queue full: client not keeping up. Drop rather than block.
			}
		}
	}
	p.clientMu.Unlock()
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
	default:
		if ev := isProgressEvent(line); ev != nil {
			p.maybeNotifyProgress(ev)
		}
	}
}

// maybeNotifyProgress fires a progress push, throttled by ProgressInterval and
// deduplicated against the previous tool call + summary. Terminal events
// (completed/failed) always push immediately so the user sees the boundary.
func (p *StdioPump) maybeNotifyProgress(ev *progressEvent) {
	p.progressMu.Lock()
	defer p.progressMu.Unlock()

	if !ev.terminal {
		// Dedupe: same tool call, same summary → skip.
		if ev.toolCallID == p.lastProgressToolCall && ev.summary == p.lastProgressSummary {
			return
		}
		// Throttle: respect the minimum interval for non-terminal updates.
		if p.ProgressInterval > 0 && time.Since(p.lastProgressPush) < p.ProgressInterval {
			return
		}
	}
	p.lastProgressPush = time.Now()
	p.lastProgressToolCall = ev.toolCallID
	p.lastProgressSummary = ev.summary
	p.onPushNotification(PushEvent{
		SessionID:    p.sessionID,
		AcpSessionID: p.AcpSessionID(),
		Category:     push.CategoryProgress,
		Title:        "Agent working",
		Body:         ev.summary,
	})
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

// progressEvent is a parsed live-progress signal from an agent's session/update
// notification. summary is the human-readable line to push; terminal is true
// when the tool call completed or failed (which should push immediately, not
// throttled).
type progressEvent struct {
	summary    string
	terminal   bool
	toolCallID string
}

// isProgressEvent parses a stdout line for an ACP session/update tool_call or
// tool_call_update event and returns a progressEvent, or nil when the line is
// not a progress-relevant session/update frame. Summary resolution order:
// required `title` (tool_call) or optional `title` (tool_call_update), then the
// first text content block, then a verb derived from `kind`. A frame with no
// resolvable summary, or a non-tool session/update variant, yields nil.
func isProgressEvent(line string) *progressEvent {
	var probe struct {
		Method string `json:"method"`
		Params *struct {
			Update *struct {
				Discriminator string `json:"sessionUpdate"`
				ToolCallID    string `json:"toolCallId"`
				Status        string `json:"status"`
				Title         string `json:"title"`
				Kind          string `json:"kind"`
				Content       []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(line), &probe); err != nil ||
		probe.Method != "session/update" ||
		probe.Params == nil || probe.Params.Update == nil {
		return nil
	}
	u := probe.Params.Update
	switch u.Discriminator {
	case "tool_call", "tool_call_update":
	default:
		return nil
	}
	if u.Status != "in_progress" && u.Status != "completed" && u.Status != "failed" {
		return nil
	}
	summary := u.Title
	if summary == "" && len(u.Content) > 0 {
		summary = u.Content[0].Text
	}
	if summary == "" {
		summary = verbForKind(u.Kind)
		if summary == "" {
			return nil
		}
	}
	return &progressEvent{
		summary:    summary,
		terminal:   u.Status == "completed" || u.Status == "failed",
		toolCallID: u.ToolCallID,
	}
}

// verbForKind maps a ToolKind to a generic progress verb, or "" for unknown/other.
func verbForKind(kind string) string {
	switch kind {
	case "read":
		return "Reading files"
	case "edit":
		return "Editing files"
	case "delete":
		return "Removing files"
	case "move":
		return "Moving files"
	case "search":
		return "Searching"
	case "execute":
		return "Running a command"
	case "think":
		return "Thinking"
	case "fetch":
		return "Fetching data"
	case "switch_mode":
		return "Switching mode"
	default:
		return ""
	}
}

func (p *StdioPump) WriteToAgent(payload []byte) error {
	p.snoopInboundSessionID(payload)
	p.snoopInboundCwd(payload)
	// Record session/load requests so the pump can recover from an "already
	// loaded" rejection by replaying buffered history (see LoadRecovery).
	if p.loadRecovery != nil {
		p.loadRecovery.OnOutbound(payload)
	}
	if p.frameLog != nil {
		p.frameLog.append(p.agentID, p.runtimeID, p.sessionID, "in", payload)
	}
	return p.pipes.WriteToAgent(payload)
}

// WriteSessionClose sends the agent a session/close request during session
// teardown. It is the only stdin write path that bypasses the client-snooping
// in WriteToAgent — the frame is gateway-originated (not a client frame, so it
// must never contaminate the acpSessionId/acpCwd caches) and it is always the
// final frame before the process is killed. It still passes through the frame
// log so the teardown handshake is auditable.
func (p *StdioPump) WriteSessionClose(payload []byte) error {
	if p.frameLog != nil {
		p.frameLog.append(p.agentID, p.runtimeID, p.sessionID, "in", payload)
	}
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

// snoopInboundCwd captures the ACP session working directory from a
// client->agent session/new or session/load request's params.cwd. Both methods
// carry cwd (session/load is what Ferngeist sends when resuming a session);
// unlike sessionId (present on every session-scoped frame), cwd is set once when
// the client opens the project. Re-captured on each such request so a project
// switch updates it.
func (p *StdioPump) snoopInboundCwd(payload []byte) {
	// Hot path: every client->agent frame passes through here, but only
	// session/new and session/load requests carry cwd. A cheap substring scan
	// avoids a full JSON parse per frame.
	if !bytes.Contains(payload, []byte("session/new")) &&
		!bytes.Contains(payload, []byte("session/load")) {
		return
	}
	var probe struct {
		Method string `json:"method"`
		Params *struct {
			Cwd string `json:"cwd"`
		} `json:"params"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil ||
		(probe.Method != "session/new" && probe.Method != "session/load") ||
		probe.Params == nil || probe.Params.Cwd == "" {
		return
	}
	p.acpMu.Lock()
	p.acpCwd = probe.Params.Cwd
	p.acpMu.Unlock()
}

// AcpCwd returns the snooped ACP session working directory, or "" if the client
// has not issued session/new yet.
func (p *StdioPump) AcpCwd() string {
	p.acpMu.Lock()
	defer p.acpMu.Unlock()
	return p.acpCwd
}

// Attach claims the pump for a new client: bumps the connection generation and
// clears the current client so no frames are written during the WebSocket
// upgrade. It returns the generation the caller must pass to Bind and Detach.
// Any previously-bound connection is evicted (closed) after the lock is
// released — once the pointer is cleared no write path can reach it, so the
// close cannot race a live forward.
func (p *StdioPump) Attach() int64 {
	p.clientMu.Lock()
	evict := p.client
	p.client = nil
	p.stopWriterLocked()
	p.connGen++
	gen := p.connGen
	p.clientMu.Unlock()
	if evict != nil {
		evict.CloseNow()
	}
	return gen
}

// Bind attaches conn to the pump iff gen is still the current generation and
// the pump has not closed. It returns false when a newer Attach has superseded
// this generation or the drain loop has exited (session closing), in which
// case the caller must discard conn. On success it spawns the per-client
// writer goroutine that drains outbound frames to conn.
func (p *StdioPump) Bind(conn *websocket.Conn, gen int64) bool {
	p.clientMu.Lock()
	defer p.clientMu.Unlock()
	if p.closed || p.connGen != gen {
		return false
	}
	p.client = conn
	p.startWriterLocked(conn)
	return true
}

// Detach clears the current client iff gen is still the current generation. A
// stale detach (from a superseded connection's handler) is a no-op and returns
// false, so an evicted connection cannot clobber the connection that replaced
// it. The pump keeps running regardless.
func (p *StdioPump) Detach(gen int64) bool {
	p.clientMu.Lock()
	defer p.clientMu.Unlock()
	if p.connGen != gen {
		return false
	}
	p.client = nil
	p.stopWriterLocked()
	return true
}

// startWriterLocked spawns the writer goroutine for conn. Caller holds clientMu.
func (p *StdioPump) startWriterLocked(conn *websocket.Conn) {
	ctx, cancel := context.WithCancel(context.Background())
	p.writerCtx = ctx
	p.writerCancel = cancel
	p.writerCh = make(chan string, 64)
	done := make(chan struct{})
	p.writerDone = done
	go func() {
		defer close(done)
		p.clientWriterLoop(ctx, conn)
	}()
}

// clientWriterLoop drains the outbound queue to conn. It runs for the lifetime
// of one bound client: a write failure or a cancelled context stops it. The
// queue is bounded, so the drain loop never blocks on it; a client that cannot
// keep up has frames dropped (see handleStdoutLine).
func (p *StdioPump) clientWriterLoop(ctx context.Context, conn *websocket.Conn) {
	ch := p.writerCh // captured at start; stopWriterLocked nil's the field
	for {
		select {
		case <-ctx.Done():
			// Agent gone or client detached: flush whatever is still queued so
			// the client receives the tail before the conn closes.
			for {
				select {
				case frame := <-ch:
					writeCtx, cancel := context.WithTimeout(context.Background(), acpWebSocketWriteTimeout)
					_ = conn.Write(writeCtx, websocket.MessageText, []byte(frame))
					cancel()
				default:
					return
				}
			}
		case frame := <-ch:
			writeCtx, cancel := context.WithTimeout(context.Background(), acpWebSocketWriteTimeout)
			err := conn.Write(writeCtx, websocket.MessageText, []byte(frame))
			cancel()
			if err != nil {
				p.logger.Warn("write to client failed", "error", err)
				// The client is dead/slow. Clear it (only if it's still the
				// same conn — a takeover may have replaced it) and close it so
				// the handler's read loop unblocks and runs its
				// (generation-fenced) DetachClient. Session state is owned by
				// the handler, not the pump, so there is a single detach path.
				p.clientMu.Lock()
				if p.client == conn {
					p.client = nil
					p.stopWriterLocked()
				}
				p.clientMu.Unlock()
				conn.CloseNow()
				return
			}
		}
	}
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

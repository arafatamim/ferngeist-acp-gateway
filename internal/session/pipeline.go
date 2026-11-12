package session

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/turns"
	"github.com/coder/acp-go-sdk"
	"github.com/coder/websocket"
)

// This file holds the frame pipeline every agent stdout line runs through:
// one parse, then a fixed, ordered list of stages. The ACP state the stages
// maintain lives in acpstate.go.

// frameProbe is the single-parse view of an ACP frame. The hot path previously
// paid 5-6 json.Unmarshal scans per stdout line (turn activity, turn complete,
// permission request, JSON-RPC error, progress event, load-recovery buffering),
// each with its own []byte(line) copy. Every stage now reads this one parse, so
// per-frame CPU/GC is one scan + one copy; rare paths (typed initialize
// capabilities, load-success synthesis) keep their second parse off the hot
// path.
type frameProbe struct {
	Method string           `json:"method"`
	ID     *json.RawMessage `json:"id"`
	Result *struct {
		ProtocolVersion *int           `json:"protocolVersion"`
		SessionID       string         `json:"sessionId"`
		StopReason      acp.StopReason `json:"stopReason"`
		Usage           *acp.Usage     `json:"usage"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	Params *struct {
		SessionID string `json:"sessionId"`
		// The client request a request-scoped elicitation/create is tied to.
		RequestID json.RawMessage `json:"requestId"`
		Update    *struct {
			Discriminator string `json:"sessionUpdate"`
			ToolCallID    string `json:"toolCallId"`
			Status        string `json:"status"`
			Kind          string `json:"kind"`
			// No content or title: no stage reads them, and unmarshalling
			// them would copy every chunk's text and tool output again.
		} `json:"update"`
	} `json:"params"`
}

// parseFrameProbe parses one frame once. ok=false means invalid JSON; callers
// treat it as opaque (no stage claims it, it forwards unchanged).
func parseFrameProbe(data []byte) (frameProbe, bool) {
	var probe frameProbe
	if err := json.Unmarshal(data, &probe); err != nil {
		return frameProbe{}, false
	}
	return probe, true
}

// frame is one agent stdout frame plus what the pipeline decided about it.
type frame struct {
	// raw is the frame's bytes: one copy, shared by every stage.
	raw []byte
	// line is the frame as it will be forwarded: a stage may replace it.
	line  string
	probe frameProbe
	ok    bool
	// origin is the client request this frame replies to, if any, and isReply
	// whether it is a reply at all.
	origin  pendingRequest
	isReply bool
	// out is what the client receives: one frame normally, a resume replay
	// burst after a rejected duplicate session/load.
	out []string
	// replaced marks a frame this pipeline rewrote itself (swallowed-failure
	// error), which has already raised its own notification.
	replaced bool
	// reject marks a frame whose BeforeState chain rejected it. Canonical stages
	// still run; forward applies the rejection instead of delivering the body.
	reject bool
	// resumed marks the out burst as a gateway-synthesized resume replay: the
	// load recovery stage replaced the frame with buffered history plus a
	// success it built itself.
	resumed bool
}

// handleStdoutLine processes one complete agent stdout frame. The frame is
// parsed once and passed through a fixed ordered list of stages:
//
//	request ids → ACP state → turn outcome → load recovery → session list → notifications → forward
//
// Every stage reads the same parse, so no two stages can disagree about what
// the frame is, and each stage is a plain function call rather than a hook.
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

	f := &frame{line: line, raw: []byte(line), out: []string{line}}
	f.probe, f.ok = parseFrameProbe(f.raw)
	// Request ids: which client request (if any) this frame answers. Read-only,
	// so later stages can use the answer without consuming it.
	f.origin, f.isReply = p.reqIDs.peek(f.probe)
	p.applyBeforeState(f)
	if f.reject && f.probe.Method != "" && f.probe.ID != nil {
		p.answerRejectedAgentRequest(f)
		return
	}

	p.observeACPState(f)
	p.recordOutcome(f)
	p.applyLoadRecovery(f)
	p.acp.acceptSession(f)
	p.notifyFrame(f)
	if !p.forwardDecorated(f) {
		p.forward(f)
	}
}

// observeACPState is the ACP-state stage: it feeds the frame into acpState
// (initialize cache, agent name, session id, cwds, turn state) and, when the
// agent swallowed a failed LLM call, replaces the frame with a JSON-RPC error
// so the client sees the turn fail instead of an empty success.
func (p *StdioPump) observeACPState(f *frame) {
	if f.isReply && f.origin.method == "session/prompt" && f.probe.Error != nil {
		p.forgetPrompt(f.origin.promptSession, f.origin.promptClientID, responseIDKey(*f.probe.ID))
	}
	p.acp.observeInitialize(f.probe, f.raw)
	p.acp.observeSessionID(f.probe)
	p.acp.bindPendingCwd(f.probe)
	replacement, handled := p.observeTurn(f.probe, f.origin, f.isReply)
	if !handled {
		return
	}
	f.line, f.raw, f.out, f.replaced = replacement, []byte(replacement), []string{replacement}, true
	// The replacement is a synthesized frame; re-probe it cheaply so the
	// stages below see the frame actually forwarded.
	if rp, ok := parseFrameProbe(f.raw); ok {
		f.probe, f.ok = rp, true
	}
}

// recordOutcome is the turn-outcome stage: a reply to a session/prompt, from
// any connection generation, reports how the turn ended. It runs after the
// ACP-state stage so a swallowed failure is judged as the error it became.
func (p *StdioPump) recordOutcome(f *frame) {
	if p.onTurnFinished == nil || !f.isReply || f.origin.promptSession == "" {
		return
	}
	var reason acp.StopReason
	if f.probe.Result != nil {
		reason = f.probe.Result.StopReason
	}
	st := p.turnStatus(f.origin.promptSession, f.origin.startedAt, turns.Outcome(f.probe.Error != nil, reason))
	if f.probe.Error == nil {
		st.StopReason = reason
	}
	p.onTurnFinished(st)
}

// applyLoadRecovery is the load-recovery stage: it buffers conversation history
// so a reconnecting client can be re-hydrated even when the agent rejects a
// duplicate session/load as "already loaded". Normally the frame passes through
// as-is; a rejected duplicate is replaced by the buffered history and a
// synthesized success, opened by an explicit `_ferngeist/resumed` notification
// (see LoadRecovery and docs/api.md).
func (p *StdioPump) applyLoadRecovery(f *frame) {
	if p.loadRecovery == nil {
		return
	}
	if replacements, handled := p.loadRecovery.OnFrame(f.line, f.probe, f.ok); handled {
		f.out = replacements
		f.resumed = true
	}
}

// decoratorTimeout bounds one decorator chain. Only the decorated reply waits;
// the stdout loop keeps draining other agent frames.
const decoratorTimeout = 2 * time.Second

// decoratorReads lets shutdown cancel decoration while the writer still accepts
// the agent's unmodified replies, then wait before its final flush.
type decoratorReads struct {
	ctx    context.Context
	cancel context.CancelFunc
	sync.WaitGroup
}

// forwardDecorated runs the decorators registered for this reply. It reports
// whether it owns the forward: if any entry does I/O (FinalOnly), the whole
// chain is deferred to one worker, so a slow decorator never stalls the stdout
// loop and registration order still holds. A chain that cannot run (no client,
// superseded generation, shutting down) leaves the reply untouched.
func (p *StdioPump) forwardDecorated(f *frame) bool {
	chain := p.decoratorsFor(f)
	if len(chain) == 0 {
		return false
	}
	if !defersDecoration(chain) {
		p.applyDecorators(p.drainContext(), f, chain)
		return false
	}
	p.clientMu.Lock()
	reads := p.inFlight
	if p.closed || reads == nil || reads.ctx.Err() != nil || f.origin.gen != p.connGen {
		p.clientMu.Unlock()
		return false
	}
	reads.Add(1)
	p.clientMu.Unlock()
	go func() {
		defer reads.Done()
		p.applyDecorators(reads.ctx, f, chain)
		// forward still restores the request id and fences a superseded client.
		p.forward(f)
	}()
	return true
}

// decoratorsFor returns the decorators that apply to this reply, in
// registration order. A FinalOnly decorator applies only to a final page.
func (p *StdioPump) decoratorsFor(f *frame) []ReplyDecorator {
	if !f.isReply || f.probe.Error != nil || len(f.out) == 0 {
		return nil
	}
	final := finalPage(f.out[len(f.out)-1])
	var chain []ReplyDecorator
	for _, decorator := range p.replyDecorators {
		if decorator.Method != f.origin.method {
			continue
		}
		if decorator.FinalOnly && !final {
			continue
		}
		chain = append(chain, decorator)
	}
	return chain
}

// defersDecoration reports whether any decorator in the chain does I/O.
func defersDecoration(chain []ReplyDecorator) bool {
	for _, decorator := range chain {
		if decorator.FinalOnly {
			return true
		}
	}
	return false
}

// applyDecorators runs the chain in order over the reply's last page. A nil
// result leaves the bytes alone; an error stops the chain, and the reply is
// forwarded as the previous decorator left it.
func (p *StdioPump) applyDecorators(ctx context.Context, f *frame, chain []ReplyDecorator) {
	last := len(f.out) - 1
	if last < 0 || len(chain) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, decoratorTimeout)
	defer cancel()
	reply := []byte(f.out[last])
	changed := false
	for _, decorator := range chain {
		decorated, err := decorator.Decorate(ctx, p.agentID, f.origin.cwd, reply)
		if err != nil {
			p.logger.Debug("reply decoration failed", "agent_id", p.agentID, "method", decorator.Method, "error", err)
			break
		}
		if decorated == nil {
			continue
		}
		reply, changed = decorated, true
	}
	if !changed {
		return
	}
	f.out[last] = string(reply)
	if last == 0 {
		f.line = f.out[last]
	}
}

// drainContext is the pump's base context, or the stdout loop's when a test
// built the pump without one, or a background context otherwise.
func (p *StdioPump) drainContext() context.Context {
	if p.ctx != nil {
		return p.ctx
	}
	if p.drainCtx != nil {
		return p.drainCtx
	}
	return context.Background()
}

// finalPage reports whether a paged reply is its last page: only that page is
// decorated, so paging never repeats an added row.
func finalPage(reply string) bool {
	var top struct {
		Result struct {
			NextCursor *string `json:"nextCursor"`
		} `json:"result"`
	}
	if json.Unmarshal([]byte(reply), &top) != nil {
		return false
	}
	return top.Result.NextCursor == nil
}

// forward is the last stage: it routes the frame(s) to the attached client.
// It restores the client's own request id on a reply, withholds a reply for a
// superseded connection (a finished prompt there becomes a _ferngeist/turn_ended
// notification), re-sends the agent's unanswered requests behind a session/load
// reply, scopes a request-bound elicitation, and queues the result to the
// client's writer goroutine. A client whose backlog overflows is disconnected
// rather than sent a stream with holes in it.
func (p *StdioPump) forward(f *frame) {
	var evict *websocket.Conn
	p.clientMu.Lock()
	if p.loadRecovery != nil {
		// Numbered and buffered under clientMu: see LoadRecovery.record.
		if line := p.loadRecovery.record(f.line, f.probe, f.ok); line != f.line {
			f.line, f.out = line, []string{line}
		}
	}
	// Under clientMu: a reply routed for one generation must not reach the next.
	out := p.reqIDs.reply(p.connGen, f.probe, f.out)
	bound := p.client != nil && p.writerQ != nil
	// A prompt that finishes while nobody is attached would lose its reply
	// entirely (reply above already consumed the pending entry), taking the
	// stop reason, usage and duration with it. Keep the outcome for the next
	// attach instead; a new prompt for the session clears it. Only the
	// current generation's prompt qualifies: an older one already ended its
	// turn on whatever connection asked for it.
	if !bound && f.isReply && f.origin.method == "session/prompt" && f.origin.gen == p.connGen &&
		f.probe.Result != nil {
		p.storeTurnResult(f.origin.promptSession, turnResult{
			stopReason: string(f.probe.Result.StopReason),
			usage:      f.probe.Result.Usage,
			durationMs: time.Since(f.origin.startedAt).Milliseconds(),
		})
	}
	if f.reject {
		evict = p.rejectToClientLocked(f, out, bound)
		p.clientMu.Unlock()
		if evict != nil {
			_ = evict.CloseNow()
		}
		return
	}
	// The agent's unanswered requests are not in load history, so the reply that
	// restores their session carries them. Only for the connection that asked,
	// and only once the load succeeded (a recovered "already loaded" counts).
	// A session/resume restores the same way a session/load does.
	if f.isReply && (f.origin.method == "session/load" || f.origin.method == "session/resume") &&
		f.origin.gen == p.connGen && bound && (f.resumed || f.probe.Error == nil) {
		out = p.restoreLocked(f.origin.loadSession, out)
	}
	var deliveredGen int64
	if bound {
		deliveredGen = p.connGen
	}
	p.reqIDs.trackAgentRequest(f.probe, f.line, deliveredGen)
	if bound {
		out = p.reqIDs.scopeElicitation(p.connGen, f.probe, out)
		evict = p.queueLocked(out)
	}
	p.clientMu.Unlock()
	if evict != nil {
		_ = evict.CloseNow()
	}
}

// restoreLocked completes a reply that restores sessionID (the last of out) for
// the current connection: the unanswered agent requests follow it, and a turn
// the client missed ends just before it, ahead of the reply its SDK waits for,
// so the app stamps the restored turn instead of leaving the context meter
// stale and the turn untimed. Caller holds clientMu.
func (p *StdioPump) restoreLocked(sessionID string, out []string) []string {
	replyIdx := len(out) - 1
	out = p.reqIDs.redeliver(p.connGen, sessionID, out)
	if replyIdx < 0 {
		return out
	}
	if res, ok := p.takeTurnResult(sessionID); ok {
		out = append(out, "")
		copy(out[replyIdx+1:], out[replyIdx:])
		out[replyIdx] = turnResultFrame(sessionID, res)
	}
	return out
}

// clientFrame is the single parse of one client→agent frame; every inbound
// stage reads it. fields keeps the top-level members raw so the request-id
// stage can rewrite the frame without parsing it again; it is nil when the
// frame is not a JSON object, which every stage passes through untouched.
type clientFrame struct {
	raw    []byte
	fields map[string]json.RawMessage
	Method string
	// ID is the frame's request id. The request-id stage replaces it with the
	// agent-side id it assigns, which is the id the agent answers with.
	ID     json.RawMessage
	Params clientParams
}

// clientParams holds the params members any inbound stage reads.
type clientParams struct {
	SessionID string            `json:"sessionId"`
	Cwd       string            `json:"cwd"`
	RequestID json.RawMessage   `json:"requestId"`
	Prompt    []json.RawMessage `json:"prompt"`
	Meta      struct {
		CatchUp  *catchUpRequest `json:"ferngeist/catchUp"`
		ClientID string          `json:"ferngeist/clientId"`
	} `json:"_meta"`
}

func parseClientFrame(payload []byte) *clientFrame {
	f := &clientFrame{raw: payload}
	if json.Unmarshal(payload, &f.fields) != nil {
		f.fields = nil
		return f
	}
	_ = json.Unmarshal(f.fields["method"], &f.Method) // non-string: not a request
	f.ID = f.fields["id"]
	// Best-effort: a member of an unexpected shape stays zero and the others
	// still decode, so one odd field never hides the session id or cwd.
	_ = json.Unmarshal(f.fields["params"], &f.Params)
	return f
}

// prepareOutbound rewrites one client→agent frame for the current connection
// and records what it says about the conversation: the request id it is given,
// the ACP session and project directory it names, a prompt starting a turn, and
// anything the re-load replay has to remember. The request-id stage runs
// first: the later stages key a pending session/new or session/load by the
// agent-side id it assigns, because that is the id the agent replies with.
func (p *StdioPump) prepareOutbound(gen int64, f *clientFrame) []byte {
	payload := p.reqIDs.outbound(gen, f)
	p.acp.observeInbound(f)
	if f.Method == "session/prompt" {
		// A new turn supersedes any kept result for the session: the next
		// attach must hear about this turn, never the stale one.
		p.clearTurnResult(f.Params.SessionID)
	}
	if p.loadRecovery != nil {
		p.loadRecovery.OnOutbound(f)
	}
	return payload
}

// observeTurn records content on the active turn and evaluates the prompt
// result. A success with end_turn, zero usage, and no content is how some
// agents (e.g. Opencode) report a swallowed LLM failure; in that case the frame
// is replaced with a JSON-RPC error (same request id) so the client sees the
// turn as failed, and handled reports the replacement. Log and push happen
// outside the state lock: the callback reaches network code, and the next
// prompt must never queue behind push I/O.
func (p *StdioPump) observeTurn(probe frameProbe, origin pendingRequest, isReply bool) (out string, handled bool) {
	if !p.acp.observeTurn(probe, origin, isReply) {
		return "", false
	}
	p.logger.Warn("agent returned empty success; rewriting as JSON-RPC error "+
		"(likely swallowed LLM failure: rate limit / invalid model setting)",
		"runtime_id", p.runtimeID, "session_id", p.sessionID)
	p.notifySwallowedFailure(probe, origin)
	frame, ok := swallowedFailureFrame(probe)
	if !ok {
		p.logger.Error("failed to marshal swallowed-failure error frame")
		return "", false // forward path treats ""+false as no replacement
	}
	return frame, true
}

// swallowedFailureFrame builds the JSON-RPC error that replaces an empty
// success, keeping the request id so the client correlates it with its prompt.
func swallowedFailureFrame(probe frameProbe) (string, bool) {
	if probe.ID == nil {
		return "", false
	}
	errFrame := map[string]any{
		"jsonrpc": "2.0",
		"id":      *probe.ID,
		"error": map[string]any{
			"code":    -32000,
			"message": "Agent produced no output for this turn. The model call likely failed (rate limit or invalid model setting); see the gateway logs for details.",
		},
	}
	outBytes, err := json.Marshal(errFrame)
	if err != nil {
		return "", false
	}
	return string(outBytes), true
}

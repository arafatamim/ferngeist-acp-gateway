package session

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/coder/acp-go-sdk"
	"github.com/coder/websocket"
)

// This file holds the ACP state the frame pipeline (pipeline.go) maintains:
// one struct (acpState) with one mutex holds everything the gateway remembers
// about the conversation on one agent process.

// acpState is everything the gateway remembers about the ACP conversation on one
// agent process: the agent's initialize response and name, whether the agent
// accepts session/close, the ACP session id and per-session working directories
// it has seen, turn state for swallowed-failure detection, and the kinds of the
// tool calls currently running. One struct, one mutex — every field is small and
// read on the stdout hot path, so splitting the lock would only add lock
// ordering to get wrong. The zero value is ready.
type acpState struct {
	mu sync.Mutex

	// initResponse is the agent's `initialize` response. A reconnecting client
	// re-runs the ACP handshake, but the agent process is already initialized —
	// forwarding a second `initialize` can make a strict agent error out and
	// exit. The pump replays this response (with the new request's id) instead
	// and never forwards the duplicate.
	initResponse []byte
	// agentName is the human-readable name from initialize's agentInfo, "" until
	// observed.
	agentName string
	// closeSupported records that initialize advertised
	// sessionCapabilities.close.
	closeSupported bool

	// sessionID is the ACP session id ("ses_…"), observed from the agent's
	// session/new response or from any client frame that names one. It is sent
	// as the push SessionID so notifications match the id the client navigates
	// and keys ActiveChat by — the gateway's own resilient session id is a
	// different namespace and never matches the client.
	sessionID string
	// cwd is the working directory of the most recently opened or loaded ACP
	// session — the anchor for the gateway's file/diff/status workspace
	// endpoints. Empty until the client opens a project.
	cwd string
	// cwdBySession maps each ACP session id to its own cwd, so the workspace
	// endpoints can target one chat when the agent hosts several. session/load
	// carries both; session/new only learns its id from the agent's response,
	// so its cwd waits in pendingCwd, keyed by the raw request id, until then.
	// ponytail: never pruned, bounded by the sessions one agent process opens.
	cwdBySession map[string]string
	pendingCwd   map[string]string

	// Swallowed-failure detection. Some agents (e.g. Opencode) never send a
	// JSON-RPC error for a failed LLM call — they return a success with
	// stopReason=end_turn, zero usage, and no streamed content. The pump tracks
	// whether the current turn produced any visible output so such failures can
	// be flagged.
	turnActive     bool // a session/prompt is awaiting its result
	turnHadContent bool // an agent_message_chunk or tool_call arrived this turn

	// toolKinds remembers each running tool call's kind: a tool_call_update
	// usually omits it. Entries leave when the call completes or fails.
	toolKinds map[string]string
}

// observeInitialize caches the agent's `initialize` response — identified by the
// presence of result.protocolVersion, which only initialize responses carry —
// along with the agent name and close capability it advertises. The typed
// decode runs once per agent process: only the first initialize response gets
// past the probe check and the cache check.
func (s *acpState) observeInitialize(probe frameProbe, raw []byte) {
	if probe.Result == nil || probe.Result.ProtocolVersion == nil || s.initializeResponse() != nil {
		return
	}
	var typed struct {
		Result *acp.InitializeResponse `json:"result"`
	}
	if err := json.Unmarshal(raw, &typed); err != nil {
		typed.Result = nil // cache the response; only name/capability are lost
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.initResponse = append([]byte(nil), raw...)
	if typed.Result == nil {
		return
	}
	if typed.Result.AgentCapabilities.SessionCapabilities.Close != nil {
		s.closeSupported = true
	}
	if info := typed.Result.AgentInfo; info != nil {
		name := info.Name
		if info.Title != nil && *info.Title != "" {
			name = *info.Title
		}
		s.agentName = name
	}
}

// initializeResponse returns the cached initialize response, or nil.
func (s *acpState) initializeResponse() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initResponse
}

// name is the agent's display name, or fallback before initialize was observed.
func (s *acpState) name(fallback string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agentName == "" {
		return fallback
	}
	return s.agentName
}

// closes reports whether the agent advertised sessionCapabilities.close.
func (s *acpState) closes() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeSupported
}

// observeSessionID caches the ACP session id the agent's session/new response
// returns. Only that response carries a top-level result.sessionId, so this
// never misfires on other frames, and the id is captured once for the
// session's lifetime.
func (s *acpState) observeSessionID(probe frameProbe) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionID != "" {
		return
	}
	if probe.Result == nil || probe.Result.SessionID == "" {
		return
	}
	s.sessionID = probe.Result.SessionID
}

// bindPendingCwd binds a pending session/new cwd to the session id the agent's
// response assigns. The request id is the one written to the agent (after
// rewriting), so the agent echoes it verbatim. An error response just drops the
// pending entry.
func (s *acpState) bindPendingCwd(probe frameProbe) {
	if probe.Method != "" || probe.ID == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := string(*probe.ID)
	cwd, ok := s.pendingCwd[key]
	if !ok {
		return
	}
	delete(s.pendingCwd, key)
	if probe.Result == nil || probe.Result.SessionID == "" {
		return
	}
	if s.cwdBySession == nil {
		s.cwdBySession = make(map[string]string)
	}
	s.cwdBySession[probe.Result.SessionID] = cwd
}

// observeInbound records what one client→agent frame says: the ACP session id it
// targets, the working directory of a session/new, session/load or
// session/resume, and a session/prompt starting a turn. One parse for all three.
//
// The id matters on reconnects: the outbound path only sees session/new
// responses, so a resilient reconnect that restores context via session/load
// would otherwise never be observed and the push SessionID would be empty. cwd
// is set once when the client opens the project, so it is re-captured on each
// such request and a project switch updates it.
func (s *acpState) observeInbound(f *clientFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.Method == "session/prompt" {
		s.turnActive = true
		s.turnHadContent = false
	}
	if f.Params.SessionID != "" && s.sessionID == "" {
		s.sessionID = f.Params.SessionID
	}
	switch f.Method {
	case "session/new", "session/load", "session/resume":
	default:
		return
	}
	cwd := f.Params.Cwd
	if cwd == "" {
		return
	}
	s.cwd = cwd
	if f.Method == "session/new" {
		if f.ID != nil {
			if s.pendingCwd == nil {
				s.pendingCwd = make(map[string]string)
			}
			s.pendingCwd[string(f.ID)] = cwd
		}
		return
	}
	if f.Params.SessionID != "" {
		if s.cwdBySession == nil {
			s.cwdBySession = make(map[string]string)
		}
		s.cwdBySession[f.Params.SessionID] = cwd
	}
}

// observeTurn records turn state from one frame and reports whether the prompt
// result is a clean success with no output — the swallowed-failure signature.
func (s *acpState) observeTurn(probe frameProbe, origin pendingRequest, isReply bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if probe.Params != nil && probe.Params.Update != nil {
		switch probe.Params.Update.Discriminator {
		case "agent_message_chunk", "tool_call", "tool_call_update":
			s.turnHadContent = true
		}
		return false
	}

	// Prompt result: only judge when a turn was tracked and the response is a
	// clean success (a JSON-RPC error already pushes its own notification). A
	// reply known to answer another request (a set_mode reply mid-turn) is not
	// the prompt result; an unknown id is still judged.
	if !s.turnActive || (isReply && origin.method != "session/prompt") ||
		probe.ID == nil || probe.Result == nil || probe.Error != nil {
		return false
	}
	s.turnActive = false
	// Any non-zero token field counts as usage: agents may populate
	// input/output without the unstable totalTokens field.
	used := probe.Result.Usage != nil && (probe.Result.Usage.TotalTokens != 0 ||
		probe.Result.Usage.InputTokens != 0 || probe.Result.Usage.OutputTokens != 0)
	return probe.Result.StopReason == "end_turn" && !used && !s.turnHadContent
}

// toolKindFor resolves the kind of a tool call: the frame's own kind when it
// carries one (remembering it for later updates, which usually omit it), else
// the kind seen before.
func (s *acpState) toolKindFor(callID, kind string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kind != "" {
		if s.toolKinds == nil {
			s.toolKinds = make(map[string]string)
		}
		s.toolKinds[callID] = kind
		return kind
	}
	return s.toolKinds[callID]
}

// forgetTool drops a finished tool call's remembered kind.
func (s *acpState) forgetTool(callID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.toolKinds, callID)
}

// AgentName is the agent's name for push text, e.g. "Claude Code", or fallback
// when initialize carried no agentInfo.
func (p *StdioPump) AgentName(fallback string) string {
	return p.acp.name(fallback)
}

// SupportsClose reports whether the agent advertised sessionCapabilities.close.
func (p *StdioPump) SupportsClose() bool {
	return p.acp.closes()
}

// AcpSessionID returns the observed ACP session id, or "" if none has been
// observed yet.
func (p *StdioPump) AcpSessionID() string {
	p.acp.mu.Lock()
	defer p.acp.mu.Unlock()
	return p.acp.sessionID
}

// AcpCwd returns the ACP session working directory, or "" if the client has not
// issued session/new or session/load yet. With several sessions on one agent
// this is whichever was opened last; AcpCwdFor targets one.
func (p *StdioPump) AcpCwd() string {
	p.acp.mu.Lock()
	defer p.acp.mu.Unlock()
	return p.acp.cwd
}

// AcpCwdFor returns the cwd of one ACP session, or "" if the gateway has not
// seen that session opened or loaded.
func (p *StdioPump) AcpCwdFor(acpSessionID string) string {
	p.acp.mu.Lock()
	defer p.acp.mu.Unlock()
	return p.acp.cwdBySession[acpSessionID]
}

// AcpSessionsWithCwd returns the ids of the ACP sessions whose cwd satisfies match.
func (p *StdioPump) AcpSessionsWithCwd(match func(cwd string) bool) []string {
	p.acp.mu.Lock()
	defer p.acp.mu.Unlock()
	var ids []string
	for id, cwd := range p.acp.cwdBySession {
		if match(cwd) {
			ids = append(ids, id)
		}
	}
	return ids
}

func (p *StdioPump) initResponseCached() bool {
	return p.acp.initializeResponse() != nil
}

// replayInitialize intercepts a client `initialize` request. If the agent has
// already been initialized (its response is cached), it answers the client
// directly with that cached response — rewritten to carry the request's id — and
// returns true so the caller does not forward a duplicate `initialize` to the
// agent. It returns false for non-initialize frames, or for the first
// `initialize` (no cache yet), which must reach the agent normally.
func (p *StdioPump) replayInitialize(f *clientFrame) bool {
	if f.Method != "initialize" {
		return false
	}

	cached := p.acp.initializeResponse()
	if cached == nil {
		return false
	}

	out, ok := rewriteResponseID(cached, f.ID)
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

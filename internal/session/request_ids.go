package session

import (
	"encoding/json"
	"strconv"
	"sync"
)

// turnEndedMethod tells a client that a prompt sent by an earlier connection has
// finished. That turn's real reply carries the old connection's request id, which
// the reattached client never issued, so the reply alone would leave its
// transcript streaming forever. Underscore-prefixed per ACP's extension rule.
const turnEndedMethod = "_ferngeist/turn_ended"

// elicitationMethod is ACP's (unstable) agent->client request for user input.
const elicitationMethod = "elicitation/create"

// requestIDs gives every client request a gateway-unique id on its way to the
// agent and restores the client's own id on the reply.
//
// Each client connection numbers its requests from 1. Without translation a
// request still in flight from a dropped connection shares an id with the new
// connection's requests: the agent sees two live requests with one id, and the
// old reply lands on whichever new request reused it. The zero value is ready.
type requestIDs struct {
	mu      sync.Mutex
	next    int64
	pending map[string]pendingRequest // agent-side id -> origin
	byOwner map[string]string         // owner key -> agent-side id, for $/cancel_request

	// The agent's own requests (permission, fs/*, terminal/*) the client has not
	// answered yet, oldest first. See trackAgentRequest.
	agentReqs []agentRequest
}

// maxPendingAgentRequests bounds agentReqs. An agent has a handful of requests
// open at once; past the cap the oldest is forgotten (and so not re-sent).
// ponytail: a plain cap, not a per-session budget.
const maxPendingAgentRequests = 64

// agentRequest is an agent->client request awaiting the client's response.
type agentRequest struct {
	key       string // responseIDKey of the agent's id
	frame     string
	sessionID string
	// The connection generation the frame was last written to; 0 if never.
	deliveredGen int64
}

type pendingRequest struct {
	gen      int64
	clientID json.RawMessage
	owner    string
	method   string
	// The ACP session a session/prompt drives; empty for every other method.
	promptSession string
	// The ACP session a session/load restores; empty for every other method.
	loadSession string
}

func ownerKey(gen int64, clientID json.RawMessage) string {
	return strconv.FormatInt(gen, 10) + ":" + string(clientID)
}

// outbound rewrites a client->agent frame for connection gen: a request gets a
// fresh id, and a $/cancel_request is pointed at the id its target was given.
// Replies to the agent's own requests carry the agent's ids and pass unchanged,
// as does anything that does not parse.
func (r *requestIDs) outbound(gen int64, payload []byte) []byte {
	var msg map[string]json.RawMessage
	if json.Unmarshal(payload, &msg) != nil {
		return payload
	}
	var method string
	if json.Unmarshal(msg["method"], &method) != nil || method == "" {
		if id, answers := msg["id"]; answers {
			r.answerAgentRequest(id)
		}
		return payload
	}
	clientID, isRequest := msg["id"]
	if !isRequest {
		if method == "$/cancel_request" && r.retargetCancel(gen, msg) {
			return marshalOr(msg, payload)
		}
		return payload
	}

	var params struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(msg["params"], &params)
	origin := pendingRequest{gen: gen, clientID: clientID, owner: ownerKey(gen, clientID), method: method}
	switch method {
	case "session/prompt":
		origin.promptSession = params.SessionID
	case "session/load":
		origin.loadSession = params.SessionID
	}

	r.mu.Lock()
	r.next++
	agentID := strconv.FormatInt(r.next, 10)
	if r.pending == nil {
		r.pending = make(map[string]pendingRequest)
		r.byOwner = make(map[string]string)
	}
	r.pending[agentID] = origin
	r.byOwner[origin.owner] = agentID
	r.mu.Unlock()

	msg["id"] = json.RawMessage(agentID)
	return marshalOr(msg, payload)
}

func (r *requestIDs) retargetCancel(gen int64, msg map[string]json.RawMessage) bool {
	var params map[string]json.RawMessage
	if json.Unmarshal(msg["params"], &params) != nil || params["requestId"] == nil {
		return false
	}
	r.mu.Lock()
	agentID, ok := r.byOwner[ownerKey(gen, params["requestId"])]
	r.mu.Unlock()
	if !ok {
		return false
	}
	params["requestId"] = json.RawMessage(agentID)
	raw, err := json.Marshal(params)
	if err != nil {
		return false
	}
	msg["params"] = raw
	return true
}

// peek returns the translated request a reply answers without consuming it, so
// classifiers that run before reply can tell what the reply is for. ok is false
// for anything that is not a reply to a translated request.
func (r *requestIDs) peek(probe frameProbe) (origin pendingRequest, ok bool) {
	if probe.Method != "" || probe.ID == nil {
		return pendingRequest{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	origin, ok = r.pending[responseIDKey(*probe.ID)]
	return origin, ok
}

// trackAgentRequest remembers a request the agent sent so it can be re-sent if
// the client was away or left without answering. deliveredGen is the connection
// generation it was just written to, or 0 when no client was attached. Frames
// that are not requests are ignored.
func (r *requestIDs) trackAgentRequest(probe frameProbe, line string, deliveredGen int64) {
	if probe.Method == "" || probe.ID == nil {
		return
	}
	req := agentRequest{key: responseIDKey(*probe.ID), frame: line, deliveredGen: deliveredGen}
	if probe.Params != nil {
		req.sessionID = probe.Params.SessionID
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropAgentRequestLocked(req.key)
	if len(r.agentReqs) >= maxPendingAgentRequests {
		r.agentReqs = r.agentReqs[1:]
	}
	r.agentReqs = append(r.agentReqs, req)
}

// answerAgentRequest forgets the agent request a client response (result or
// error) answers.
func (r *requestIDs) answerAgentRequest(id json.RawMessage) {
	r.mu.Lock()
	r.dropAgentRequestLocked(responseIDKey(id))
	r.mu.Unlock()
}

func (r *requestIDs) dropAgentRequestLocked(key string) {
	for i, req := range r.agentReqs {
		if req.key == key {
			r.agentReqs = append(r.agentReqs[:i], r.agentReqs[i+1:]...)
			return
		}
	}
}

func (r *requestIDs) dropSessionAgentRequestsLocked(sessionID string) {
	kept := r.agentReqs[:0]
	for _, req := range r.agentReqs {
		if req.sessionID != sessionID {
			kept = append(kept, req)
		}
	}
	r.agentReqs = kept
}

func (r *requestIDs) hasAgentRequests() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.agentReqs) > 0
}

// scopeElicitation points a request-scoped elicitation/create (the frame in
// frames that probe describes) at the id the client gave the request it is tied
// to. The agent only knows the translated id, which the client never issued.
// Elicitations tied to an earlier connection's request pass unchanged.
func (r *requestIDs) scopeElicitation(gen int64, probe frameProbe, frames []string) []string {
	if probe.Method != elicitationMethod || probe.Params == nil || probe.Params.RequestID == nil || len(frames) != 1 {
		return frames
	}
	r.mu.Lock()
	origin, ok := r.pending[responseIDKey(probe.Params.RequestID)]
	r.mu.Unlock()
	if !ok || origin.gen != gen {
		return frames
	}
	var msg, params map[string]json.RawMessage
	if json.Unmarshal([]byte(frames[0]), &msg) != nil || json.Unmarshal(msg["params"], &params) != nil {
		return frames
	}
	params["requestId"] = origin.clientID
	raw, err := json.Marshal(params)
	if err != nil {
		return frames
	}
	msg["params"] = raw
	return []string{string(marshalOr(msg, []byte(frames[0])))}
}

// redeliver appends, after a session/load reply, every unanswered agent request
// for sessionID that connection gen has not been sent yet, and marks them sent.
// Requests are not part of load history: a client that was away (or dropped
// before answering) would otherwise leave the agent waiting forever.
func (r *requestIDs) redeliver(gen int64, sessionID string, frames []string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.agentReqs {
		if req := &r.agentReqs[i]; req.sessionID == sessionID && req.deliveredGen != gen {
			req.deliveredGen = gen
			frames = append(frames, req.frame)
		}
	}
	return frames
}

// reply routes the agent's reply to a translated request. frames is what the
// pump would send for it, the reply last. A reply for the connection that asked
// gets its client id back; one for an earlier connection is withheld, and a
// finished prompt becomes a turnEndedMethod notification instead. Frames that
// are not replies to a translated request pass unchanged.
func (r *requestIDs) reply(currentGen int64, probe frameProbe, frames []string) []string {
	if probe.Method != "" || probe.ID == nil || len(frames) == 0 {
		return frames
	}
	agentID := responseIDKey(*probe.ID)
	r.mu.Lock()
	origin, ok := r.pending[agentID]
	if ok {
		delete(r.pending, agentID)
		delete(r.byOwner, origin.owner)
		if origin.promptSession != "" {
			// The turn is over, so requests it raised can no longer be answered
			// usefully; re-sending them on a later load would show stale prompts.
			r.dropSessionAgentRequestsLocked(origin.promptSession)
		}
	}
	r.mu.Unlock()
	if !ok {
		return frames
	}

	if origin.gen != currentGen {
		if origin.promptSession == "" {
			return nil
		}
		return []string{turnEndedFrame(origin.promptSession, probe)}
	}
	last := len(frames) - 1
	if out, rewritten := rewriteResponseID([]byte(frames[last]), origin.clientID); rewritten {
		frames[last] = string(out)
	}
	return frames
}

func turnEndedFrame(sessionID string, probe frameProbe) string {
	stopReason := "end_turn"
	if probe.Result != nil && probe.Result.StopReason != "" {
		stopReason = string(probe.Result.StopReason)
	}
	out, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  turnEndedMethod,
		"params":  map[string]string{"sessionId": sessionID, "stopReason": stopReason},
	})
	return string(out)
}

func marshalOr(msg map[string]json.RawMessage, fallback []byte) []byte {
	out, err := json.Marshal(msg)
	if err != nil {
		return fallback
	}
	return out
}

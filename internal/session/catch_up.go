package session

import (
	"encoding/json"
	"maps"
	"time"
)

// This file holds catch-up resume: a reattaching client names the last update
// it holds and the gateway sends only what came after, instead of the client
// re-loading the whole session. LoadRecovery numbers and buffers the updates.

// seqMetaKey numbers each session/update per ACP session, in
// params.update._meta. Not params._meta: the Kotlin ACP SDK drops
// notification-level _meta for updates that arrive during a prompt.
const seqMetaKey = "ferngeist/seq"

// catchUpMetaKey marks a session/resume, in params._meta, as a catch-up the
// gateway answers itself; its reply carries the outcome under the same key in
// result._meta.
const catchUpMetaKey = "ferngeist/catchUp"

// catchUpRequest is the client's position: the epoch and last seq it holds.
type catchUpRequest struct {
	Epoch    string `json:"epoch"`
	AfterSeq int64  `json:"afterSeq"`
}

// catchUpResult tells the client where the session stands once the missed
// updates are sent. Gap means none were: the client must session/load.
type catchUpResult struct {
	Epoch         string     `json:"epoch"`
	HeadSeq       int64      `json:"headSeq"`
	TurnActive    bool       `json:"turnActive"`
	TurnStartedAt *time.Time `json:"turnStartedAt,omitempty"`
	Gap           bool       `json:"gap"`
}

// answerCatchUp answers a catch-up session/resume for an ACP session this
// agent process holds, without the agent (which need not support resume): the
// buffered updates after the client's seq, the missed turn's result, the
// reply, then the unanswered agent requests. It reports false for any other
// frame, which then goes to the agent as usual. Only connection gen is
// answered; a superseded connection's resume is dropped.
func (p *StdioPump) answerCatchUp(gen int64, f *clientFrame) bool {
	req, sid := f.Params.Meta.CatchUp, f.Params.SessionID
	if f.Method != "session/resume" || req == nil || f.ID == nil || p.loadRecovery == nil || p.AcpCwdFor(sid) == "" {
		return false
	}
	p.acp.observeInbound(f)

	p.clientMu.Lock()
	if gen != p.connGen || p.client == nil || p.writerQ == nil {
		p.clientMu.Unlock()
		return true
	}
	// Under clientMu, which also covers numbering a live update and queueing
	// it: no update is sent twice or out of order across the replay.
	frames, head, gap := p.loadRecovery.catchUp(sid, req.Epoch, req.AfterSeq)
	res := catchUpResult{Epoch: p.loadRecovery.epoch, HeadSeq: head, Gap: gap}
	if startedAt, ok := p.reqIDs.promptStartedAt(sid); ok {
		res.TurnActive, res.TurnStartedAt = true, &startedAt
	}
	frames = p.restoreLocked(sid, append(frames, catchUpReplyFrame(f.ID, res)))
	evict := p.queueLocked(frames)
	p.clientMu.Unlock()
	if evict != nil {
		_ = evict.CloseNow()
	}
	return true
}

// catchUpReplyFrame builds the session/resume result the gateway answers with.
func catchUpReplyFrame(id json.RawMessage, res catchUpResult) string {
	out, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  map[string]any{"_meta": map[string]any{catchUpMetaKey: res}},
	})
	return string(out)
}

// withUpdateMeta merges add into a session/update's params.update._meta,
// keeping the agent's own keys. ok is false, with line unchanged, when the
// frame has no object update to carry them.
func withUpdateMeta(line string, add map[string]json.RawMessage) (string, bool) {
	var msg, params, update, meta map[string]json.RawMessage
	if json.Unmarshal([]byte(line), &msg) != nil || json.Unmarshal(msg["params"], &params) != nil ||
		json.Unmarshal(params["update"], &update) != nil || update == nil {
		return line, false
	}
	if raw, ok := update["_meta"]; ok && json.Unmarshal(raw, &meta) != nil {
		return line, false
	}
	if meta == nil {
		meta = make(map[string]json.RawMessage, len(add))
	}
	maps.Copy(meta, add)
	var err error
	if update["_meta"], err = json.Marshal(meta); err != nil {
		return line, false
	}
	if params["update"], err = json.Marshal(update); err != nil {
		return line, false
	}
	if msg["params"], err = json.Marshal(params); err != nil {
		return line, false
	}
	out, err := json.Marshal(msg)
	if err != nil {
		return line, false
	}
	return string(out), true
}

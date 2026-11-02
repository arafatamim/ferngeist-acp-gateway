package session

import (
	"bytes"
	"encoding/json"
	"maps"
	"strings"
	"time"

	"github.com/go-json-experiment/json/jsontext"
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
	if f.Method != "session/resume" || req == nil || f.ID == nil || p.loadRecovery == nil || !p.acp.holds(sid) {
		return false
	}
	p.acp.observeInbound(f)

	p.queueClientReply(gen, func() []string {
		// Under clientMu, which also covers numbering a live update and queueing
		// it: no update is sent twice or out of order across the replay.
		frames, head, gap := p.loadRecovery.catchUp(sid, req.Epoch, req.AfterSeq)
		res := catchUpResult{Epoch: p.loadRecovery.epoch, HeadSeq: head, Gap: gap}
		if startedAt, ok := p.reqIDs.promptStartedAt(sid); ok {
			startedAt = startedAt.UTC()
			res.TurnActive, res.TurnStartedAt = true, &startedAt
		}
		frames = append(frames, catchUpReplyFrame(f.ID, res))
		if !gap {
			frames = p.restoreLocked(sid, frames)
		}
		return frames
	})
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
// frame has no object update to carry them. The frame was already validated by
// parseFrameProbe before record calls this helper.
func withUpdateMeta(line string, add map[string]json.RawMessage) (string, bool) {
	// jsontext reads a bytes.Buffer directly, without copying large tool output.
	// Match encoding/json's acceptance of malformed Unicode in opaque content.
	dec := jsontext.NewDecoder(bytes.NewBuffer([]byte(line)), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	bounds, ok := updateMetaBounds(dec, 0)
	if !ok {
		return line, false
	}
	var meta map[string]json.RawMessage
	if bounds.hasMeta && json.Unmarshal([]byte(line[bounds.metaStart:bounds.metaEnd]), &meta) != nil {
		return line, false
	}
	if meta == nil {
		meta = make(map[string]json.RawMessage, len(add))
	}
	maps.Copy(meta, add)
	raw, err := json.Marshal(meta)
	if err != nil {
		return line, false
	}
	if bounds.hasMeta {
		return line[:bounds.metaStart] + string(raw) + line[bounds.metaEnd:], true
	}
	// Insert before the update's closing brace, retaining opaque payloads
	// byte-for-byte instead of re-encoding all three enclosing objects.
	prefix := `"_meta":`
	if strings.TrimSpace(line[bounds.start+1:bounds.end-1]) != "" {
		prefix = "," + prefix
	}
	return line[:bounds.end-1] + prefix + string(raw) + line[bounds.end-1:], true
}

type updateMetaSpan struct {
	start, end         int
	metaStart, metaEnd int
	hasMeta            bool
}

// updateMetaBounds returns the last occurrence's byte range, matching
// encoding/json when an object repeats a key. Its caller passes validated JSON.
// One traversal finds params/update/_meta without rescanning enclosing values.
func updateMetaBounds(dec *jsontext.Decoder, depth int) (span updateMetaSpan, found bool) {
	if dec.PeekKind() != '{' {
		if err := dec.SkipValue(); err != nil {
			return updateMetaSpan{}, false
		}
		return updateMetaSpan{}, false
	}
	if token, err := dec.ReadToken(); err != nil || token.Kind() != '{' {
		return updateMetaSpan{}, false
	}
	start := int(dec.InputOffset()) - 1
	for dec.PeekKind() != '}' {
		token, err := dec.ReadToken()
		if err != nil {
			return updateMetaSpan{}, false
		}
		name := token.String()
		if depth < 2 && name == [2]string{"params", "update"}[depth] {
			span, found = updateMetaBounds(dec, depth+1)
			continue
		}
		raw, err := dec.ReadValue()
		if err != nil {
			return updateMetaSpan{}, false
		}
		if depth == 2 && name == "_meta" {
			span.metaEnd = int(dec.InputOffset())
			span.metaStart, span.hasMeta = span.metaEnd-len(raw), true
		}
	}
	if _, err := dec.ReadToken(); err != nil {
		return updateMetaSpan{}, false
	}
	if depth == 2 {
		span.start, span.end, found = start, int(dec.InputOffset()), true
	}
	return span, found
}

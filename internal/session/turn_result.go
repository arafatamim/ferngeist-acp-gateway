package session

import (
	"encoding/json"

	"github.com/coder/acp-go-sdk"
)

// turnResultMethod tells a reattached client how the turn it missed ended.
// When a prompt finishes while no client is attached its reply is dropped, so
// the next attach replays the outcome — stop reason, usage and duration —
// ahead of the session/load (or session/resume) reply. Underscore-prefixed
// per ACP's extension rule, like turnEndedMethod.
const turnResultMethod = "_ferngeist/turn_result"

// turnResult is one finished turn's outcome, kept while no client is attached
// to hear it. Latest wins per ACP session; a new prompt for the session
// clears it, and the next load/resume flushes it.
type turnResult struct {
	stopReason string
	usage      *acp.Usage
	durationMs int64
}

// storeTurnResult keeps res for ACP session sessionID, replacing any earlier
// one. Empty session ids are never stored: without a session the result could
// never be flushed to the right chat.
func (p *StdioPump) storeTurnResult(sessionID string, res turnResult) {
	if sessionID == "" {
		return
	}
	p.turnMu.Lock()
	defer p.turnMu.Unlock()
	if p.turnResults == nil {
		p.turnResults = make(map[string]turnResult)
	}
	p.turnResults[sessionID] = res
}

// takeTurnResult returns and clears the kept result for sessionID.
func (p *StdioPump) takeTurnResult(sessionID string) (turnResult, bool) {
	p.turnMu.Lock()
	defer p.turnMu.Unlock()
	res, ok := p.turnResults[sessionID]
	if ok {
		delete(p.turnResults, sessionID)
	}
	return res, ok
}

// clearTurnResult drops any kept result for sessionID, e.g. when a new prompt
// starts a later turn there.
func (p *StdioPump) clearTurnResult(sessionID string) {
	p.turnMu.Lock()
	defer p.turnMu.Unlock()
	delete(p.turnResults, sessionID)
}

// turnResultFrame builds the _ferngeist/turn_result notification announcing
// how the client's missed turn ended.
func turnResultFrame(sessionID string, res turnResult) string {
	out, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  turnResultMethod,
		"params": map[string]any{
			"sessionId":  sessionID,
			"stopReason": res.stopReason,
			"usage":      res.usage,
			"durationMs": res.durationMs,
		},
	})
	return string(out)
}

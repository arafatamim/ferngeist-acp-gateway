package session

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// promptAndReply sends a session/prompt as client id 1, then feeds the agent's
// result reply through the drain path. The prompt goes through prepareOutbound
// (the production outbound path) so prompt-time bookkeeping runs.
func promptAndReply(t *testing.T, p *StdioPump, sessionID, result string) {
	t.Helper()
	frame := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":%q,"prompt":[]}}`, sessionID)
	agent := agentIDOf(t, p.prepareOutbound(parseClientFrame([]byte(frame))))
	p.handleStdoutLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, agent, result))
}

func turnSlot(t *testing.T, p *StdioPump, sessionID string) (turnResult, bool) {
	t.Helper()
	p.turnMu.Lock()
	defer p.turnMu.Unlock()
	res, ok := p.turnResults[sessionID]
	return res, ok
}

// A prompt that finishes while no client is attached keeps its result on the
// pump instead of dropping it with the reply.
func TestDetachedPromptResultKept(t *testing.T) {
	pump := newPump()
	promptAndReply(t, pump, "ses_a", `{"stopReason":"end_turn","usage":{"totalTokens":15}}`)

	res, ok := turnSlot(t, pump, "ses_a")
	if !ok {
		t.Fatal("detached prompt result was not kept")
	}
	if res.stopReason != "end_turn" {
		t.Fatalf("stopReason = %q, want end_turn", res.stopReason)
	}
	if res.usage == nil || res.usage.TotalTokens != 15 {
		t.Fatalf("usage = %+v, want totalTokens 15", res.usage)
	}
	if res.durationMs < 0 {
		t.Fatalf("durationMs = %d, want >= 0", res.durationMs)
	}
}

// A newer prompt for the session clears a stale kept result.
func TestNewPromptClearsKeptResult(t *testing.T) {
	pump := newPump()
	promptAndReply(t, pump, "ses_a", `{"stopReason":"end_turn","usage":{"totalTokens":1}}`)
	if _, ok := turnSlot(t, pump, "ses_a"); !ok {
		t.Fatal("setup: result was not kept")
	}
	pump.prepareOutbound(parseClientFrame([]byte(`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"ses_a","prompt":[]}}`)))
	if _, ok := turnSlot(t, pump, "ses_a"); ok {
		t.Fatal("new prompt did not clear the kept result")
	}
}

// A reply to an earlier generation's prompt is not kept: only a current-gen
// prompt's result lands in the slot.
func TestStaleGenerationPromptResultNotKept(t *testing.T) {
	pump := newPump()
	frame := `{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"ses_a","prompt":[]}}`
	agent := agentIDOf(t, pump.prepareOutbound(parseClientFrame([]byte(frame))))
	pump.Attach() // generation moves on with nobody bound
	pump.handleStdoutLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}`, agent))
	if _, ok := turnSlot(t, pump, "ses_a"); ok {
		t.Fatal("older-generation prompt result was kept")
	}
}

// turnResultParams decodes one _ferngeist/turn_result notification frame.
func turnResultParams(t *testing.T, frame string) map[string]json.RawMessage {
	t.Helper()
	var msg struct {
		Method string                     `json:"method"`
		Params map[string]json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal([]byte(frame), &msg); err != nil {
		t.Fatalf("turn frame is not JSON: %v", err)
	}
	if msg.Method != turnResultMethod {
		t.Fatalf("method = %q, want %q", msg.Method, turnResultMethod)
	}
	return msg.Params
}

// The next attach's session/load carries the kept result before the load
// reply, then the slot is empty.
func TestKeptResultFlushesBeforeLoadReply(t *testing.T) {
	pump := newPump()
	promptAndReply(t, pump, "ses_a", `{"stopReason":"end_turn","usage":{"totalTokens":15}}`)

	gen, c := connect(t, pump)
	agent := pump.reqIDs.outbound(gen, parseClientFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/load","params":{"sessionId":"ses_a"}}`)))
	pump.handleStdoutLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":null}`, agentIDOf(t, agent)))
	got := readFrames(t, pump, c, 2)

	params := turnResultParams(t, got[0])
	var sessionID, stopReason string
	_ = json.Unmarshal(params["sessionId"], &sessionID)
	_ = json.Unmarshal(params["stopReason"], &stopReason)
	var usage struct {
		TotalTokens int `json:"totalTokens"`
	}
	_ = json.Unmarshal(params["usage"], &usage)
	var durationMs int64 = -1
	_ = json.Unmarshal(params["durationMs"], &durationMs)
	if sessionID != "ses_a" || stopReason != "end_turn" || usage.TotalTokens != 15 || durationMs < 0 {
		t.Fatalf("turn_result params = %s", got[0])
	}
	if !strings.Contains(got[1], `"id":1`) {
		t.Fatalf("load reply did not follow turn_result: %q", got[1])
	}
	if _, ok := turnSlot(t, pump, "ses_a"); ok {
		t.Fatal("slot was not cleared after flush")
	}
}

// session/resume records its session like session/load, so the kept result
// flushes after a resume too — along with unanswered agent requests, which
// were never re-sent after a resume before.
func TestKeptResultFlushesAfterResumeWithRedelivery(t *testing.T) {
	pump := newPump()
	promptAndReply(t, pump, "ses_a", `{"stopReason":"end_turn","usage":{"totalTokens":1}}`)
	// Tracked after the prompt's reply: a reply drops its session's pending
	// agent requests, so anything tracked before it would already be gone.
	pump.handleStdoutLine(permissionRequest) // unanswered while detached

	gen, c := connect(t, pump)
	agent := pump.reqIDs.outbound(gen, parseClientFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/resume","params":{"sessionId":"ses_a"}}`)))
	pump.handleStdoutLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":null}`, agentIDOf(t, agent)))
	got := readFrames(t, pump, c, 3)

	turnResultParams(t, got[0])
	if !strings.Contains(got[1], `"id":1`) {
		t.Fatalf("resume reply did not follow turn_result: %q", got[1])
	}
	if got[2] != permissionRequest {
		t.Fatalf("unanswered request was not re-sent after resume: %q", got[2])
	}
	if _, ok := turnSlot(t, pump, "ses_a"); ok {
		t.Fatal("slot was not cleared after resume flush")
	}
}

// Without a kept result the load path is byte-for-byte what it was.
func TestLoadWithoutKeptResultUnchanged(t *testing.T) {
	pump := newPump()
	gen, c := connect(t, pump)
	loadAndReply(t, pump, gen, "ses_a")
	if got := readFrames(t, pump, c, 1); got[0] != `{"id":1,"jsonrpc":"2.0","result":null}` {
		t.Fatalf("load reply = %q", got[0])
	}
}

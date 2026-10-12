package session

import (
	"encoding/json"
	"strings"
	"testing"
)

func agentIDOf(t *testing.T, frame []byte) string {
	t.Helper()
	var msg struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(frame, &msg); err != nil {
		t.Fatalf("frame is not JSON: %v", err)
	}
	return string(msg.ID)
}

func replyProbe(t *testing.T, line string) frameProbe {
	t.Helper()
	probe, ok := parseFrameProbe([]byte(line))
	if !ok {
		t.Fatalf("reply is not JSON: %s", line)
	}
	return probe
}

// A prompt from a dropped connection and a request from its replacement share
// client id 3. The agent must see two ids; the old reply must not reach the new
// client as the answer to its request, and must instead end the old turn.
func TestRequestIDsKeepConnectionsApart(t *testing.T) {
	var ids requestIDs
	oldPrompt := ids.outbound(1, parseClientFrame([]byte(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"ses_a","prompt":[]}}`)))
	newLoad := ids.outbound(2, parseClientFrame([]byte(`{"jsonrpc":"2.0","id":3,"method":"session/load","params":{"sessionId":"ses_a"}}`)))

	oldID, newID := agentIDOf(t, oldPrompt), agentIDOf(t, newLoad)
	if oldID == newID {
		t.Fatalf("both requests reached the agent as id %s", oldID)
	}

	oldReply := `{"jsonrpc":"2.0","id":` + oldID + `,"result":{"stopReason":"end_turn"}}`
	frames := ids.reply(2, replyProbe(t, oldReply), []string{oldReply})
	if len(frames) != 1 || !strings.Contains(frames[0], turnEndedMethod) || !strings.Contains(frames[0], "ses_a") {
		t.Fatalf("old prompt reply should become a turn-ended notice, got %v", frames)
	}

	newReply := `{"jsonrpc":"2.0","id":` + newID + `,"result":null}`
	frames = ids.reply(2, replyProbe(t, newReply), []string{newReply})
	if len(frames) != 1 || agentIDOf(t, []byte(frames[0])) != "3" {
		t.Fatalf("new reply should carry the client's id 3, got %v", frames)
	}
}

// A prompt's reply carries how long the turn ran, next to the agent's own _meta.
func TestRequestIDsTimePromptReplies(t *testing.T) {
	var ids requestIDs
	out := ids.outbound(1, parseClientFrame([]byte(`{"jsonrpc":"2.0","id":7,"method":"session/prompt","params":{"sessionId":"s","prompt":[]}}`)))
	reply := `{"jsonrpc":"2.0","id":` + agentIDOf(t, out) + `,"result":{"stopReason":"end_turn","_meta":{"agent":1}}}`
	frames := ids.reply(1, replyProbe(t, reply), []string{reply})

	var msg struct {
		ID     json.RawMessage `json:"id"`
		Result struct {
			StopReason string `json:"stopReason"`
			Meta       struct {
				Agent     *int `json:"agent"`
				Ferngeist struct {
					DurationMs *int64 `json:"durationMs"`
				} `json:"ferngeist"`
			} `json:"_meta"`
		} `json:"result"`
	}
	if len(frames) != 1 || json.Unmarshal([]byte(frames[0]), &msg) != nil {
		t.Fatalf("want one JSON reply, got %v", frames)
	}
	if string(msg.ID) != "7" || msg.Result.StopReason != "end_turn" || msg.Result.Meta.Agent == nil {
		t.Fatalf("reply lost its id, stop reason or the agent's _meta: %s", frames[0])
	}
	if d := msg.Result.Meta.Ferngeist.DurationMs; d == nil || *d < 0 {
		t.Fatalf("reply carries no turn duration: %s", frames[0])
	}

	// An error reply has no result to annotate and passes untouched.
	out = ids.outbound(1, parseClientFrame([]byte(`{"jsonrpc":"2.0","id":8,"method":"session/prompt","params":{"sessionId":"s","prompt":[]}}`)))
	failed := `{"jsonrpc":"2.0","id":` + agentIDOf(t, out) + `,"error":{"code":-1,"message":"x"}}`
	if frames := ids.reply(1, replyProbe(t, failed), []string{failed}); strings.Contains(frames[0], "durationMs") {
		t.Fatalf("error reply was annotated: %s", frames[0])
	}
}

func TestRequestIDsDropOtherRepliesForAnEarlierConnection(t *testing.T) {
	var ids requestIDs
	out := ids.outbound(1, parseClientFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/set_mode","params":{"sessionId":"s"}}`)))
	reply := `{"jsonrpc":"2.0","id":` + agentIDOf(t, out) + `,"result":{}}`
	if frames := ids.reply(2, replyProbe(t, reply), []string{reply}); len(frames) != 0 {
		t.Fatalf("a stale non-prompt reply must be withheld, got %v", frames)
	}
}

func TestRequestIDsRetargetCancelRequest(t *testing.T) {
	var ids requestIDs
	agentID := agentIDOf(t, ids.outbound(4, parseClientFrame([]byte(`{"jsonrpc":"2.0","id":7,"method":"session/prompt","params":{"sessionId":"s"}}`))))
	cancel := ids.outbound(4, parseClientFrame([]byte(`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":7}}`)))
	if !strings.Contains(string(cancel), `"requestId":`+agentID) {
		t.Fatalf("cancel should target agent id %s, got %s", agentID, cancel)
	}
}

func TestRequestIDsPassAgentRequestRepliesThrough(t *testing.T) {
	var ids requestIDs
	reply := []byte(`{"jsonrpc":"2.0","id":0,"result":{"outcome":{"outcome":"cancelled"}}}`)
	if out := ids.outbound(1, parseClientFrame(reply)); string(out) != string(reply) {
		t.Fatalf("a reply to the agent's own request must pass unchanged, got %s", out)
	}
}

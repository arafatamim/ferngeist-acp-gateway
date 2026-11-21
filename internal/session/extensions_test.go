package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/agentext"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/turns"
	"github.com/coder/acp-go-sdk"
)

// attachPumpExtensions binds an engine with the given BeforeState registrations
// and closes it when the test ends.
func attachPumpExtensions(t *testing.T, p *StdioPump, transformers ...agentext.Transformer) {
	t.Helper()
	registry, err := agentext.New(agentext.Set{Transformers: transformers})
	if err != nil {
		t.Fatalf("agentext.New: %v", err)
	}
	p.ext = registry.Bind(context.Background(),
		agentext.Identity{AgentID: "agent", RuntimeID: "rt", SessionID: "gs"}, nil, nil)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := p.ext.Close(ctx); err != nil {
			t.Errorf("Close extensions: %v", err)
		}
	})
}

func beforeState(name string, selector agentext.Selector, failure agentext.Failure, run agentext.TransformFunc) agentext.Transformer {
	return agentext.Transformer{
		Name: name, Selector: selector, Stage: agentext.BeforeState, Mode: agentext.Inline, Failure: failure, Run: run,
	}
}

// rewriteObject copies body, lets mutate change its top-level members, and
// returns the owned result.
func rewriteObject(t *testing.T, body json.RawMessage, mutate func(map[string]json.RawMessage)) agentext.Payload {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	mutate(object)
	raw, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("encode body: %v", err)
	}
	payload, err := agentext.NewPayload(raw)
	if err != nil {
		t.Fatalf("NewPayload: %v", err)
	}
	return payload
}

func rewriteParams(t *testing.T, body json.RawMessage, mutate func(map[string]json.RawMessage)) agentext.Payload {
	t.Helper()
	return rewriteObject(t, body, func(object map[string]json.RawMessage) {
		var params map[string]json.RawMessage
		if err := json.Unmarshal(object["params"], &params); err != nil {
			t.Fatalf("decode params: %v", err)
		}
		mutate(params)
		raw, err := json.Marshal(params)
		if err != nil {
			t.Fatalf("encode params: %v", err)
		}
		object["params"] = raw
	})
}

func rejectTransform(name string, selector agentext.Selector) agentext.Transformer {
	return beforeState(name, selector, agentext.Reject, func(context.Context, agentext.TransformInput) (agentext.Payload, error) {
		return agentext.Payload{}, errors.New("refused by test adapter")
	})
}

func TestSessionCloseWaitsForAfterTurnWork(t *testing.T) {
	rs, store, agent, _, ctx := setupTest(t, Config{})
	defer store.Close()
	defer rs.Shutdown()
	started := make(chan struct{})
	release := make(chan struct{})
	rs.cfg.ResolveHarness = func(string, string, []string) Harness {
		return Harness{TurnFinished: func(context.Context, string) error {
			close(started)
			<-release
			return nil
		}}
	}
	sess, _, err := rs.Create(ctx, agent, "device", nil)
	if err != nil {
		t.Fatal(err)
	}
	sess.pump.onTurnFinished(turns.Status{SessionID: "chat"})
	<-started
	closed := make(chan error, 1)
	go func() { closed <- rs.Close(ctx, sess.ID, "device") }()
	select {
	case err := <-closed:
		close(release)
		t.Fatalf("session closed while after-turn work was running: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session did not close after after-turn work finished")
	}
}

func TestBeforeStateCwdBecomesCanonical(t *testing.T) {
	p := newPump()
	p.agentID, p.sessionID, p.runtimeID = "agent", "gs", "rt"
	var writes []string
	p.proc = &runtime.Process{Stdin: recordingWriteCloser{record: func(b []byte) {
		writes = append(writes, string(bytes.TrimSpace(b)))
	}}}
	attachPumpExtensions(t, p, beforeState("cwd",
		agentext.Selector{Direction: agentext.ToAgent, Kind: agentext.Request, Method: "session/load"},
		agentext.KeepOriginal,
		func(_ context.Context, in agentext.TransformInput) (agentext.Payload, error) {
			return rewriteParams(t, in.Message.Body.Bytes(), func(params map[string]json.RawMessage) {
				params["cwd"] = json.RawMessage(`"/canonical"`)
			}), nil
		}))

	request := `{"jsonrpc":"2.0","id":1,"method":"session/load","params":{"sessionId":"ses_a","cwd":"/original"}}`
	if err := p.WriteToAgent(p.connGen, []byte(request)); err != nil {
		t.Fatalf("WriteToAgent: %v", err)
	}
	if len(writes) != 1 {
		t.Fatalf("stdin writes = %q, want one", writes)
	}
	var sent struct {
		Params struct {
			Cwd string `json:"cwd"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(writes[0]), &sent); err != nil {
		t.Fatalf("decode forwarded frame: %v", err)
	}
	if sent.Params.Cwd != "/canonical" {
		t.Fatalf("forwarded cwd = %q, want /canonical", sent.Params.Cwd)
	}
	if got := p.AcpCwdFor("ses_a"); got != "/canonical" {
		t.Fatalf("canonical cwd = %q, want /canonical", got)
	}
}

func TestNormalizedPromptResultUsesCanonicalOutcome(t *testing.T) {
	p := newPump()
	p.agentID, p.sessionID, p.runtimeID = "agent", "gs", "rt"
	recorded := outcomeCollector(p)
	attachPumpExtensions(t, p, beforeState("prompt-outcome",
		agentext.Selector{Direction: agentext.ToClient, Kind: agentext.Reply, Method: "session/prompt"},
		agentext.KeepOriginal,
		func(_ context.Context, in agentext.TransformInput) (agentext.Payload, error) {
			return rewriteObject(t, in.Message.Body.Bytes(), func(object map[string]json.RawMessage) {
				var result map[string]json.RawMessage
				if err := json.Unmarshal(object["result"], &result); err != nil {
					t.Fatalf("decode result: %v", err)
				}
				result["stopReason"] = json.RawMessage(`"refusal"`)
				raw, err := json.Marshal(result)
				if err != nil {
					t.Fatalf("encode result: %v", err)
				}
				object["result"] = raw
			}), nil
		}))

	id := sendPrompt(t, p, 1, "ses_a")
	p.handleStdoutLine(agentReply(id, `"result":{"stopReason":"end_turn"}`))
	if len(*recorded) != 1 {
		t.Fatalf("outcomes = %+v, want one", *recorded)
	}
	if got := (*recorded)[0]; got.State != turns.StateFailed || got.StopReason != acp.StopReasonRefusal {
		t.Fatalf("outcome = %+v, want failed/refusal from the effective body", got)
	}
}

func TestNormalizedPromptSuccessStillObeysSwallowedFailure(t *testing.T) {
	p := newPump()
	p.agentID, p.sessionID, p.runtimeID = "agent", "gs", "rt"
	recorded := outcomeCollector(p)
	attachPumpExtensions(t, p, beforeState("prompt-outcome",
		agentext.Selector{Direction: agentext.ToClient, Kind: agentext.Reply, Method: "session/prompt"},
		agentext.KeepOriginal,
		func(_ context.Context, in agentext.TransformInput) (agentext.Payload, error) {
			return rewriteObject(t, in.Message.Body.Bytes(), func(object map[string]json.RawMessage) {
				var result map[string]json.RawMessage
				if err := json.Unmarshal(object["result"], &result); err != nil {
					t.Fatalf("decode result: %v", err)
				}
				result["stopReason"] = json.RawMessage(`"end_turn"`)
				raw, err := json.Marshal(result)
				if err != nil {
					t.Fatalf("encode result: %v", err)
				}
				object["result"] = raw
			}), nil
		}))

	id := sendPrompt(t, p, 1, "ses_a")
	p.handleStdoutLine(agentReply(id, `"result":{}`))
	if len(*recorded) != 1 {
		t.Fatalf("outcomes = %+v, want one", *recorded)
	}
	if got := (*recorded)[0]; got.State != turns.StateFailed {
		t.Fatalf("outcome = %+v, want failed: the canonical swallowed-failure check overrides the normalized success", got)
	}
}

func TestRejectedStalePromptStillEmitsTurnEnded(t *testing.T) {
	p := newPump()
	p.agentID, p.sessionID, p.runtimeID = "agent", "gs", "rt"
	recorded := outcomeCollector(p)
	attachPumpExtensions(t, p, rejectTransform("reject-prompt",
		agentext.Selector{Direction: agentext.ToClient, Kind: agentext.Reply, Method: "session/prompt"}))

	gen, _ := connect(t, p)
	id := sendPrompt(t, p, 1, "ses_a")
	if gen != p.connGen {
		t.Fatalf("first generation = %d, current %d", gen, p.connGen)
	}
	p.handleStdoutLine(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_a","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hi"}}}}`)
	_, successor := connect(t, p) // a newer connection supersedes the prompt's generation
	p.handleStdoutLine(agentReply(id, `"result":{"stopReason":"end_turn"}`))

	if len(*recorded) != 1 || (*recorded)[0].State != turns.StateSucceeded {
		t.Fatalf("outcomes = %+v, want one successful turn from canonical bookkeeping", *recorded)
	}
	got := readFrames(t, p, successor, 1)
	if !strings.Contains(got[0], turnEndedMethod) || !strings.Contains(got[0], "ses_a") {
		t.Fatalf("successor frame = %s, want a turn-ended notification for ses_a", got[0])
	}
}

func TestRejectedClientRequestGetsGatewayError(t *testing.T) {
	p := newPump()
	p.agentID, p.sessionID, p.runtimeID = "agent", "gs", "rt"
	var writes []string
	p.proc = &runtime.Process{Stdin: recordingWriteCloser{record: func(b []byte) {
		writes = append(writes, string(b))
	}}}
	gen, client := connect(t, p)
	attachPumpExtensions(t, p, rejectTransform("reject-inspect",
		agentext.Selector{Direction: agentext.ToAgent, Kind: agentext.Request, Method: "example/inspect"}))

	request := `{"jsonrpc":"2.0","id":"client-1","method":"example/inspect","params":{"path":"/x"}}`
	if err := p.WriteToAgent(gen, []byte(request)); err != nil {
		t.Fatalf("WriteToAgent: %v", err)
	}
	got := readFrames(t, p, client, 1)
	var reply struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(got[0]), &reply); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if string(reply.ID) != `"client-1"` || reply.Error.Code != agentext.RejectRPCErrorCode ||
		reply.Error.Message != agentext.RejectRPCErrorMessage {
		t.Fatalf("reply = %s, want the client id with -32603", got[0])
	}
	if len(writes) != 0 {
		t.Fatalf("rejected request reached the agent: %q", writes)
	}
	p.reqIDs.mu.Lock()
	pending := len(p.reqIDs.pending)
	p.reqIDs.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending requests = %d, want none", pending)
	}
}

func TestRejectedAgentNotificationClosesGeneration(t *testing.T) {
	p := newPump()
	p.agentID, p.sessionID, p.runtimeID = "agent", "gs", "rt"
	_, client := connect(t, p)
	attachPumpExtensions(t, p, rejectTransform("reject-update",
		agentext.Selector{Direction: agentext.ToClient, Kind: agentext.Notification, Method: "session/update"}))

	p.handleStdoutLine(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_a","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hi"}}}}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := client.Read(ctx); err == nil {
		t.Fatal("rejected notification was delivered to the client")
	}
}

func TestRejectedAgentRequestAnsweredOnStdin(t *testing.T) {
	p := newPump()
	p.agentID, p.sessionID, p.runtimeID = "agent", "gs", "rt"
	var writes []string
	p.proc = &runtime.Process{Stdin: recordingWriteCloser{record: func(b []byte) {
		writes = append(writes, string(bytes.TrimSpace(b)))
	}}}
	_, client := connect(t, p)
	attachPumpExtensions(t, p, rejectTransform("reject-permission",
		agentext.Selector{Direction: agentext.ToClient, Kind: agentext.Request, Method: "session/request_permission"}))

	p.handleStdoutLine(`{"jsonrpc":"2.0","id":"perm-1","method":"session/request_permission","params":{"sessionId":"ses_a","options":[]}}`)
	if got := readFrames(t, p, client, 0); len(got) != 0 {
		t.Fatalf("client saw frames for a rejected agent request: %q", got)
	}
	if len(writes) != 1 {
		t.Fatalf("stdin writes = %q, want one error answer", writes)
	}
	var reply struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(writes[0]), &reply); err != nil {
		t.Fatalf("decode stdin answer: %v", err)
	}
	if string(reply.ID) != `"perm-1"` || reply.Error.Code != agentext.RejectRPCErrorCode {
		t.Fatalf("stdin answer = %s, want -32603 with the agent's id", writes[0])
	}
	p.reqIDs.mu.Lock()
	tracked := len(p.reqIDs.agentReqs)
	p.reqIDs.mu.Unlock()
	if tracked != 0 {
		t.Fatalf("agent requests tracked = %d, want none for a rejected request", tracked)
	}
}

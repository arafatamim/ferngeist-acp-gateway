package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/agentext"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/turns"
	"github.com/coder/acp-go-sdk"
	"github.com/coder/websocket"
)

// attachPresentation binds a BeforeDelivery engine with an optional shared
// budget, closing it when the test ends.
func attachPresentation(t *testing.T, p *StdioPump, budget *agentext.Budget, transformers ...agentext.Transformer) {
	t.Helper()
	registry, err := agentext.New(agentext.Set{Transformers: transformers})
	if err != nil {
		t.Fatalf("agentext.New: %v", err)
	}
	p.ext = registry.Bind(context.Background(),
		agentext.Identity{AgentID: p.agentID, RuntimeID: p.runtimeID, SessionID: p.sessionID}, budget,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := p.ext.Close(ctx); err != nil {
			t.Errorf("Close extensions: %v", err)
		}
	})
}

func deferredReply(name, method string, failure agentext.Failure, run agentext.TransformFunc) agentext.Transformer {
	return agentext.Transformer{
		Name:             name,
		Selector:         agentext.Selector{Direction: agentext.ToClient, Kind: agentext.Reply, Method: agentext.Method(method)},
		Stage:            agentext.BeforeDelivery,
		Mode:             agentext.Deferred,
		Failure:          failure,
		IndependentReply: true,
		Run:              run,
	}
}

func inlineReply(name, method string, run agentext.TransformFunc) agentext.Transformer {
	transformer := deferredReply(name, method, agentext.KeepOriginal, run)
	transformer.Mode = agentext.Inline
	return transformer
}

// readOne reads exactly one client frame within the test deadline.
func readOne(t *testing.T, client *websocket.Conn) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, msg, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return string(msg)
}

// listRequest prepares a session/list request and returns its agent-side id.
func listRequest(t *testing.T, p *StdioPump, params string) string {
	t.Helper()
	frame := fmt.Sprintf(`{"jsonrpc":"2.0","id":"list","method":"session/list","params":%s}`, params)
	return agentIDOf(t, p.prepareOutbound(p.connGen, parseClientFrame([]byte(frame))))
}

func newPresentationPump(t *testing.T, budget *agentext.Budget, transformers ...agentext.Transformer) (*StdioPump, *websocket.Conn) {
	t.Helper()
	p := newPump()
	p.agentID, p.sessionID, p.runtimeID = "hermes", "gs", "rt"
	attachPresentation(t, p, budget, transformers...)
	_, client := connect(t, p)
	return p, client
}

func TestDeferredPresentationMergesOnTheForwardedReply(t *testing.T) {
	var receivedParams string
	var calls int
	appendNative := deferredReply("list", "session/list", agentext.KeepOriginal,
		func(_ context.Context, in agentext.TransformInput) (agentext.Payload, error) {
			calls++
			receivedParams = string(in.Message.RequestParams.Bytes())
			return rewriteObject(t, in.Message.Body.Bytes(), func(object map[string]json.RawMessage) {
				object["result"] = json.RawMessage(`{"sessions":[{"sessionId":"native"}]}`)
			}), nil
		})
	p, client := newPresentationPump(t, nil, appendNative)
	agentID := listRequest(t, p, `{"cwd":"/work"}`)
	p.handleStdoutLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"sessions":[]}}`, agentID))

	got := readOne(t, client)
	if !strings.Contains(got, `"id":"list"`) {
		t.Fatalf("frame = %s, want the client's original id", got)
	}
	if !strings.Contains(got, `"native"`) {
		t.Fatalf("frame = %s, want the presented native chat", got)
	}
	if calls != 1 || !strings.Contains(receivedParams, `"cwd":"/work"`) {
		t.Fatalf("calls = %d, params = %q, want one call with the request's params", calls, receivedParams)
	}
}

func TestDeferredPresentationDoesNotDelayFollowingFrames(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	presented := deferredReply("list", "session/list", agentext.KeepOriginal,
		func(ctx context.Context, in agentext.TransformInput) (agentext.Payload, error) {
			close(entered)
			select {
			case <-release:
				return rewriteObject(t, in.Message.Body.Bytes(), func(object map[string]json.RawMessage) {
					object["result"] = json.RawMessage(`{"sessions":[{"sessionId":"native"}]}`)
				}), nil
			case <-ctx.Done():
				return agentext.Payload{}, ctx.Err()
			}
		})
	p, client := newPresentationPump(t, nil, presented)
	agentID := listRequest(t, p, `{}`)
	p.handleStdoutLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"sessions":[]}}`, agentID))
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("presentation never started")
	}
	const notification = `{"jsonrpc":"2.0","method":"agent/progress","params":{}}`
	p.handleStdoutLine(notification)
	if got := readOne(t, client); got != notification {
		t.Fatalf("first frame = %s, want the later notification to overtake the held reply", got)
	}
	close(release)
	if got := readOne(t, client); !strings.Contains(got, `"native"`) {
		t.Fatalf("second frame = %s, want the presented reply", got)
	}
}

func TestStalePresentationIsSkippedEntirely(t *testing.T) {
	var calls int
	p, _ := newPresentationPump(t, nil,
		deferredReply("list", "session/list", agentext.Reject,
			func(context.Context, agentext.TransformInput) (agentext.Payload, error) {
				calls++
				return agentext.Payload{}, fmt.Errorf("refused")
			}))
	agentID := listRequest(t, p, `{}`)
	_, successor := connect(t, p) // a newer connection supersedes the request's generation
	p.handleStdoutLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"sessions":[]}}`, agentID))

	if calls != 0 {
		t.Fatalf("presentation ran %d times for a stale reply", calls)
	}
	if got := readFrames(t, p, successor, 0); len(got) != 0 {
		t.Fatalf("successor saw frames for a stale reply: %q", got)
	}
}

func TestPresentationSkipsAgentErrors(t *testing.T) {
	var calls int
	p, client := newPresentationPump(t, nil,
		deferredReply("list", "session/list", agentext.KeepOriginal,
			func(context.Context, agentext.TransformInput) (agentext.Payload, error) {
				calls++
				return agentext.Payload{}, nil
			}))
	agentID := listRequest(t, p, `{}`)
	p.handleStdoutLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-1,"message":"boom"}}`, agentID))

	if got := readOne(t, client); !strings.Contains(got, `"error"`) {
		t.Fatalf("frame = %s, want the agent's error untouched", got)
	}
	if calls != 0 {
		t.Fatalf("presentation ran %d times on an agent error", calls)
	}
}

func TestDroppedRequestParamsSkipPresentation(t *testing.T) {
	var calls int
	transform := deferredReply("list", "session/list", agentext.KeepOriginal,
		func(context.Context, agentext.TransformInput) (agentext.Payload, error) {
			calls++
			return agentext.Payload{}, nil
		})
	p, client := newPresentationPump(t, agentext.NewBudget(0), transform)
	agentID := listRequest(t, p, `{"cwd":"/work"}`)
	const agentReply = `{"jsonrpc":"2.0","id":%s,"result":{"sessions":[]}}`
	p.handleStdoutLine(fmt.Sprintf(agentReply, agentID))

	if got := readOne(t, client); !strings.Contains(got, `"sessions":[]`) {
		t.Fatalf("frame = %s, want the agent's original reply", got)
	}
	if calls != 0 {
		t.Fatalf("presentation ran without its retained params (calls = %d)", calls)
	}
}

func TestPresentationUsesTheRegistrationContractForAnotherMethod(t *testing.T) {
	transform := inlineReply("inspect", "example/inspect",
		func(_ context.Context, in agentext.TransformInput) (agentext.Payload, error) {
			return rewriteObject(t, in.Message.Body.Bytes(), func(object map[string]json.RawMessage) {
				object["result"] = json.RawMessage(`{"transformed":true}`)
			}), nil
		})
	p, client := newPresentationPump(t, nil, transform)
	agentID := agentIDOf(t, p.prepareOutbound(p.connGen,
		parseClientFrame([]byte(`{"jsonrpc":"2.0","id":5,"method":"example/inspect","params":{}}`))))
	p.handleStdoutLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, agentID))

	got := readOne(t, client)
	if !strings.Contains(got, `"id":5`) || !strings.Contains(got, `"transformed":true`) {
		t.Fatalf("frame = %s, want the transformed reply under the client's id", got)
	}
}

func TestPresentationDoesNotRewriteRecordedTurn(t *testing.T) {
	recorded := make([]turns.Status, 0, 1)
	var mu sync.Mutex
	transform := deferredReply("prompt", "session/prompt", agentext.KeepOriginal,
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
		})
	p, client := newPresentationPump(t, nil, transform)
	p.onTurnFinished = func(st turns.Status) {
		mu.Lock()
		recorded = append(recorded, st)
		mu.Unlock()
	}
	id := sendPrompt(t, p, 1, "ses_a")
	p.handleStdoutLine(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_a","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hi"}}}}`)
	p.handleStdoutLine(agentReply(id, `"result":{"stopReason":"end_turn"}`))

	if update := readOne(t, client); !strings.Contains(update, "agent_message_chunk") {
		t.Fatalf("first frame = %s, want the turn's content update", update)
	}
	if got := readOne(t, client); !strings.Contains(got, `"stopReason":"refusal"`) {
		t.Fatalf("frame = %s, want the presented refusal", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(recorded) != 1 || recorded[0].State != turns.StateSucceeded || recorded[0].StopReason != acp.StopReasonEndTurn {
		t.Fatalf("outcomes = %+v, want the original successful end_turn", recorded)
	}
}

func TestStalePromptPresentationYieldsTurnEnded(t *testing.T) {
	var calls int
	p, _ := newPresentationPump(t, nil,
		deferredReply("prompt", "session/prompt", agentext.KeepOriginal,
			func(context.Context, agentext.TransformInput) (agentext.Payload, error) {
				calls++
				return agentext.Payload{}, nil
			}))
	id := sendPrompt(t, p, 1, "ses_a")
	p.handleStdoutLine(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_a","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hi"}}}}`)
	_, successor := connect(t, p)
	p.handleStdoutLine(agentReply(id, `"result":{"stopReason":"end_turn"}`))

	if calls != 0 {
		t.Fatalf("presentation ran %d times for a stale prompt reply", calls)
	}
	got := readFrames(t, p, successor, 1)
	if !strings.Contains(got[0], turnEndedMethod) {
		t.Fatalf("successor frame = %s, want turn-ended", got[0])
	}
}

func TestStaleDeferredFailureCannotCloseSuccessor(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	p, _ := newPresentationPump(t, nil,
		deferredReply("list", "session/list", agentext.Reject,
			func(ctx context.Context, _ agentext.TransformInput) (agentext.Payload, error) {
				close(entered)
				select {
				case <-release:
					return agentext.Payload{}, fmt.Errorf("refused")
				case <-ctx.Done():
					return agentext.Payload{}, ctx.Err()
				}
			}))
	agentID := listRequest(t, p, `{}`)
	p.handleStdoutLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"sessions":[]}}`, agentID))
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("presentation never started")
	}
	_, successor := connect(t, p) // takeover while the callback is held
	close(release)
	if got := readFrames(t, p, successor, 0); len(got) != 0 {
		t.Fatalf("successor saw frames from a stale failure: %q", got)
	}
}

func TestPresentationReleasesBudgetBetweenReplies(t *testing.T) {
	var calls int
	transform := deferredReply("list", "session/list", agentext.KeepOriginal,
		func(_ context.Context, in agentext.TransformInput) (agentext.Payload, error) {
			calls++
			return rewriteObject(t, in.Message.Body.Bytes(), func(object map[string]json.RawMessage) {
				object["result"] = json.RawMessage(`{"sessions":[{"sessionId":"native"}]}`)
			}), nil
		})
	p, client := newPresentationPump(t, agentext.NewBudget(150), transform)
	for i := 0; i < 2; i++ {
		agentID := listRequest(t, p, `{"cwd":"/work"}`)
		p.handleStdoutLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"sessions":[]}}`, agentID))
		if got := readOne(t, client); !strings.Contains(got, `"native"`) {
			t.Fatalf("reply %d = %s, want presentation after the previous budget release", i+1, got)
		}
	}
	if calls != 2 {
		t.Fatalf("presentation calls = %d, want both replies presented", calls)
	}
}

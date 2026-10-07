package session

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/push"
	"github.com/coder/websocket"
)

// wsPair returns a connected server-side conn (what the pump binds) and the
// client-side conn the test reads from.
func wsPair(t *testing.T) (server, client *websocket.Conn) {
	t.Helper()
	serverCh := make(chan *websocket.Conn, 1)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		serverCh <- c
	}))
	t.Cleanup(s.Close)
	client, _, err := websocket.Dial(context.Background(), "ws://"+s.Listener.Addr().String()+"/", nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { client.CloseNow() })
	return <-serverCh, client
}

// quietFrame is the sentinel readFrames pushes to prove nothing else was queued.
const quietFrame = `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"quiet","update":{}}}`

// readFrames reads n frames, then checks no other frame was queued ahead of a
// sentinel. (A timed-out Read closes the connection, so it cannot probe.)
func readFrames(t *testing.T, p *StdioPump, c *websocket.Conn, n int) []string {
	t.Helper()
	var out []string
	p.handleStdoutLine(quietFrame)
	for i := 0; i < n+1; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, msg, err := c.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("read frame %d of %d: %v", i+1, n, err)
		}
		out = append(out, string(msg))
	}
	if last := out[n]; last != quietFrame {
		t.Fatalf("unexpected extra frame before sentinel: %s", last)
	}
	return out[:n]
}

// connect attaches and binds a fresh client, returning its generation and conn.
func connect(t *testing.T, p *StdioPump) (int64, *websocket.Conn) {
	t.Helper()
	server, client := wsPair(t)
	gen := p.Attach()
	if !p.Bind(server, gen) {
		t.Fatal("Bind should succeed")
	}
	t.Cleanup(func() {
		p.Detach(gen)
		_ = server.CloseNow()
	})
	return gen, client
}

// loadAndReply sends session/load for sessionID as client id 1 on gen and feeds
// the agent's success reply through the drain path.
func loadAndReply(t *testing.T, p *StdioPump, gen int64, sessionID string) {
	t.Helper()
	agent := p.reqIDs.outbound(gen, []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"session/load","params":{"sessionId":%q}}`, sessionID)))
	p.handleStdoutLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":null}`, agentIDOf(t, agent)))
}

const permissionRequest = `{"jsonrpc":"2.0","id":7,"method":"session/request_permission","params":{"sessionId":"ses_a","options":[]}}`

func newPump() *StdioPump {
	return &StdioPump{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// A permission request emitted while no client is attached, or delivered to a
// client that left without answering, is re-sent right after the next
// connection's session/load reply, and not again once the client answers it.
func TestAgentRequestRedeliveredAfterLoadUntilAnswered(t *testing.T) {
	pump := newPump()
	pump.handleStdoutLine(permissionRequest) // app backgrounded: nobody bound

	gen1, c1 := connect(t, pump)
	loadAndReply(t, pump, gen1, "ses_a")
	got := readFrames(t, pump, c1, 2)
	if got[0] != `{"id":1,"jsonrpc":"2.0","result":null}` || got[1] != permissionRequest {
		t.Fatalf("first connection frames = %q", got)
	}

	// Left without answering: the next connection gets it again.
	gen2, c2 := connect(t, pump)
	loadAndReply(t, pump, gen2, "ses_a")
	if got := readFrames(t, pump, c2, 2); got[1] != permissionRequest {
		t.Fatalf("second connection frames = %q", got)
	}

	// Another session's load does not carry it.
	loadAndReply(t, pump, gen2, "ses_other")
	readFrames(t, pump, c2, 1)

	// Answered: gone for good.
	pump.reqIDs.outbound(gen2, []byte(`{"jsonrpc":"2.0","id":7,"result":{"outcome":{"outcome":"cancelled"}}}`))
	gen3, c3 := connect(t, pump)
	loadAndReply(t, pump, gen3, "ses_a")
	readFrames(t, pump, c3, 1)
	if n := len(pump.reqIDs.agentReqs); n != 0 {
		t.Fatalf("answered request leaked: %d tracked", n)
	}
}

// A request delivered live to the current connection is not repeated by that
// connection's own session/load reply.
func TestAgentRequestLiveDeliveryNotRepeatedOnSameGeneration(t *testing.T) {
	pump := newPump()
	gen, c := connect(t, pump)
	pump.handleStdoutLine(permissionRequest)
	readFrames(t, pump, c, 1)
	loadAndReply(t, pump, gen, "ses_a")
	readFrames(t, pump, c, 1)
}

// Recovering an "already loaded" rejection ends in a synthesized success, which
// carries the pending requests too.
func TestAgentRequestRedeliveredAfterRecoveredLoad(t *testing.T) {
	pump := newPump()
	pump.loadRecovery = newTestLoadRecovery()
	pump.handleStdoutLine(permissionRequest)

	gen, c := connect(t, pump)
	agent := pump.reqIDs.outbound(gen, []byte(`{"jsonrpc":"2.0","id":1,"method":"session/load","params":{"sessionId":"ses_a"}}`))
	pump.loadRecovery.OnOutbound(agent)
	pump.handleStdoutLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32602,"message":"Session already loaded"}}`, agentIDOf(t, agent)))
	if got := readFrames(t, pump, c, 2); got[1] != permissionRequest {
		t.Fatalf("frames = %q", got)
	}
}

func TestAgentRequestsAreBounded(t *testing.T) {
	var ids requestIDs
	for i := 0; i < maxPendingAgentRequests+10; i++ {
		line := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"fs/read_text_file","params":{"sessionId":"s"}}`, i)
		ids.trackAgentRequest(replyProbe(t, line), line, 0)
	}
	if n := len(ids.agentReqs); n != maxPendingAgentRequests {
		t.Fatalf("tracked %d requests, want cap %d", n, maxPendingAgentRequests)
	}
}

func pushCollector(p *StdioPump) *[]PushEvent {
	var events []PushEvent
	p.onPushNotification = func(e PushEvent) { events = append(events, e) }
	return &events
}

// The "already loaded" rejection that load recovery hides from the client, and
// error replies to anything other than a prompt, must not push "Agent Error".
func TestErrorPushOnlyForPromptReplies(t *testing.T) {
	pump := newPump()
	pump.loadRecovery = newTestLoadRecovery()
	events := pushCollector(pump)

	load := pump.reqIDs.outbound(1, []byte(`{"jsonrpc":"2.0","id":1,"method":"session/load","params":{"sessionId":"ses_a"}}`))
	pump.loadRecovery.OnOutbound(load)
	pump.handleStdoutLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32602,"message":"Session already loaded"}}`, agentIDOf(t, load)))

	mode := pump.reqIDs.outbound(1, []byte(`{"jsonrpc":"2.0","id":2,"method":"session/set_mode","params":{"sessionId":"ses_a","modeId":"x"}}`))
	pump.handleStdoutLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"nope"}}`, agentIDOf(t, mode)))

	pump.handleStdoutLine(`{"jsonrpc":"2.0","id":99,"error":{"code":-1,"message":"unknown id"}}`)
	if len(*events) != 0 {
		t.Fatalf("unexpected pushes: %+v", *events)
	}

	prompt := pump.reqIDs.outbound(1, []byte(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"ses_b","prompt":[]}}`))
	pump.handleStdoutLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-1,"message":"boom"}}`, agentIDOf(t, prompt)))
	if len(*events) != 1 || (*events)[0].Category != push.CategoryError || (*events)[0].AcpSessionID != "ses_b" {
		t.Fatalf("prompt error push = %+v", *events)
	}
}

// One agent process can host several chats: each push deep-links to the session
// its own frame belongs to, not to the first one the pump saw.
func TestPushSessionIDFollowsTheFrame(t *testing.T) {
	pump := newPump()
	events := pushCollector(pump)
	pump.handleStdoutLine(`{"jsonrpc":"2.0","id":"n1","result":{"sessionId":"ses_first"}}`)

	pump.handleStdoutLine(`{"jsonrpc":"2.0","id":5,"method":"session/request_permission","params":{"sessionId":"ses_second","options":[]}}`)
	pump.handleStdoutLine(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_second","update":{"sessionUpdate":"tool_call","toolCallId":"c","status":"in_progress","title":"Editing"}}}`)
	prompt := pump.reqIDs.outbound(1, []byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"ses_second","prompt":[]}}`))
	pump.handleStdoutLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn","usage":{"totalTokens":3}}}`, agentIDOf(t, prompt)))

	if len(*events) != 3 {
		t.Fatalf("events = %+v", *events)
	}
	for _, e := range *events {
		if e.AcpSessionID != "ses_second" {
			t.Errorf("%s push deep-links to %q, want ses_second", e.Category, e.AcpSessionID)
		}
	}
	if got := pump.AcpSessionID(); got != "ses_first" {
		t.Fatalf("AcpSessionID() = %q, want it unchanged", got)
	}
}

// A reply to another request arriving mid-turn (set_mode) is not the prompt
// result, so it must not stop the swallowed-failure detector from judging the
// real prompt result.
func TestSetModeReplyMidTurnDoesNotDisarmSwallowedFailureDetection(t *testing.T) {
	pump := newPump()
	prompt := pump.reqIDs.outbound(1, []byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"ses_a","prompt":[]}}`))
	pump.markTurnStart(prompt)
	mode := pump.reqIDs.outbound(1, []byte(`{"jsonrpc":"2.0","id":2,"method":"session/set_mode","params":{"sessionId":"ses_a","modeId":"x"}}`))

	modeReply := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, agentIDOf(t, mode))
	if _, handled := pump.markTurnActivity(modeReply); handled {
		t.Fatal("set_mode reply must pass through")
	}
	promptReply := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}`, agentIDOf(t, prompt))
	if _, handled := pump.markTurnActivity(promptReply); !handled {
		t.Fatal("empty prompt result must still be flagged as a swallowed failure")
	}
}

func TestAgentRequestsDroppedWhenTheirTurnEnds(t *testing.T) {
	var r requestIDs
	r.outbound(1, []byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s1"}}`))
	req := `{"jsonrpc":"2.0","id":"perm-1","method":"session/request_permission","params":{"sessionId":"s1"}}`
	probe, _ := parseFrameProbe([]byte(req))
	r.trackAgentRequest(probe, req, 0)

	reply := `{"jsonrpc":"2.0","id":1,"result":{"stopReason":"cancelled"}}`
	replyProbe, _ := parseFrameProbe([]byte(reply))
	r.reply(1, replyProbe, []string{reply})

	if got := r.redeliver(2, "s1", nil); len(got) != 0 {
		t.Fatalf("redeliver after turn ended = %v, want none", got)
	}
}

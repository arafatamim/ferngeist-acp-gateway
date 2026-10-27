package session

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/push"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/turns"
	"github.com/coder/acp-go-sdk"
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
	t.Cleanup(func() {
		if err := client.CloseNow(); err != nil {
			t.Errorf("close client websocket: %v", err)
		}
	})
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
	agent := p.reqIDs.outbound(gen, parseClientFrame([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"session/load","params":{"sessionId":%q}}`, sessionID))))
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
	pump.reqIDs.outbound(gen2, parseClientFrame([]byte(`{"jsonrpc":"2.0","id":7,"result":{"outcome":{"outcome":"cancelled"}}}`)))
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

// Recovering an "already loaded" rejection announces the resume, ends in a
// synthesized success, and then carries the pending requests too.
func TestAgentRequestRedeliveredAfterRecoveredLoad(t *testing.T) {
	pump := newPump()
	pump.loadRecovery = newTestLoadRecovery()
	pump.handleStdoutLine(permissionRequest)

	gen, c := connect(t, pump)
	agent := pump.reqIDs.outbound(gen, parseClientFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/load","params":{"sessionId":"ses_a"}}`)))
	pump.loadRecovery.OnOutbound(parseClientFrame(agent))
	pump.handleStdoutLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32602,"message":"Session already loaded"}}`, agentIDOf(t, agent)))
	got := readFrames(t, pump, c, 3)
	if got[0] != resumedFrame("ses_a", 0, false) {
		t.Fatalf("resume notice = %s", got[0])
	}
	if got[1] != `{"id":1,"jsonrpc":"2.0","result":null}` {
		t.Fatalf("synthesized load success = %s", got[1])
	}
	if got[2] != permissionRequest {
		t.Fatalf("redelivered request = %s", got[2])
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

	load := pump.reqIDs.outbound(1, parseClientFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/load","params":{"sessionId":"ses_a"}}`)))
	pump.loadRecovery.OnOutbound(parseClientFrame(load))
	pump.handleStdoutLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32602,"message":"Session already loaded"}}`, agentIDOf(t, load)))

	mode := pump.reqIDs.outbound(1, parseClientFrame([]byte(`{"jsonrpc":"2.0","id":2,"method":"session/set_mode","params":{"sessionId":"ses_a","modeId":"x"}}`)))
	pump.handleStdoutLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"nope"}}`, agentIDOf(t, mode)))

	pump.handleStdoutLine(`{"jsonrpc":"2.0","id":99,"error":{"code":-1,"message":"unknown id"}}`)
	if len(*events) != 0 {
		t.Fatalf("unexpected pushes: %+v", *events)
	}

	prompt := pump.reqIDs.outbound(1, parseClientFrame([]byte(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"ses_b","prompt":[]}}`)))
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

	prompt := pump.reqIDs.outbound(1, parseClientFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"ses_second","prompt":[]}}`)))
	pump.handleStdoutLine(`{"jsonrpc":"2.0","id":5,"method":"session/request_permission","params":{"sessionId":"ses_second","options":[]}}`)
	pump.handleStdoutLine(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_second","update":{"sessionUpdate":"tool_call","toolCallId":"c","status":"in_progress","title":"Editing"}}}`)
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
	prompt := pump.reqIDs.outbound(1, parseClientFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"ses_a","prompt":[]}}`)))
	pump.acp.observeInbound(parseClientFrame(prompt))
	mode := pump.reqIDs.outbound(1, parseClientFrame([]byte(`{"jsonrpc":"2.0","id":2,"method":"session/set_mode","params":{"sessionId":"ses_a","modeId":"x"}}`)))

	modeReply := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, agentIDOf(t, mode))
	if _, handled := turnReplacement(t, pump, modeReply); handled {
		t.Fatal("set_mode reply must pass through")
	}
	promptReply := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}`, agentIDOf(t, prompt))
	if _, handled := turnReplacement(t, pump, promptReply); !handled {
		t.Fatal("empty prompt result must still be flagged as a swallowed failure")
	}
}

func TestAgentRequestsDroppedWhenTheirTurnEnds(t *testing.T) {
	var r requestIDs
	r.outbound(1, parseClientFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s1"}}`)))
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

// An elicitation pushes like a permission prompt, and a request-scoped one names
// the id the client used rather than the gateway's translated id.
func TestElicitationPushesAndNamesTheClientRequest(t *testing.T) {
	pump := newPump()
	events := pushCollector(pump)
	gen, c := connect(t, pump)

	auth := pump.reqIDs.outbound(gen, parseClientFrame([]byte(`{"jsonrpc":"2.0","id":"auth-1","method":"authenticate","params":{"methodId":"x"}}`)))
	pump.handleStdoutLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":"e1","method":"elicitation/create","params":{"requestId":%s,"mode":"form","message":"key?","requestedSchema":{}}}`, agentIDOf(t, auth)))
	got := readFrames(t, pump, c, 1)
	if got[0] != `{"id":"e1","jsonrpc":"2.0","method":"elicitation/create","params":{"message":"key?","mode":"form","requestId":"auth-1","requestedSchema":{}}}` {
		t.Fatalf("request-scoped elicitation = %s", got[0])
	}

	session := `{"jsonrpc":"2.0","id":"e2","method":"elicitation/create","params":{"sessionId":"ses_a","mode":"url","elicitationId":"x","url":"https://example.com","message":"sign in"}}`
	pump.handleStdoutLine(session)
	if got := readFrames(t, pump, c, 1); got[0] != session {
		t.Fatalf("session-scoped elicitation altered: %s", got[0])
	}
	if len(*events) != 2 || (*events)[1].Category != push.CategoryPermissionRequest || (*events)[1].AcpSessionID != "ses_a" {
		t.Fatalf("elicitation pushes = %+v", *events)
	}
}

// Progress pushes only fire while a prompt runs (not for the finished tool calls
// session/load replays), and each chat on a shared agent throttles on its own.
func TestProgressPushOnlyDuringPromptAndPerSession(t *testing.T) {
	pump := newPump()
	pump.ProgressInterval = time.Hour
	events := pushCollector(pump)
	update := func(session, call, status string) {
		pump.handleStdoutLine(fmt.Sprintf(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":%q,"update":{"sessionUpdate":"tool_call","toolCallId":%q,"status":%q,"title":"Read %s"}}}`, session, call, status, call))
	}

	load := pump.reqIDs.outbound(1, parseClientFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/load","params":{"sessionId":"a"}}`)))
	update("a", "old1", "completed")
	update("a", "old2", "completed")
	pump.handleStdoutLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":null}`, agentIDOf(t, load)))
	if len(*events) != 0 {
		t.Fatalf("load replay pushed: %+v", *events)
	}

	pump.reqIDs.outbound(1, parseClientFrame([]byte(`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"a","prompt":[]}}`)))
	pump.reqIDs.outbound(1, parseClientFrame([]byte(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"b","prompt":[]}}`)))
	update("a", "c1", "in_progress")
	update("b", "c2", "in_progress") // within a's interval, but b's own throttle
	update("a", "c3", "in_progress") // throttled
	if len(*events) != 2 || (*events)[0].AcpSessionID != "a" || (*events)[1].AcpSessionID != "b" {
		t.Fatalf("progress pushes = %+v", *events)
	}
}

// An empty "success" rewritten into an error pushes exactly one titled error,
// and a cancelled turn pushes nothing.
func TestSwallowedFailurePushesOnceAndCancelIsSilent(t *testing.T) {
	pump := newPump()
	events := pushCollector(pump)
	prompt := func(stopReason string) {
		req := []byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s","prompt":[]}}`)
		pump.acp.observeInbound(parseClientFrame(req))
		id := agentIDOf(t, pump.reqIDs.outbound(1, parseClientFrame(req)))
		pump.handleStdoutLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"stopReason":%q}}`, id, stopReason))
	}

	prompt("end_turn")
	if len(*events) != 1 || (*events)[0].Category != push.CategoryError || (*events)[0].Title == "" {
		t.Fatalf("swallowed failure pushes = %+v", *events)
	}
	prompt("cancelled")
	if len(*events) != 1 {
		t.Fatalf("cancelled turn pushed: %+v", *events)
	}
}

func outcomeCollector(p *StdioPump) *[]turns.Status {
	var got []turns.Status
	p.onTurnFinished = func(st turns.Status) { got = append(got, st) }
	return &got
}

// sendPrompt sends a client prompt for sessionID through the outbound path and
// returns the agent-side id its reply carries.
func sendPrompt(t *testing.T, p *StdioPump, clientID int, sessionID string) string {
	t.Helper()
	frame := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/prompt","params":{"sessionId":%q,"prompt":[]}}`, clientID, sessionID)
	return agentIDOf(t, p.prepareOutbound(parseClientFrame([]byte(frame))))
}

func agentReply(id, body string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,%s}`, id, body)
}

// A prompt's reply records how its turn ended, judged after the swallowed-
// failure rewrite: an empty end_turn is a failure, never a success.
func TestPromptRepliesRecordTheirOutcome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content bool
		reply   string
		state   string
		reason  acp.StopReason
	}{
		{"success", true, `"result":{"stopReason":"end_turn"}`, turns.StateSucceeded, acp.StopReasonEndTurn},
		{"swallowed failure", false, `"result":{"stopReason":"end_turn"}`, turns.StateFailed, ""},
		{"cancelled", false, `"result":{"stopReason":"cancelled"}`, turns.StateCancelled, acp.StopReasonCancelled},
		{"refusal", false, `"result":{"stopReason":"refusal"}`, turns.StateFailed, acp.StopReasonRefusal},
		{"error", false, `"error":{"code":-1,"message":"boom"}`, turns.StateFailed, ""},
		{"no stop reason", false, `"result":{}`, turns.StateUnknown, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPump()
			p.agentID, p.sessionID, p.runtimeID = "agent", "gs", "rt"
			got := outcomeCollector(p)
			id := sendPrompt(t, p, 1, "ses_a")
			if tc.content {
				p.handleStdoutLine(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_a","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hi"}}}}`)
			}
			p.handleStdoutLine(agentReply(id, tc.reply))
			if len(*got) != 1 {
				t.Fatalf("outcomes = %+v, want one", *got)
			}
			st := (*got)[0]
			if st.State != tc.state || st.StopReason != tc.reason || st.AgentID != "agent" || st.SessionID != "ses_a" ||
				st.GatewaySessionID != "gs" || st.RuntimeID != "rt" || st.StartedAt.IsZero() {
				t.Fatalf("outcome = %+v, want state %q reason %q", st, tc.state, tc.reason)
			}
		})
	}
}

// Replies to anything but a prompt, and cancelling a prompt, record nothing:
// only the prompt's own reply ends its turn.
func TestOnlyPromptRepliesRecordOutcomes(t *testing.T) {
	p := newPump()
	got := outcomeCollector(p)
	for i, method := range []string{"initialize", "session/load", "session/list", "session/set_mode"} {
		id := agentIDOf(t, p.prepareOutbound(parseClientFrame([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":{"sessionId":"ses_a"}}`, i+10, method)))))
		p.handleStdoutLine(agentReply(id, `"result":{"stopReason":"end_turn"}`))
	}
	sendPrompt(t, p, 1, "ses_a")
	p.prepareOutbound(parseClientFrame([]byte(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"ses_a"}}`)))
	p.prepareOutbound(parseClientFrame([]byte(`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":1}}`)))
	p.handleStdoutLine(agentReply("999", `"result":{"stopReason":"end_turn"}`))
	if len(*got) != 0 {
		t.Fatalf("outcomes = %+v, want none", *got)
	}
}

// A superseded connection's prompt still records its own outcome, but arriving
// after a newer turn's outcome it does not replace it, and the reconnect replay
// of the newer turn's result leaves the outcome in place.
func TestSupersededPromptReplyNeverClobbersANewerOutcome(t *testing.T) {
	p := newPump()
	store := turns.NewStore()
	var recorded []string
	p.onTurnFinished = func(st turns.Status) {
		recorded = append(recorded, st.State)
		store.Record(st)
	}
	p.Attach()
	older := sendPrompt(t, p, 1, "ses_a")
	p.Attach()
	newer := sendPrompt(t, p, 1, "ses_a")
	p.handleStdoutLine(agentReply(newer, `"result":{"stopReason":"refusal"}`))
	p.handleStdoutLine(agentReply(older, `"result":{"stopReason":"cancelled"}`))
	if !slices.Equal(recorded, []string{turns.StateFailed, turns.StateCancelled}) {
		t.Fatalf("recorded = %v", recorded)
	}

	gen, c := connect(t, p)
	loadAndReply(t, p, gen, "ses_a")
	if got := readFrames(t, p, c, 2); !strings.Contains(got[0], "turn_result") {
		t.Fatalf("reconnect frames = %q, want the kept turn result first", got)
	}
	got := store.Snapshot(nil)
	if len(got) != 1 || got[0].State != turns.StateFailed || got[0].StopReason != acp.StopReasonRefusal {
		t.Fatalf("stored = %+v, want the newer refusal", got)
	}
}

// A prompt that never reached the agent is not left looking like it runs.
func TestFailedPromptWriteLeavesNoRunningTurn(t *testing.T) {
	p := newPump()
	r, w := io.Pipe()
	_ = r.Close() // every write now fails
	p.proc = &runtime.Process{Stdin: w}
	if err := p.WriteToAgent([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"ses_a","prompt":[]}}`)); err == nil {
		t.Fatal("write to a closed stdin succeeded")
	}
	if got := liveSessions(p); len(got) != 0 {
		t.Fatalf("running after failed write = %v", got)
	}
}

// Live turns report the human input their agent waits on, per chat on a shared
// runtime: answered one by one, kept across reconnects, gone with the turn.
func TestLiveTurnsReportAttention(t *testing.T) {
	p := newPump()
	gen := p.Attach()
	promptA := sendPrompt(t, p, 1, "ses_a")
	sendPrompt(t, p, 2, "ses_b")
	auth := agentIDOf(t, p.prepareOutbound(parseClientFrame([]byte(`{"jsonrpc":"2.0","id":3,"method":"authenticate","params":{"methodId":"x"}}`))))
	for _, line := range []string{
		permissionRequest, // id 7, ses_a
		fmt.Sprintf(`{"jsonrpc":"2.0","id":"e1","method":"elicitation/create","params":{"requestId":%s,"mode":"form","message":"?","requestedSchema":{}}}`, promptA),
		`{"jsonrpc":"2.0","id":"e2","method":"elicitation/create","params":{"sessionId":"ses_b","mode":"url","elicitationId":"x","url":"https://example.com","message":"sign in"}}`,
		fmt.Sprintf(`{"jsonrpc":"2.0","id":"e3","method":"elicitation/create","params":{"requestId":%s,"mode":"form","message":"key?","requestedSchema":{}}}`, auth),
		`{"jsonrpc":"2.0","id":"r1","method":"fs/read_text_file","params":{"sessionId":"ses_b","path":"/x"}}`,
	} {
		p.handleStdoutLine(line)
	}
	assertAttention(t, p, map[string]string{"ses_a": "elicitation,permission", "ses_b": "elicitation"})

	if !p.Detach(gen) {
		t.Fatal("detach rejected")
	}
	p.Attach()
	assertAttention(t, p, map[string]string{"ses_a": "elicitation,permission", "ses_b": "elicitation"})

	answer := func(id string) {
		p.prepareOutbound(parseClientFrame([]byte(`{"jsonrpc":"2.0","id":` + id + `,"result":{}}`)))
	}
	answer("7")
	assertAttention(t, p, map[string]string{"ses_a": "elicitation", "ses_b": "elicitation"})
	// The agent reuses the answered id for a request on the other chat.
	p.handleStdoutLine(`{"jsonrpc":"2.0","id":7,"method":"session/request_permission","params":{"sessionId":"ses_b","options":[]}}`)
	answer(`"e2"`)
	assertAttention(t, p, map[string]string{"ses_a": "elicitation", "ses_b": "permission"})

	p.handleStdoutLine(agentReply(promptA, `"result":{"stopReason":"cancelled"}`))
	sendPrompt(t, p, 4, "ses_a")
	assertAttention(t, p, map[string]string{"ses_a": "", "ses_b": "permission"})
}

func assertAttention(t *testing.T, p *StdioPump, want map[string]string) {
	t.Helper()
	got := map[string]string{}
	for _, st := range p.LiveTurns() {
		got[st.SessionID] = strings.Join(st.Attention, ",")
	}
	if !maps.Equal(got, want) {
		t.Fatalf("attention = %v, want %v", got, want)
	}
}

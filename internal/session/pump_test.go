package session

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
)

// newRecoveryPump builds a bare StdioPump suitable for exercising pump helpers
// that never touch the websocket client or the agent process.
func newRecoveryPump() *StdioPump {
	return &StdioPump{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// probeOf parses one ACP frame the way the pump's pipeline does.
func probeOf(t *testing.T, line string) frameProbe {
	t.Helper()
	probe, ok := parseFrameProbe([]byte(line))
	if !ok {
		t.Fatalf("parseFrameProbe(%s) failed", line)
	}
	return probe
}

// feedInitialize runs the pump's initialize stage for one agent frame.
func feedInitialize(p *StdioPump, line string) {
	raw := []byte(line)
	probe, _ := parseFrameProbe(raw)
	p.acp.observeInitialize(probe, raw)
}

// closeCapableInit is an initialize response advertising sessionCapabilities.close.
const closeCapableInit = `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":1,"agentCapabilities":{"sessionCapabilities":{"close":{}}}}}`

// markCloseCapable gives the pump an agent that accepts session/close.
func markCloseCapable(p *StdioPump) { feedInitialize(p, closeCapableInit) }

// feedSessionID runs the pump's session/new-response stage for one agent frame.
func feedSessionID(t *testing.T, p *StdioPump, line string) {
	t.Helper()
	p.acp.observeSessionID(probeOf(t, line))
}

// turnReplacement runs the pump's turn stage for one agent frame, exactly as
// handleStdoutLine does, and reports the replacement frame (if any).
func turnReplacement(t *testing.T, p *StdioPump, line string) (string, bool) {
	t.Helper()
	probe := probeOf(t, line)
	origin, isReply := p.reqIDs.peek(probe)
	return p.observeTurn(probe, origin, isReply)
}

func TestIsProgressEventReadsToolCall(t *testing.T) {
	line := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"c1","kind":"edit","status":"in_progress","title":"Editing auth.go"}}}`
	ev := progressEventFromProbe(probeOf(t, line))
	want := progressEvent{toolCallID: "c1", kind: "edit", status: "in_progress"}
	if ev == nil || *ev != want {
		t.Fatalf("progressEventFromProbe = %+v, want %+v", ev, want)
	}
}

func TestIsProgressEventIgnoresNonTool(t *testing.T) {
	cases := []string{
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","text":"hi"}}}`,
		`{"jsonrpc":"2.0","method":"other","params":{}}`,
		`not json`,
	}
	for _, line := range cases {
		probe, ok := parseFrameProbe([]byte(line))
		if !ok {
			continue // no parse, no progress event
		}
		if ev := progressEventFromProbe(probe); ev != nil {
			t.Errorf("progressEventFromProbe(%q) = %+v, want nil", line, ev)
		}
	}
}

func TestVerbForKind(t *testing.T) {
	if got := verbForKind("execute"); got != "Running a command" {
		t.Errorf("execute verb = %q", got)
	}
	if got := verbForKind("other"); got != "Using a tool" {
		t.Errorf("other verb = %q", got)
	}
	if got := verbForKind(""); got != "Using a tool" {
		t.Errorf("empty verb = %q", got)
	}
}

func TestMaybeNotifyProgressPushesRunningVerbs(t *testing.T) {
	var fired []string
	p := &StdioPump{
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		onPushNotification: func(e PushEvent) { fired = append(fired, e.Body) },
	}

	// Pending: nothing runs yet. The update that starts it omits the kind.
	p.maybeNotifyProgress(&progressEvent{toolCallID: "c1", kind: "read", status: "pending"})
	p.maybeNotifyProgress(&progressEvent{toolCallID: "c1", status: "in_progress"})
	// Finished calls never push.
	p.maybeNotifyProgress(&progressEvent{toolCallID: "c1", status: "completed"})
	// Another read says the same thing: deduped.
	p.maybeNotifyProgress(&progressEvent{toolCallID: "c2", kind: "read", status: "in_progress"})
	p.maybeNotifyProgress(&progressEvent{toolCallID: "c3", kind: "execute", status: "in_progress"})

	if want := []string{"Reading a file", "Running a command"}; fmt.Sprint(fired) != fmt.Sprint(want) {
		t.Fatalf("pushes = %v, want %v", fired, want)
	}
	if _, ok := p.acp.toolKinds["c1"]; ok {
		t.Error("finished call's kind was kept")
	}
}

func TestMaybeNotifyProgressSendsLatestWhenWindowEnds(t *testing.T) {
	fired := make(chan string, 10)
	p := &StdioPump{
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		ProgressInterval:   50 * time.Millisecond,
		onPushNotification: func(e PushEvent) { fired <- e.Body },
	}
	p.maybeNotifyProgress(&progressEvent{acpSessionID: "s", toolCallID: "c1", kind: "edit", status: "in_progress"})
	p.maybeNotifyProgress(&progressEvent{acpSessionID: "s", toolCallID: "c2", kind: "execute", status: "in_progress"})
	p.maybeNotifyProgress(&progressEvent{acpSessionID: "s", toolCallID: "c3", kind: "read", status: "in_progress"})
	if got := <-fired; got != "Editing a file" {
		t.Fatalf("first push = %q", got)
	}
	select {
	case got := <-fired:
		if got != "Reading a file" {
			t.Fatalf("window-end push = %q, want the latest", got)
		}
	case <-time.After(time.Second):
		t.Fatal("throttled summary never sent")
	}

	// A turn that ends inside the window drops its throttled summary.
	p.maybeNotifyProgress(&progressEvent{acpSessionID: "s", toolCallID: "c4", kind: "search", status: "in_progress"})
	p.endProgress("s")
	select {
	case got := <-fired:
		t.Fatalf("push after turn end = %q", got)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestWorkingTitleUsesAgentInfo(t *testing.T) {
	p := newRecoveryPump()
	if got := p.workingTitle(); got != "Agent working" {
		t.Errorf("before initialize = %q", got)
	}
	feedInitialize(p, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":1,"agentInfo":{"name":"claude-code-acp","title":"Claude Code","version":"1"}}}`)
	if got := p.workingTitle(); got != "Claude Code working" {
		t.Errorf("after initialize = %q", got)
	}
}

// session/new only learns its ACP session id from the agent's response, so its
// cwd binds when that response comes back with the same request id.
func TestSessionNewCwdBindsOnResponse(t *testing.T) {
	p := newRecoveryPump()
	p.acp.observeInbound(parseClientFrame([]byte(`{"jsonrpc":"2.0","id":7,"method":"session/new","params":{"cwd":"/a"}}`)))
	p.acp.observeInbound(parseClientFrame([]byte(`{"jsonrpc":"2.0","id":8,"method":"session/new","params":{"cwd":"/b"}}`)))

	respond := func(line string) {
		t.Helper()
		probe, ok := parseFrameProbe([]byte(line))
		if !ok {
			t.Fatalf("parseFrameProbe(%s) failed", line)
		}
		p.acp.bindPendingCwd(probe)
	}
	// Responses arrive out of order; each binds to its own request's cwd.
	respond(`{"jsonrpc":"2.0","id":8,"result":{"sessionId":"sb"}}`)
	respond(`{"jsonrpc":"2.0","id":7,"result":{"sessionId":"sa"}}`)
	if got := p.AcpCwdFor("sa"); got != "/a" {
		t.Fatalf("AcpCwdFor(sa) = %q, want /a", got)
	}
	if got := p.AcpCwdFor("sb"); got != "/b" {
		t.Fatalf("AcpCwdFor(sb) = %q, want /b", got)
	}

	// A failed session/new binds nothing and drops its pending entry.
	p.acp.observeInbound(parseClientFrame([]byte(`{"jsonrpc":"2.0","id":9,"method":"session/new","params":{"cwd":"/c"}}`)))
	respond(`{"jsonrpc":"2.0","id":9,"error":{"code":-32000,"message":"nope"}}`)
	if len(p.acp.pendingCwd) != 0 {
		t.Fatalf("pendingCwd = %v, want empty after error response", p.acp.pendingCwd)
	}
}

// Through the real write path the client's request id is rewritten before the
// cwd is recorded, so the agent's reply (which carries the agent-side id) still
// binds the cwd to the new ACP session.
func TestSessionNewCwdBindsThroughIDRewrite(t *testing.T) {
	p := newRecoveryPump()
	p.proc = &runtime.Process{Stdin: nopWriteCloser{}}
	if err := p.WriteToAgent([]byte(`{"jsonrpc":"2.0","id":"client-7","method":"session/new","params":{"cwd":"/proj"}}`)); err != nil {
		t.Fatalf("WriteToAgent: %v", err)
	}
	p.handleStdoutLine(`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"ses_x"}}`)
	if got := p.AcpCwdFor("ses_x"); got != "/proj" {
		t.Fatalf("AcpCwdFor(ses_x) = %q, want /proj", got)
	}
}

// Reopened chats name their session in the request, so load and resume bind
// their cwd immediately; pushes for them carry it for the client's deep-link.
func TestSessionLoadAndResumeCwdBindImmediately(t *testing.T) {
	p := newRecoveryPump()
	p.acp.observeInbound(parseClientFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/load","params":{"sessionId":"sl","cwd":"/l"}}`)))
	p.acp.observeInbound(parseClientFrame([]byte(`{"jsonrpc":"2.0","id":2,"method":"session/resume","params":{"sessionId":"sr","cwd":"/r"}}`)))
	if got := p.AcpCwdFor("sl"); got != "/l" {
		t.Fatalf("AcpCwdFor(sl) = %q, want /l", got)
	}
	if got := p.AcpCwdFor("sr"); got != "/r" {
		t.Fatalf("AcpCwdFor(sr) = %q, want /r", got)
	}
}

func TestObserveInboundCwd(t *testing.T) {
	p := newRecoveryPump()

	// A non-session/new frame must not set cwd.
	p.acp.observeInbound(parseClientFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s1","cwd":"/should/not/apply"}}`)))
	if got := p.AcpCwd(); got != "" {
		t.Fatalf("AcpCwd() after session/prompt = %q, want empty", got)
	}

	// A session/new with cwd captures it.
	p.acp.observeInbound(parseClientFrame([]byte(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/home/user/project"}}`)))
	if got := p.AcpCwd(); got != "/home/user/project" {
		t.Fatalf("AcpCwd() = %q, want %q", got, "/home/user/project")
	}

	// A session/load with cwd also captures it (Ferngeist sends session/load on resume).
	p.acp.observeInbound(parseClientFrame([]byte(`{"jsonrpc":"2.0","id":5,"method":"session/load","params":{"sessionId":"s1","cwd":"/home/user/loaded"}}`)))
	if got := p.AcpCwd(); got != "/home/user/loaded" {
		t.Fatalf("AcpCwd() after session/load = %q, want %q", got, "/home/user/loaded")
	}

	// A project switch updates it.
	p.acp.observeInbound(parseClientFrame([]byte(`{"jsonrpc":"2.0","id":3,"method":"session/new","params":{"cwd":"/home/user/other"}}`)))
	if got := p.AcpCwd(); got != "/home/user/other" {
		t.Fatalf("AcpCwd() after switch = %q, want %q", got, "/home/user/other")
	}

	// Empty cwd is ignored.
	p.acp.observeInbound(parseClientFrame([]byte(`{"jsonrpc":"2.0","id":4,"method":"session/new","params":{"cwd":""}}`)))
	if got := p.AcpCwd(); got != "/home/user/other" {
		t.Fatalf("AcpCwd() after empty = %q, want unchanged", got)
	}
}

// repeatReader yields n copies of b, then a newline if nl, then EOF.
// It allocates nothing for the payload, so allocation tests measure the
// frame reader rather than the input.
type repeatReader struct {
	n  int
	b  byte
	nl bool
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.n == 0 {
		if r.nl {
			r.nl = false
			if len(p) == 0 {
				return 0, nil
			}
			p[0] = '\n'
			return 1, nil
		}
		return 0, io.EOF
	}
	k := len(p)
	if k > r.n {
		k = r.n
	}
	for i := 0; i < k; i++ {
		p[i] = r.b
	}
	r.n -= k
	return k, nil
}

func TestReadStdoutFrameDropsUnterminatedStreamPastCap(t *testing.T) {
	// Breaks if the EOF path still returns the buffered tail (the cap check
	// used to run only after a successful newline read).
	r := bufio.NewReader(strings.NewReader(strings.Repeat("x", 100)))
	frame, dropped, err := readStdoutFrame(r, 16)
	if err != io.EOF {
		t.Fatalf("err = %v, want EOF", err)
	}
	if !dropped || frame != "" {
		t.Fatalf("frame = %q, dropped = %v, want dropped empty frame", frame, dropped)
	}
}

func TestReadStdoutFrameKeepsPartialLineUnderCapAtEOF(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("hello"))
	frame, dropped, err := readStdoutFrame(r, 16)
	if err != io.EOF {
		t.Fatalf("err = %v, want EOF", err)
	}
	if dropped || frame != "hello" {
		t.Fatalf("frame = %q, dropped = %v, want hello", frame, dropped)
	}
}

func TestReadStdoutFrameKeepsPayloadExactlyAtCapAcrossReads(t *testing.T) {
	// bufio will not allocate a reader buffer under 16 bytes, so the payload
	// has to be longer than that to arrive as more than one ReadSlice. The
	// delimiter must not count against the cap.
	const payload = "0123456789abcdefWXYZ" // 20 bytes
	r := bufio.NewReaderSize(strings.NewReader(payload+"\n"), 16)
	frame, dropped, err := readStdoutFrame(r, len(payload))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if dropped || frame != payload {
		t.Fatalf("frame = %q, dropped = %v, want %q", frame, dropped, payload)
	}
}

func TestReadStdoutFrameDropsPayloadOnePastCapAcrossReads(t *testing.T) {
	const payload = "0123456789abcdefWXYZ!" // 21 bytes
	r := bufio.NewReaderSize(strings.NewReader(payload+"\n"), 16)
	frame, dropped, err := readStdoutFrame(r, 20)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !dropped || frame != "" {
		t.Fatalf("frame = %q, dropped = %v, want dropped", frame, dropped)
	}
}

func TestReadStdoutFrameReturnsFollowingFrameAfterDroppedFrame(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("0123456789\nok\n"))
	frame, dropped, err := readStdoutFrame(r, 4)
	if err != nil || !dropped || frame != "" {
		t.Fatalf("first frame = %q, dropped = %v, err = %v, want dropped", frame, dropped, err)
	}
	frame, dropped, err = readStdoutFrame(r, 4)
	if err != nil || dropped || frame != "ok" {
		t.Fatalf("next frame = %q, dropped = %v, err = %v, want ok", frame, dropped, err)
	}
}

func TestReadStdoutFrameReturnsEmptyFrameForBlankLine(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("\n"))
	frame, dropped, err := readStdoutFrame(r, 8)
	if err != nil || dropped || frame != "" {
		t.Fatalf("frame = %q, dropped = %v, err = %v, want empty frame", frame, dropped, err)
	}
}

func TestReadStdoutFrameDoesNotCopyTheDiscardedTail(t *testing.T) {
	// Breaks if the reader copies the tail the way ReadString does
	// (one allocation per internal buffer, plus a contiguous copy).
	const max = 32
	const tail = 64 << 10
	var bad string
	allocs := testing.AllocsPerRun(20, func() {
		br := bufio.NewReaderSize(&repeatReader{n: max + tail, b: 'x', nl: true}, 256)
		frame, dropped, err := readStdoutFrame(br, max)
		if err != nil || !dropped || frame != "" {
			if bad == "" {
				bad = fmt.Sprintf("frame=%q dropped=%v err=%v", frame, dropped, err)
			}
		}
	})
	if bad != "" {
		t.Fatal(bad)
	}
	if allocs > 16 {
		t.Fatalf("allocs per run = %g, want <= 16; discarded tail is being copied", allocs)
	}
}

func TestLiveTurnsTrackRepliesAcrossDetach(t *testing.T) {
	for _, outcome := range []string{`"result":{"stopReason":"end_turn"}`, `"result":{"stopReason":"cancelled"}`, `"error":{"code":-1,"message":"failed"}`} {
		t.Run(outcome, func(t *testing.T) {
			p := newRecoveryPump()
			gen := p.Attach()
			p.prepareOutbound(parseClientFrame([]byte(`{"id":0,"method":"session/load","params":{"sessionId":"idle"}}`)))
			if got := liveSessions(p); len(got) != 0 {
				t.Fatalf("before prompt = %v", got)
			}
			first := p.prepareOutbound(parseClientFrame([]byte(`{"id":1,"method":"session/prompt","params":{"sessionId":"active"}}`)))
			second := p.prepareOutbound(parseClientFrame([]byte(`{"id":2,"method":"session/prompt","params":{"sessionId":"active"}}`)))
			p.prepareOutbound(parseClientFrame([]byte(`{"id":3,"method":"session/prompt","params":{"sessionId":"other"}}`)))
			if !p.Detach(gen) {
				t.Fatal("detach rejected")
			}
			if got := liveSessions(p); !slices.Equal(got, []string{"active", "other"}) {
				t.Fatalf("after detach = %v", got)
			}
			p.handleStdoutLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,%s}`, agentIDOf(t, first), outcome))
			if got := liveSessions(p); len(got) != 2 {
				t.Fatalf("one prompt remains for active: %v", got)
			}
			p.handleStdoutLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,%s}`, agentIDOf(t, second), outcome))
			if got := liveSessions(p); !slices.Equal(got, []string{"other"}) {
				t.Fatalf("after reply = %v", got)
			}
		})
	}
}

// liveSessions returns the sorted ACP sessions p reports as running.
func liveSessions(p *StdioPump) []string {
	var ids []string
	for _, st := range p.LiveTurns() {
		ids = append(ids, st.SessionID)
	}
	slices.Sort(ids)
	return ids
}

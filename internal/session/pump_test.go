package session

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// newRecoveryPump builds a bare StdioPump suitable for exercising pump helpers
// that never touch the websocket client or leased pipes.
func newRecoveryPump() *StdioPump {
	return &StdioPump{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestIsProgressEventToolCallCreate(t *testing.T) {
	line := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"c1","kind":"edit","status":"in_progress","title":"Editing auth.go"}}}`
	ev := isProgressEvent(line)
	if ev == nil {
		t.Fatal("expected progress event, got nil")
	}
	if ev.summary != "Editing auth.go" {
		t.Errorf("summary = %q, want %q", ev.summary, "Editing auth.go")
	}
	if ev.terminal {
		t.Error("in_progress should not be terminal")
	}
	if ev.toolCallID != "c1" {
		t.Errorf("toolCallID = %q, want c1", ev.toolCallID)
	}
}

func TestIsProgressEventFallsBackToContentText(t *testing.T) {
	line := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call_update","toolCallId":"c1","status":"in_progress","content":[{"type":"text","text":"Running go test..."}]}}}`
	ev := isProgressEvent(line)
	if ev == nil {
		t.Fatal("expected progress event, got nil")
	}
	if ev.summary != "Running go test..." {
		t.Errorf("summary = %q, want %q", ev.summary, "Running go test...")
	}
}

func TestIsProgressEventFallsBackToKindVerb(t *testing.T) {
	line := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"c1","kind":"execute","status":"in_progress"}}}`
	ev := isProgressEvent(line)
	if ev == nil {
		t.Fatal("expected progress event, got nil")
	}
	if ev.summary != "Running a command" {
		t.Errorf("summary = %q, want %q", ev.summary, "Running a command")
	}
}

func TestIsProgressEventTerminal(t *testing.T) {
	line := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call_update","toolCallId":"c1","status":"completed","title":"Edited auth.go"}}}`
	ev := isProgressEvent(line)
	if ev == nil {
		t.Fatal("expected progress event, got nil")
	}
	if !ev.terminal {
		t.Error("completed should be terminal")
	}
}

func TestIsProgressEventIgnoresNonToolAndNonProgress(t *testing.T) {
	cases := []string{
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","text":"hi"}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"c1","status":"pending","title":"Waiting"}}}`,
		`{"jsonrpc":"2.0","method":"other","params":{}}`,
		`not json`,
	}
	for _, line := range cases {
		if ev := isProgressEvent(line); ev != nil {
			t.Errorf("isProgressEvent(%q) = %+v, want nil", line, ev)
		}
	}
}

func TestVerbForKind(t *testing.T) {
	if got := verbForKind("execute"); got != "Running a command" {
		t.Errorf("execute verb = %q", got)
	}
	if got := verbForKind("other"); got != "" {
		t.Errorf("other verb = %q, want empty", got)
	}
	if got := verbForKind(""); got != "" {
		t.Errorf("empty verb = %q, want empty", got)
	}
}

func TestMaybeNotifyProgressThrottlesAndDedupes(t *testing.T) {
	var fired []string
	p := &StdioPump{
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		ProgressInterval:   time.Hour, // effectively disable throttle between first and second
		onPushNotification: func(e PushEvent) { fired = append(fired, e.Body) },
	}

	// First in_progress: fires.
	p.maybeNotifyProgress(&progressEvent{summary: "Editing auth.go", toolCallID: "c1"})
	if len(fired) != 1 || fired[0] != "Editing auth.go" {
		t.Fatalf("first push = %v, want [Editing auth.go]", fired)
	}

	// Same tool + same summary: deduped, no throttled push.
	p.maybeNotifyProgress(&progressEvent{summary: "Editing auth.go", toolCallID: "c1"})
	if len(fired) != 1 {
		t.Fatalf("dedupe failed, fired = %v", fired)
	}

	// New tool + new summary within interval: throttled.
	p.maybeNotifyProgress(&progressEvent{summary: "Editing routes.go", toolCallID: "c2"})
	if len(fired) != 1 {
		t.Fatalf("throttle failed, fired = %v", fired)
	}

	// Terminal event: fires immediately despite interval.
	p.maybeNotifyProgress(&progressEvent{summary: "Edited auth.go", toolCallID: "c1", terminal: true})
	if len(fired) != 2 || fired[1] != "Edited auth.go" {
		t.Fatalf("terminal push = %v, want appends", fired)
	}
}

func TestSnoopInboundCwd(t *testing.T) {
	p := newRecoveryPump()

	// A non-session/new frame must not set cwd.
	p.snoopInboundCwd([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s1","cwd":"/should/not/apply"}}`))
	if got := p.AcpCwd(); got != "" {
		t.Fatalf("AcpCwd() after session/prompt = %q, want empty", got)
	}

	// A session/new with cwd captures it.
	p.snoopInboundCwd([]byte(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/home/user/project"}}`))
	if got := p.AcpCwd(); got != "/home/user/project" {
		t.Fatalf("AcpCwd() = %q, want %q", got, "/home/user/project")
	}

	// A session/load with cwd also captures it (Ferngeist sends session/load on resume).
	p.snoopInboundCwd([]byte(`{"jsonrpc":"2.0","id":5,"method":"session/load","params":{"sessionId":"s1","cwd":"/home/user/loaded"}}`))
	if got := p.AcpCwd(); got != "/home/user/loaded" {
		t.Fatalf("AcpCwd() after session/load = %q, want %q", got, "/home/user/loaded")
	}

	// A project switch updates it.
	p.snoopInboundCwd([]byte(`{"jsonrpc":"2.0","id":3,"method":"session/new","params":{"cwd":"/home/user/other"}}`))
	if got := p.AcpCwd(); got != "/home/user/other" {
		t.Fatalf("AcpCwd() after switch = %q, want %q", got, "/home/user/other")
	}

	// Empty cwd is ignored.
	p.snoopInboundCwd([]byte(`{"jsonrpc":"2.0","id":4,"method":"session/new","params":{"cwd":""}}`))
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

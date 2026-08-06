package session

import (
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// newRecoveryPump builds a bare StdioPump suitable for exercising the re-load
// recovery helpers, which never touch the websocket client or leased pipes.
func newRecoveryPump() *StdioPump {
	return &StdioPump{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func loadRequest(id, sessionID string) []byte {
	return []byte(`{"jsonrpc":"2.0","id":` + id + `,"method":"session/load","params":{"sessionId":"` + sessionID + `"}}`)
}

func updateFrame(sessionID, text string) string {
	return `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"` + sessionID +
		`","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"` + text + `"}}}}`
}

// TestRecoverAlreadyLoaded covers the core fix: after a first successful load
// (whose history is buffered), a second load that the agent rejects as "already
// loaded" is transparently recovered by replaying the buffered history followed
// by a synthesized success carrying the second request's id.
func TestRecoverAlreadyLoaded(t *testing.T) {
	p := newRecoveryPump()
	const sid = "ses_abc"

	// First load: record the request, stream history, observe the success.
	p.noteOutboundLoad(loadRequest("1", sid))
	h1 := updateFrame(sid, "What is this?")
	h2 := updateFrame(sid, "It is Ferngeist.")
	p.bufferLoadHistory(h1)
	p.bufferLoadHistory(h2)

	firstSuccess := `{"jsonrpc":"2.0","id":1,"result":{"modes":{"currentModeId":"chat","availableModes":[]}}}`
	if frames, handled := p.maybeRecoverLoad(firstSuccess); handled {
		t.Fatalf("first successful load must pass through unchanged, got replacement frames: %v", frames)
	}

	// Second load: agent rejects the duplicate; the pump must recover.
	p.noteOutboundLoad(loadRequest("2", sid))
	alreadyLoaded := `{"jsonrpc":"2.0","id":2,"error":{"code":-32602,"message":"Session ses_abc is already loaded"}}`
	frames, handled := p.maybeRecoverLoad(alreadyLoaded)
	if !handled {
		t.Fatal("expected the already-loaded error to be recovered")
	}
	if len(frames) != 3 {
		t.Fatalf("expected 2 history frames + 1 success, got %d frames: %v", len(frames), frames)
	}
	if frames[0] != h1 || frames[1] != h2 {
		t.Fatalf("buffered history not replayed in order:\n got %q, %q", frames[0], frames[1])
	}

	// The synthesized success must reuse the cached result shape and carry id 2.
	var got struct {
		ID     json.Number     `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(frames[2]), &got); err != nil {
		t.Fatalf("synthesized success is not valid JSON: %v", err)
	}
	if got.ID.String() != "2" {
		t.Fatalf("synthesized success must echo the second request id, got id=%s", got.ID)
	}
	if got.Error != nil {
		t.Fatalf("synthesized response must not carry an error: %s", frames[2])
	}
	if !strings.Contains(string(got.Result), "currentModeId") {
		t.Fatalf("synthesized success should reuse the cached load result, got result=%s", got.Result)
	}
}

// TestRecoverFallsBackToNullResult verifies that when no first-load response was
// cached (e.g. the agent was already loaded before this pump observed a success),
// recovery still produces a valid null-result success rather than surfacing the error.
func TestRecoverFallsBackToNullResult(t *testing.T) {
	p := newRecoveryPump()
	const sid = "ses_xyz"

	p.noteOutboundLoad(loadRequest("7", sid))
	frames, handled := p.maybeRecoverLoad(
		`{"jsonrpc":"2.0","id":7,"error":{"message":"session is already loaded"}}`)
	if !handled {
		t.Fatal("expected recovery even without buffered history or cached response")
	}
	if len(frames) != 1 {
		t.Fatalf("expected a single synthesized success frame, got %d: %v", len(frames), frames)
	}
	var got struct {
		ID     json.Number     `json:"id"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(frames[0]), &got); err != nil {
		t.Fatalf("fallback success is not valid JSON: %v", err)
	}
	if got.ID.String() != "7" || string(got.Result) != "null" {
		t.Fatalf("expected {id:7, result:null}, got %s", frames[0])
	}
}

// TestUnrelatedLoadErrorPassesThrough ensures a load failure that is not an
// already-loaded rejection is surfaced to the client unchanged.
func TestUnrelatedLoadErrorPassesThrough(t *testing.T) {
	p := newRecoveryPump()
	p.noteOutboundLoad(loadRequest("3", "ses_def"))
	if _, handled := p.maybeRecoverLoad(
		`{"jsonrpc":"2.0","id":3,"error":{"message":"unknown session"}}`); handled {
		t.Fatal("a non already-loaded error must not be recovered")
	}
}

// TestRecoverIgnoresFramesWithoutPendingLoad guards the hot path: ordinary
// streaming frames with no in-flight load are never rewritten.
func TestRecoverIgnoresFramesWithoutPendingLoad(t *testing.T) {
	p := newRecoveryPump()
	if _, handled := p.maybeRecoverLoad(updateFrame("ses_ghi", "hello")); handled {
		t.Fatal("frames must pass through when no session/load is in flight")
	}
}

// TestBufferLoadHistoryEvictsOldest verifies the byte cap drops the oldest
// frames while always retaining at least the most recent one.
func TestBufferLoadHistoryEvictsOldest(t *testing.T) {
	p := newRecoveryPump()
	const sid = "ses_big"
	big := strings.Repeat("x", maxLoadHistoryBytes)
	p.bufferLoadHistory(updateFrame(sid, "first"))
	p.bufferLoadHistory(updateFrame(sid, big))

	p.loadMu.Lock()
	defer p.loadMu.Unlock()
	if got := len(p.loadHistory[sid]); got != 1 {
		t.Fatalf("expected oldest frame evicted leaving 1, got %d", got)
	}
	if !strings.Contains(p.loadHistory[sid][0], "xxxx") {
		t.Fatal("expected the most recent (large) frame to be retained")
	}
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

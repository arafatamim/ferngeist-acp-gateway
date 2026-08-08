package session

import (
	"io"
	"log/slog"
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

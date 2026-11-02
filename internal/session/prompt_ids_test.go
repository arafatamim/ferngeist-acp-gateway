package session

import (
	"io"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
)

func promptWithClientID(id int, clientID string) []byte {
	meta := ""
	if clientID != "" {
		meta = `,"_meta":{"ferngeist/clientId":"` + clientID + `"}`
	}
	return []byte(`{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"session/prompt","params":{"sessionId":"ses_a",` +
		`"prompt":[{"type":"text","text":"go"}]` + meta + `}}`)
}

// recordingPump is a held pump whose agent stdin records each write.
func recordingPump(writes *[]string) *StdioPump {
	pump := heldPump()
	pump.proc = &runtime.Process{Stdin: recordingWriteCloser{record: func(b []byte) { *writes = append(*writes, string(b)) }}}
	return pump
}

// A prompt that reached the agent is never run twice: its repeat, from the
// connection that resent it, is answered as already delivered instead.
func TestRepeatedPromptIsAnsweredNotForwarded(t *testing.T) {
	var writes []string
	pump := recordingPump(&writes)
	gen, c := connect(t, pump)
	if err := pump.WriteToAgent(gen, promptWithClientID(1, "c1")); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := pump.WriteToAgent(gen, promptWithClientID(2, "c1")); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if len(writes) != 1 {
		t.Fatalf("agent got %d prompts, want 1", len(writes))
	}
	want := `{"error":{"code":-32050,"data":{"clientId":"c1"},"message":"prompt already delivered"},"id":2,"jsonrpc":"2.0"}`
	if got := readFrames(t, pump, c, 1); got[0] != want {
		t.Fatalf("repeat reply = %s", got[0])
	}
}

func TestPromptsWithoutClientIDAreAlwaysForwarded(t *testing.T) {
	var writes []string
	pump := recordingPump(&writes)
	for i := range 2 {
		if err := pump.WriteToAgent(0, promptWithClientID(i, "")); err != nil {
			t.Fatalf("prompt %d: %v", i, err)
		}
	}
	if len(writes) != 2 {
		t.Fatalf("agent got %d prompts, want 2", len(writes))
	}
}

// A reattaching client learns from catch-up which of its prompts arrived.
func TestBufferedPromptCarriesClientID(t *testing.T) {
	pump := heldPump()
	pump.prepareOutbound(0, parseClientFrame(promptWithClientID(1, "c1")))
	frames, _, _ := pump.loadRecovery.catchUp("ses_a", pump.loadRecovery.epoch, 0)
	want := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_a","update":{"_meta":{"ferngeist/clientId":"c1","ferngeist/seq":1},"content":{"type":"text","text":"go"},"sessionUpdate":"user_message_chunk"}}}`
	if len(frames) != 1 || frames[0] != want {
		t.Fatalf("buffered prompt = %q", frames)
	}
}

func TestDeliveredPromptsRememberTheLast32PerSession(t *testing.T) {
	var d deliveredPrompts
	for i := range maxDeliveredPrompts + 1 {
		d.claim("ses_a", strconv.Itoa(i))
	}
	if d.claim("ses_a", strconv.Itoa(maxDeliveredPrompts)) {
		t.Fatal("a recent clientId was forgotten")
	}
	if !d.claim("ses_a", "0") {
		t.Fatal("the oldest clientId should have left the window")
	}
	if !d.claim("ses_b", "1") {
		t.Fatal("clientIds are per session")
	}
}

// A prompt whose write failed never reached the agent, so its resend must.
func TestFailedPromptWriteDoesNotBlockItsResend(t *testing.T) {
	pump := heldPump()
	r, w := io.Pipe()
	_ = r.Close() // every write now fails
	pump.proc = &runtime.Process{Stdin: w}
	if err := pump.WriteToAgent(0, promptWithClientID(1, "c1")); err == nil {
		t.Fatal("write to a closed stdin succeeded")
	}
	if !pump.prompts.claim("ses_a", "c1") {
		t.Fatal("failed prompt still counts as delivered")
	}
}

func TestPromptRetryFollowsAgentOutcome(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reply  string
		writes int
	}{
		{"rejected", `"error":{"code":-32602,"message":"invalid model"}`, 2},
		{"completed", `"result":{"stopReason":"end_turn","usage":{"totalTokens":15}}`, 1},
		{"swallowed failure", `"result":{"stopReason":"end_turn"}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var writes []string
			pump := recordingPump(&writes)
			if err := pump.WriteToAgent(0, promptWithClientID(1, "c1")); err != nil {
				t.Fatal(err)
			}
			id := agentIDOf(t, []byte(writes[0]))
			pump.handleStdoutLine(`{"jsonrpc":"2.0","id":` + id + `,` + tc.reply + `}`)
			if err := pump.WriteToAgent(0, promptWithClientID(2, "c1")); err != nil {
				t.Fatal(err)
			}
			if len(writes) != tc.writes {
				t.Fatalf("agent got %d prompts, want %d", len(writes), tc.writes)
			}
		})
	}
}

func TestDuplicatePromptWaitsForFailedWrite(t *testing.T) {
	pump := heldPump()
	r, w := io.Pipe()
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	entered := make(chan struct{})
	pump.proc = &runtime.Process{Stdin: blockingPromptWriter{w, entered}}
	first := make(chan error, 1)
	go func() { first <- pump.WriteToAgent(0, promptWithClientID(1, "c1")) }()
	<-entered
	second := make(chan error, 1)
	go func() { second <- pump.WriteToAgent(1, promptWithClientID(2, "c1")) }()
	select {
	case err := <-second:
		t.Fatalf("duplicate completed before delivery: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	_ = r.Close()
	if err := <-first; err == nil {
		t.Fatal("first write succeeded")
	}
	if err := <-second; err == nil {
		t.Fatal("duplicate falsely acknowledged delivery")
	}
}

type blockingPromptWriter struct {
	*io.PipeWriter
	entered chan struct{}
}

func (w blockingPromptWriter) Write(b []byte) (int, error) {
	select {
	case <-w.entered:
	default:
		close(w.entered)
	}
	return w.PipeWriter.Write(b)
}

func TestUndeliveredPromptIsAbsentFromReplay(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(strconv.FormatBool(rejected), func(t *testing.T) {
			var writes []string
			pump := recordingPump(&writes)
			if !rejected {
				r, w := io.Pipe()
				_ = r.Close()
				t.Cleanup(func() { _ = w.Close() })
				pump.proc.Stdin = w
			}
			err := pump.WriteToAgent(0, promptWithClientID(1, "c1"))
			if rejected {
				if err != nil {
					t.Fatal(err)
				}
				id := agentIDOf(t, []byte(writes[0]))
				pump.handleStdoutLine(`{"jsonrpc":"2.0","id":` + id + `,"error":{"code":-32602,"message":"invalid model"}}`)
			} else if err == nil {
				t.Fatal("write unexpectedly succeeded")
			}
			frames, _, _ := pump.loadRecovery.catchUp("ses_a", pump.loadRecovery.epoch, 0)
			if len(frames) != 0 {
				t.Fatalf("undelivered prompt replayed: %q", frames)
			}
		})
	}
}

func TestPromptErrorAfterOutputPreservesUserBoundary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		update string
		gap    bool
	}{
		{"message", updateFrame("ses_a", "partial"), false},
		{"tool", `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_a","update":{"sessionUpdate":"tool_call","toolCallId":"t1","status":"in_progress"}}}`, false},
		{"unnumbered", `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_a","update":{"sessionUpdate":"plan","_meta":[]}}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var writes []string
			pump := recordingPump(&writes)
			if err := pump.WriteToAgent(0, promptWithClientID(1, "c1")); err != nil {
				t.Fatal(err)
			}
			pump.handleStdoutLine(tc.update)
			id := agentIDOf(t, []byte(writes[0]))
			pump.handleStdoutLine(`{"jsonrpc":"2.0","id":` + id + `,"error":{"code":-32603,"message":"turn failed"}}`)

			r := pump.loadRecovery
			r.OnOutbound(loadRequest("99", "ses_a"))
			replay, handled := onFrame(t, r, `{"jsonrpc":"2.0","id":99,"error":{"message":"already loaded"}}`)
			const user = `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_a","update":{"_meta":{"ferngeist/clientId":"c1","ferngeist/seq":1},"content":{"type":"text","text":"go"},"sessionUpdate":"user_message_chunk"}}}`
			if !handled || len(replay) != 4 || replay[1] != user {
				t.Fatalf("replay lost the partial turn's user boundary: %q", replay)
			}
			frames, _, gap := r.catchUp("ses_a", r.epoch, 0)
			if gap != tc.gap || (!gap && !slices.Equal(frames, replay[1:3])) {
				t.Fatalf("catch-up = %q, gap %v; want preserved turn, gap %v", frames, gap, tc.gap)
			}
			if err := pump.WriteToAgent(0, promptWithClientID(2, "c1")); err != nil {
				t.Fatal(err)
			}
			if len(writes) != 2 {
				t.Fatal("error after partial output prevented clientId retry")
			}
		})
	}
}

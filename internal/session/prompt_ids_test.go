package session

import (
	"io"
	"strconv"
	"testing"

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

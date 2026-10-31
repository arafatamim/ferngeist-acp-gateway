package session

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// catchUpResume is a catch-up session/resume for ses_a, as client id 5.
func catchUpResume(epoch string, afterSeq int) []byte {
	return []byte(`{"jsonrpc":"2.0","id":5,"method":"session/resume","params":{"sessionId":"ses_a","cwd":"/proj",` +
		`"_meta":{"ferngeist/catchUp":{"epoch":"` + epoch + `","afterSeq":` + strconv.Itoa(afterSeq) + `}}}}`)
}

// catchUpOf decodes the gateway's reply to a catch-up resume.
func catchUpOf(t *testing.T, frame string) (id string, res catchUpResult) {
	t.Helper()
	var reply struct {
		ID     json.RawMessage `json:"id"`
		Result struct {
			Meta map[string]catchUpResult `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(frame), &reply); err != nil {
		t.Fatalf("catch-up reply %s: %v", frame, err)
	}
	res, ok := reply.Result.Meta[catchUpMetaKey]
	if !ok {
		t.Fatalf("reply carries no %s: %s", catchUpMetaKey, frame)
	}
	return string(reply.ID), res
}

// heldPump is a pump whose agent holds ses_a, as after the client loaded it.
func heldPump() *StdioPump {
	pump := newPump()
	pump.loadRecovery = newTestLoadRecovery()
	pump.acp.observeInbound(parseClientFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/load","params":{"sessionId":"ses_a","cwd":"/proj"}}`)))
	return pump
}

func TestUpdatesAreNumberedPerSessionKeepingAgentMeta(t *testing.T) {
	pump := heldPump()
	_, c := connect(t, pump)
	pump.handleStdoutLine(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_a","update":{"sessionUpdate":"plan","_meta":{"agent":"x"}}}}`)
	pump.handleStdoutLine(updateFrame("ses_b", "b"))
	pump.handleStdoutLine(updateFrame("ses_a", "a"))

	want := []string{
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_a","update":{"_meta":{"agent":"x","ferngeist/seq":1},"sessionUpdate":"plan"}}}`,
		numberedUpdateFrame("ses_b", "b", 1),
		numberedUpdateFrame("ses_a", "a", 2),
	}
	if got := readFrames(t, pump, c, 3); !slices.Equal(got, want) {
		t.Fatalf("forwarded frames:\n got %q\nwant %q", got, want)
	}
}

// A catch-up resume gets exactly the updates after the client's seq, the reply
// reporting the still-running turn, and then the agent's unanswered request.
func TestCatchUpResumeSendsOnlyMissedUpdates(t *testing.T) {
	pump := heldPump()
	pump.prepareOutbound(0, promptRequest("ses_a", "go")) // seq 1; its turn runs on
	pump.handleStdoutLine(updateFrame("ses_a", "one"))    // seq 2, the client's last
	pump.handleStdoutLine(updateFrame("ses_a", "two"))    // seq 3, missed
	pump.handleStdoutLine(permissionRequest)

	gen, c := connect(t, pump)
	if err := pump.WriteToAgent(gen, catchUpResume(pump.loadRecovery.epoch, 2)); err != nil {
		t.Fatalf("WriteToAgent: %v", err)
	}
	got := readFrames(t, pump, c, 3)
	if got[0] != numberedUpdateFrame("ses_a", "two", 3) {
		t.Fatalf("missed update = %s", got[0])
	}
	id, res := catchUpOf(t, got[1])
	if id != "5" || res.Epoch != pump.loadRecovery.epoch || res.HeadSeq != 3 || res.Gap ||
		!res.TurnActive || res.TurnStartedAt == nil {
		t.Fatalf("reply id %s, catch-up %+v", id, res)
	}
	if got[2] != permissionRequest {
		t.Fatalf("redelivered request = %s", got[2])
	}
}

// When the buffer cannot fill the client's gap, the reply says so and replays
// nothing, so the client falls back to session/load.
func TestCatchUpResumeReportsGap(t *testing.T) {
	for _, tc := range []struct {
		name     string
		epoch    string
		afterSeq int
	}{
		{"another epoch", "restarted", 1},
		{"seq never assigned", "", 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pump := heldPump()
			pump.handleStdoutLine(updateFrame("ses_a", "one"))
			epoch := tc.epoch
			if epoch == "" {
				epoch = pump.loadRecovery.epoch
			}
			gen, c := connect(t, pump)
			if err := pump.WriteToAgent(gen, catchUpResume(epoch, tc.afterSeq)); err != nil {
				t.Fatalf("WriteToAgent: %v", err)
			}
			if _, res := catchUpOf(t, readFrames(t, pump, c, 1)[0]); !res.Gap || res.HeadSeq != 1 {
				t.Fatalf("catch-up %+v, want gap at head 1", res)
			}
		})
	}
}

func TestCatchUpReportsGapOnceNeededUpdatesWereEvicted(t *testing.T) {
	r := newTestLoadRecovery()
	onFrame(t, r, updateFrame("ses_a", "first"))
	onFrame(t, r, updateFrame("ses_a", strings.Repeat("x", maxLoadHistoryBytes))) // evicts seq 1

	if _, _, gap := r.catchUp("ses_a", r.epoch, 0); !gap {
		t.Fatal("catching up from seq 0 needs the evicted seq 1: want gap")
	}
	if frames, _, gap := r.catchUp("ses_a", r.epoch, 1); gap || len(frames) != 1 {
		t.Fatalf("from seq 1: gap %v, %d frames; want the retained frame", gap, len(frames))
	}
}

// A resume the agent must answer (no catch-up asked, or a session this process
// does not hold) is not the gateway's, and a superseded connection's catch-up
// gets no reply on its successor.
func TestCatchUpResumeLeavesOtherResumesAlone(t *testing.T) {
	pump := heldPump()
	plain := parseClientFrame([]byte(`{"jsonrpc":"2.0","id":5,"method":"session/resume","params":{"sessionId":"ses_a","cwd":"/proj"}}`))
	if pump.answerCatchUp(0, plain) {
		t.Fatal("a resume without catch-up _meta must reach the agent")
	}
	if newPump().answerCatchUp(0, parseClientFrame(catchUpResume("e", 0))) {
		t.Fatal("a session this process does not hold must reach the agent")
	}

	stale, _ := connect(t, pump)
	_, c := connect(t, pump)
	if err := pump.WriteToAgent(stale, catchUpResume(pump.loadRecovery.epoch, 0)); err != nil {
		t.Fatalf("WriteToAgent: %v", err)
	}
	readFrames(t, pump, c, 0)
}

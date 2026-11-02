package session

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
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
	out := pump.prepareOutbound(0, parseClientFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/load","params":{"sessionId":"ses_a","cwd":"/proj"}}`)))
	pump.handleStdoutLine(`{"jsonrpc":"2.0","id":` + string(parseClientFrame(out).ID) + `,"result":null}`)
	return pump
}

func TestUpdatesAreNumberedPerSessionKeepingAgentMeta(t *testing.T) {
	pump := heldPump()
	_, c := connect(t, pump)
	pump.handleStdoutLine(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_a","update":{"sessionUpdate":"plan","_meta":{"agent":"x"}}}}`)
	pump.handleStdoutLine(updateFrame("ses_b", "b"))
	pump.handleStdoutLine(updateFrame("ses_a", "a"))

	want := []string{
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_a","update":{"sessionUpdate":"plan","_meta":{"agent":"x","ferngeist/seq":1}}}}`,
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

func TestRejectedPromptInvalidatesOnlyCursorsThatCouldContainIt(t *testing.T) {
	r := newTestLoadRecovery()
	onFrame(t, r, updateFrame("ses_a", "before"))                     // seq 1
	r.OnOutbound(parseClientFrame(promptWithClientID(2, "rejected"))) // seq 2
	r.forgetPrompt("ses_a", "2")
	onFrame(t, r, updateFrame("ses_a", "after")) // seq 3
	for _, tc := range []struct {
		after int64
		gap   bool
		count int
	}{{0, false, 2}, {1, false, 1}, {2, true, 0}, {3, false, 0}} {
		frames, head, gap := r.catchUp("ses_a", r.epoch, tc.after)
		if gap != tc.gap || len(frames) != tc.count || head != 3 {
			t.Errorf("after %d: frames=%d head=%d gap=%v; want frames=%d gap=%v", tc.after, len(frames), head, gap, tc.count, tc.gap)
		}
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

func TestCatchUpGapPreservesRestoreForLoad(t *testing.T) {
	pump := heldPump()
	promptAndReply(t, pump, "ses_a", `{"stopReason":"end_turn","usage":{"totalTokens":15}}`)
	pump.handleStdoutLine(permissionRequest)
	gen, c := connect(t, pump)
	if !pump.answerCatchUp(gen, parseClientFrame(catchUpResume("old-epoch", 0))) {
		t.Fatal("catch-up was not handled")
	}
	if _, res := catchUpOf(t, readFrames(t, pump, c, 1)[0]); !res.Gap {
		t.Fatal("expected a gap")
	}
	loadAndReply(t, pump, gen, "ses_a")
	got := readFrames(t, pump, c, 3)
	turnResultParams(t, got[0])
	if got[2] != permissionRequest {
		t.Fatalf("load lost unanswered request: %q", got)
	}
}

func TestCatchUpReportsGapForUnnumberedUpdate(t *testing.T) {
	r := newTestLoadRecovery()
	onFrame(t, r, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_a","update":{"sessionUpdate":"plan","_meta":[]}}}`)
	onFrame(t, r, updateFrame("ses_a", "next"))
	if frames, head, gap := r.catchUp("ses_a", r.epoch, 0); !gap || len(frames) != 0 || head != 1 {
		t.Fatalf("catch-up = %q, %d, %v; want gap without replay", frames, head, gap)
	}
}

func TestCatchUpSkipsUnnumberedUpdatesBeforeCursor(t *testing.T) {
	r := newTestLoadRecovery()
	onFrame(t, r, updateFrame("ses_a", "before"))
	onFrame(t, r, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_a","update":{"sessionUpdate":"plan","_meta":[]}}}`)
	onFrame(t, r, updateFrame("ses_a", "boundary"))
	onFrame(t, r, updateFrame("ses_a", "after"))
	for _, after := range []int64{0, 1, 2, 3} {
		frames, head, gap := r.catchUp("ses_a", r.epoch, after)
		if gap != (after < 2) || head != 3 {
			t.Errorf("after %d: head=%d gap=%v", after, head, gap)
		}
		if after == 2 && !slices.Equal(frames, []string{numberedUpdateFrame("ses_a", "after", 3)}) {
			t.Errorf("after boundary: frames=%q", frames)
		}
		if after == 3 && len(frames) != 0 {
			t.Errorf("at head: frames=%q", frames)
		}
	}
}

func TestCatchUpTurnStartedAtUsesUTC(t *testing.T) {
	pump := heldPump()
	pump.prepareOutbound(0, promptRequest("ses_a", "go"))
	for id, req := range pump.reqIDs.pending {
		req.startedAt = time.Date(2026, 11, 1, 12, 0, 0, 0, time.FixedZone("host", 6*60*60))
		pump.reqIDs.pending[id] = req
	}
	gen, c := connect(t, pump)
	pump.answerCatchUp(gen, parseClientFrame(catchUpResume(pump.Epoch(), 1)))
	got := readFrames(t, pump, c, 1)[0]
	if !strings.Contains(got, `"turnStartedAt":"2026-11-01T06:00:00Z"`) {
		t.Fatalf("catch-up timestamp is not UTC: %s", got)
	}
}

func TestCachedInitializeWithholdsSupersededReply(t *testing.T) {
	pump := newPump()
	feedInitialize(pump, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}`)
	stale, _ := connect(t, pump)
	gen, c := connect(t, pump)
	request := []byte(`{"jsonrpc":"2.0","id":42,"method":"initialize","params":{}}`)
	if err := pump.WriteToAgent(stale, request); err != nil {
		t.Fatal(err)
	}
	readFrames(t, pump, c, 0)
	if err := pump.WriteToAgent(gen, request); err != nil {
		t.Fatal(err)
	}
	if got := readFrames(t, pump, c, 1)[0]; got != `{"id":42,"jsonrpc":"2.0","result":{"protocolVersion":1}}` {
		t.Fatalf("current initialize reply = %s", got)
	}
}

func TestOnlyAcceptedSessionsAreHeld(t *testing.T) {
	for _, method := range []string{"session/new", "session/load", "session/resume"} {
		for _, accepted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/accepted=%v", method, accepted), func(t *testing.T) {
				pump := newPump()
				pump.loadRecovery = newTestLoadRecovery()
				sess := &Session{ID: "gw", AgentID: "agent", DeviceID: "device", Status: StatusActive, pump: pump}
				rs := &RuntimeSession{sessions: map[string]*Session{"gw": sess}}
				request := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":{"sessionId":"ses_a","cwd":"/proj"}}`, method)
				out := pump.prepareOutbound(0, parseClientFrame([]byte(request)))
				if rs.holding("agent", "device", "ses_a") != nil ||
					pump.answerCatchUp(0, parseClientFrame(catchUpResume(pump.Epoch(), 0))) {
					t.Fatal("unanswered request made session held")
				}
				reply := `"error":{"code":-32602,"message":"unknown session"}`
				if accepted {
					reply = `"result":{"sessionId":"ses_a"}`
				}
				pump.handleStdoutLine(`{"jsonrpc":"2.0","id":` + agentIDOf(t, out) + `,` + reply + `}`)
				if held := rs.holding("agent", "device", "ses_a") != nil; held != accepted {
					t.Fatalf("held = %v, accepted = %v", held, accepted)
				}
				if handled := pump.answerCatchUp(0, parseClientFrame(catchUpResume(pump.Epoch(), 0))); handled != accepted {
					t.Fatalf("catch-up handled = %v, accepted = %v", handled, accepted)
				}
			})
		}
	}
}

func TestAcceptedSessionWithoutCwdIsHeld(t *testing.T) {
	pump := newPump()
	pump.loadRecovery = newTestLoadRecovery()
	out := pump.prepareOutbound(0, parseClientFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/load","params":{"sessionId":"ses_a"}}`)))
	pump.handleStdoutLine(`{"jsonrpc":"2.0","id":` + agentIDOf(t, out) + `,"result":null}`)
	if !pump.answerCatchUp(0, parseClientFrame(catchUpResume(pump.Epoch(), 0))) {
		t.Fatal("accepted session without a cwd was not held")
	}
}

func TestUpdateMetadataPreservesOpaqueFields(t *testing.T) {
	line := `{"jsonrpc":"2.0","method":"session/update","extension":{"n":9007199254740993},"params":{"sessionId":"s","extra":[1,null],"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"<hello>"},"_meta":{"vendor":{"x":true},"ferngeist/seq":90}}}}`
	got, ok := withUpdateMeta(line, map[string]json.RawMessage{seqMetaKey: json.RawMessage(`2`)})
	if !ok {
		t.Fatal("metadata could not be added")
	}
	// UseNumber preserves integer metadata beyond float64 precision.
	var expected, result any
	decoder := json.NewDecoder(strings.NewReader(strings.Replace(line, `"ferngeist/seq":90`, `"ferngeist/seq":2`, 1)))
	decoder.UseNumber()
	if err := decoder.Decode(&expected); err != nil {
		t.Fatal(err)
	}
	decoder = json.NewDecoder(strings.NewReader(got))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result, expected) {
		t.Fatalf("metadata rewrite lost fields: %s", got)
	}
}

func TestUpdatesAcceptedByFrameParserReceiveSequenceNumbers(t *testing.T) {
	for _, content := range []string{`"\ud800"`, `"\udc00"`, "\"invalid\xff\""} {
		t.Run(content, func(t *testing.T) {
			line := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_a","update":{"sessionUpdate":"agent_message_chunk","content":{"text":` + content + `}}}}`
			probe, ok := parseFrameProbe([]byte(line))
			if !ok {
				t.Fatal("frame parser rejected test input")
			}
			r := newTestLoadRecovery()
			stamped := r.record(line, probe, ok)
			frames, head, gap := r.catchUp("ses_a", r.epoch, 0)
			if gap || head != 1 || !slices.Equal(frames, []string{stamped}) || !strings.Contains(stamped, `"ferngeist/seq":1`) || !strings.Contains(stamped, content) {
				t.Fatalf("numbering changed opaque content or lost update: head=%d gap=%v frame=%q", head, gap, stamped)
			}
		})
	}
}

func TestUpdateMetadataHandlesObjectBoundaries(t *testing.T) {
	for _, tc := range []struct {
		params string
		ok     bool
	}{
		{`{"update":{ }}`, true},
		{`{"update":{"_meta":null}}`, true},
		{`{"update":null}`, false},
		{`{"update":[]}`, false},
		{`{"update":{"_meta":[]}}`, false},
		{`{"update":{"_meta":false}}`, false},
		{`{"update":{"_meta":{}},"up\u0064ate":{"_meta":{"ferngeist/seq":1},"_meta":null}}`, true},
	} {
		t.Run(tc.params, func(t *testing.T) {
			line := `{"jsonrpc":"2.0","method":"session/update","params":` + tc.params + `}`
			got, ok := withUpdateMeta(line, map[string]json.RawMessage{seqMetaKey: json.RawMessage(`42`)})
			if ok != tc.ok || (!ok && got != line) {
				t.Fatalf("stamped = %s, %v; want ok=%v", got, ok, tc.ok)
			}
			if !ok {
				return
			}
			var msg struct {
				Params struct {
					Update struct {
						Meta map[string]int `json:"_meta"`
					} `json:"update"`
				} `json:"params"`
			}
			if err := json.Unmarshal([]byte(got), &msg); err != nil || msg.Params.Update.Meta[seqMetaKey] != 42 {
				t.Fatalf("metadata was not stamped in update: %s (%v)", got, err)
			}
		})
	}
}

func BenchmarkUpdateMetadata(b *testing.B) {
	for _, size := range []int{128, 16 << 10, 1 << 20} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			line := updateFrame("s", strings.Repeat("x", size))
			meta := map[string]json.RawMessage{seqMetaKey: json.RawMessage(`42`)}
			b.ReportAllocs()
			b.SetBytes(int64(len(line)))
			for b.Loop() {
				if _, ok := withUpdateMeta(line, meta); !ok {
					b.Fatal("metadata could not be added")
				}
			}
		})
	}
}

func TestEvictedUnnumberedUpdateStillReportsGap(t *testing.T) {
	r := newTestLoadRecovery()
	onFrame(t, r, updateFrame("ses_a", "before"))
	onFrame(t, r, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_a","update":{"sessionUpdate":"plan","_meta":[]}}}`)
	onFrame(t, r, updateFrame("ses_a", strings.Repeat("x", maxLoadHistoryBytes)))
	if _, _, gap := r.catchUp("ses_a", r.epoch, 1); !gap {
		t.Fatal("evicted unnumbered update lost its gap marker")
	}
}

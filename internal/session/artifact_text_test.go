package session

import (
	"encoding/json"
	"io"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
	"strings"
	"testing"
)

const artifactMessageID = "20b1d75e-d971-4e59-a789-c083d060a291"

func artifactText(sid, id, text string) string {
	type content struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type update struct {
		Kind      string  `json:"sessionUpdate"`
		MessageID string  `json:"messageId,omitempty"`
		Content   content `json:"content"`
	}
	type params struct {
		SessionID string `json:"sessionId"`
		Update    update `json:"update"`
	}
	raw, _ := json.Marshal(struct {
		Method string `json:"method"`
		Params params `json:"params"`
	}{
		"session/update", params{sid, update{"agent_message_chunk", id, content{"text", text}}},
	})
	return string(raw)
}

func assertArtifactEvidence(t *testing.T, rs *RuntimeSession, want bool) {
	t.Helper()
	got, err := rs.ArtifactAdvertised("device", "runtime", "acp", "/tmp/video.mp4")
	if err != nil || got != want {
		t.Fatalf("advertised = %v, %v; want %v", got, err, want)
	}
}

func TestArtifactTextMatchesExactDeliberateLocalTokens(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{"Recorded: /tmp/video.mp4 done", true},
		{"[Recorded test](/tmp/video.mp4)", true},
		{"[Recorded test](file:///tmp/%76ideo.mp4)", true},
		{"Use `/tmp/video.mp4`", true},
		{"<file:///tmp/video.mp4>", true},
		{" 'file:///tmp/video.mp4' ", true},
		{" /tmp/video.mp4.bak ", false},
		{" /tmp/video.mp4, ", false},
		{" /tmp/video.mp4. ", false},
		{" /tmp/video.mp4?x ", false},
		{" /tmp/video.mp4#fragment ", false},
		{" prefix/tmp/video.mp4 ", false},
		{" //tmp/video.mp4 ", false},
		{" /tmp/x/../video.mp4 ", false},
		{" file://host/tmp/video.mp4 ", false},
		{" file:///tmp/%2576ideo.mp4 ", false},
		{" https://host/tmp/video.mp4 ", false},
		{" https://host/?q=/tmp/video.mp4#file:///tmp/video.mp4 ", false},
		{"[URL](https://host/[nested](/tmp/video.mp4))", false},
		{"<HTTPS://host/?q=/tmp/video.mp4#x>", false},
		{" https://host/'[nested](/tmp/video.mp4) ", false},
		{" file://host/[nested](/tmp/video.mp4) ", false},
		{"/tmp/video.mp4", false}, // No message ID or delimiters: chunk edges are ambiguous.
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			rs, p := artifactSession()
			p.handleStdoutLine(artifactText("acp", "", tt.text))
			assertArtifactEvidence(t, rs, tt.want)
		})
	}
}

func TestArtifactSplitTextRequiresIdentifiedUnbrokenMessage(t *testing.T) {
	tests := []struct {
		name     string
		boundary func(*StdioPump)
		want     bool
	}{
		{"same message", func(*StdioPump) {}, true},
		{"user event", func(p *StdioPump) {
			p.handleStdoutLine(strings.Replace(artifactText("acp", "", "x"), "agent_message_chunk", "user_message_chunk", 1))
		}, false},
		{"thought event", func(p *StdioPump) {
			p.handleStdoutLine(strings.Replace(artifactText("acp", "", "x"), "agent_message_chunk", "agent_thought_chunk", 1))
		}, false},
		{"tool event", func(p *StdioPump) {
			p.handleStdoutLine(`{"method":"session/update","params":{"sessionId":"acp","update":{"sessionUpdate":"tool_call","title":"x"}}}`)
		}, false},
		{"independent message", func(p *StdioPump) { p.handleStdoutLine(artifactText("acp", "other", "x")) }, false},
		{"unidentified message", func(p *StdioPump) { p.handleStdoutLine(artifactText("acp", "", "x")) }, false},
		{"prompt reply", func(p *StdioPump) { p.handleStdoutLine(`{"id":99,"result":{"stopReason":"end_turn"}}`) }, false},
		{"rejected prompt", func(p *StdioPump) {
			p.prepareOutbound(1, parseClientFrame([]byte(`{"id":1,"method":"session/prompt","params":{"sessionId":"acp","prompt":[]}}`)))
			p.handleStdoutLine(`{"id":1,"error":{"message":"rejected"}}`)
		}, false},
		{"load", func(p *StdioPump) { p.prepareOutbound(1, loadRequest("1", "acp")) }, false},
		{"different session", func(p *StdioPump) { p.handleStdoutLine(artifactText("other", artifactMessageID, "x")) }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rs, p := artifactSession()
			p.handleStdoutLine(artifactText("acp", artifactMessageID, "[Video](/tmp/vid"))
			tt.boundary(p)
			p.handleStdoutLine(artifactText("acp", artifactMessageID, "eo.mp4)"))
			assertArtifactEvidence(t, rs, tt.want)
		})
	}
}

func TestArtifactIdentifiedTextEdgesNeedCompleteMessage(t *testing.T) {
	rs, p := artifactSession()
	p.prepareOutbound(1, parseClientFrame([]byte(`{"id":1,"method":"session/prompt","params":{"sessionId":"acp","prompt":[]}}`)))
	p.handleStdoutLine(artifactText("acp", artifactMessageID, "/tmp/video.mp4"))
	assertArtifactEvidence(t, rs, false)
	p.handleStdoutLine(`{"id":1,"result":{"stopReason":"end_turn"}}`)
	assertArtifactEvidence(t, rs, true)
}

func TestArtifactSplitURLsNeverExposeNestedLocalPaths(t *testing.T) {
	for _, id := range []string{"", artifactMessageID} {
		rs, p := artifactSession()
		p.handleStdoutLine(artifactText("acp", id, "https://host"))
		p.handleStdoutLine(artifactText("acp", id, "/tmp/video.mp4"))
		p.handleStdoutLine(`{"id":1,"result":{"stopReason":"end_turn"}}`)
		assertArtifactEvidence(t, rs, false)
	}
}

func TestArtifactTextExcludesUserThoughtAndToolContent(t *testing.T) {
	for _, kind := range []string{"user_message_chunk", "agent_thought_chunk", "tool_call", "tool_call_update", "arbitrary"} {
		t.Run(kind, func(t *testing.T) {
			rs, p := artifactSession()
			p.handleStdoutLine(strings.Replace(artifactText("acp", artifactMessageID, "[Video](/tmp/video.mp4)"), "agent_message_chunk", kind, 1))
			assertArtifactEvidence(t, rs, false)
		})
	}
	rs, p := artifactSession()
	p.prepareOutbound(1, parseClientFrame([]byte(`{"id":1,"method":"session/prompt","params":{"sessionId":"acp","prompt":[{"type":"text","text":"[Video](/tmp/video.mp4)"}]}}`)))
	assertArtifactEvidence(t, rs, false)
}

func TestArtifactLoadedAgentReplayUsesOnlyItsOwnAcceptedSession(t *testing.T) {
	rs, p := artifactSession()
	p.acp.held = nil
	p.prepareOutbound(1, loadRequest("1", "acp"))
	p.handleStdoutLine(artifactText("acp", artifactMessageID, "[Video](/tmp/vid"))
	p.handleStdoutLine(artifactText("acp", artifactMessageID, "eo.mp4)"))
	if got, err := rs.ArtifactAdvertised("device", "runtime", "acp", "/tmp/video.mp4"); got || err == nil {
		t.Fatalf("unaccepted load = %v, %v", got, err)
	}
	p.handleStdoutLine(`{"id":1,"result":{}}`)
	assertArtifactEvidence(t, rs, true)
	p.prepareOutbound(1, loadRequest("2", "other"))
	p.handleStdoutLine(`{"id":2,"result":{}}`)
	if got, err := rs.ArtifactAdvertised("device", "runtime", "other", "/tmp/video.mp4"); got || err != nil {
		t.Fatalf("other session = %v, %v", got, err)
	}
	// A gateway replay reuses frames rather than re-advertising user content.
	p.prepareOutbound(1, loadRequest("3", "acp"))
	p.handleStdoutLine(`{"id":3,"error":{"message":"already loaded"}}`)
	assertArtifactEvidence(t, rs, true)
	rs.sessions["gateway"].Status = StatusClosing
	if got, err := rs.ArtifactAdvertised("device", "runtime", "acp", "/tmp/video.mp4"); got || err == nil {
		t.Fatalf("closed = %v, %v", got, err)
	}
	fresh, _ := artifactSession()
	assertArtifactEvidence(t, fresh, false)
}

func TestArtifactEvictionLosesTextEvidenceAndPartialMessageEdges(t *testing.T) {
	rs, p := artifactSession()
	p.handleStdoutLine(artifactText("acp", artifactMessageID, "[Video](/tmp/video.mp4)"))
	assertArtifactEvidence(t, rs, true)
	// Force the actual byte budget to evict the start of this same message.
	p.handleStdoutLine(artifactText("acp", artifactMessageID, strings.Repeat("x", maxLoadHistoryBytes)))
	p.handleStdoutLine(artifactText("acp", artifactMessageID, "/tmp/video.mp4"))
	p.handleStdoutLine(`{"id":1,"result":{"stopReason":"end_turn"}}`)
	assertArtifactEvidence(t, rs, false)
	p.loadRecovery.mu.Lock()
	if p.loadRecovery.histSize["acp"] > maxLoadHistoryBytes && len(p.loadRecovery.history["acp"]) > 1 {
		t.Error("history exceeded the existing multi-frame byte bound")
	}
	p.loadRecovery.mu.Unlock()
	p.handleStdoutLine(artifactText("acp", "new-message", "[Video](/tmp/video.mp4)"))
	assertArtifactEvidence(t, rs, true)
}

func TestArtifactSplitTextWithoutMessageIdentityNeverJoins(t *testing.T) {
	rs, p := artifactSession()
	for _, text := range []string{"[Video](/tmp/vid", "eo.mp4)"} {
		p.handleStdoutLine(artifactText("acp", "", text))
	}
	assertArtifactEvidence(t, rs, false)
}

func TestArtifactUnknownChunkAndEvictedURLPrefixesFailClosed(t *testing.T) {
	for _, evict := range []bool{false, true} {
		rs, p := artifactSession()
		id := ""
		prefix := "https://host/"
		if evict {
			id = artifactMessageID
			prefix = strings.Repeat("x", maxLoadHistoryBytes) + "https://host/"
		}
		p.handleStdoutLine(artifactText("acp", id, prefix))
		p.handleStdoutLine(artifactText("acp", id, "[nested](/tmp/video.mp4)"))
		p.handleStdoutLine(`{"id":1,"result":{}}`)
		assertArtifactEvidence(t, rs, false)
	}
}

func TestArtifactGatewayHandledRequestsStillBreakTextStreams(t *testing.T) {
	for _, request := range []string{
		`{"id":2,"method":"session/prompt","params":{"sessionId":"acp","prompt":[],"_meta":{"ferngeist/clientId":"dup"}}}`,
		`{"id":2,"method":"session/resume","params":{"sessionId":"acp","_meta":{"ferngeist/catchUp":{"epoch":"old","afterSeq":0}}}}`,
	} {
		t.Run(request, func(t *testing.T) {
			rs, p := artifactSession()
			p.prompts.claim("acp", "dup")
			p.handleStdoutLine(artifactText("acp", artifactMessageID, "[Video](/tmp/vid"))
			if err := p.WriteToAgent(1, []byte(request)); err != nil {
				t.Fatal(err)
			}
			p.handleStdoutLine(artifactText("acp", artifactMessageID, "eo.mp4)"))
			assertArtifactEvidence(t, rs, false)
		})
	}
}

func TestArtifactInterruptionsNeverEstablishMessageEdges(t *testing.T) {
	for _, boundary := range []string{
		`{"method":"session/update","params":{"sessionId":"acp","update":{"sessionUpdate":"agent_thought_chunk"}}}`,
		`{"method":"session/update","params":{"sessionId":"acp","update":{"sessionUpdate":"tool_call"}}}`,
		`{"id":99,"result":{}}`,
	} {
		t.Run(boundary, func(t *testing.T) {
			rs, p := artifactSession()
			p.handleStdoutLine(artifactText("acp", artifactMessageID, "/tmp/video.mp4"))
			p.handleStdoutLine(boundary)
			assertArtifactEvidence(t, rs, false)
			p.handleStdoutLine(artifactText("acp", artifactMessageID, ".bak"))
			assertArtifactEvidence(t, rs, false)
			rs, p = artifactSession()
			p.handleStdoutLine(artifactText("acp", artifactMessageID, "https://host"))
			p.handleStdoutLine(boundary)
			p.handleStdoutLine(artifactText("acp", artifactMessageID, "/tmp/video.mp4 "))
			assertArtifactEvidence(t, rs, false)
		})
	}
}

func TestArtifactDroppedStdoutFrameBreaksTextProvenance(t *testing.T) {
	rs, p := artifactSession()
	p.proc = &runtime.Process{Stdin: nopWriteCloser{}, Stdout: io.NopCloser(io.MultiReader(
		strings.NewReader(artifactText("acp", artifactMessageID, "[Video](/tmp/vid")+"\n"),
		&repeatReader{n: maxAgentFrameBytes + 1, b: 'x', nl: true},
		strings.NewReader(artifactText("acp", artifactMessageID, "eo.mp4)")+"\n"),
	))}
	p.StdoutDrainLoop(t.Context())
	assertArtifactEvidence(t, rs, false)
}

func TestArtifactMissingIdentityCannotJoinOrRestoreLeadingProvenance(t *testing.T) {
	rs, p := artifactSession()
	p.handleStdoutLine(artifactText("acp", artifactMessageID, "old message"))
	p.handleStdoutLine(artifactText("acp", "", "[Video](/tmp/vid"))
	p.handleStdoutLine(artifactText("acp", artifactMessageID, "eo.mp4)"))
	assertArtifactEvidence(t, rs, false)
	rs, p = artifactSession()
	p.handleStdoutLine(artifactText("acp", "", "https://host"))
	p.handleStdoutLine(artifactText("acp", artifactMessageID, "/tmp/video.mp4 "))
	assertArtifactEvidence(t, rs, false)
}

func TestArtifactDroppedNewMessageCannotExposeURLSuffix(t *testing.T) {
	for _, prior := range []string{"", artifactText("acp", artifactMessageID, "old message") + "\n"} {
		t.Run(prior, func(t *testing.T) {
			rs, p := artifactSession()
			prefix := artifactText("acp", "new-message", "https://host/")
			prefix = prefix[:strings.Index(prefix, "https://host/")+len("https://host/")]
			p.proc = &runtime.Process{Stdin: nopWriteCloser{}, Stdout: io.NopCloser(io.MultiReader(
				strings.NewReader(prior+prefix),
				&repeatReader{n: maxAgentFrameBytes, b: 'x'},
				strings.NewReader("\"}}}}\n"+artifactText("acp", "new-message", "/tmp/video.mp4 ")+"\n"),
			))}
			p.StdoutDrainLoop(t.Context())
			assertArtifactEvidence(t, rs, false)
		})
	}
}

func TestArtifactTextEvidenceStaysWithinItsRuntime(t *testing.T) {
	rs, _ := artifactSession()
	other, p := artifactSession()
	s := other.sessions["gateway"]
	s.RuntimeID, s.DeviceID = "other-runtime", "other-device"
	rs.sessions["other-gateway"] = s
	p.handleStdoutLine(artifactText("acp", artifactMessageID, "[Video](/tmp/video.mp4)"))
	assertArtifactEvidence(t, rs, false)
	got, err := rs.ArtifactAdvertised("other-device", "other-runtime", "acp", "/tmp/video.mp4")
	if err != nil || !got {
		t.Fatalf("originating runtime = %v, %v", got, err)
	}
}

func TestArtifactAdjacentNewIdentityCompletesPreviousMessage(t *testing.T) {
	rs, p := artifactSession()
	p.handleStdoutLine(artifactText("acp", artifactMessageID, "/tmp/video.mp4"))
	assertArtifactEvidence(t, rs, false)
	p.handleStdoutLine(artifactText("acp", "new-message", "An independent message."))
	assertArtifactEvidence(t, rs, true)
}

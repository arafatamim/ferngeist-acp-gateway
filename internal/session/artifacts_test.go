package session

import (
	"errors"
	"strings"
	"testing"
)

func TestArtifactEvidenceUsesOnlyStructuredAgentResources(t *testing.T) {
	tests := []struct {
		name, update string
		want         bool
	}{
		{"link", `{"sessionUpdate":"agent_message_chunk","content":{"type":"resource_link","uri":"file:///tmp/video.mp4"}}`, true},
		{"embedded", `{"sessionUpdate":"agent_message_chunk","content":{"type":"resource","resource":{"uri":"file:///tmp/video.mp4","text":"x"}}}`, true},
		{"tool link", `{"sessionUpdate":"tool_call","content":[{"type":"content","content":{"type":"resource_link","uri":"/tmp/video.mp4"}}]}`, true},
		{"tool embedded", `{"sessionUpdate":"tool_call_update","content":[{"type":"content","content":{"type":"resource","resource":{"uri":"file:///tmp/video.mp4","blob":"eA=="}}}]}`, true},
		{"text", `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"/tmp/video.mp4"}}`, false},
		{"user", `{"sessionUpdate":"user_message_chunk","content":{"type":"resource_link","uri":"file:///tmp/video.mp4"}}`, false},
		{"thought", `{"sessionUpdate":"agent_thought_chunk","content":{"type":"resource_link","uri":"file:///tmp/video.mp4"}}`, false},
		{"tool text", `{"sessionUpdate":"tool_call_update","title":"/tmp/video.mp4","content":[{"type":"content","content":{"type":"text","text":"file:///tmp/video.mp4"}},{"type":"diff","path":"/tmp/video.mp4"}]}`, false},
		{"remote", `{"sessionUpdate":"agent_message_chunk","content":{"type":"resource_link","uri":"file://host/tmp/video.mp4"}}`, false},
		{"http", `{"sessionUpdate":"agent_message_chunk","content":{"type":"resource_link","uri":"https://host/tmp/video.mp4"}}`, false},
		{"alias", `{"sessionUpdate":"agent_message_chunk","content":{"type":"resource_link","uri":"file:///tmp/x/../video.mp4"}}`, false},
		{"fragment", `{"sessionUpdate":"agent_message_chunk","content":{"type":"resource_link","uri":"file:///tmp/video.mp4#x"}}`, false},
		{"empty fragment", `{"sessionUpdate":"agent_message_chunk","content":{"type":"resource_link","uri":"file:///tmp/video.mp4#"}}`, false},
		{"query", `{"sessionUpdate":"agent_message_chunk","content":{"type":"resource_link","uri":"file:///tmp/video.mp4?x"}}`, false},
		{"prefix", `{"sessionUpdate":"agent_message_chunk","content":{"type":"resource_link","uri":"file:///tmp/video.mp4.bak"}}`, false},
		{"once decoded", `{"sessionUpdate":"agent_message_chunk","content":{"type":"resource_link","uri":"file:///tmp/%76ideo.mp4"}}`, true},
		{"twice encoded", `{"sessionUpdate":"agent_message_chunk","content":{"type":"resource_link","uri":"file:///tmp/%2576ideo.mp4"}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rs, p := artifactSession()
			onFrame(t, p.loadRecovery, `{"method":"session/update","params":{"sessionId":"acp","update":`+tt.update+`}}`)
			got, err := rs.ArtifactAdvertised("device", "runtime", "acp", "/tmp/video.mp4")
			if err != nil || got != tt.want {
				t.Fatalf("advertised = %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

func artifactSession() (*RuntimeSession, *StdioPump) {
	p := newRecoveryPump()
	p.loadRecovery = newTestLoadRecovery()
	p.acp.held = map[string]bool{"acp": true}
	s := &Session{RuntimeID: "runtime", DeviceID: "device", Status: StatusActive, pump: p}
	return &RuntimeSession{sessions: map[string]*Session{"gateway": s}}, p
}

func TestArtifactContextRejectsForeignDeviceBeforeInspectingPump(t *testing.T) {
	rs, _ := artifactSession()
	rs.sessions["gateway"].pump = nil
	if _, err := rs.ArtifactAdvertised("foreign", "runtime", "acp", "/tmp/x"); !errors.Is(err, ErrDeviceMismatch) {
		t.Fatalf("foreign context = %v, want device mismatch", err)
	}
}

func TestArtifactEvidenceExpiresWithRetainedHistoryAndContext(t *testing.T) {
	rs, p := artifactSession()
	link := `{"method":"session/update","params":{"sessionId":"acp","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"resource_link","uri":"file:///tmp/video.mp4"}}}}`
	onFrame(t, p.loadRecovery, link)
	if got, err := rs.ArtifactAdvertised("device", "runtime", "wrong", "/tmp/video.mp4"); got || !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("unaccepted context = %v, %v", got, err)
	}
	onFrame(t, p.loadRecovery, updateFrame("acp", strings.Repeat("x", maxLoadHistoryBytes)))
	if got, err := rs.ArtifactAdvertised("device", "runtime", "acp", "/tmp/video.mp4"); got || err != nil {
		t.Fatalf("evicted evidence = %v, %v", got, err)
	}
	onFrame(t, p.loadRecovery, link)
	if got, err := rs.ArtifactAdvertised("device", "runtime", "acp", "/tmp/video.mp4"); !got || err != nil {
		t.Fatalf("replayed evidence = %v, %v", got, err)
	}
	rs.sessions["gateway"].Status = StatusClosing
	if _, err := rs.ArtifactAdvertised("device", "runtime", "acp", "/tmp/video.mp4"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("closed context = %v", err)
	}
	rs.sessions = nil
	if _, err := rs.ArtifactAdvertised("device", "runtime", "acp", "/tmp/video.mp4"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("restarted context = %v", err)
	}
}

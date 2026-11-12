package session

import (
	"net/url"
	"strings"
	"testing"
)

func TestArtifactWindowsStructuredURIMatchesExactDrivePath(t *testing.T) {
	path := `C:\recordings\video file.mp4`
	for _, tc := range []struct {
		uri  string
		want bool
	}{
		{"file:///C:/recordings/video%20file.mp4", true},
		{"file:///C:/recordings/video%2520file.mp4", false},
		{"file://host/C:/recordings/video%20file.mp4", false},
		{"file:///c:/recordings/video%20file.mp4", false},
		{"file:///C:/recordings/x/../video%20file.mp4", false},
		{"file:///C:%5Crecordings%5Cvideo%20file.mp4", false},
		{"file:///C:/recordings/video%20file.mp4#", false},
	} {
		t.Run(tc.uri, func(t *testing.T) {
			rs, p := artifactSession()
			resource := `{"method":"session/update","params":{"sessionId":"acp","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"resource_link","uri":"URI"}}}}`
			p.handleStdoutLine(strings.Replace(resource, "URI", tc.uri, 1))
			got, err := rs.ArtifactAdvertised("device", "runtime", "acp", path)
			if got != tc.want || err != nil {
				t.Fatalf("advertised = %v, %v", got, err)
			}
		})
	}
	// Once-decoded colons and percent signs remain exact filename data.
	if !(artifactContent{Type: "resource_link", URI: (&url.URL{Scheme: "file", Path: "/C:/recordings/100%.mp4"}).String()}).matches(`C:\recordings\100%.mp4`) {
		t.Fatal("escaped filename did not match")
	}
}

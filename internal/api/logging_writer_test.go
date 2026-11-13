package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRequestLogsRedactSensitiveQueries(t *testing.T) {
	tests := []struct {
		name, query, want string
	}{
		{"plain selectors", "path=/tmp/private-recording.mp4&acpSessionId=private-acp-id", "acpSessionId=redacted&path=redacted"},
		{"encoded selectors", "p%61th=%2Ftmp%2Fprivate-recording.mp4&acpSession%49d=private%2Dacp%2Did", "acpSessionId=redacted&path=redacted"},
		{"duplicate mixed case selectors", "path=/tmp/private-first&path=/tmp/private-second&PATH=%2Ftmp%2Fprivate-third&acpSessionId=private-first&AcPsEsSiOnId=private-second&AcPsEsSiOnId=private-third", "AcPsEsSiOnId=redacted&PATH=redacted&acpSessionId=redacted&path=redacted"},
		{"credentials", "ACCESS_TOKEN=private-access&token=private-first&token=private-second&Authorization=private-auth&view=summary", "ACCESS_TOKEN=redacted&Authorization=redacted&token=redacted&view=summary"},
		{"malformed value", "path=/tmp/private-recording.mp4&acpSessionId=private-acp-id&view=%ZZ", "redacted"},
		{"malformed key", "path=%2Ftmp%2Fprivate-recording.mp4&acpSessionId=private-acp-id&%ZZ=value", "redacted"},
		{"malformed selector", "path=%2Ftmp%2Fprivate-recording.mp4%&acpSessionId=private-acp-id", "redacted"},
		{"semicolon", "path=/tmp/private-recording.mp4;acpSessionId=private-acp-id&token=private-token", "redacted"},
		{"ordinary query", "view=summary&limit=2", "limit=2&view=summary"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			server := NewServer(Deps{Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
			req := httptest.NewRequest(http.MethodGet, "/healthz?"+tt.query, nil)
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, req)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", recorder.Code)
			}
			var entry struct {
				Message string `json:"msg"`
				Path    string `json:"path"`
			}
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
				t.Fatalf("decode request log: %v", err)
			}
			if entry.Message != "http request" || entry.Path != "/healthz?"+tt.want {
				t.Fatalf("request log = %s, want path %q", logs.String(), "/healthz?"+tt.want)
			}
			if strings.Contains(logs.String(), "private") {
				t.Fatalf("sensitive value leaked into request log: %s", logs.String())
			}
			if req.URL.RawQuery != tt.query {
				t.Fatalf("logging changed the request query to %q", req.URL.RawQuery)
			}
		})
	}
}

// TestLoggingResponseWriterExposesTheConnectionDeadline guards the handlers
// that extend their write deadline (worktree create): without Unwrap the
// request-logging wrapper hides the connection and the extension is a no-op.
func TestLoggingResponseWriterExposesTheConnectionDeadline(t *testing.T) {
	var deadlineErr error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wrapped := &loggingResponseWriter{ResponseWriter: w}
		deadlineErr = http.NewResponseController(wrapped).SetWriteDeadline(time.Now().Add(time.Minute))
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if deadlineErr != nil {
		t.Fatalf("SetWriteDeadline through the logging writer: %v", deadlineErr)
	}
}

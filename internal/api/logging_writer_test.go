package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

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

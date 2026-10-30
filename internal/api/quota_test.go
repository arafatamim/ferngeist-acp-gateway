package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestQuotasRequireAuthentication(t *testing.T) {
	server := newTestServerWithBaseDir(newHarnessBaseDir(t))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/quotas", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestQuotasReturnSanitizedFailure(t *testing.T) {
	server := newTestServerWithBaseDir(newHarnessBaseDir(t))
	token := pairDevice(t, server)
	request := httptest.NewRequest(http.MethodGet, "/v1/quotas", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
}

package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/catalog"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/config"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/discovery"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
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

func TestQuotasReturnAuthorizedResponse(t *testing.T) {
	base := newHarnessBaseDir(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := newTestServerFromDeps(Deps{
		Quotas: func(context.Context) (json.RawMessage, error) {
			return json.RawMessage(`{"providers":[{"provider":"codex"}]}`), nil
		},
		Config:    config.Config{ListenAddr: "127.0.0.1:0"},
		Logger:    logger,
		Catalog:   catalog.New(base, nil),
		Runtime:   runtime.NewSupervisor(logger, base, nil, nil),
		Pairing:   pairing.NewService(logger, nil),
		Discovery: discovery.New(logger),
	})
	token := pairDevice(t, server)
	request := httptest.NewRequest(http.MethodGet, "/v1/quotas", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if strings.TrimSpace(recorder.Body.String()) != `{"providers":[{"provider":"codex"}]}` {
		t.Fatalf("unexpected response: %s", recorder.Body.String())
	}
}

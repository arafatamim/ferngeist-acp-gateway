package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/catalog"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/config"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/discovery"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/quota"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestQuotasIdentifyInvalidCodexBarResponse(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "codexbar"), []byte("#!/bin/sh\nprintf 'hidden-diagnostic'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	server := newTestServerWithBaseDir(newHarnessBaseDir(t))
	server.quotas = quota.NewCache(quota.Collect, time.Minute).Get
	token := pairDevice(t, server)
	request := httptest.NewRequest(http.MethodGet, "/v1/quotas", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable || strings.TrimSpace(recorder.Body.String()) != `{"error":"quotas unavailable","code":"codexbar_invalid_response"}` {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestQuotasIdentifyMissingCodexBar(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	server := newTestServerWithBaseDir(newHarnessBaseDir(t))
	server.quotas = quota.NewCache(quota.Collect, time.Minute).Get
	token := pairDevice(t, server)
	request := httptest.NewRequest(http.MethodGet, "/v1/quotas", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable || strings.TrimSpace(recorder.Body.String()) != `{"error":"quotas unavailable","code":"codexbar_not_found"}` {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

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
	if recorder.Code != http.StatusServiceUnavailable || strings.TrimSpace(recorder.Body.String()) != `{"error":"quotas unavailable"}` {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
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

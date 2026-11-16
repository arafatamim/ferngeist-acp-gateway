package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func configureActiveService(t *testing.T, address string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$*\" in *show-environment*) exit 0;; *) printf 'loaded\\nactive\\nrunning\\nenabled\\n';; esac\n"
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("FERNGEIST_GATEWAY_ADMIN_ADDR", strings.TrimPrefix(address, "http://"))
}

func TestDaemonStatusRetriesWhileActiveServiceStarts(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			conn.Close()
			return
		}
		_, _ = w.Write([]byte(`{"name":"ready"}`))
	}))
	t.Cleanup(server.Close)
	configureActiveService(t, server.URL)
	if err := runDaemonStatus(); err != nil {
		t.Fatalf("status during startup: %v", err)
	}
	if calls.Load() < 2 {
		t.Fatalf("requests = %d, want retry after connection failure", calls.Load())
	}
}

func TestDaemonStatusPreservesStartupFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"startup failed"}`, http.StatusInternalServerError)
	}))
	configureActiveService(t, server.URL)
	if err := runDaemonStatus(); exitCodeFor(err) != 1 {
		t.Fatalf("answered error exit code = %d, want 1", exitCodeFor(err))
	}
	server.Close()
	if err := runDaemonStatus(); exitCodeFor(err) != exitDaemonUnreachable {
		t.Fatalf("unreachable exit code = %d, want %d", exitCodeFor(err), exitDaemonUnreachable)
	}
}

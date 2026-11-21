package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrettyLogLineFormatsSlogJSONAndPassesThroughOtherText(t *testing.T) {
	cases := []struct {
		name, line, wantSuffix string
	}{
		{"slog", `{"time":"2026-11-16T10:00:00Z","level":"WARN","msg":"agent exited","runtime":"rt-1","code":2,"err":"exit status 2","meta":{"a": 1}}`,
			` WARN  agent exited code=2 err="exit status 2" meta={"a":1} runtime=rt-1`},
		{"plain", "panic: boom", "panic: boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			WritePrettyLogLine(&out, tc.line)
			if got := strings.TrimSuffix(out.String(), "\n"); !strings.HasSuffix(got, tc.wantSuffix) {
				t.Fatalf("got %q, want suffix %q", got, tc.wantSuffix)
			}
		})
	}
}

func TestLoggerWritesPrettyStdoutAndJSONFile(t *testing.T) {
	dir := t.TempDir()
	var stdout strings.Builder
	logger, service, err := newLogger("info", dir, 0, 0, &stdout)
	if err != nil {
		t.Fatalf("newLogger() error = %v", err)
	}
	defer service.Close()

	logger.Warn("agent exited", "code", 2)

	if got := strings.TrimSpace(stdout.String()); !strings.HasSuffix(got, " WARN  agent exited code=2") {
		t.Fatalf("stdout = %q, want pretty log line", got)
	}
	file, err := os.ReadFile(filepath.Join(dir, defaultLogFileName))
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !json.Valid(bytes.TrimSpace(file)) {
		t.Fatalf("log file = %q, want JSON", file)
	}
}

func TestServiceTailLinesReadsAcrossRotations(t *testing.T) {
	service, err := NewService(t.TempDir(), "gateway.log", 120, 2)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	defer service.Close()

	logger := slog.New(slog.NewJSONHandler(service, nil))
	logger.Info("line one")
	logger.Info("line two")
	logger.Info("line three")
	logger.Info("line four")

	lines, err := service.TailLines(10)
	if err != nil {
		t.Fatalf("TailLines() error = %v", err)
	}
	if len(lines) == 0 {
		t.Fatal("TailLines() should return log lines")
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "line four") {
		t.Fatalf("TailLines() output missing latest line: %q", joined)
	}
}

func TestFollowGatewayLogEmitsNewLinesAcrossRotation(t *testing.T) {
	dir := t.TempDir()
	service, err := NewService(dir, defaultLogFileName, 40, 2)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	defer service.Close()
	if _, err := service.Write([]byte("old line before follow\n")); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines := make(chan string, 16)
	done := make(chan error, 1)
	go func() {
		done <- FollowGatewayLog(ctx, dir, time.Millisecond, func(line string) { lines <- line })
	}()

	// Wait until the follower is past its seek so the writes below count as new.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := service.Write([]byte("probe\n")); err != nil {
			t.Fatal(err)
		}
		select {
		case <-lines:
		case <-time.After(20 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("follower never emitted the probe")
			}
			continue
		}
		break
	}
	for len(lines) > 0 {
		<-lines
	}

	// Each 30-byte write overflows the 40-byte limit, so every write after the first rotates.
	want := []string{"first line after follow aaaaa", "second line rotates the file", "third line in the new file xx"}
	for _, line := range want {
		if _, err := service.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-lines:
			if got != line {
				t.Fatalf("got %q, want %q", got, line)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %q", line)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("FollowGatewayLog() error = %v", err)
	}
}

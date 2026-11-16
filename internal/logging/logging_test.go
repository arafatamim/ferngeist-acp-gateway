package logging

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

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

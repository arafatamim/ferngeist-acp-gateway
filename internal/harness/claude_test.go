package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeUnhideRelabelsTheFirstSDKEntrypoint(t *testing.T) {
	projects := t.TempDir()
	const id = "64f8ce81-f971-4d32-80b0-bdd82e4c1d2f"
	transcript := filepath.Join(projects, "-home-user-repo", id+".jsonl")
	writeFile(t, transcript, `{"type":"session-owner","sessionId":"`+id+`"}
{"type":"user","message":{"content":"say \"entrypoint\":\"sdk-ts\""},"entrypoint":"sdk-ts","sessionId":"`+id+`"}
{"type":"assistant","entrypoint":"sdk-ts","sessionId":"`+id+`"}
`)
	other := filepath.Join(projects, "-home-user-other", "other.jsonl")
	writeFile(t, other, `{"entrypoint":"sdk-cli"}`+"\n")
	before, _ := os.ReadFile(transcript)

	unhide := claudeTranscripts{projects: projects}.unhide
	for range 2 { // a second turn finds nothing left to change
		if err := unhide(context.Background(), id); err != nil {
			t.Fatalf("unhide() error = %v", err)
		}
	}

	after, _ := os.ReadFile(transcript)
	if len(after) != len(before) {
		t.Fatalf("transcript length %d, want unchanged %d", len(after), len(before))
	}
	if got := bytes.Count(after, []byte(`"entrypoint":"cli"   ,`)); got != 1 {
		t.Fatalf("relabelled entrypoints = %d, want the first one only:\n%s", got, after)
	}
	if !bytes.Contains(after, []byte(`"entrypoint":"sdk-ts","sessionId`)) || !bytes.Contains(after, []byte(`\"entrypoint\":\"sdk-ts\"`)) {
		t.Fatalf("later rows or escaped text changed:\n%s", after)
	}
	for _, line := range bytes.Split(bytes.TrimSpace(after), []byte("\n")) {
		if !json.Valid(line) {
			t.Fatalf("invalid JSON line after relabel: %s", line)
		}
	}
	if got, _ := os.ReadFile(other); string(got) != `{"entrypoint":"sdk-cli"}`+"\n" {
		t.Fatalf("another chat's transcript changed: %s", got)
	}
}

func TestClaudeUnhideRejectsAPathAsSessionID(t *testing.T) {
	if err := (claudeTranscripts{projects: t.TempDir()}).unhide(context.Background(), "../x"); err == nil {
		t.Fatal("unhide(../x) error = nil, want an invalid id")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

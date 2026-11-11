package harness

import (
	"maps"
	"testing"
)

func TestRegistryReturnsTheProfilesLaunchEnv(t *testing.T) {
	registry := New(Profile{ID: "claude-acp", LaunchEnv: map[string]string{"CLAUDE_CODE_ENTRYPOINT": "cli"}})

	got := registry.LaunchEnv("claude-acp")
	want := map[string]string{"CLAUDE_CODE_ENTRYPOINT": "cli"}
	if !maps.Equal(got, want) {
		t.Fatalf("LaunchEnv(claude-acp) = %v, want %v", got, want)
	}
	if got := registry.LaunchEnv("mock-acp"); got != nil {
		t.Fatalf("LaunchEnv(mock-acp) = %v, want nil", got)
	}
}

func TestRegistryLaunchEnvIsACopy(t *testing.T) {
	registry := Default()
	env := registry.LaunchEnv("claude-acp")
	if env == nil {
		t.Fatal("LaunchEnv(claude-acp) = nil, want the Claude profile")
	}
	env["CLAUDE_CODE_ENTRYPOINT"] = "mutated"

	if got := registry.LaunchEnv("claude-acp")["CLAUDE_CODE_ENTRYPOINT"]; got != "cli" {
		t.Fatalf("after mutating a returned map, LaunchEnv = %q, want cli", got)
	}
}

func TestNilRegistryServesNoOverrides(t *testing.T) {
	var registry *Registry
	if got := registry.LaunchEnv("claude-acp"); got != nil {
		t.Fatalf("nil Registry LaunchEnv = %v, want nil", got)
	}
}

func TestDefaultTagsClaudeSessionsAsCLI(t *testing.T) {
	if got := Default().LaunchEnv("claude-acp")["CLAUDE_CODE_ENTRYPOINT"]; got != "cli" {
		t.Fatalf("Default claude-acp CLAUDE_CODE_ENTRYPOINT = %q, want cli", got)
	}
}

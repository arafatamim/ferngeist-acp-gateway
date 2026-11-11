package harness

// Claude is the Claude Agent adapter.
//
// Claude Code records the SDK entrypoint on every transcript row, and its
// /resume picker filters on it. The Agent SDK only defaults
// CLAUDE_CODE_ENTRYPOINT when it is unset, so "cli" keeps chats the gateway
// creates visible in the native CLI.
// ponytail: drop this when the adapter stops writing sdk-* rows.
func Claude() Profile {
	return Profile{
		ID:        "claude-acp",
		LaunchEnv: map[string]string{"CLAUDE_CODE_ENTRYPOINT": "cli"},
	}
}

package harness

import (
	"path/filepath"
	"runtime"
	"strings"
)

// Resolve binds the default profile for agentID to the launched command's
// working directory and effective environment.
func Resolve(agentID, workingDir string, env []string) Profile {
	switch agentID {
	case "claude-acp":
		return claudeAt(storeDir(workingDir, env, "CLAUDE_CONFIG_DIR", ".claude"))
	case "hermes":
		return hermesAt(storeDir(workingDir, env, "HERMES_HOME", ".hermes"))
	default:
		return Profile{}
	}
}

func storeDir(workingDir string, env []string, key, defaultName string) string {
	dir := envValue(env, key)
	if dir == "" {
		homeKey := "HOME"
		if runtime.GOOS == "windows" {
			homeKey = "USERPROFILE"
		}
		home := envValue(env, homeKey)
		if home == "" {
			return ""
		}
		dir = filepath.Join(home, defaultName)
	}
	if filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Join(workingDir, dir)
}

func envValue(env []string, key string) string {
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if ok && (name == key || runtime.GOOS == "windows" && strings.EqualFold(name, key)) {
			return value
		}
	}
	return ""
}

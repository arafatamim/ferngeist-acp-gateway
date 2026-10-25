package projects

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/catalog"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
)

// newMockLister builds the mock stdio agent into a temp base dir, where the
// catalog detects it as mock-acp, and returns a factory of Listers over it,
// each with its own cache and warm processes.
func newMockLister(t *testing.T) func() *Lister {
	t.Helper()
	baseDir := t.TempDir()
	t.Setenv("FERNGEIST_MOCK_LIST_ROOT", baseDir)
	mustMkdir(t, filepath.Join(baseDir, "project"))
	mustMkdir(t, filepath.Join(baseDir, "bulk"))
	binary := "mock-stdio-agent"
	if os.PathSeparator == '\\' {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-o", filepath.Join(baseDir, "bin", binary), "./cmd/mock-stdio-agent")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build mock agent: %v: %s", err, out)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return func() *Lister {
		lister := New(catalog.New(baseDir, nil), runtime.NewSupervisor(logger, baseDir, nil, nil), nil)
		t.Cleanup(lister.Close)
		return lister
	}
}

func TestListMapsEachAgentAnswerToItsStatus(t *testing.T) {
	newLister := newMockLister(t)
	cases := []struct {
		name, mode, agentID, status string
		chats                       int
	}{
		{"lists every chat", "", "mock-acp", StatusOK, 2},
		{"follows nextCursor", "paged", "mock-acp", StatusOK, 2},
		{"auth error is authRequired", "auth", "mock-acp", StatusAuthRequired, 0},
		{"no list capability is unsupported", "none", "mock-acp", StatusUnsupported, 0},
		{"unknown agent fails", "", "no-such-agent", StatusFailed, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FERNGEIST_MOCK_LIST", tc.mode)
			var lines []AgentLine
			newLister().List(context.Background(), []string{tc.agentID}, func(line AgentLine) { lines = append(lines, line) })
			if len(lines) != 1 {
				t.Fatalf("got %d lines, want 1", len(lines))
			}
			line := lines[0]
			if line.Status != tc.status || len(line.Chats) != tc.chats {
				t.Fatalf("status = %q with %d chats (error %v), want %q with %d", line.Status, len(line.Chats), line.Error, tc.status, tc.chats)
			}
			if (line.Error == nil) != (tc.status == StatusOK) {
				t.Errorf("error = %v for status %q", line.Error, line.Status)
			}
		})
	}
}

func TestListPassesTheAgentsSessionCapabilitiesThrough(t *testing.T) {
	newLister := newMockLister(t)
	for _, advertised := range []bool{false, true} {
		if advertised {
			t.Setenv("FERNGEIST_MOCK_DELETE", "1")
		}
		var lines []AgentLine
		newLister().List(context.Background(), []string{"mock-acp"}, func(line AgentLine) { lines = append(lines, line) })
		if len(lines) != 1 {
			t.Fatalf("got %d lines, want 1", len(lines))
		}
		var caps map[string]json.RawMessage
		if err := json.Unmarshal(lines[0].SessionCapabilities, &caps); err != nil {
			t.Fatalf("sessionCapabilities %s: %v", lines[0].SessionCapabilities, err)
		}
		if _, got := caps["delete"]; got != advertised {
			t.Fatalf("advertised %v: sessionCapabilities = %s", advertised, lines[0].SessionCapabilities)
		}
	}
}

func TestListServesARepeatFetchFromCacheWithoutAskingTheAgent(t *testing.T) {
	lister := newMockLister(t)()
	var first, second AgentLine
	lister.List(context.Background(), []string{"mock-acp"}, func(line AgentLine) { first = line })
	// A fresh process would now answer auth-required; the cached line must win.
	t.Setenv("FERNGEIST_MOCK_LIST", "auth")
	lister.List(context.Background(), []string{"mock-acp"}, func(line AgentLine) { second = line })
	if first.Status != StatusOK || second.Status != StatusOK || len(second.Chats) != len(first.Chats) {
		t.Fatalf("first = %q, second = %q with %d chats, want the cached ok line", first.Status, second.Status, len(second.Chats))
	}
}

func TestListPlacesChatOutsideGitInItsOwnCwd(t *testing.T) {
	lister := newMockLister(t)()
	var chat Chat
	lister.List(context.Background(), []string{"mock-acp"}, func(line AgentLine) {
		if len(line.Chats) != 1 && len(line.Chats) != 2 {
			t.Errorf("got %d chats, want listed chats", len(line.Chats))
			return
		}
		chat = line.Chats[0]
	})
	if chat.SessionID != "mock_sess_1" || chat.Repo != chat.Cwd || chat.Worktree != nil {
		t.Fatalf("chat = %+v, want mock_sess_1 grouped under its own cwd with no worktree", chat)
	}
}

func TestListRechecksCachedChatsAfterTheirFolderIsRemoved(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "project")
	mustMkdir(t, cwd)
	lister := New(nil, nil, nil)
	lister.cached["agent"] = cachedLine{at: time.Now(), line: AgentLine{
		Type: "agent", AgentID: "agent", Status: StatusOK,
		Chats: []Chat{{SessionID: "chat", Cwd: cwd, Repo: cwd}},
	}}
	var line AgentLine
	lister.List(context.Background(), []string{"agent"}, func(got AgentLine) { line = got })
	if len(line.Chats) != 1 {
		t.Fatalf("existing folder: got %d chats, want 1", len(line.Chats))
	}
	if err := os.Remove(cwd); err != nil {
		t.Fatal(err)
	}
	lister.List(context.Background(), []string{"agent"}, func(got AgentLine) { line = got })
	if len(line.Chats) != 0 {
		t.Fatalf("deleted folder: got %d chats, want 0", len(line.Chats))
	}
}

func TestListResolvesCachedChatAgainWhenCheckoutChanges(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "project")
	mustMkdir(t, cwd)
	lister := New(nil, nil, nil)
	lister.cached["agent"] = cachedLine{at: time.Now(), line: AgentLine{
		Type: "agent", AgentID: "agent", Status: StatusOK,
		Chats: []Chat{{SessionID: "chat", Cwd: cwd, Repo: cwd}},
	}}
	var line AgentLine
	lister.List(context.Background(), []string{"agent"}, func(got AgentLine) { line = got })
	cmd := exec.Command("git", "-C", root, "init", "-q", "-b", "main")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	lister.List(context.Background(), []string{"agent"}, func(got AgentLine) { line = got })
	if len(line.Chats) != 1 || line.Chats[0].Repo != root {
		t.Fatalf("new checkout: got %+v, want repo %s", line.Chats, root)
	}
}

func connectListing(t *testing.T, lister *Lister) *agentConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	agent, err := lister.connect(ctx, "mock-acp")
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

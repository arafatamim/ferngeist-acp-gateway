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

	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/workspace"
)

func TestCheckoutsListsAChatRepoWithItsBranchAndWorktreesAndSkipsPathsOutsideGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	mustMkdir(t, repo)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "commit.gpgSign", "false")
	git("commit", "-q", "--allow-empty", "-m", "init")
	linked := filepath.Join(repo, ".worktrees", "email")
	git("worktree", "add", "-q", "-b", "email", linked)
	plain := filepath.Join(root, "plain")
	mustMkdir(t, plain)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ws := workspace.New(func() *storage.SQLiteStore { return nil }, logger, time.Now, nil)

	line := New(nil, nil, ws).Checkouts(context.Background(), []string{plain, repo})

	if line.Type != "checkouts" || len(line.Repos) != 1 {
		t.Fatalf("line = %+v, want one checkout", line)
	}
	got := line.Repos[0]
	if got.Repo != repo || got.Branch == nil || *got.Branch != "main" {
		t.Fatalf("checkout = %+v, want %s on main", got, repo)
	}
	if len(got.Worktrees) != 1 || got.Worktrees[0].Path != linked || got.Worktrees[0].Branch != "email" || got.Worktrees[0].ID == "" {
		t.Fatalf("worktrees = %+v, want %s on email with an id", got.Worktrees, linked)
	}
}

func TestCheckoutsReportsConfirmedMissingRepoAfterAgentListingFails(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	mustMkdir(t, repo)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ws := workspace.New(func() *storage.SQLiteStore { return nil }, logger, time.Now, nil)
	lister := New(nil, nil, ws)
	lister.cached["agent"] = cachedLine{at: time.Now(), line: AgentLine{
		Type: "agent", AgentID: "agent", Status: StatusOK,
		Chats: []Chat{{SessionID: "chat", Cwd: repo, Repo: repo}},
	}}
	lister.List(context.Background(), []string{"agent"}, func(AgentLine) {})
	if err := os.Remove(repo); err != nil {
		t.Fatal(err)
	}
	lister.cached["agent"] = cachedLine{at: time.Now(), line: AgentLine{
		Type: "agent", AgentID: "agent", Status: StatusFailed, Chats: []Chat{},
	}}
	lister.List(context.Background(), []string{"agent"}, func(AgentLine) {})
	line := lister.Checkouts(context.Background(), nil)
	encoded, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		MissingRepos []string `json:"missingRepos"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.MissingRepos) != 1 || wire.MissingRepos[0] != repo || len(line.Repos) != 0 {
		t.Fatalf("checkouts = %s, want confirmed removal %s and no repos", encoded, repo)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	encoded, err = json.Marshal(lister.Checkouts(ctx, nil))
	if err != nil {
		t.Fatal(err)
	}
	wire.MissingRepos = nil
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.MissingRepos) != 0 {
		t.Fatalf("canceled listing must not confirm deletion: %s", encoded)
	}
}

func TestCheckoutsDoesNotTreatPermissionDeniedAsMissing(t *testing.T) {
	parent := t.TempDir()
	repo := filepath.Join(parent, "repo")
	mustMkdir(t, repo)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ws := workspace.New(func() *storage.SQLiteStore { return nil }, logger, time.Now, nil)
	lister := New(nil, nil, ws)
	if err := os.Chmod(parent, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(parent, 0o755); err != nil {
			t.Error(err)
		}
	})
	if _, err := os.Stat(repo); !os.IsPermission(err) {
		t.Skip("filesystem does not deny access to the test process")
	}
	line := lister.Checkouts(context.Background(), []string{repo})
	if len(line.MissingRepos) != 0 {
		t.Fatalf("permission denied must not remove snapshots: %+v", line)
	}
}

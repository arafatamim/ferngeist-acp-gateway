package workspace

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

// initMainRepo builds a git repo with one commit on branch main and reports
// the commit sha.
func initMainRepo(t *testing.T) (repo, c1 string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo = t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run(repo, "init", "-q")
	run(repo, "checkout", "-qb", "main")
	run(repo, "config", "user.email", "t@t")
	run(repo, "config", "user.name", "t")
	run(repo, "config", "commit.gpgSign", "false")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(repo, "add", "a.txt")
	run(repo, "commit", "-qm", "init")
	out, err := RunGit(context.Background(), repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return repo, strings.TrimSpace(out)
}

func newWorktreeTestService(t *testing.T) (*Service, func() *storage.SQLiteStore) {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "wt.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc := New(func() *storage.SQLiteStore { return store }, slog.Default(), time.Now)
	return svc, func() *storage.SQLiteStore { return store }
}

func commitFile(t *testing.T, dir, name, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("add", name)
	run("commit", "-qm", msg)
}

// TestBaseRefTracksRebasedBranch rebases a managed worktree's branch onto a
// newer base: BaseRef must follow the merge-base (the new base), not the fixed
// base commit, so the diff shows only the agent's changes.
func TestBaseRefTracksRebasedBranch(t *testing.T) {
	repo, c1 := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()

	rec, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/x"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rec.BaseRef != "main" {
		t.Fatalf("BaseRef = %q, want the branch the worktree was cut from", rec.BaseRef)
	}
	if got := svc.BaseRef(ctx, rec.Path); got != c1 {
		t.Fatalf("BaseRef before rebase = %q, want base commit %q", got, c1)
	}

	commitFile(t, rec.Path, "agent.txt", "work\n", "agent work")
	commitFile(t, repo, "upstream.txt", "up\n", "upstream")
	c2, err := RunGit(ctx, repo, "rev-parse", "main")
	if err != nil {
		t.Fatal(err)
	}
	c2 = strings.TrimSpace(c2)
	if _, err := RunGit(ctx, rec.Path, "rebase", "main"); err != nil {
		t.Fatalf("rebase: %v", err)
	}

	if got := svc.BaseRef(ctx, rec.Path); got != c2 {
		t.Fatalf("BaseRef after rebase = %q, want merge-base %q (old base %q)", got, c2, c1)
	}
}

// TestBaseRefFallsBackToBaseCommit covers rows without a usable base ref:
// pre-migration rows and rows whose base branch was deleted.
func TestBaseRefFallsBackToBaseCommit(t *testing.T) {
	repo, c1 := initMainRepo(t)
	svc, stores := newWorktreeTestService(t)
	ctx := context.Background()

	// A pre-migration row has no base ref.
	if err := stores().SaveWorktree(ctx, storage.WorktreeRecord{
		ID: "legacy", Repo: repo, Path: filepath.Join(repo, ".worktrees", "legacy"),
		Branch: "legacy", BaseCommit: c1, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(repo, ".worktrees", "legacy")
	if err := os.MkdirAll(legacyPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := svc.BaseRef(ctx, legacyPath); got != c1 {
		t.Fatalf("legacy BaseRef = %q, want base commit %q", got, c1)
	}

	// A worktree whose base branch was deleted falls back too.
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("branch", "temp-base", "main")
	rec, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "temp-base", Branch: "feat/gone"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	run("branch", "-D", "temp-base")
	if got := svc.BaseRef(ctx, rec.Path); got != rec.BaseCommit {
		t.Fatalf("deleted-base BaseRef = %q, want base commit %q", got, rec.BaseCommit)
	}
}

// TestCreateDefaultsBaseRefToCurrentBranch cuts a worktree off HEAD: the
// recorded ref is HEAD's branch, not the literal "HEAD" (which would
// merge-base to the worktree tip itself and hide every commit).
func TestCreateDefaultsBaseRefToCurrentBranch(t *testing.T) {
	repo, _ := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)
	rec, err := svc.Create(context.Background(), CreateRequest{Repo: repo, Branch: "feat/h"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rec.BaseRef != "main" {
		t.Fatalf("BaseRef = %q, want HEAD's branch main", rec.BaseRef)
	}
}

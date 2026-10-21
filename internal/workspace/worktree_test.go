package workspace

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
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
	svc := New(func() *storage.SQLiteStore { return store }, slog.Default(), time.Now, nil)
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

// TestLogBaseNamesAPlainCheckoutsBranchStart covers the checkout the gateway
// never created: the branch's own commit must still read as its own after the
// trunk has moved past it, which merge-base alone cannot answer.
func TestLogBaseNamesAPlainCheckoutsBranchStart(t *testing.T) {
	repo, c1 := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()

	runIn := func(dir string, args ...string) {
		t.Helper()
		if _, err := RunGit(ctx, dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}

	runIn(repo, "checkout", "-qb", "feat/x")
	commitFile(t, repo, "work.txt", "mine\n", "my work")

	if got := svc.LogBase(ctx, repo); got != c1 {
		t.Fatalf("LogBase = %q, want the branch start %q", got, c1)
	}

	// The trunk takes the branch's commit, then the branch is checked out
	// again: the start it names must not have moved.
	runIn(repo, "checkout", "-q", "main")
	commitFile(t, repo, "up.txt", "up\n", "upstream")
	runIn(repo, "merge", "-q", "--no-ff", "-m", "merge feat/x", "feat/x")
	runIn(repo, "checkout", "-q", "feat/x")

	if got := svc.LogBase(ctx, repo); got != c1 {
		t.Fatalf("LogBase after the trunk moved = %q, want %q", got, c1)
	}
	if n, err := RunGit(ctx, repo, "rev-list", "--count", c1+"..HEAD"); err != nil || strings.TrimSpace(n) != "1" {
		t.Fatalf("commits since the base = %q (err %v), want 1", n, err)
	}
}

// TestLogBaseFallsBackToMergeBaseWhenTheReflogForgets: a branch whose reflog no
// longer records its creation still names its trunk fork point while it has
// commits of its own, and stops claiming anything once the trunk has them.
func TestLogBaseFallsBackToMergeBaseWhenTheReflogForgets(t *testing.T) {
	repo, c1 := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()

	runIn := func(args ...string) {
		t.Helper()
		if _, err := RunGit(ctx, repo, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}

	runIn("checkout", "-qb", "feat/x")
	commitFile(t, repo, "work.txt", "mine\n", "my work")
	runIn("reflog", "expire", "--expire=now", "--all")

	if got := svc.LogBase(ctx, repo); got != c1 {
		t.Fatalf("LogBase without a reflog = %q, want the fork point %q", got, c1)
	}

	// Merged into the trunk and with no reflog to read, the branch's own
	// commits are not knowable: the trunk's history must not be claimed.
	runIn("checkout", "-q", "main")
	runIn("merge", "-q", "--no-ff", "-m", "merge feat/x", "feat/x")
	runIn("checkout", "-q", "feat/x")

	if got := svc.LogBase(ctx, repo); got != "HEAD" {
		t.Fatalf("LogBase for a merged branch without a reflog = %q, want HEAD", got)
	}
}

// TestLogBaseIsHeadOnTheTrunk: a checkout sitting on its own trunk has nothing
// of its own to mark.
func TestLogBaseIsHeadOnTheTrunk(t *testing.T) {
	repo, _ := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)

	if got := svc.LogBase(context.Background(), repo); got != "HEAD" {
		t.Fatalf("LogBase on the trunk = %q, want HEAD", got)
	}
}

// TestLogBasePrefersAManagedWorktreesBase: worktrees this gateway created keep
// answering from their record, base ref and all.
func TestLogBasePrefersAManagedWorktreesBase(t *testing.T) {
	repo, c1 := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()

	rec, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/x"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	commitFile(t, rec.Path, "agent.txt", "work\n", "agent work")

	if got := svc.LogBase(ctx, rec.Path); got != c1 {
		t.Fatalf("LogBase in a managed worktree = %q, want %q", got, c1)
	}
}

// TestDeleteKeepsWorktreeWhenStatusFails breaks the worktree's .git link so
// git status errors: a non-force delete must refuse rather than wipe the folder.
func TestDeleteKeepsWorktreeWhenStatusFails(t *testing.T) {
	repo, _ := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()
	rec, err := svc.Create(ctx, CreateRequest{Repo: repo, Branch: "feat/broken"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rec.Path, "unsaved.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rec.Path, ".git"), []byte("gitdir: /nonexistent\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Delete(ctx, rec.ID, false); statusOf(err) != 409 {
		t.Fatalf("Delete() error = %v, want 409", err)
	}
	if _, err := os.Stat(filepath.Join(rec.Path, "unsaved.txt")); err != nil {
		t.Fatalf("uncommitted file was removed: %v", err)
	}
}

// TestListCountsAheadFromMergeBase rebases a worktree onto a newer base: the
// list badge must count only the branch's own commits, as the diff view does.
func TestListCountsAheadFromMergeBase(t *testing.T) {
	repo, _ := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()

	rec, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/x"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	commitFile(t, rec.Path, "agent.txt", "work\n", "agent work")
	commitFile(t, repo, "upstream.txt", "up\n", "upstream")
	if _, err := RunGit(ctx, rec.Path, "rebase", "main"); err != nil {
		t.Fatalf("rebase: %v", err)
	}

	statuses, err := svc.List(ctx)
	if err != nil || len(statuses) != 1 {
		t.Fatalf("List = %v, %v", statuses, err)
	}
	if got := statuses[0].Ahead; got == nil || *got != 1 {
		t.Fatalf("Ahead after rebase = %v, want 1", got)
	}
}

// TestGet returns one worktree with its status, and 404s for an unknown id or
// a folder deleted behind the gateway's back, as the list hides it.
func TestGet(t *testing.T) {
	repo, _ := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()
	rec, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/x"})
	if err != nil {
		t.Fatal(err)
	}
	commitFile(t, rec.Path, "agent.txt", "work\n", "agent work")

	got, err := svc.Get(ctx, rec.ID)
	if err != nil || got.ID != rec.ID || got.Ahead == nil || *got.Ahead != 1 || got.Dirty == nil || *got.Dirty {
		t.Fatalf("Get = %+v, %v; want feat/x one ahead and clean", got, err)
	}
	_, err = svc.Get(ctx, "nope")
	wantWorkspaceStatus(t, err, 404)
	if err := os.RemoveAll(rec.Path); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Get(ctx, rec.ID)
	wantWorkspaceStatus(t, err, 404)
}

// TestListReportsRunningChats lists every chat working in a worktree, one in a
// subdirectory included, and none for a worktree no chat uses.
func TestListReportsRunningChats(t *testing.T) {
	repo, _ := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()
	used, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/x"})
	if err != nil {
		t.Fatal(err)
	}
	idle, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/y"})
	if err != nil {
		t.Fatal(err)
	}
	svc.chatUsing = func(match func(string) bool) []string {
		var ids []string
		if match(used.Path) {
			ids = append(ids, "ses_a")
		}
		if match(filepath.Join(used.Path, "sub")) {
			ids = append(ids, "ses_b")
		}
		return ids
	}

	statuses, err := svc.List(ctx)
	if err != nil || len(statuses) != 2 {
		t.Fatalf("List = %v, %v", statuses, err)
	}
	for _, st := range statuses {
		switch st.ID {
		case used.ID:
			if len(st.Chats) != 2 {
				t.Errorf("used Chats = %v, want 2", st.Chats)
			}
		case idle.ID:
			if len(st.Chats) != 0 {
				t.Errorf("idle Chats = %v, want none", st.Chats)
			}
		}
	}
}

func TestRebaseOntoMovedBase(t *testing.T) {
	repo, _ := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()
	rec, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/x"})
	if err != nil {
		t.Fatal(err)
	}
	commitFile(t, rec.Path, "agent.txt", "work\n", "agent work")
	commitFile(t, repo, "upstream.txt", "up\n", "upstream")

	status, err := svc.Rebase(ctx, rec.ID)
	if err != nil {
		t.Fatalf("Rebase: %v", err)
	}
	if status.Ahead == nil || *status.Ahead != 1 {
		t.Fatalf("Ahead = %v, want 1", status.Ahead)
	}
	if _, err := os.Stat(filepath.Join(rec.Path, "upstream.txt")); err != nil {
		t.Fatalf("upstream commit not in the worktree after rebase: %v", err)
	}
}

func TestRebaseRefusesDirtyAndAbortsConflicts(t *testing.T) {
	repo, _ := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()
	rec, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/x"})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus := func(err error, want int) {
		t.Helper()
		var we Error
		if !errors.As(err, &we) || we.StatusCode() != want {
			t.Fatalf("err = %v, want status %d", err, want)
		}
	}

	if _, err := svc.Rebase(ctx, "nope"); err == nil {
		t.Fatal("unknown worktree rebased")
	} else {
		wantStatus(err, 404)
	}

	if err := os.WriteFile(filepath.Join(rec.Path, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Rebase(ctx, rec.ID)
	wantStatus(err, 409)
	if err := os.Remove(filepath.Join(rec.Path, "wip.txt")); err != nil {
		t.Fatal(err)
	}

	commitFile(t, rec.Path, "a.txt", "mine\n", "agent edit")
	commitFile(t, repo, "a.txt", "theirs\n", "upstream edit")
	before, _ := RunGit(ctx, rec.Path, "rev-parse", "HEAD")
	_, err = svc.Rebase(ctx, rec.ID)
	wantStatus(err, 409)
	if after, _ := RunGit(ctx, rec.Path, "rev-parse", "HEAD"); after != before {
		t.Fatalf("HEAD moved by a conflicted rebase: %s -> %s", before, after)
	}
	if st, _ := RunGit(ctx, rec.Path, "status", "--porcelain"); strings.TrimSpace(st) != "" {
		t.Fatalf("worktree left dirty after the abort: %q", st)
	}
}

// chatsAt fakes the session service: the given chats run in dir.
func chatsAt(dir string, ids ...string) func(match func(string) bool) []string {
	return func(match func(string) bool) []string {
		if match(dir) {
			return ids
		}
		return nil
	}
}

func wantWorkspaceStatus(t *testing.T, err error, want int) {
	t.Helper()
	var we Error
	if !errors.As(err, &we) || we.StatusCode() != want {
		t.Fatalf("err = %v, want status %d", err, want)
	}
}

func TestMergeFastForwardsAndDeletes(t *testing.T) {
	repo, _ := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()
	rec, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Merge(ctx, rec.ID, MergeRequest{}); err == nil {
		t.Fatal("merged a branch with no commits")
	} else {
		wantWorkspaceStatus(t, err, 409)
	}
	commitFile(t, rec.Path, "agent.txt", "work\n", "agent work")

	res, err := svc.Merge(ctx, rec.ID, MergeRequest{Delete: true})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	head, _ := RunGit(ctx, repo, "rev-parse", "HEAD")
	if res.Commit != strings.TrimSpace(head) || !res.WorktreeDeleted || !res.BranchDeleted {
		t.Fatalf("result = %+v, head %s", res, head)
	}
	if _, err := os.Stat(filepath.Join(repo, "agent.txt")); err != nil {
		t.Fatalf("merged file missing from the main checkout: %v", err)
	}
	if _, err := os.Stat(rec.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree folder survived: %v", err)
	}
}

func TestMergeFastForwardRefusesAMovedBase(t *testing.T) {
	repo, _ := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()
	rec, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/x"})
	if err != nil {
		t.Fatal(err)
	}
	commitFile(t, rec.Path, "agent.txt", "work\n", "agent work")
	commitFile(t, repo, "upstream.txt", "up\n", "upstream")
	before, _ := RunGit(ctx, repo, "rev-parse", "HEAD")

	_, err = svc.Merge(ctx, rec.ID, MergeRequest{})
	wantWorkspaceStatus(t, err, 409)
	if after, _ := RunGit(ctx, repo, "rev-parse", "HEAD"); after != before {
		t.Fatalf("base moved by a refused merge: %s -> %s", before, after)
	}
}

func TestMergeSquashLandsOneCommitAndDropsTheBranch(t *testing.T) {
	repo, _ := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()
	rec, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/x"})
	if err != nil {
		t.Fatal(err)
	}
	commitFile(t, rec.Path, "one.txt", "1\n", "one")
	commitFile(t, rec.Path, "two.txt", "2\n", "two")
	commitFile(t, repo, "upstream.txt", "up\n", "upstream")

	res, err := svc.Merge(ctx, rec.ID, MergeRequest{Squash: true, Message: "land x", Delete: true})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !res.WorktreeDeleted || !res.BranchDeleted {
		t.Fatalf("result = %+v", res)
	}
	subjects, _ := RunGit(ctx, repo, "log", "--format=%s", "-3")
	if !strings.HasPrefix(subjects, "land x\nupstream\n") {
		t.Fatalf("log = %q, want squash commit on top of upstream", subjects)
	}
	if _, err := RunGit(ctx, repo, "show-ref", "--verify", "--quiet", "refs/heads/feat/x"); err == nil {
		t.Fatal("squashed branch survived")
	}
}

func TestMergeSquashConflictIsUndone(t *testing.T) {
	repo, _ := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()
	rec, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/x"})
	if err != nil {
		t.Fatal(err)
	}
	commitFile(t, rec.Path, "a.txt", "mine\n", "agent edit")
	commitFile(t, repo, "a.txt", "theirs\n", "upstream edit")
	before, _ := RunGit(ctx, repo, "rev-parse", "HEAD")

	_, err = svc.Merge(ctx, rec.ID, MergeRequest{Squash: true})
	wantWorkspaceStatus(t, err, 409)
	if after, _ := RunGit(ctx, repo, "rev-parse", "HEAD"); after != before {
		t.Fatalf("base moved by a conflicted squash: %s -> %s", before, after)
	}
	if st, _ := RunGit(ctx, repo, "status", "--porcelain", "--untracked-files=no"); strings.TrimSpace(st) != "" {
		t.Fatalf("main checkout left dirty: %q", st)
	}
}

func TestMergeRefusesWhenTheMainCheckoutIsOnAnotherBranchOrDirty(t *testing.T) {
	repo, _ := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()
	rec, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/x"})
	if err != nil {
		t.Fatal(err)
	}
	commitFile(t, rec.Path, "agent.txt", "work\n", "agent work")

	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Merge(ctx, rec.ID, MergeRequest{})
	wantWorkspaceStatus(t, err, 409)
	if _, err := RunGit(ctx, repo, "checkout", "--", "a.txt"); err != nil {
		t.Fatal(err)
	}

	if _, err := RunGit(ctx, repo, "checkout", "-b", "other"); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Merge(ctx, rec.ID, MergeRequest{})
	wantWorkspaceStatus(t, err, 409)
}

func TestMergeDeleteRefusesWhileAChatUsesTheWorktree(t *testing.T) {
	repo, _ := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()
	rec, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/x"})
	if err != nil {
		t.Fatal(err)
	}
	commitFile(t, rec.Path, "agent.txt", "work\n", "agent work")
	svc.chatUsing = chatsAt(rec.Path, "ses_1")
	before, _ := RunGit(ctx, repo, "rev-parse", "HEAD")

	_, err = svc.Merge(ctx, rec.ID, MergeRequest{Delete: true})
	wantWorkspaceStatus(t, err, 409)
	if after, _ := RunGit(ctx, repo, "rev-parse", "HEAD"); after != before {
		t.Fatal("merge landed although its cleanup was refused")
	}
}

// addBareOrigin points repo's origin at a fresh bare repository and returns it.
func addBareOrigin(t *testing.T, repo string) string {
	t.Helper()
	bare := t.TempDir()
	for _, args := range [][]string{{"-C", bare, "init", "-q", "--bare"}, {"-C", repo, "remote", "add", "origin", bare}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return bare
}

func TestPushPublishesTheBranchAndNeverOverwritesOrigin(t *testing.T) {
	repo, _ := initMainRepo(t)
	bare := addBareOrigin(t, repo)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()
	rec, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/x"})
	if err != nil {
		t.Fatal(err)
	}
	commitFile(t, rec.Path, "agent.txt", "work\n", "agent work")

	if _, err := svc.Push(ctx, rec.ID, PushRequest{}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	local, _ := RunGit(ctx, rec.Path, "rev-parse", "HEAD")
	if remote, _ := RunGit(ctx, bare, "rev-parse", "refs/heads/feat/x"); remote != local {
		t.Fatalf("origin feat/x = %s, want %s", remote, local)
	}
	if up, _ := RunGit(ctx, rec.Path, "rev-parse", "--abbrev-ref", "@{upstream}"); strings.TrimSpace(up) != "origin/feat/x" {
		t.Fatalf("upstream = %q", up)
	}

	// Rewriting history makes the next push a non-fast-forward: it must be refused.
	if _, err := RunGit(ctx, rec.Path, "commit", "--amend", "-qm", "rewritten"); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Push(ctx, rec.ID, PushRequest{})
	wantWorkspaceStatus(t, err, 409)
	if remote, _ := RunGit(ctx, bare, "rev-parse", "refs/heads/feat/x"); remote != local {
		t.Fatalf("origin was overwritten: %s", remote)
	}
}

func TestPushRefusesADirtyWorktreeAndAMissingOrigin(t *testing.T) {
	repo, _ := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()
	rec, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/x"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Push(ctx, rec.ID, PushRequest{})
	wantWorkspaceStatus(t, err, 422) // no origin remote

	if err := os.WriteFile(filepath.Join(rec.Path, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Push(ctx, rec.ID, PushRequest{})
	wantWorkspaceStatus(t, err, 409)
}

func TestPushOpensAPullRequestWithGh(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake gh is a shell script")
	}
	repo, _ := initMainRepo(t)
	addBareOrigin(t, repo)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()
	rec, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/x"})
	if err != nil {
		t.Fatal(err)
	}
	commitFile(t, rec.Path, "agent.txt", "work\n", "agent work")

	bin := t.TempDir()
	argsFile := filepath.Join(bin, "args")
	script := "#!/bin/sh\necho \"$@\" > " + argsFile + "\necho 'Creating pull request'\necho https://example.test/pr/7\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	res, err := svc.Push(ctx, rec.ID, PushRequest{PR: true})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.PRURL != "https://example.test/pr/7" {
		t.Fatalf("PRURL = %q", res.PRURL)
	}
	if args, _ := os.ReadFile(argsFile); strings.TrimSpace(string(args)) != "pr create --head feat/x --base main --fill" {
		t.Fatalf("gh args = %q", args)
	}

	if _, err := svc.Push(ctx, rec.ID, PushRequest{PR: true, Title: "Add x", Body: "why"}); err != nil {
		t.Fatalf("Push with title: %v", err)
	}
	if args, _ := os.ReadFile(argsFile); strings.TrimSpace(string(args)) != "pr create --head feat/x --base main --title Add x --body why" {
		t.Fatalf("gh args = %q", args)
	}
}

func TestListReportsHowFarTheBaseMoved(t *testing.T) {
	repo, _ := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()
	rec, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/x"})
	if err != nil {
		t.Fatal(err)
	}
	commitFile(t, rec.Path, "agent.txt", "work\n", "agent work")
	commitFile(t, repo, "up1.txt", "1\n", "upstream one")
	commitFile(t, repo, "up2.txt", "2\n", "upstream two")

	counts := func() (ahead, behind int) {
		t.Helper()
		statuses, err := svc.List(ctx)
		if err != nil || len(statuses) != 1 || statuses[0].Ahead == nil || statuses[0].Behind == nil {
			t.Fatalf("List = %+v, %v", statuses, err)
		}
		return *statuses[0].Ahead, *statuses[0].Behind
	}
	if ahead, behind := counts(); ahead != 1 || behind != 2 {
		t.Fatalf("before rebase ahead/behind = %d/%d, want 1/2", ahead, behind)
	}
	if _, err := svc.Rebase(ctx, rec.ID); err != nil {
		t.Fatal(err)
	}
	if ahead, behind := counts(); ahead != 1 || behind != 0 {
		t.Fatalf("after rebase ahead/behind = %d/%d, want 1/0", ahead, behind)
	}
}

func TestListLeavesBehindWithoutABaseRef(t *testing.T) {
	repo, c1 := initMainRepo(t)
	svc, stores := newWorktreeTestService(t)
	ctx := context.Background()
	path := filepath.Join(repo, ".worktrees", "legacy")
	if _, err := RunGit(ctx, repo, "worktree", "add", "-b", "legacy", path, c1); err != nil {
		t.Fatal(err)
	}
	if err := stores().SaveWorktree(ctx, storage.WorktreeRecord{ID: "legacy", Repo: repo, Path: path, Branch: "legacy", BaseCommit: c1, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	statuses, err := svc.List(ctx)
	if err != nil || len(statuses) != 1 {
		t.Fatalf("List = %+v, %v", statuses, err)
	}
	if statuses[0].Behind != nil || statuses[0].Ahead == nil {
		t.Fatalf("ahead/behind = %v/%v, want ahead only", statuses[0].Ahead, statuses[0].Behind)
	}
}

func TestListChecksEveryWorktreeInOrder(t *testing.T) {
	repo, _ := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()
	const n = 3 * listConcurrency
	for i := range n {
		rec, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/" + strconv.Itoa(i)})
		if err != nil {
			t.Fatal(err)
		}
		// i commits on branch i, so a status filled into the wrong slot shows.
		for j := range i % 3 {
			commitFile(t, rec.Path, "f"+strconv.Itoa(j)+".txt", "x\n", "c")
		}
	}
	statuses, err := svc.List(ctx)
	if err != nil || len(statuses) != n {
		t.Fatalf("List returned %d, %v; want %d", len(statuses), err, n)
	}
	for i, st := range statuses {
		if want := "feat/" + strconv.Itoa(i); st.Branch != want {
			t.Fatalf("slot %d branch = %s, want %s", i, st.Branch, want)
		}
		if st.Ahead == nil || *st.Ahead != i%3 || st.Dirty == nil || *st.Dirty {
			t.Fatalf("%s status = %+v", st.Branch, st)
		}
	}
}

func TestListDoesNotForgetAVanishedWorktreeButPruneDoes(t *testing.T) {
	repo, _ := initMainRepo(t)
	svc, stores := newWorktreeTestService(t)
	ctx := context.Background()
	gone, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/gone"})
	if err != nil {
		t.Fatal(err)
	}
	kept, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/kept"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(gone.Path); err != nil {
		t.Fatal(err)
	}

	statuses, err := svc.List(ctx)
	if err != nil || len(statuses) != 1 || statuses[0].ID != kept.ID {
		t.Fatalf("List = %+v, %v; want only the surviving worktree", statuses, err)
	}
	if records, _ := stores().ListWorktrees(ctx); len(records) != 2 {
		t.Fatalf("a read forgot a worktree: %d records left", len(records))
	}

	svc.Prune(ctx)
	records, _ := stores().ListWorktrees(ctx)
	if len(records) != 1 || records[0].ID != kept.ID {
		t.Fatalf("records after Prune = %+v", records)
	}
	if out, _ := RunGit(ctx, repo, "worktree", "list", "--porcelain"); strings.Contains(out, "feat-gone") {
		t.Fatalf("git still lists the vanished worktree:\n%s", out)
	}
}

func TestCreatePrunesAVanishedWorktreeOfTheSameBranch(t *testing.T) {
	repo, _ := initMainRepo(t)
	svc, stores := newWorktreeTestService(t)
	ctx := context.Background()
	old, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(old.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/y"}); err != nil {
		t.Fatal(err)
	}
	records, _ := stores().ListWorktrees(ctx)
	if len(records) != 1 || records[0].Branch != "feat/y" {
		t.Fatalf("records = %+v, want the vanished one forgotten", records)
	}
}

// TestCreateCleansUpAfterAKilledWorktreeAdd stalls the checkout in a
// post-checkout hook past a short CreateTimeout: Create must fail with 422 and
// leave no folder, registration, branch or record behind.
func TestCreateCleansUpAfterAKilledWorktreeAdd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stalling hook is a shell script")
	}
	repo, _ := initMainRepo(t)
	svc, stores := newWorktreeTestService(t)
	ctx := context.Background()
	hook := filepath.Join(repo, ".git", "hooks", "post-checkout")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	defer func(old time.Duration) { CreateTimeout = old }(CreateTimeout)
	CreateTimeout = 500 * time.Millisecond

	start := time.Now()
	_, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/slow"})
	wantWorkspaceStatus(t, err, 422)
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("Create took %s; the timeout did not bound it", took)
	}
	if _, err := os.Stat(filepath.Join(repo, ".worktrees", "feat-slow")); !os.IsNotExist(err) {
		t.Fatalf("partial folder survived: %v", err)
	}
	if out, _ := RunGit(ctx, repo, "worktree", "list", "--porcelain"); strings.Contains(out, "feat-slow") {
		t.Fatalf("git still lists the worktree:\n%s", out)
	}
	if _, err := RunGit(ctx, repo, "show-ref", "--verify", "--quiet", "refs/heads/feat/slow"); err == nil {
		t.Fatal("branch survived the failed create")
	}
	if records, _ := stores().ListWorktrees(ctx); len(records) != 0 {
		t.Fatalf("records = %+v", records)
	}

	// The same branch can be created once the hook is gone.
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/slow"}); err != nil {
		t.Fatalf("retry after cleanup: %v", err)
	}
}

func TestUnmanagedWorktreesList(t *testing.T) {
	repo, _ := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()

	// Error cases
	if _, err := svc.Unmanaged(ctx, "relative/path"); err == nil {
		t.Fatal("expected error for relative path")
	} else {
		wantWorkspaceStatus(t, err, 400)
	}
	if _, err := svc.Unmanaged(ctx, t.TempDir()); err == nil {
		t.Fatal("expected error for non-git repo")
	} else {
		wantWorkspaceStatus(t, err, 422)
	}

	// Create an external worktree outside .worktrees
	extPath := filepath.Join(t.TempDir(), "external-wt")
	if _, err := RunGit(ctx, repo, "worktree", "add", "-b", "ext-feature", extPath, "main"); err != nil {
		t.Fatalf("git worktree add: %v", err)
	}

	// Create a gateway-managed worktree
	managed, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/managed"})
	if err != nil {
		t.Fatalf("svc.Create: %v", err)
	}

	unmanaged, err := svc.Unmanaged(ctx, repo)
	if err != nil {
		t.Fatalf("svc.Unmanaged: %v", err)
	}

	if len(unmanaged) != 1 {
		t.Fatalf("Unmanaged returned %d worktrees, want 1: %+v", len(unmanaged), unmanaged)
	}
	if unmanaged[0].Path != extPath {
		t.Fatalf("Unmanaged path = %q, want %q", unmanaged[0].Path, extPath)
	}
	if unmanaged[0].Branch != "ext-feature" {
		t.Fatalf("Unmanaged branch = %q, want ext-feature", unmanaged[0].Branch)
	}
	if unmanaged[0].Head == "" {
		t.Fatal("Unmanaged head is empty")
	}

	// Ensure main repo and managed worktree are not in the list
	for _, wt := range unmanaged {
		if wt.Path == repo {
			t.Fatalf("main repo %q found in unmanaged list", repo)
		}
		if wt.Path == managed.Path {
			t.Fatalf("managed worktree %q found in unmanaged list", managed.Path)
		}
	}
}

func TestAdoptWorktree(t *testing.T) {
	repo, c1 := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()

	// External worktree created outside gateway
	extPath := filepath.Join(t.TempDir(), "external-wt")
	if _, err := RunGit(ctx, repo, "worktree", "add", "-b", "feat/adopted", extPath, "main"); err != nil {
		t.Fatalf("git worktree add: %v", err)
	}

	// Add 1 commit to external worktree (ahead = 1)
	if err := os.WriteFile(filepath.Join(extPath, "ext.txt"), []byte("ext content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RunGit(ctx, extPath, "add", "ext.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := RunGit(ctx, extPath, "commit", "-m", "worktree commit"); err != nil {
		t.Fatal(err)
	}

	// Add 1 commit to main (behind = 1)
	if err := os.WriteFile(filepath.Join(repo, "main2.txt"), []byte("main content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RunGit(ctx, repo, "add", "main2.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := RunGit(ctx, repo, "commit", "-m", "main commit"); err != nil {
		t.Fatal(err)
	}

	// Add unstaged edit in external worktree (dirty = true)
	if err := os.WriteFile(filepath.Join(extPath, "ext.txt"), []byte("dirty content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Adopt without repo explicitly given (inferred from path)
	status, err := svc.Adopt(ctx, AdoptRequest{Path: extPath})
	if err != nil {
		t.Fatalf("svc.Adopt: %v", err)
	}

	if status.ID == "" {
		t.Fatal("Adopt returned empty ID")
	}
	if status.Repo != repo {
		t.Fatalf("status.Repo = %q, want %q", status.Repo, repo)
	}
	if status.Path != extPath {
		t.Fatalf("status.Path = %q, want %q", status.Path, extPath)
	}
	if status.Branch != "feat/adopted" {
		t.Fatalf("status.Branch = %q, want feat/adopted", status.Branch)
	}
	if status.BaseRef != "main" {
		t.Fatalf("status.BaseRef = %q, want main", status.BaseRef)
	}
	if status.BaseCommit != c1 {
		t.Fatalf("status.BaseCommit = %q, want %q", status.BaseCommit, c1)
	}
	if status.Ahead == nil || *status.Ahead != 1 {
		t.Fatalf("status.Ahead = %v, want 1", status.Ahead)
	}
	if status.Behind == nil || *status.Behind != 1 {
		t.Fatalf("status.Behind = %v, want 1", status.Behind)
	}
	if status.Dirty == nil || !*status.Dirty {
		t.Fatalf("status.Dirty = %v, want true", status.Dirty)
	}

	// Check BaseRef for base-aware diffs
	if baseRef := svc.BaseRef(ctx, extPath); baseRef != c1 {
		t.Fatalf("svc.BaseRef = %q, want %q", baseRef, c1)
	}

	// It must appear in List
	list, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("svc.List: %v", err)
	}
	if len(list) != 1 || list[0].ID != status.ID {
		t.Fatalf("svc.List = %+v, want adopted worktree %s", list, status.ID)
	}

	// It must be accessible via Get
	got, err := svc.Get(ctx, status.ID)
	if err != nil {
		t.Fatalf("svc.Get: %v", err)
	}
	if got.ID != status.ID {
		t.Fatalf("svc.Get = %+v, want %s", got, status.ID)
	}

	// Adopting again should 409
	_, err = svc.Adopt(ctx, AdoptRequest{Path: extPath})
	wantWorkspaceStatus(t, err, 409)
}

func TestAdoptRefusesBadInputAndChecksOutADetachedWorktree(t *testing.T) {
	repo, _ := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()

	// Relative path
	_, err := svc.Adopt(ctx, AdoptRequest{Path: "relative/path"})
	wantWorkspaceStatus(t, err, 400)

	// Non-existent path
	_, err = svc.Adopt(ctx, AdoptRequest{Path: filepath.Join(t.TempDir(), "non-existent")})
	wantWorkspaceStatus(t, err, 404)

	// Main checkout
	_, err = svc.Adopt(ctx, AdoptRequest{Path: repo})
	wantWorkspaceStatus(t, err, 400)

	// Path not a worktree of repo
	notWT := t.TempDir()
	_, err = svc.Adopt(ctx, AdoptRequest{Repo: repo, Path: notWT})
	wantWorkspaceStatus(t, err, 422)

	// Detached HEAD worktree
	extPath := filepath.Join(t.TempDir(), "detached-wt")
	if _, err := RunGit(ctx, repo, "worktree", "add", "--detach", extPath, "main"); err != nil {
		t.Fatalf("git worktree add --detach: %v", err)
	}

	// Detached without branch fails with 400
	_, err = svc.Adopt(ctx, AdoptRequest{Path: extPath})
	wantWorkspaceStatus(t, err, 400)

	// An option-like branch is refused before any checkout runs: "-f" would
	// otherwise reach `git checkout -f` and discard the uncommitted edit.
	if err := os.WriteFile(filepath.Join(extPath, "README.md"), []byte("unsaved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Adopt(ctx, AdoptRequest{Path: extPath, Branch: "-f"})
	wantWorkspaceStatus(t, err, 400)
	if got, _ := os.ReadFile(filepath.Join(extPath, "README.md")); string(got) != "unsaved\n" {
		t.Fatalf("uncommitted edit lost: %q", got)
	}

	// Detached with branch succeeds and checks out the branch
	status, err := svc.Adopt(ctx, AdoptRequest{Path: extPath, Branch: "feat/from-detached"})
	if err != nil {
		t.Fatalf("Adopt with branch: %v", err)
	}
	if status.Branch != "feat/from-detached" {
		t.Fatalf("status.Branch = %q, want feat/from-detached", status.Branch)
	}
}

func TestCreateFromRemoteBranchAndPR(t *testing.T) {
	repo, c1 := initMainRepo(t)
	addBareOrigin(t, repo)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()

	// Push a remote branch to bare origin
	if out, err := exec.Command("git", "-C", repo, "push", "-q", "origin", "main:refs/heads/feat-remote").CombinedOutput(); err != nil {
		t.Fatalf("push feat-remote: %v: %s", err, out)
	}

	// Push a PR ref to bare origin
	if out, err := exec.Command("git", "-C", repo, "push", "-q", "origin", "main:refs/pull/123/head").CombinedOutput(); err != nil {
		t.Fatalf("push pr/123: %v: %s", err, out)
	}

	// 1. Invalid base ref syntax returns 400
	_, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "--flag"})
	wantWorkspaceStatus(t, err, 400)

	_, err = svc.Create(ctx, CreateRequest{Repo: repo, Base: "bad..ref"})
	wantWorkspaceStatus(t, err, 400)

	_, err = svc.Create(ctx, CreateRequest{Repo: repo, Base: "bad~ref"})
	wantWorkspaceStatus(t, err, 400)

	// 2. Unknown remote branch and unknown PR return 422
	_, err = svc.Create(ctx, CreateRequest{Repo: repo, Base: "origin/non-existent"})
	wantWorkspaceStatus(t, err, 422)

	_, err = svc.Create(ctx, CreateRequest{Repo: repo, Base: "pr/999"})
	wantWorkspaceStatus(t, err, 422)

	// 3. Create from remote branch (origin/feat-remote)
	recRemote, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "origin/feat-remote", Branch: "review/feat-remote"})
	if err != nil {
		t.Fatalf("create from origin/feat-remote: %v", err)
	}
	if recRemote.BaseCommit != c1 {
		t.Fatalf("BaseCommit = %q, want %q", recRemote.BaseCommit, c1)
	}
	if recRemote.BaseRef != "origin/feat-remote" {
		t.Fatalf("BaseRef = %q, want origin/feat-remote", recRemote.BaseRef)
	}
	if recRemote.Branch != "review/feat-remote" {
		t.Fatalf("Branch = %q, want review/feat-remote", recRemote.Branch)
	}

	// Verify status of the remote worktree
	stRemote, err := svc.Get(ctx, recRemote.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stRemote.Ahead == nil || *stRemote.Ahead != 0 {
		t.Fatalf("Ahead = %v, want 0", stRemote.Ahead)
	}
	if stRemote.Behind == nil || *stRemote.Behind != 0 {
		t.Fatalf("Behind = %v, want 0", stRemote.Behind)
	}

	// 4. Create from PR number (pr/123)
	recPR, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "pr/123", Branch: "review/pr-123"})
	if err != nil {
		t.Fatalf("create from pr/123: %v", err)
	}
	if recPR.BaseCommit != c1 {
		t.Fatalf("BaseCommit = %q, want %q", recPR.BaseCommit, c1)
	}
	if recPR.BaseRef != "origin/pr/123" {
		t.Fatalf("BaseRef = %q, want origin/pr/123", recPR.BaseRef)
	}
	if recPR.Branch != "review/pr-123" {
		t.Fatalf("Branch = %q, want review/pr-123", recPR.Branch)
	}

	// Verify status of the PR worktree
	stPR, err := svc.Get(ctx, recPR.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stPR.Ahead == nil || *stPR.Ahead != 0 {
		t.Fatalf("Ahead = %v, want 0", stPR.Ahead)
	}
	if stPR.Behind == nil || *stPR.Behind != 0 {
		t.Fatalf("Behind = %v, want 0", stPR.Behind)
	}
}

func TestALocalBranchNamedLikeAPRIsUsedAsIs(t *testing.T) {
	repo, c1 := initMainRepo(t)
	if out, err := exec.Command("git", "-C", repo, "branch", "pr/7").CombinedOutput(); err != nil {
		t.Fatalf("branch pr/7: %v: %s", err, out)
	}
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()
	// With no origin, treating pr/7 as a pull request would fail the fetch.
	rec, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "pr/7", Branch: "on-pr7"})
	if err != nil {
		t.Fatalf("create from local pr/7: %v", err)
	}
	if rec.BaseRef != "pr/7" || rec.BaseCommit != c1 {
		t.Fatalf("base = %q %q, want pr/7 at %q", rec.BaseRef, rec.BaseCommit, c1)
	}
	if got := prBase(ctx, rec); got != "pr/7" {
		t.Fatalf("prBase = %q, want pr/7", got)
	}
}

func TestAdoptRefusesTheMainCheckoutNamedThroughALinkedWorktree(t *testing.T) {
	repo, _ := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()
	linked := filepath.Join(t.TempDir(), "linked")
	if _, err := RunGit(ctx, repo, "worktree", "add", "-b", "feat/linked", linked, "main"); err != nil {
		t.Fatal(err)
	}

	_, err := svc.Adopt(ctx, AdoptRequest{Repo: linked, Path: repo})
	wantWorkspaceStatus(t, err, 400)
	list, err := svc.Unmanaged(ctx, linked)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Path != linked {
		t.Fatalf("Unmanaged via linked worktree = %+v, want only %s", list, linked)
	}

	// The recorded repo is the main checkout, whichever checkout named it.
	status, err := svc.Adopt(ctx, AdoptRequest{Repo: linked, Path: linked})
	if err != nil {
		t.Fatal(err)
	}
	if status.Repo != repo {
		t.Fatalf("Repo = %q, want %q", status.Repo, repo)
	}
}

func TestAdoptRefusesABranchTheWorktreeIsNotOn(t *testing.T) {
	repo, _ := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()
	ext := filepath.Join(t.TempDir(), "ext")
	if _, err := RunGit(ctx, repo, "worktree", "add", "-b", "feat/real", ext, "main"); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Adopt(ctx, AdoptRequest{Path: ext, Branch: "feat/other"})
	wantWorkspaceStatus(t, err, 409)
	if _, err := svc.Adopt(ctx, AdoptRequest{Path: ext, Branch: "feat/real"}); err != nil {
		t.Fatalf("Adopt with its own branch: %v", err)
	}
}

func TestDeleteRefusesAMainCheckoutRecord(t *testing.T) {
	repo, _ := initMainRepo(t)
	svc, store := newWorktreeTestService(t)
	ctx := context.Background()
	// A record from before Adopt refused main checkouts.
	if err := store().SaveWorktree(ctx, storage.WorktreeRecord{ID: "main0001", Repo: repo, Path: repo, Branch: "main", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Delete(ctx, "main0001", true)
	wantWorkspaceStatus(t, err, 409)
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		t.Fatalf("repository was removed: %v", err)
	}
}

func TestPushReportsAHookRejectionAsAFailureNotADivergence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hook is a shell script")
	}
	repo, _ := initMainRepo(t)
	bare := addBareOrigin(t, repo)
	if err := os.WriteFile(filepath.Join(bare, "hooks", "pre-receive"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()
	rec, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/x"})
	if err != nil {
		t.Fatal(err)
	}
	commitFile(t, rec.Path, "agent.txt", "work\n", "agent work")
	_, err = svc.Push(ctx, rec.ID, PushRequest{})
	wantWorkspaceStatus(t, err, 422)
}

func TestPRBaseTargetsTheBranchOnOrigin(t *testing.T) {
	repo, _ := initMainRepo(t)
	ctx := context.Background()
	for base, want := range map[string]string{
		"":             "",
		"main":         "main",
		"no-such":      "",
		"origin/feat":  "feat",
		"origin/pr/12": "",
		"pr/12":        "",
	} {
		if got := prBase(ctx, storage.WorktreeRecord{Repo: repo, BaseRef: base}); got != want {
			t.Errorf("prBase(%q) = %q, want %q", base, got, want)
		}
	}
}

func TestConcurrentCreatesOfOneFolderLeaveTheWinnerIntact(t *testing.T) {
	repo, _ := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()
	// feat/x and feat-x share the folder .worktrees/feat-x.
	branches := []string{"feat/x", "feat-x"}
	recs := make([]storage.WorktreeRecord, len(branches))
	errs := make([]error, len(branches))
	var wg sync.WaitGroup
	for i, b := range branches {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recs[i], errs[i] = svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: b})
		}()
	}
	wg.Wait()
	won := -1
	for i, err := range errs {
		if err == nil {
			if won >= 0 {
				t.Fatal("both creates succeeded for one folder")
			}
			won = i
		} else {
			wantWorkspaceStatus(t, err, 409)
		}
	}
	if won < 0 {
		t.Fatalf("no create succeeded: %v", errs)
	}
	if _, err := os.Stat(filepath.Join(recs[won].Path, ".git")); err != nil {
		t.Fatalf("winner's worktree was removed: %v", err)
	}
	if _, err := RunGit(ctx, repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branches[won]); err != nil {
		t.Fatalf("winner's branch was deleted: %v", err)
	}
}

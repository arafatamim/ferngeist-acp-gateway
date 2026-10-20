package workspace

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	svc.chatUsing = func(match func(string) bool) (string, bool) { return "ses_1", match(rec.Path) }
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

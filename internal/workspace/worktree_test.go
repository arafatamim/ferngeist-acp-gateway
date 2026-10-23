package workspace

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

// gitT runs git in dir and fails the test on error.
func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := RunGit(context.Background(), dir, args...); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// initMainRepo builds a git repo with one commit on branch main and reports
// the commit sha.
func initMainRepo(t *testing.T) (repo, c1 string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo = t.TempDir()
	run := func(dir string, args ...string) { gitT(t, dir, args...) }
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
	run := func(args ...string) { gitT(t, dir, args...) }
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
	run := func(args ...string) { gitT(t, repo, args...) }
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

// TestBranchOfAnswersFromTheRecord covers the push-path lookup: the recorded
// branch of the worktree containing cwd, "" outside any managed worktree, and
// "" when the lookup fails.
func TestBranchOfAnswersFromTheRecord(t *testing.T) {
	svc, stores := newWorktreeTestService(t)
	ctx := context.Background()
	rec := storage.WorktreeRecord{
		ID: "wt1", Repo: "/r", Path: "/r/.worktrees/feat-x", Branch: "feat/x",
		BaseCommit: "abc123", CreatedAt: time.Now(),
	}
	if err := stores().SaveWorktree(ctx, rec); err != nil {
		t.Fatal(err)
	}
	for cwd, want := range map[string]string{
		"/r/.worktrees/feat-x":       "feat/x",
		"/r/.worktrees/feat-x/sub":   "feat/x",
		"/r":                         "",
		"/r/.worktrees/feat-x-other": "",
		"":                           "",
	} {
		if got := svc.BranchOf(ctx, cwd); got != want {
			t.Errorf("BranchOf(%q) = %q, want %q", cwd, got, want)
		}
	}

	// A lookup failure (here: no store bound) costs the branch, nothing else.
	broken := New(func() *storage.SQLiteStore { return nil }, slog.Default(), time.Now, nil)
	if got := broken.BranchOf(ctx, "/r/.worktrees/feat-x"); got != "" {
		t.Errorf("BranchOf on a failed lookup = %q, want empty", got)
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

	statuses, err := svc.List(ctx, "")
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

	statuses, err := svc.List(ctx, "")
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

// edit is one file change a table row makes: with a msg it is committed,
// without one it is left uncommitted on disk.
type edit struct {
	file    string
	content string
	msg     string
}

// mergeCase is one row of the merge table: the repo state to arrange for, the
// request to make, and the outcome that must follow.
type mergeCase struct {
	name       string
	trunk      []edit // commits made in the main checkout first
	wt         []edit // commits made in the managed worktree
	dirty      []edit // uncommitted writes left in the main checkout
	mainBranch string // moves the main checkout onto a fresh branch of its own
	chats      []string
	req        MergeRequest
	status     int      // 0 lands the merge; anything else is the refusal status
	subjects   []string // main checkout commit subjects from HEAD downwards
	clean      bool     // the main checkout's tracked status must come back empty
}

// TestMergeScenarios covers every merge refusal and every landing state the
// service promises; a refusal leaves the main checkout exactly as the setup
// left it. The table carries each scenario's state, request and expected
// outcome; the runner arranges the state per row and checks the aftermath.
func TestMergeScenarios(t *testing.T) {
	for _, tc := range []mergeCase{
		{name: "refuses a branch with no commits", req: MergeRequest{}, status: 409},
		{name: "fast-forward lands the commit and deletes the worktree and the branch",
			wt: []edit{{"agent.txt", "work\n", "agent work"}}, req: MergeRequest{Delete: true}},
		{name: "refuses a fast-forward with a moved base",
			wt:    []edit{{"agent.txt", "work\n", "agent work"}},
			trunk: []edit{{"upstream.txt", "up\n", "upstream"}}, status: 409},
		{name: "squash lands one commit on top of upstream and drops the branch",
			wt:       []edit{{"one.txt", "1\n", "one"}, {"two.txt", "2\n", "two"}},
			trunk:    []edit{{"upstream.txt", "up\n", "upstream"}},
			req:      MergeRequest{Squash: true, Message: "land x", Delete: true},
			subjects: []string{"land x", "upstream"}},
		{name: "a squashed conflict is undone without moving the base",
			wt:    []edit{{"a.txt", "mine\n", "agent edit"}},
			trunk: []edit{{"a.txt", "theirs\n", "upstream edit"}},
			req:   MergeRequest{Squash: true}, status: 409, clean: true},
		{name: "refuses a dirty main checkout",
			wt: []edit{{"agent.txt", "work\n", "agent work"}}, dirty: []edit{{"a.txt", "dirty\n", ""}},
			status: 409},
		{name: "refuses a main checkout on another branch",
			wt: []edit{{"agent.txt", "work\n", "agent work"}}, mainBranch: "other", status: 409},
		{name: "refuses to delete while a chat uses the worktree",
			wt: []edit{{"agent.txt", "work\n", "agent work"}}, chats: []string{"ses_1"},
			req: MergeRequest{Delete: true}, status: 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, _ := initMainRepo(t)
			svc, _ := newWorktreeTestService(t)
			ctx := context.Background()
			rec, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "feat/x"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			for _, c := range tc.trunk {
				commitFile(t, repo, c.file, c.content, c.msg)
			}
			for _, c := range tc.wt {
				commitFile(t, rec.Path, c.file, c.content, c.msg)
			}
			for _, f := range tc.dirty {
				if err := os.WriteFile(filepath.Join(repo, f.file), []byte(f.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.mainBranch != "" {
				if _, err := RunGit(ctx, repo, "checkout", "-b", tc.mainBranch); err != nil {
					t.Fatal(err)
				}
			}
			if len(tc.chats) > 0 {
				svc.chatUsing = chatsAt(rec.Path, tc.chats...)
			}
			before, _ := RunGit(ctx, repo, "rev-parse", "HEAD")

			res, err := svc.Merge(ctx, rec.ID, tc.req)
			if tc.status != 0 {
				if err == nil {
					t.Fatalf("Merge %+v succeeded, want status %d", tc.req, tc.status)
				}
				wantWorkspaceStatus(t, err, tc.status)
				if after, _ := RunGit(ctx, repo, "rev-parse", "HEAD"); after != before {
					t.Fatalf("base moved by a refused merge: %s -> %s", before, after)
				}
				if st, _ := RunGit(ctx, repo, "status", "--porcelain", "--untracked-files=no"); tc.clean && strings.TrimSpace(st) != "" {
					t.Fatalf("main checkout left dirty: %q", st)
				}
				return
			}
			if err != nil {
				t.Fatalf("Merge: %v", err)
			}
			if head, _ := RunGit(ctx, repo, "rev-parse", "HEAD"); res.Commit != strings.TrimSpace(head) {
				t.Fatalf("landed commit = %q, want %q", res.Commit, head)
			}
			if tc.req.Delete && (!res.WorktreeDeleted || !res.BranchDeleted) {
				t.Fatalf("result = %+v, want the worktree and branch deleted", res)
			}
			if _, err := RunGit(ctx, repo, "show-ref", "--verify", "--quiet", "refs/heads/feat/x"); err == nil {
				t.Fatal("branch survived the merge")
			}
			if _, stat := os.Stat(rec.Path); !os.IsNotExist(stat) {
				t.Fatalf("worktree folder survived: %v", stat)
			}
			for _, c := range tc.wt {
				if _, err := os.Stat(filepath.Join(repo, c.file)); err != nil {
					t.Fatalf("merged file %s missing from the main checkout: %v", c.file, err)
				}
			}
			if len(tc.subjects) > 0 {
				out, _ := RunGit(ctx, repo, "log", "--format=%s", "-"+strconv.Itoa(len(tc.subjects)))
				if want := strings.Join(tc.subjects, "\n") + "\n"; !strings.HasPrefix(out, want) {
					t.Fatalf("log = %q, want it to start with %q", out, want)
				}
			}
		})
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
		statuses, err := svc.List(ctx, "")
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
	statuses, err := svc.List(ctx, "")
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
	statuses, err := svc.List(ctx, "")
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

	statuses, err := svc.List(ctx, "")
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

// adoptCase is one row of the adoption table: the worktree the setup carves
// out of the repo, where the request points, and what must come back.
type adoptCase struct {
	name string
	// wtArgs is what the setup appends to `git worktree add` (after that,
	// before the path and "main"): "-b feat/…" branches the checkout,
	// "--detach" makes a detached one; nil carves out no worktree.
	wtArgs  []string
	wtFiles []edit // files written into the fresh worktree, uncommitted
	// repo, path select where the request points, resolved by the runner:
	// "repo", "worktree", "plain", "missing", "relative".
	repo, path string
	reqBranch  string
	// wantStatus 0 expects the adoption to succeed; anything else expects
	// that refusal and the worktree left untouched.
	wantStatus int
	branch     string // the adopted record's branch
	repoIsMain bool   // the adopted record must name the main checkout as repo
	keep       []edit // files a refusal must leave with their content
	// after checks side state a refusal must leave; it keeps the scenarios
	// whose extra assertions rode the hand-rolled tests.
	after func(t *testing.T, svc *Service, ctx context.Context, repo, wt string)
}

// TestAdoptInputValidation covers every adoption refusal and the success
// shapes its refusal tests carried. The table carries each scenario's
// state, request and expected outcome; the runner checks the aftermath.

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

func TestMergedFlagsWorkThatLandedOnTheBase(t *testing.T) {
	repo, _ := initMainRepo(t)
	svc, _ := newWorktreeTestService(t)
	ctx := context.Background()
	merged := func(rec storage.WorktreeRecord) bool {
		t.Helper()
		st, err := svc.Get(ctx, rec.ID)
		if err != nil || st.Merged == nil {
			t.Fatalf("Get = %+v, %v; want a merged flag", st, err)
		}
		return *st.Merged
	}
	fresh, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "fresh"})
	if err != nil {
		t.Fatal(err)
	}
	ff, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "ff"})
	if err != nil {
		t.Fatal(err)
	}
	squashed, err := svc.Create(ctx, CreateRequest{Repo: repo, Base: "main", Branch: "squashed"})
	if err != nil {
		t.Fatal(err)
	}
	commitFile(t, ff.Path, "ff.txt", "ff\n", "ff work")
	commitFile(t, squashed.Path, "one.txt", "1\n", "one")
	commitFile(t, squashed.Path, "two.txt", "2\n", "two")
	if merged(fresh) || merged(ff) || merged(squashed) {
		t.Fatal("flagged a worktree whose work has not landed")
	}

	if _, err := svc.Merge(ctx, ff.ID, MergeRequest{}); err != nil {
		t.Fatalf("merge ff: %v", err)
	}
	if _, err := svc.Merge(ctx, squashed.ID, MergeRequest{Squash: true}); err != nil {
		t.Fatalf("merge squashed: %v", err)
	}
	if !merged(ff) || !merged(squashed) {
		t.Fatal("missed a fast-forwarded or squashed branch")
	}
	// The base moved, but the fresh branch never had work of its own.
	if merged(fresh) {
		t.Fatal("flagged a branch with no commits of its own")
	}
}

// TestListShowsWorktreesMadeOutsideTheGateway finds a plain `git worktree add`
// through a running chat's cwd, measures it against the default branch, acts
// on it by id without adopting, and drops it once git no longer lists it.
func TestListShowsWorktreesMadeOutsideTheGateway(t *testing.T) {
	repo, c1 := initMainRepo(t)
	svc, stores := newWorktreeTestService(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "outside")
	if _, err := RunGit(ctx, repo, "worktree", "add", "-b", "feat/out", path, c1); err != nil {
		t.Fatal(err)
	}
	commitFile(t, path, "w.txt", "w\n", "work")

	if got, _ := svc.List(ctx, ""); len(got) != 0 {
		t.Fatalf("List with no known repo = %+v, want none", got)
	}
	svc.chatUsing = func(match func(string) bool) []string { match(repo); return nil }
	list, err := svc.List(ctx, "")
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %+v, %v; want the outside worktree only", list, err)
	}
	wt := list[0]
	if wt.Path != path || wt.Branch != "feat/out" || wt.Repo != repo || wt.Ahead == nil || *wt.Ahead != 1 || wt.Dirty == nil || *wt.Dirty || wt.CreatedAt.IsZero() {
		t.Fatalf("outside worktree = %+v", wt)
	}
	if narrowed, err := svc.List(ctx, path); err != nil || len(narrowed) != 1 || narrowed[0].ID != wt.ID {
		t.Fatalf("List(repo) = %+v, %v; want the same entry", narrowed, err)
	}
	if got, err := svc.Get(ctx, wt.ID); err != nil || got.Path != path {
		t.Fatalf("Get = %+v, %v", got, err)
	}

	if err := os.WriteFile(filepath.Join(path, "w.txt"), []byte("edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Delete(ctx, wt.ID, false)
	wantWorkspaceStatus(t, err, 409)
	if deleted, err := svc.Delete(ctx, wt.ID, true); err != nil || deleted {
		t.Fatalf("Delete = %v, %v; want the unmerged branch kept", deleted, err)
	}
	if !folderMissing(path) {
		t.Fatal("folder survived Delete")
	}
	if got, _ := svc.List(ctx, ""); len(got) != 0 {
		t.Fatalf("List after delete = %+v", got)
	}
	if records, _ := stores().ListWorktrees(ctx); len(records) != 0 {
		t.Fatalf("acting on it saved records: %+v", records)
	}
}

// setRemoteHeadT makes origin/HEAD point at ref, as a clone records it.
func setRemoteHeadT(t *testing.T, dir, ref string) {
	t.Helper()
	gitT(t, dir, "update-ref", "refs/remotes/"+ref, "HEAD")
	gitT(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/"+ref)
}

// TestTrunkRefs pins the base an unmanaged worktree without a recorded base is
// measured from: the local branch carrying origin's default name, the main
// checkout's branch when that branch is missing, origin's ref when no local
// branch carries the name, and the conventional names with no remote at all.
func TestTrunkRefs(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, repo string)
		want  []string
	}{
		{
			name: "the local branch origin names as default wins over the checkout",
			setup: func(t *testing.T, repo string) {
				gitT(t, repo, "branch", "master")
				setRemoteHeadT(t, repo, "origin/main")
				// The checkout is transient; a feature branch must not decide the base.
				gitT(t, repo, "checkout", "-qb", "feat/x")
			},
			want: []string{"main", "feat/x", "origin/main"},
		},
		{
			name: "the checkout's branch stands in when origin names no local branch",
			setup: func(t *testing.T, repo string) {
				gitT(t, repo, "branch", "-m", "develop")
				setRemoteHeadT(t, repo, "origin/master")
			},
			want: []string{"develop", "origin/master"},
		},
		{
			name: "a repo with no remote measures from its checkout",
			want: []string{"main"},
		},
		{
			name: "a repo that names no trunk gets no base",
			setup: func(t *testing.T, repo string) {
				gitT(t, repo, "checkout", "--detach")
			},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, _ := initMainRepo(t)
			if tc.setup != nil {
				tc.setup(t, repo)
			}

			got := trunkRefs(context.Background(), repo)

			if !slices.Equal(got, tc.want) {
				t.Fatalf("trunkRefs() = %v, want %v", got, tc.want)
			}
		})
	}
}

package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

// CreateTimeout bounds `git worktree add`, which checks out a whole tree and
// so can outlast GitCommandTimeout on a big repository. The HTTP layer extends
// its write deadline to cover it.
var CreateTimeout = 2 * time.Minute

// Error carries the HTTP status the transport must answer with, so the
// worktree operations can live outside the HTTP package without losing their
// status semantics.
type Error struct {
	status  int
	message string
}

func (e Error) Error() string { return e.message }

// StatusCode is the HTTP status the transport must answer with.
func (e Error) StatusCode() int { return e.status }

func errStatus(status int, message string) error {
	return Error{status: status, message: message}
}

// Service owns the managed-worktree git and filesystem operations. store and
// now are accessors because the API server rebinds them after construction in
// tests. chatUsing reports a running chat whose working directory satisfies the
// predicate; it may be nil.
type Service struct {
	store     func() *storage.SQLiteStore
	logger    *slog.Logger
	now       func() time.Time
	chatUsing func(match func(cwd string) bool) []string
	// finishMu serializes rebase, merge and push, which change a branch or the
	// main checkout and must not interleave.
	finishMu sync.Mutex
}

func New(store func() *storage.SQLiteStore, logger *slog.Logger, now func() time.Time, chatUsing func(match func(cwd string) bool) []string) *Service {
	return &Service{store: store, logger: logger, now: now, chatUsing: chatUsing}
}

// CreateRequest is the client-supplied shape of a worktree create.
type CreateRequest struct {
	Repo   string
	Base   string
	Branch string
}

// WorktreeStatus is a managed worktree plus its ahead/behind/dirty state for
// listing.
type WorktreeStatus struct {
	storage.WorktreeRecord
	Ahead  *int
	Behind *int
	Dirty  *bool
	// Chats are the ACP session ids of the running chats working in the
	// worktree.
	Chats []string
}

// worktreeDirName turns a branch name into a single safe folder name
// ("feat/x" -> "feat-x"), so worktrees are easy to find under <repo>/.worktrees.
func worktreeDirName(branch string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			return r
		}
		return '-'
	}, branch)
}

// excludeWorktreesDir adds /.worktrees/ to the repo's local .git/info/exclude
// (never the committed .gitignore) so the main checkout's git status does not
// list the worktrees as untracked.
func excludeWorktreesDir(ctx context.Context, top string) error {
	exclude, err := RunGit(ctx, top, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return err
	}
	exclude = strings.TrimSpace(exclude)
	if !filepath.IsAbs(exclude) {
		exclude = filepath.Join(top, exclude)
	}
	data, err := os.ReadFile(exclude)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "/.worktrees/" {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(exclude), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(exclude, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	prefix := ""
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		prefix = "\n"
	}
	_, err = f.WriteString(prefix + "/.worktrees/\n")
	return err
}

// Create makes a new branch at base (default: the repo's HEAD) checked out in a
// fresh worktree at <repo>/.worktrees/<branch>, where it is easy to find
// outside the gateway, and records it.
func (s *Service) Create(ctx context.Context, req CreateRequest) (storage.WorktreeRecord, error) {
	repo := strings.TrimSpace(req.Repo)
	if !filepath.IsAbs(repo) {
		return storage.WorktreeRecord{}, errStatus(400, "repo must be an absolute path")
	}
	s.Prune(ctx)
	top, err := RunGit(ctx, repo, "rev-parse", "--show-toplevel")
	if err != nil {
		return storage.WorktreeRecord{}, errStatus(422, err.Error())
	}
	top = filepath.Clean(strings.TrimSpace(top))
	base := strings.TrimSpace(req.Base)
	if base == "" {
		base = "HEAD"
	}
	// "--end-of-options" stops a base like "--foo" being read as a flag.
	baseCommit, err := RunGit(ctx, top, "rev-parse", "--verify", "--quiet", "--end-of-options", base+"^{commit}")
	if err != nil {
		return storage.WorktreeRecord{}, errStatus(422, "unknown base: "+base)
	}
	baseCommit = strings.TrimSpace(baseCommit)

	idBytes := make([]byte, 4)
	if _, err := rand.Read(idBytes); err != nil {
		return storage.WorktreeRecord{}, errStatus(500, "failed to generate id")
	}
	id := hex.EncodeToString(idBytes)
	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		branch = "ferngeist/" + id
	}
	if strings.HasPrefix(branch, "-") {
		return storage.WorktreeRecord{}, errStatus(400, "invalid branch name")
	}
	if _, err := RunGit(ctx, top, "check-ref-format", "--branch", branch); err != nil {
		return storage.WorktreeRecord{}, errStatus(400, "invalid branch name")
	}
	if _, err := RunGit(ctx, top, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		return storage.WorktreeRecord{}, errStatus(409, "branch already exists: "+branch)
	}

	root := filepath.Join(top, ".worktrees")
	path := filepath.Join(root, worktreeDirName(branch))
	if !SubpathOf(path, root) || path == root {
		return storage.WorktreeRecord{}, errStatus(400, "invalid branch name")
	}
	if _, err := os.Stat(path); err == nil {
		return storage.WorktreeRecord{}, errStatus(409, "worktree folder already exists: "+path)
	}
	if err := excludeWorktreesDir(ctx, top); err != nil {
		s.logger.Warn("exclude .worktrees", "repo", top, "error", err)
	}
	// ponytail: synchronous, bounded by CreateTimeout; a checkout that outlasts
	// it needs an async create (202 + poll).
	if _, err := RunGitTimeout(ctx, CreateTimeout, top, "worktree", "add", "-b", branch, path, baseCommit); err != nil {
		// The folder and branch did not exist before this call, so whatever a
		// failed or killed add left behind is ours to remove.
		discardWorktree(top, path, branch)
		return storage.WorktreeRecord{}, errStatus(422, err.Error())
	}
	rec := storage.WorktreeRecord{ID: id, Repo: top, Path: path, Branch: branch, BaseCommit: baseCommit, BaseRef: baseRefOf(ctx, top, base), CreatedAt: s.now()}
	if err := s.store().SaveWorktree(ctx, rec); err != nil {
		s.logger.Error("save worktree", "path", path, "error", err)
		discardWorktree(top, path, branch)
		return storage.WorktreeRecord{}, errStatus(500, "failed to save worktree")
	}
	return rec, nil
}

// listConcurrency bounds how many worktrees List inspects at once, so a long
// list does not spawn a git process per worktree all at the same time.
const listConcurrency = 8

// discardWorktree removes a worktree this process just created, folder, git
// registration and branch, after the create failed part-way.
func discardWorktree(top, path, branch string) {
	ctx := context.Background()
	_, _ = RunGit(ctx, top, "worktree", "remove", "--force", path)
	_ = removeAllRetrying(path)
	_, _ = RunGit(ctx, top, "worktree", "prune")
	_, _ = RunGit(ctx, top, "branch", "-D", branch)
}

// List returns managed worktrees with their ahead/behind/dirty state, checked
// concurrently. Records whose directory was deleted out from under the gateway
// are left out; Prune forgets them.
func (s *Service) List(ctx context.Context) ([]WorktreeStatus, error) {
	records, err := s.store().ListWorktrees(ctx)
	if err != nil {
		s.logger.Error("list worktrees", "error", err)
		return nil, errStatus(500, "failed to list worktrees")
	}
	present := records[:0:0]
	for _, rec := range records {
		if !folderMissing(rec.Path) {
			present = append(present, rec)
		}
	}
	out := make([]WorktreeStatus, len(present))
	slots := make(chan struct{}, listConcurrency)
	var wg sync.WaitGroup
	for i, rec := range present {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			out[i] = s.statusOf(ctx, rec)
		}()
	}
	wg.Wait()
	return out, nil
}

// Get returns one managed worktree as List shows it. A worktree whose folder
// was deleted outside the gateway is unknown here too, like in the list.
func (s *Service) Get(ctx context.Context, id string) (WorktreeStatus, error) {
	rec, ok := s.find(ctx, func(rec storage.WorktreeRecord) bool { return rec.ID == id })
	if !ok || folderMissing(rec.Path) {
		return WorktreeStatus{}, errStatus(404, "unknown worktree")
	}
	return s.statusOf(ctx, rec), nil
}

// folderMissing reports that path was deleted (as opposed to merely unreadable).
func folderMissing(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, os.ErrNotExist)
}

// Prune forgets managed worktrees whose directory was deleted outside the
// gateway and prunes git's bookkeeping for them. It runs when a worktree is
// created and when the server starts, never on a read.
func (s *Service) Prune(ctx context.Context) {
	store := s.store()
	if store == nil {
		return
	}
	records, err := store.ListWorktrees(ctx)
	if err != nil {
		s.logger.Error("prune worktrees", "error", err)
		return
	}
	for _, rec := range records {
		if !folderMissing(rec.Path) {
			continue
		}
		_, _ = RunGit(ctx, rec.Repo, "worktree", "prune")
		if err := store.DeleteWorktree(ctx, rec.ID); err != nil && !errors.Is(err, storage.ErrNotFound) {
			s.logger.Error("prune worktree record", "id", rec.ID, "error", err)
		}
	}
}

// statusOf fills a record's ahead, behind and dirty state; each stays nil when
// git cannot answer. Behind is how far the base has moved since the fork, and
// stays nil without a base ref.
func (s *Service) statusOf(ctx context.Context, rec storage.WorktreeRecord) WorktreeStatus {
	status := WorktreeStatus{WorktreeRecord: rec, Chats: s.chatsOf(rec)}
	if n, err := RunGit(ctx, rec.Path, "rev-list", "--count", s.baseOf(ctx, rec)+"..HEAD"); err == nil {
		if ahead, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			status.Ahead = &ahead
		}
	}
	if rec.BaseRef != "" {
		if n, err := RunGit(ctx, rec.Path, "rev-list", "--count", "--end-of-options", "HEAD.."+rec.BaseRef); err == nil {
			if behind, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
				status.Behind = &behind
			}
		}
	}
	if st, err := RunGit(ctx, rec.Path, "status", "--porcelain"); err == nil {
		dirty := strings.TrimSpace(st) != ""
		status.Dirty = &dirty
	}
	return status
}

// inUse refuses (409) while a running chat uses the worktree as its cwd.
func (s *Service) inUse(rec storage.WorktreeRecord) error {
	if chats := s.chatsOf(rec); len(chats) > 0 {
		return errStatus(409, "worktree is in use by chat "+chats[0])
	}
	return nil
}

// chatsOf lists the running chats whose working directory is in the worktree.
func (s *Service) chatsOf(rec storage.WorktreeRecord) []string {
	if s.chatUsing == nil {
		return nil
	}
	return s.chatUsing(func(cwd string) bool { return SubpathOf(cwd, rec.Path) })
}

// Delete removes a worktree's directory. It refuses (409) while a running chat
// uses the worktree as its working directory, and without force when it has
// uncommitted changes (409). The branch is deleted only when git considers it
// merged (git branch -d); otherwise it is kept so committed work survives. The
// returned bool reports whether the branch was deleted.
func (s *Service) Delete(ctx context.Context, id string, force bool) (bool, error) {
	rec, ok := s.find(ctx, func(rec storage.WorktreeRecord) bool { return rec.ID == id })
	if !ok {
		return false, errStatus(404, "unknown worktree")
	}
	if err := s.inUse(rec); err != nil {
		return false, err
	}
	if _, err := os.Stat(rec.Path); err == nil {
		// The dirty check is ours rather than git's: git deregisters a worktree even
		// when it fails to delete the folder (on Windows, a process still holding it
		// as cwd), so a refused `git worktree remove` could not be told apart from a
		// half-done one, and a retry would only report "is not a working tree".
		// A leftover folder without its .git file has nothing git can check.
		if !force {
			if _, err := os.Stat(filepath.Join(rec.Path, ".git")); err == nil {
				st, err := RunGit(ctx, rec.Path, "status", "--porcelain")
				if err != nil {
					// An unknown state must not fall through to the forced remove below.
					return false, errStatus(409, "could not check worktree for uncommitted changes; delete with force to discard them")
				}
				if strings.TrimSpace(st) != "" {
					return false, errStatus(409, "worktree has uncommitted changes")
				}
			}
		}
		_, _ = RunGit(ctx, rec.Repo, "worktree", "remove", "--force", rec.Path)
		if err := removeAllRetrying(rec.Path); err != nil {
			s.logger.Error("delete worktree folder", "path", rec.Path, "error", err)
			return false, errStatus(500, "worktree folder is still in use; close its chat and try again")
		}
	}
	_, _ = RunGit(ctx, rec.Repo, "worktree", "prune")
	_, branchErr := RunGit(ctx, rec.Repo, "branch", "-d", rec.Branch)
	if err := s.store().DeleteWorktree(ctx, rec.ID); err != nil && !errors.Is(err, storage.ErrNotFound) {
		s.logger.Error("delete worktree record", "id", rec.ID, "error", err)
	}
	return branchErr == nil, nil
}

// baseRefOf resolves what to remember as the worktree's base ref: the base
// the caller named, except a default HEAD is resolved to the branch it points
// at. A literal "HEAD" would merge-base against the worktree tip itself and
// hide every commit, and HEAD moves; the branch name stays put. A detached
// HEAD has no branch, so it records "" and BaseRef falls back to BaseCommit.
func baseRefOf(ctx context.Context, top, base string) string {
	if base != "HEAD" {
		return base
	}
	if sym, err := RunGit(ctx, top, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		if name := strings.TrimSpace(sym); name != "" {
			return name
		}
	}
	return ""
}

// BaseRef is the commit the workspace git endpoints compare against: a managed
// worktree's base commit (so the agent's commits stay visible in the review),
// otherwise HEAD.
func (s *Service) BaseRef(ctx context.Context, cwd string) string {
	if rec, ok := s.find(ctx, func(rec storage.WorktreeRecord) bool { return SubpathOf(cwd, rec.Path) }); ok {
		return s.baseOf(ctx, rec)
	}
	return "HEAD"
}

// baseOf is the commit a managed worktree's own work is counted from: the
// merge-base with its base ref, else the fixed base commit.
func (s *Service) baseOf(ctx context.Context, rec storage.WorktreeRecord) string {
	if mb, ok := s.mergeBase(ctx, rec); ok {
		return mb
	}
	return rec.BaseCommit
}

// LogBase is the commit /git/log reports as the workspace's base, which a
// client uses to tell the branch's own commits from the history it inherited.
// A managed worktree answers from its record; any other checkout asks git,
// because a session that began on a PC must read the same way as one that
// began in a worktree this gateway created. "HEAD" means there is nothing to
// tell apart.
func (s *Service) LogBase(ctx context.Context, cwd string) string {
	if base := s.BaseRef(ctx, cwd); base != "HEAD" {
		return base
	}
	if start, ok := branchStart(ctx, cwd); ok {
		return start
	}
	return forkPoint(ctx, cwd)
}

// branchStart is the commit the checked-out branch was created at, read from
// its reflog: the oldest "branch: Created from ..." entry. Unlike merge-base or
// --fork-point, it keeps naming the right commit after the branch is merged
// into its base branch, which is the case the client's marking exists for. It
// reports false for a detached HEAD, a repo without reflogs, and a branch whose
// reflog has been trimmed past its creation.
func branchStart(ctx context.Context, cwd string) (string, bool) {
	name, err := RunGit(ctx, cwd, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", false
	}
	if name = strings.TrimSpace(name); name == "" {
		return "", false
	}
	out, err := RunGit(ctx, cwd, "reflog", "show", "--no-abbrev", "--format=%H%x00%gs", name)
	if err != nil {
		return "", false
	}
	// Reflog entries are newest first, so the creation is the last match.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		sha, subject, ok := strings.Cut(lines[i], "\x00")
		if !ok || !strings.HasPrefix(subject, "branch: Created from") {
			continue
		}
		if sha = strings.TrimSpace(sha); sha != "" {
			return sha, true
		}
	}
	return "", false
}

// forkPoint is merge-base of the default branch and HEAD, for a branch whose
// reflog no longer names its creation: the trunk it diverged from, which is
// the same commit until the branch is merged or rebased. A branch that reaches
// HEAD from there (already merged, or checked out on the trunk itself) has
// nothing of its own to mark, which is what "HEAD" says.
func forkPoint(ctx context.Context, cwd string) string {
	for _, ref := range []string{defaultRefName(ctx, cwd), "main", "master"} {
		if ref == "" || ref == "HEAD" {
			continue
		}
		mb, err := RunGit(ctx, cwd, "merge-base", "--end-of-options", ref, "HEAD")
		if err != nil {
			continue
		}
		if mb = strings.TrimSpace(mb); mb == "" {
			continue
		}
		head, err := RunGit(ctx, cwd, "rev-parse", "--verify", "HEAD")
		if err != nil {
			continue
		}
		if mb != strings.TrimSpace(head) {
			return mb
		}
	}
	return "HEAD"
}

// defaultRefName is the trunk the default branch hint would name: origin/HEAD's
// target when the remote names one, else "". The caller tries local main and
// master itself, so this only answers what the remote said.
func defaultRefName(ctx context.Context, cwd string) string {
	return strings.TrimSpace(gitOrEmpty(ctx, cwd, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"))
}

// mergeBase returns merge-base of the worktree's recorded base ref and its
// HEAD: the fork point even after the branch is rebased onto a newer base.
// It reports false when no ref was recorded (pre-migration rows) or the ref
// no longer resolves (a deleted base branch), and the caller falls back to
// the fixed base commit.
func (s *Service) mergeBase(ctx context.Context, rec storage.WorktreeRecord) (string, bool) {
	if rec.BaseRef == "" {
		return "", false
	}
	mb, err := RunGit(ctx, rec.Path, "merge-base", "--end-of-options", rec.BaseRef, "HEAD")
	if err != nil {
		return "", false
	}
	if mb = strings.TrimSpace(mb); mb == "" {
		return "", false
	}
	return mb, true
}

// find returns the first managed worktree matching match.
func (s *Service) find(ctx context.Context, match func(storage.WorktreeRecord) bool) (storage.WorktreeRecord, bool) {
	store := s.store()
	if store == nil {
		return storage.WorktreeRecord{}, false
	}
	records, err := store.ListWorktrees(ctx)
	if err != nil {
		return storage.WorktreeRecord{}, false
	}
	for _, rec := range records {
		if match(rec) {
			return rec, true
		}
	}
	return storage.WorktreeRecord{}, false
}

// removeAllRetrying deletes dir, retrying briefly: on Windows a just-closed
// agent process can hold the folder for a moment after it exits.
// ponytail: fixed ~2s budget; make it configurable if slow agents need longer.
func removeAllRetrying(dir string) error {
	var err error
	for range 10 {
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return err
}

// RepoRefs is Refs for a repository named by absolute path, so a client can
// learn the default branch and ref list before any chat exists in it. A
// relative path is a 400; a non-repo is a 422.
func RepoRefs(ctx context.Context, repo string) (RefsPayload, error) {
	repo = strings.TrimSpace(repo)
	if !filepath.IsAbs(repo) {
		return RefsPayload{}, errStatus(400, "repo must be an absolute path")
	}
	return Refs(ctx, repo)
}

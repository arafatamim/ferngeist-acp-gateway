package workspace

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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
	// creating holds the folders a Create is making, so a second create for the
	// same folder cannot pass the existence checks and then discard the first.
	creatingMu sync.Mutex
	creating   map[string]bool
	// repos are the repositories ?repo= listings named, which discover keeps
	// listing; in memory only, so a restart forgets them.
	reposMu sync.Mutex
	repos   []string
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
	// Merged reports that the branch has commits of its own and they are all
	// on its base: merging it again would change nothing. Merged and not
	// Dirty marks a worktree that is safe to suggest cleaning up.
	Merged *bool
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

// claimFolder reserves path for one Create; the returned func releases it.
func (s *Service) claimFolder(path string) (func(), bool) {
	s.creatingMu.Lock()
	defer s.creatingMu.Unlock()
	if s.creating[path] {
		return nil, false
	}
	if s.creating == nil {
		s.creating = make(map[string]bool)
	}
	s.creating[path] = true
	return func() {
		s.creatingMu.Lock()
		delete(s.creating, path)
		s.creatingMu.Unlock()
	}, true
}

// Create makes a new branch at base (default: the repo's HEAD) checked out in a
// fresh worktree at <repo>/.worktrees/<branch>, where it is easy to find
// outside the gateway, and records it. Fetching the base and checking it out
// share one CreateTimeout, which the HTTP layer's write deadline covers.
func (s *Service) Create(ctx context.Context, req CreateRequest) (storage.WorktreeRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, CreateTimeout)
	defer cancel()
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
	baseCommit, baseRef, err := s.resolveBaseCommitAndRef(ctx, top, base)
	if err != nil {
		return storage.WorktreeRecord{}, err
	}

	id, err := newWorktreeID()
	if err != nil {
		return storage.WorktreeRecord{}, err
	}
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
	root := filepath.Join(top, ".worktrees")
	path := filepath.Join(root, worktreeDirName(branch))
	if !SubpathOf(path, root) || path == root {
		return storage.WorktreeRecord{}, errStatus(400, "invalid branch name")
	}
	// Claimed before the checks below, which a concurrent create would pass too.
	// One folder per branch name, so this also covers two creates of one branch.
	release, ok := s.claimFolder(path)
	if !ok {
		return storage.WorktreeRecord{}, errStatus(409, "worktree folder is being created: "+path)
	}
	defer release()
	if branchExists(ctx, top, branch) {
		return storage.WorktreeRecord{}, errStatus(409, "branch already exists: "+branch)
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
	rec := storage.WorktreeRecord{ID: id, Repo: top, Path: path, Branch: branch, BaseCommit: baseCommit, BaseRef: baseRef, CreatedAt: s.now()}
	if err := s.store().SaveWorktree(ctx, rec); err != nil {
		discardWorktree(top, path, branch)
		if errors.Is(err, storage.ErrWorktreePathTaken) {
			return storage.WorktreeRecord{}, errStatus(409, "worktree is already managed: "+path)
		}
		s.logger.Error("save worktree", "path", path, "error", err)
		return storage.WorktreeRecord{}, errStatus(500, "failed to save worktree")
	}
	return rec, nil
}

// listConcurrency bounds how many worktrees List inspects at once, so a long
// list does not spawn a git process per worktree all at the same time.
const listConcurrency = 8

// newWorktreeID returns a random 8-hex-digit worktree id.
func newWorktreeID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", errStatus(500, "failed to generate id")
	}
	return hex.EncodeToString(b), nil
}

// discardWorktree removes a worktree this process just created, folder, git
// registration and branch, after the create failed part-way.
func discardWorktree(top, path, branch string) {
	ctx := context.Background()
	_, _ = RunGit(ctx, top, "worktree", "remove", "--force", path)
	_ = removeAllRetrying(path)
	_, _ = RunGit(ctx, top, "worktree", "prune")
	_, _ = RunGit(ctx, top, "branch", "-D", branch)
}

// List returns every linked git worktree of the repositories the gateway
// knows (or of repo alone, when set) with its ahead/behind/dirty state, checked
// concurrently. Managed worktrees come first in the order they were recorded;
// the rest follow as git lists them, measured against the repo's default
// branch. Folders deleted outside the gateway are left out; Prune forgets them.
func (s *Service) List(ctx context.Context, repo string) ([]WorktreeStatus, error) {
	present, trunks, err := s.discover(ctx, repo)
	if err != nil {
		return nil, err
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
			out[i] = s.statusOf(ctx, withDefaultBase(ctx, rec, trunks[rec.Repo]))
		}()
	}
	wg.Wait()
	return out, nil
}

// discover lists the linked worktrees of every repository the gateway knows:
// those of managed records, of running chats' working directories and of
// earlier ?repo= listings, or repo alone when set. A worktree the gateway did
// not make gets a record that is never saved, with an id derived from its path
// and no base yet (see withDefaultBase), so it drops out as soon as git stops
// listing it. trunks holds each such repo's candidate bases, resolved once.
func (s *Service) discover(ctx context.Context, repo string) (_ []storage.WorktreeRecord, trunks map[string][]string, _ error) {
	var records []storage.WorktreeRecord
	if store := s.store(); store != nil {
		var err error
		if records, err = store.ListWorktrees(ctx); err != nil {
			s.logger.Error("list worktrees", "error", err)
			return nil, nil, errStatus(500, "failed to list worktrees")
		}
	}
	var dirs []string
	if repo = strings.TrimSpace(repo); repo != "" {
		if !filepath.IsAbs(repo) {
			return nil, nil, errStatus(400, "repo must be an absolute path")
		}
		dirs = []string{repo}
	} else {
		for _, rec := range records {
			dirs = append(dirs, rec.Repo)
		}
		s.reposMu.Lock()
		dirs = append(dirs, s.repos...)
		s.reposMu.Unlock()
		if s.chatUsing != nil {
			// The predicate sees every running chat's cwd; matching none collects them.
			s.chatUsing(func(cwd string) bool { dirs = append(dirs, cwd); return false })
		}
	}

	listed := make(map[string]bool) // canonical paths of every worktree git listed
	var seen []string               // checkouts already covered by a listing
	var unmanaged []storage.WorktreeRecord
	trunks = make(map[string][]string)
	managed := make(map[string]bool)
	for _, rec := range records {
		managed[canonPath(rec.Path)] = true
	}
	for _, dir := range dirs {
		dir = canonPath(dir)
		if slices.ContainsFunc(seen, func(p string) bool { return SubpathOf(dir, p) }) {
			continue
		}
		out, err := RunGit(ctx, dir, "worktree", "list", "--porcelain")
		if err != nil {
			if repo != "" {
				return nil, nil, errStatus(422, err.Error())
			}
			continue
		}
		parsed := parseWorktreeListPorcelain(out)
		if len(parsed) == 0 {
			continue
		}
		top := filepath.Clean(parsed[0].path)
		if repo != "" {
			s.rememberRepo(top)
		}
		for i, wt := range parsed {
			canon := canonPath(wt.path)
			seen = append(seen, canon)
			// The first entry is the main checkout, never a worktree to list.
			if i == 0 || wt.prunable || folderMissing(wt.path) {
				continue
			}
			listed[canon] = true
			if !managed[canon] {
				if _, ok := trunks[top]; !ok {
					trunks[top] = trunkRefs(ctx, top)
				}
				unmanaged = append(unmanaged, storage.WorktreeRecord{ID: pathID(canon), Repo: top, Path: filepath.Clean(wt.path), Branch: wt.branch, CreatedAt: createdAt(wt.path)})
			}
		}
	}
	present := records[:0:0]
	for _, rec := range records {
		if listed[canonPath(rec.Path)] {
			present = append(present, rec)
		}
	}
	return append(present, unmanaged...), trunks, nil
}

// rememberRepo adds top to the repositories discover lists, so a worktree
// shown by a ?repo= listing can be acted on by id afterwards.
func (s *Service) rememberRepo(top string) {
	s.reposMu.Lock()
	defer s.reposMu.Unlock()
	if !slices.Contains(s.repos, top) {
		s.repos = append(s.repos, top)
	}
}

// pathID is the id of a worktree the gateway did not make: stable for its
// folder, so the client can act on it by id.
func pathID(canon string) string {
	sum := sha256.Sum256([]byte(canon))
	return hex.EncodeToString(sum[:4])
}

// createdAt is when git made the worktree, which wrote its .git file then.
func createdAt(path string) time.Time {
	if fi, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

// withDefaultBase fills the base of a record discover made up, measuring it
// against the first of its repo's trunks it shares history with; a saved
// record already has one. With none, it counts from its own HEAD.
func withDefaultBase(ctx context.Context, rec storage.WorktreeRecord, trunks []string) storage.WorktreeRecord {
	if rec.BaseCommit != "" {
		return rec
	}
	for _, trunk := range trunks {
		if trunk == rec.Branch {
			continue
		}
		if mb, err := RunGit(ctx, rec.Path, "merge-base", "--end-of-options", trunk, "HEAD"); err == nil && strings.TrimSpace(mb) != "" {
			rec.BaseRef, rec.BaseCommit = trunk, strings.TrimSpace(mb)
			return rec
		}
	}
	rec.BaseCommit, _ = commitOf(ctx, rec.Path, "HEAD")
	return rec
}

// trunkRefs are the refs a worktree with no recorded base may be measured
// from, in order of authority: the local branch carrying origin's default
// name (a stable trunk — the checkout's branch is transient), the main
// checkout's branch when that local branch does not exist, then origin's
// default ref when no local branch carries the name. All of them are local, so
// ahead/behind answers what still has to land on the local trunk, not what is
// unpushed. A repo that names none of them gets no base: guessing main or
// master would measure against a branch the user never chose.
func trunkRefs(ctx context.Context, top string) []string {
	var out []string
	add := func(ref string) {
		if ref == "" || slices.Contains(out, ref) {
			return
		}
		if _, ok := commitOf(ctx, top, ref); ok {
			out = append(out, ref)
		}
	}
	remoteDefault := defaultRefName(ctx, top)
	if name, ok := strings.CutPrefix(remoteDefault, "origin/"); ok {
		add(name)
	}
	add(baseRefOf(ctx, top, "HEAD"))
	add(remoteDefault)
	return out
}

// worktree finds the worktree id names: a managed one, else one List shows.
func (s *Service) worktree(ctx context.Context, id string) (storage.WorktreeRecord, bool) {
	if rec, ok := s.find(ctx, func(rec storage.WorktreeRecord) bool { return rec.ID == id }); ok {
		return rec, true
	}
	records, trunks, err := s.discover(ctx, "")
	if err != nil {
		return storage.WorktreeRecord{}, false
	}
	for _, rec := range records {
		if rec.ID == id {
			return withDefaultBase(ctx, rec, trunks[rec.Repo]), true
		}
	}
	return storage.WorktreeRecord{}, false
}

// Get returns one managed worktree as List shows it. A worktree whose folder
// was deleted outside the gateway is unknown here too, like in the list.
func (s *Service) Get(ctx context.Context, id string) (WorktreeStatus, error) {
	rec, ok := s.worktree(ctx, id)
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
// git cannot answer. Ahead and behind count the branch's and the base's
// commits since they forked, in one rev-list; without a base ref that
// resolves, behind and merged stay nil and ahead counts from the base commit.
func (s *Service) statusOf(ctx context.Context, rec storage.WorktreeRecord) WorktreeStatus {
	status := WorktreeStatus{WorktreeRecord: rec, Chats: s.chatsOf(rec)}
	if rec.BaseRef != "" {
		if out, err := RunGit(ctx, rec.Path, "rev-list", "--left-right", "--count", "--end-of-options", rec.BaseRef+"...HEAD"); err == nil {
			l, r, _ := strings.Cut(strings.TrimSpace(out), "\t")
			behind, errB := strconv.Atoi(l)
			ahead, errA := strconv.Atoi(r)
			if errB == nil && errA == nil {
				status.Ahead, status.Behind = &ahead, &behind
				merged := mergedOf(ctx, rec, ahead)
				status.Merged = &merged
			}
		}
	}
	if status.Ahead == nil {
		if n, err := RunGit(ctx, rec.Path, "rev-list", "--count", "--end-of-options", rec.BaseCommit+"..HEAD"); err == nil {
			if ahead, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
				status.Ahead = &ahead
			}
		}
	}
	if st, err := RunGit(ctx, rec.Path, "status", "--porcelain"); err == nil {
		dirty := strings.TrimSpace(st) != ""
		status.Dirty = &dirty
	}
	return status
}

// mergedOf reports whether the worktree's work has landed on its base ref, by
// fast-forward, merge, rebase or squash alike. With nothing ahead of the base,
// HEAD is on it, and has work only if it moved off the base commit. Otherwise
// merging HEAD into the base must yield the base's own tree, which a rebased or
// squashed branch does; merge-tree runs only then, as it writes objects.
func mergedOf(ctx context.Context, rec storage.WorktreeRecord, ahead int) bool {
	if ahead == 0 {
		head, ok := commitOf(ctx, rec.Path, "HEAD")
		return ok && head != rec.BaseCommit
	}
	baseTree, err := RunGit(ctx, rec.Path, "rev-parse", "--verify", "--quiet", "--end-of-options", rec.BaseRef+"^{tree}")
	if err != nil {
		return false
	}
	// A conflict exits non-zero, which is rightly not merged.
	out, err := RunGit(ctx, rec.Path, "merge-tree", "--write-tree", "--end-of-options", rec.BaseRef, "HEAD")
	if err != nil {
		return false
	}
	tree, _, _ := strings.Cut(out, "\n")
	return strings.TrimSpace(tree) == strings.TrimSpace(baseTree)
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
	rec, ok := s.worktree(ctx, id)
	if !ok {
		return false, errStatus(404, "unknown worktree")
	}
	if err := s.inUse(rec); err != nil {
		return false, err
	}
	// A main checkout's .git is a folder, not a file; removing it would take the
	// whole repository with it.
	if fi, err := os.Stat(filepath.Join(rec.Path, ".git")); err == nil && fi.IsDir() {
		return false, errStatus(409, "refusing to delete a main repository checkout")
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

// isDigits reports whether s consists entirely of ASCII digits.
func isDigits(s string) bool {
	_, err := strconv.ParseUint(s, 10, 64)
	return err == nil
}

// commitOf resolves ref to the commit it names in dir.
func commitOf(ctx context.Context, dir, ref string) (string, bool) {
	out, err := RunGit(ctx, dir, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
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

// BranchOf is the branch of the managed worktree containing cwd, and "" when
// none does or the lookup fails, so a caller that decorates with the branch
// (the push path) loses only the branch, never its event. Unlike BaseRef it
// needs no git round-trips: the record's branch is the answer.
func (s *Service) BranchOf(ctx context.Context, cwd string) string {
	if rec, ok := s.find(ctx, func(rec storage.WorktreeRecord) bool { return SubpathOf(cwd, rec.Path) }); ok {
		return rec.Branch
	}
	return ""
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

type porcelainWorktree struct {
	path     string
	head     string
	branch   string
	detached bool
	bare     bool
	prunable bool
}

// parseWorktreeListPorcelain parses `git worktree list --porcelain`. Git always
// lists the main checkout first.
func parseWorktreeListPorcelain(output string) []porcelainWorktree {
	var worktrees []porcelainWorktree
	var current porcelainWorktree
	flush := func() {
		if current.path != "" {
			worktrees = append(worktrees, current)
		}
		current = porcelainWorktree{}
	}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "worktree "):
			flush()
			current.path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "HEAD "):
			current.head = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			current.branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		case line == "detached":
			current.detached = true
		case line == "bare":
			current.bare = true
		case strings.HasPrefix(line, "prunable"):
			current.prunable = true
		}
	}
	flush()
	return worktrees
}

// canonPath is p with symlinks resolved, or just cleaned when it does not
// exist, so two spellings of one folder compare equal.
func canonPath(p string) string {
	if eval, err := filepath.EvalSymlinks(p); err == nil {
		return eval
	}
	return filepath.Clean(p)
}

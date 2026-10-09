package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

// worktreeResponse is one gateway-managed git worktree. Path is what the
// client passes as the session/new cwd to run an agent in it.
type worktreeResponse struct {
	ID         string    `json:"id"`
	Repo       string    `json:"repo"`
	Path       string    `json:"path"`
	Branch     string    `json:"branch"`
	BaseCommit string    `json:"baseCommit"`
	CreatedAt  time.Time `json:"createdAt"`
	// Ahead is the number of commits on the branch since BaseCommit; Dirty
	// reports uncommitted changes. Both are filled only by the list endpoint.
	Ahead *int  `json:"ahead,omitempty"`
	Dirty *bool `json:"dirty,omitempty"`
}

type worktreeCreateRequest struct {
	Repo   string `json:"repo"`
	Base   string `json:"base"`
	Branch string `json:"branch"`
}

func toWorktreeResponse(r storage.WorktreeRecord) worktreeResponse {
	return worktreeResponse{ID: r.ID, Repo: r.Repo, Path: r.Path, Branch: r.Branch, BaseCommit: r.BaseCommit, CreatedAt: r.CreatedAt}
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
	exclude, err := runGit(ctx, top, "rev-parse", "--git-path", "info/exclude")
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

// POST /v1/worktrees {repo, base?, branch?}
// Creates a new branch at base (default: the repo's HEAD) checked out in a
// fresh worktree at <repo>/.worktrees/<branch>, where it is easy to find
// outside the gateway.
func (s *Server) handleWorktreeCreate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeControl); !ok {
		return
	}
	var req worktreeCreateRequest
	r.Body = http.MaxBytesReader(w, r.Body, jsonBodyLimit)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	repo := strings.TrimSpace(req.Repo)
	if !filepath.IsAbs(repo) {
		writeError(w, http.StatusBadRequest, "repo must be an absolute path")
		return
	}
	ctx := r.Context()
	top, err := runGit(ctx, repo, "rev-parse", "--show-toplevel")
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	top = filepath.Clean(strings.TrimSpace(top))
	base := strings.TrimSpace(req.Base)
	if base == "" {
		base = "HEAD"
	}
	// "--end-of-options" stops a base like "--foo" being read as a flag.
	baseCommit, err := runGit(ctx, top, "rev-parse", "--verify", "--quiet", "--end-of-options", base+"^{commit}")
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "unknown base: "+base)
		return
	}
	baseCommit = strings.TrimSpace(baseCommit)

	idBytes := make([]byte, 4)
	if _, err := rand.Read(idBytes); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate id")
		return
	}
	id := hex.EncodeToString(idBytes)
	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		branch = "ferngeist/" + id
	}
	if strings.HasPrefix(branch, "-") {
		writeError(w, http.StatusBadRequest, "invalid branch name")
		return
	}
	if _, err := runGit(ctx, top, "check-ref-format", "--branch", branch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid branch name")
		return
	}
	if _, err := runGit(ctx, top, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		writeError(w, http.StatusConflict, "branch already exists: "+branch)
		return
	}

	root := filepath.Join(top, ".worktrees")
	path := filepath.Join(root, worktreeDirName(branch))
	if !subpathOf(path, root) || path == root {
		writeError(w, http.StatusBadRequest, "invalid branch name")
		return
	}
	if _, err := os.Stat(path); err == nil {
		writeError(w, http.StatusConflict, "worktree folder already exists: "+path)
		return
	}
	if err := excludeWorktreesDir(ctx, top); err != nil {
		s.logger.Warn("exclude .worktrees", "repo", top, "error", err)
	}
	// ponytail: bounded by runGit's 15s timeout; a huge checkout needs an
	// async create (202 + poll) if that ever bites.
	if _, err := runGit(ctx, top, "worktree", "add", "-b", branch, path, baseCommit); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	rec := storage.WorktreeRecord{ID: id, Repo: top, Path: path, Branch: branch, BaseCommit: baseCommit, CreatedAt: s.now()}
	if err := s.store.SaveWorktree(ctx, rec); err != nil {
		s.logger.Error("save worktree", "path", path, "error", err)
		_, _ = runGit(context.Background(), top, "worktree", "remove", "--force", path)
		_, _ = runGit(context.Background(), top, "branch", "-D", branch)
		writeError(w, http.StatusInternalServerError, "failed to save worktree")
		return
	}
	writeJSON(w, http.StatusOK, toWorktreeResponse(rec))
}

// GET /v1/worktrees
// Lists managed worktrees with their ahead/dirty state. Records whose
// directory was deleted out from under the gateway are dropped (and git's
// bookkeeping pruned) instead of being listed.
func (s *Server) handleWorktreeList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeRead); !ok {
		return
	}
	ctx := r.Context()
	records, err := s.store.ListWorktrees(ctx)
	if err != nil {
		s.logger.Error("list worktrees", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list worktrees")
		return
	}
	out := make([]worktreeResponse, 0, len(records))
	for _, rec := range records {
		if _, err := os.Stat(rec.Path); errors.Is(err, os.ErrNotExist) {
			_, _ = runGit(ctx, rec.Repo, "worktree", "prune")
			_ = s.store.DeleteWorktree(ctx, rec.ID)
			continue
		}
		resp := toWorktreeResponse(rec)
		if n, err := runGit(ctx, rec.Path, "rev-list", "--count", rec.BaseCommit+"..HEAD"); err == nil {
			if ahead, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
				resp.Ahead = &ahead
			}
		}
		if st, err := runGit(ctx, rec.Path, "status", "--porcelain"); err == nil {
			dirty := strings.TrimSpace(st) != ""
			resp.Dirty = &dirty
		}
		out = append(out, resp)
	}
	writeJSON(w, http.StatusOK, out)
}

// DELETE /v1/worktrees/{worktreeId}?force=true
// Removes the worktree directory. Without force, git refuses when it has
// uncommitted changes (409). The branch is deleted only when git considers it
// merged (git branch -d); otherwise it is kept so committed work survives.
func (s *Server) handleWorktreeDelete(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeControl); !ok {
		return
	}
	ctx := r.Context()
	rec, ok := s.findWorktree(ctx, func(rec storage.WorktreeRecord) bool { return rec.ID == r.PathValue("worktreeId") })
	if !ok {
		writeError(w, http.StatusNotFound, "unknown worktree")
		return
	}
	if _, err := os.Stat(rec.Path); err == nil {
		args := []string{"worktree", "remove"}
		if r.URL.Query().Get("force") == "true" {
			args = append(args, "--force")
		}
		if _, err := runGit(ctx, rec.Repo, append(args, rec.Path)...); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
	} else {
		_, _ = runGit(ctx, rec.Repo, "worktree", "prune")
	}
	_, branchErr := runGit(ctx, rec.Repo, "branch", "-d", rec.Branch)
	if err := s.store.DeleteWorktree(ctx, rec.ID); err != nil && !errors.Is(err, storage.ErrNotFound) {
		s.logger.Error("delete worktree record", "id", rec.ID, "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": rec.ID, "branchDeleted": branchErr == nil})
}

// findWorktree returns the first managed worktree matching match.
func (s *Server) findWorktree(ctx context.Context, match func(storage.WorktreeRecord) bool) (storage.WorktreeRecord, bool) {
	if s.store == nil {
		return storage.WorktreeRecord{}, false
	}
	records, err := s.store.ListWorktrees(ctx)
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

// workspaceBaseRef is the commit the workspace git endpoints compare against:
// a managed worktree's base commit (so the agent's commits stay visible in
// the review), otherwise HEAD.
// ponytail: fixed base commit; after rebasing the branch onto a newer base
// the diff also shows the base's new commits. Use merge-base with the
// original base ref if that matters.
func (s *Server) workspaceBaseRef(ctx context.Context, cwd string) string {
	if rec, ok := s.findWorktree(ctx, func(rec storage.WorktreeRecord) bool { return subpathOf(cwd, rec.Path) }); ok {
		return rec.BaseCommit
	}
	return "HEAD"
}

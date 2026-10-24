package api

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/workspace"
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
	// Ahead is the number of commits on the branch since its merge-base with the
	// base ref; Behind is how many commits the base has gained since; Dirty
	// reports uncommitted changes. Filled only by the list and finish endpoints.
	Ahead  *int  `json:"ahead,omitempty"`
	Behind *int  `json:"behind,omitempty"`
	Dirty  *bool `json:"dirty,omitempty"`
	// Merged reports the branch's commits are all on its base ref; with Dirty
	// false the worktree can be suggested for cleanup.
	Merged *bool `json:"merged,omitempty"`
	// Chats are the ACP session ids of the running chats working in the
	// worktree, so a client can show a count and open one.
	Chats []string `json:"chats,omitempty"`
}

type worktreeCreateRequest struct {
	Repo   string `json:"repo"`
	Base   string `json:"base"`
	Branch string `json:"branch"`
}

func toWorktreeStatusResponse(status workspace.WorktreeStatus) worktreeResponse {
	resp := worktreeResponse{ID: status.ID, Repo: status.Repo, Path: status.Path, Branch: status.Branch, BaseCommit: status.BaseCommit, CreatedAt: status.CreatedAt}
	resp.Ahead = status.Ahead
	resp.Behind = status.Behind
	resp.Dirty = status.Dirty
	resp.Merged = status.Merged
	resp.Chats = status.Chats
	return resp
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
	// The server's write timeout would drop the response of a create that is
	// still checking out a big tree.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(workspace.CreateTimeout + 5*time.Second))
	rec, err := s.workspaceSvc.Create(r.Context(), workspace.CreateRequest{Repo: req.Repo, Base: req.Base, Branch: req.Branch})
	if err != nil {
		writeWorkspaceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, worktreeResponse{ID: rec.ID, Repo: rec.Repo, Path: rec.Path, Branch: rec.Branch, BaseCommit: rec.BaseCommit, CreatedAt: rec.CreatedAt})
}

// GET /v1/worktrees?repo=<absolute path, optional>
// Lists every git worktree of the repositories the gateway knows, whoever made
// it, with its ahead/behind/dirty state; repo narrows it to one repository.
// Worktrees whose directory was deleted are left out.
func (s *Server) handleWorktreeList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeRead); !ok {
		return
	}
	statuses, err := s.workspaceSvc.List(r.Context(), r.URL.Query().Get("repo"))
	if err != nil {
		writeWorkspaceError(w, err)
		return
	}
	out := make([]worktreeResponse, 0, len(statuses))
	for _, status := range statuses {
		out = append(out, toWorktreeStatusResponse(status))
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /v1/worktrees/refs?repo=<absolute path>
// Lists a repository's branches, remote refs and tags with its default branch,
// the /git/refs payload, for pickers that run before any chat exists in the
// repo. Same scope as creating a worktree.
func (s *Server) handleWorktreeRefs(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeControl); !ok {
		return
	}
	payload, err := workspace.RepoRefs(r.Context(), r.URL.Query().Get("repo"))
	if err != nil {
		writeWorkspaceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

// GET /v1/worktrees/{worktreeId}
// Returns one worktree as the list shows it, so a client can refresh a single
// card. 404 for an unknown worktree or one whose directory was deleted.
func (s *Server) handleWorktreeGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeRead); !ok {
		return
	}
	status, err := s.workspaceSvc.Get(r.Context(), r.PathValue("worktreeId"))
	if err != nil {
		writeWorkspaceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toWorktreeStatusResponse(status))
}

// DELETE /v1/worktrees/{worktreeId}?force=true
// Removes the worktree directory. Without force, git refuses when it has
// uncommitted changes (409). The branch is deleted only when git considers it
// merged (git branch -d); otherwise it is kept so committed work survives.
func (s *Server) handleWorktreeDelete(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeControl); !ok {
		return
	}
	id := r.PathValue("worktreeId")
	branchDeleted, err := s.workspaceSvc.Delete(r.Context(), id, r.URL.Query().Get("force") == "true")
	if err != nil {
		writeWorkspaceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id, "branchDeleted": branchDeleted})
}

// POST /v1/worktrees/{worktreeId}/rebase
// Rebases the worktree's branch onto its base ref. Refused (409) while the
// worktree has uncommitted changes; a conflict aborts the rebase (409) and
// leaves the branch untouched. Responds with the worktree as the list shows it.
func (s *Server) handleWorktreeRebase(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeControl); !ok {
		return
	}
	status, err := s.workspaceSvc.Rebase(r.Context(), r.PathValue("worktreeId"))
	if err != nil {
		writeWorkspaceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toWorktreeStatusResponse(status))
}

type worktreeMergeRequest struct {
	Squash  bool   `json:"squash"`
	Message string `json:"message"`
	Delete  bool   `json:"delete"`
}

// POST /v1/worktrees/{worktreeId}/merge {squash?, message?, delete?}
// Lands the worktree's branch on its base branch in the main checkout:
// fast-forward by default, one commit with squash. delete also removes the
// worktree and branch afterwards. Refused (409) while the worktree or the main
// checkout is dirty, when the base branch is not the main checkout's branch,
// when a fast-forward is not possible, or when a squash conflicts.
func (s *Server) handleWorktreeMerge(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeControl); !ok {
		return
	}
	var req worktreeMergeRequest
	r.Body = http.MaxBytesReader(w, r.Body, jsonBodyLimit)
	// An empty body means a plain fast-forward.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	result, err := s.workspaceSvc.Merge(r.Context(), r.PathValue("worktreeId"), workspace.MergeRequest{Squash: req.Squash, Message: req.Message, Delete: req.Delete})
	if err != nil {
		writeWorkspaceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"commit": result.Commit, "worktreeDeleted": result.WorktreeDeleted, "branchDeleted": result.BranchDeleted})
}

type worktreePushRequest struct {
	PR    bool   `json:"pr"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// pushWriteDeadline covers the full bounded budget of a worktree push above
// the server's 20s write timeout: git push and, with pr, gh pr create run
// back to back, each bounded by workspace.GitCommandTimeout, plus slack for
// serializing the response.
const pushWriteDeadline = 2*workspace.GitCommandTimeout + 5*time.Second

// POST /v1/worktrees/{worktreeId}/push {pr?, title?, body?}
// Pushes the worktree's branch to origin and, with pr, opens a pull request
// with the gh CLI. Never force-pushes. Refused (409) while the worktree has
// uncommitted changes or when origin has commits the branch lacks.
func (s *Server) handleWorktreePush(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeControl); !ok {
		return
	}
	var req worktreePushRequest
	r.Body = http.MaxBytesReader(w, r.Body, jsonBodyLimit)
	// An empty body means a plain push.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// git push and, with pr, gh pr create run back to back, each bounded by
	// git's command timeout, so together they can outlast the server's write
	// timeout. The extension passes through the request logging
	// ResponseWriter's Unwrap to the connection.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(pushWriteDeadline))
	result, err := s.workspaceSvc.Push(r.Context(), r.PathValue("worktreeId"), workspace.PushRequest{PR: req.PR, Title: req.Title, Body: req.Body})
	if err != nil {
		writeWorkspaceError(w, err)
		return
	}
	resp := map[string]any{"pushed": true}
	if result.PRURL != "" {
		resp["prUrl"] = result.PRURL
	}
	writeJSON(w, http.StatusOK, resp)
}

type pathsMissingRequest struct {
	Paths []string `json:"paths"`
}

// POST /v1/paths/missing {paths}
// Returns the given absolute paths that no longer exist on disk, so a client
// can set aside chats whose cwd (often a deleted worktree) is gone. A path
// that cannot be checked counts as still there.
func (s *Server) handlePathsMissing(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeRead); !ok {
		return
	}
	var req pathsMissingRequest
	r.Body = http.MaxBytesReader(w, r.Body, jsonBodyLimit)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	missing := []string{}
	for _, p := range req.Paths {
		if !filepath.IsAbs(p) {
			writeError(w, http.StatusBadRequest, "path must be absolute: "+p)
			return
		}
		if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
			missing = append(missing, p)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"missing": missing})
}

// writeWorkspaceError maps a workspace service error to the HTTP status it
// carries, falling back to 500 for anything untyped.
func writeWorkspaceError(w http.ResponseWriter, err error) {
	var workspaceError workspace.Error
	if errors.As(err, &workspaceError) {
		writeError(w, workspaceError.StatusCode(), workspaceError.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, "workspace operation failed")
}

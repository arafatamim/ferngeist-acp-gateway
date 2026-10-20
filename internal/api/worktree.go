package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
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
}

type worktreeCreateRequest struct {
	Repo   string `json:"repo"`
	Base   string `json:"base"`
	Branch string `json:"branch"`
}

func toWorktreeResponse(r storage.WorktreeRecord) worktreeResponse {
	return worktreeResponse{ID: r.ID, Repo: r.Repo, Path: r.Path, Branch: r.Branch, BaseCommit: r.BaseCommit, CreatedAt: r.CreatedAt}
}

func toWorktreeStatusResponse(status workspace.WorktreeStatus) worktreeResponse {
	resp := toWorktreeResponse(status.WorktreeRecord)
	resp.Ahead = status.Ahead
	resp.Behind = status.Behind
	resp.Dirty = status.Dirty
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
	writeJSON(w, http.StatusOK, toWorktreeResponse(rec))
}

// GET /v1/worktrees
// Lists managed worktrees with their ahead/behind/dirty state. Worktrees whose
// directory was deleted out from under the gateway are left out; they are
// forgotten when a worktree is next created or the server restarts.
func (s *Server) handleWorktreeList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeRead); !ok {
		return
	}
	statuses, err := s.workspaceSvc.List(r.Context())
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

package api

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/session"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/workspace"
	"github.com/coder/acp-go-sdk"
)

// fileReadResponse is the payload for GET /v1/runtimes/{id}/files. It embeds
// the ACP v1 resource type matching the file kind: TextResourceContents for
// text files, BlobResourceContents for binary files. Size and Truncated are
// gateway extensions; Size is the full on-disk file size (not the truncated
// payload length); Uri carries the file:// URI of the absolute path.
type fileReadResponse struct {
	acp.TextResourceContents
	Size      int  `json:"size"`
	Truncated bool `json:"truncated"`
}

// blobReadResponse is the binary counterpart of fileReadResponse.
type blobReadResponse struct {
	acp.BlobResourceContents
	Size      int  `json:"size"`
	Truncated bool `json:"truncated"`
}

// gitStatusResponse is the payload for GET /v1/runtimes/{id}/git/status.
type gitStatusResponse struct {
	Branch  string                  `json:"branch"`
	Ahead   int                     `json:"ahead"`
	Behind  int                     `json:"behind"`
	Changed []workspace.ChangedFile `json:"changed"`
}

// resolveWorkspaceCwd resolves a runtime's ACP project directory via the session
// layer, mapping session-layer errors to HTTP statuses. Returns the absolute
// cwd on success. The optional ?acpSessionId= picks one ACP session's project
// when the agent hosts several; without it the most recently opened one wins.
func (s *Server) resolveWorkspaceCwd(w http.ResponseWriter, r *http.Request, runtimeID string) (string, bool) {
	acpSessionID := strings.TrimSpace(r.URL.Query().Get("acpSessionId"))
	cwd, err := s.sessionSvc.WorkingDir(runtimeID, acpSessionID)
	switch {
	case err == nil:
		return cwd, true
	case errors.Is(err, session.ErrSessionNotFound), errors.Is(err, session.ErrCwdUnknown):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		s.logger.Error("resolve workspace cwd", "runtime_id", runtimeID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to resolve agent working directory")
	}
	return "", false
}

// handleWorkspaceFile reads a file inside a runtime's project directory.
// GET /v1/runtimes/{runtimeId}/files?path=<rel>
// Text files return a TextResourceContents; binary files return a
// BlobResourceContents (base64 blob). Both embed the absolute file URI and
// size/truncated gateway extensions.
func (s *Server) handleWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeRead); !ok {
		return
	}
	runtimeID := r.PathValue("runtimeId")
	cwd, ok := s.resolveWorkspaceCwd(w, r, runtimeID)
	if !ok {
		return
	}
	rel := strings.TrimSpace(r.URL.Query().Get("path"))
	abs, err := workspace.ResolveWithinRoot(cwd, rel)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}
	if info.IsDir() {
		writeError(w, http.StatusBadRequest, "path is a directory")
		return
	}
	data, err := workspace.ReadFileLimited(abs, workspace.MaxFileReadBytes)
	if err != nil {
		s.logger.Error("read workspace file", "path", abs, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to read file")
		return
	}
	truncated := len(data) > workspace.MaxFileReadBytes
	if truncated {
		data = data[:workspace.MaxFileReadBytes]
	}
	uri := workspace.FileURI(abs)
	mimeType := workspace.MimeTypeFor(rel)
	var mimePtr *string
	if mimeType != "" {
		mimePtr = &mimeType
	}
	if workspace.IsBinary(data) {
		writeJSON(w, http.StatusOK, blobReadResponse{
			BlobResourceContents: acp.BlobResourceContents{
				Blob:     base64.StdEncoding.EncodeToString(data),
				Uri:      uri,
				MimeType: mimePtr,
			},
			Size:      int(info.Size()),
			Truncated: truncated,
		})
		return
	}
	writeJSON(w, http.StatusOK, fileReadResponse{
		TextResourceContents: acp.TextResourceContents{
			Text:     string(data),
			Uri:      uri,
			MimeType: mimePtr,
		},
		Size:      int(info.Size()),
		Truncated: truncated,
	})
}

// handleWorkspaceGitStatus returns branch, ahead/behind, and changed files.
// GET /v1/runtimes/{runtimeId}/git/status
func (s *Server) handleWorkspaceGitStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeRead); !ok {
		return
	}
	runtimeID := r.PathValue("runtimeId")
	cwd, ok := s.resolveWorkspaceCwd(w, r, runtimeID)
	if !ok {
		return
	}
	ref := s.workspaceSvc.BaseRef(r.Context(), cwd)
	out, changed, err := workspace.Changes(r.Context(), cwd, ref)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	// Enrich tracked changes with per-file line counts from the diff against
	// HEAD — the same comparison /git/diff reports — so partially staged files
	// (both staged and unstaged edits) get counts for the whole HEAD → worktree
	// delta. Untracked files are not covered by git diff, so count their lines
	// directly from disk.
	// --relative keys numstat by cwd-relative path, matching git status, and
	// drops changes outside cwd.
	numstat, err := workspace.RunGit(r.Context(), cwd, "diff", ref, "--numstat", "--relative")
	if err != nil && ref == "HEAD" {
		// No HEAD yet (fresh repo with staged-but-uncommitted files): git diff
		// HEAD fails, so fall back to the staged diff (index vs the empty tree)
		// to still report real counts for tracked changes.
		numstat, err = workspace.RunGit(r.Context(), cwd, "diff", "--cached", "--numstat", "--relative")
	}
	if err == nil {
		stats := workspace.ParseGitNumstat(numstat)
		for i := range changed {
			// Rename entries report their numstat path as the braced
			// `{old => new}` form, which never matches changed[i].Path, and a
			// pure rename has 0/0 counts anyway, so skip them.
			if changed[i].Status == "R" {
				continue
			}
			if s, ok := stats[changed[i].Path]; ok {
				changed[i].Added = s.Added
				changed[i].Removed = s.Removed
				changed[i].Binary = s.Binary
			}
		}
	}
	for i := range changed {
		if changed[i].Status == "?" {
			added, binary := workspace.CountFileLines(filepath.Join(cwd, filepath.FromSlash(changed[i].Path)))
			changed[i].Added = added
			changed[i].Binary = binary
		}
	}

	resp := gitStatusResponse{Branch: "", Changed: changed}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "# branch.head "):
			resp.Branch = strings.TrimSpace(strings.TrimPrefix(line, "# branch.head "))
		case strings.HasPrefix(line, "# branch.ab "):
			var ahead, behind int
			if _, err := fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(line, "# branch.ab ")), "+%d -%d", &ahead, &behind); err != nil {
				continue
			}
			resp.Ahead, resp.Behind = ahead, behind
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleWorkspaceGitDiff returns ACP ToolCallContentDiff entries: a single
// object with ?path=, otherwise a JSON array with one entry per changed file
// (the same set /git/status reports). oldText is the committed version
// (git show HEAD:<path>), newText the working-tree copy. This replaces the
// old raw unified-patch shape {path?,diff}. GET /v1/runtimes/{runtimeId}/git/diff
func (s *Server) handleWorkspaceGitDiff(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeRead); !ok {
		return
	}
	runtimeID := r.PathValue("runtimeId")
	cwd, ok := s.resolveWorkspaceCwd(w, r, runtimeID)
	if !ok {
		return
	}
	if commit := strings.TrimSpace(r.URL.Query().Get("commit")); commit != "" {
		s.writeCommitFileDiff(w, r, cwd, commit)
		return
	}
	if rel := strings.TrimSpace(r.URL.Query().Get("path")); rel != "" {
		abs, err := workspace.ResolveWithinRoot(cwd, rel)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// Submodules and directories are not diffable file content; reject them
		// with a clear 400 instead of letting git show/read fail into a 422.
		// Whole-tree diffs skip them via IsDir; this is the single-file guard.
		if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
			writeError(w, http.StatusBadRequest, "path is a directory or submodule, not a file: "+rel)
			return
		}
		entry, err := workspace.DiffEntry(r.Context(), cwd, s.workspaceSvc.BaseRef(r.Context(), cwd), rel, abs)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, entry)
		return
	}

	// Whole-tree: enumerate changed files via git status and emit one entry
	// per file that has readable or deleted working content.
	ref := s.workspaceSvc.BaseRef(r.Context(), cwd)
	_, changed, err := workspace.Changes(r.Context(), cwd, ref)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	entries := make([]acp.ToolCallContentDiff, 0, len(changed))
	for _, f := range changed {
		// Submodules and collapsed untracked dirs are directories, not files;
		// they cannot be diffed as file content (git show HEAD:<path> fails
		// for a gitlink, ReadFileLimited fails on a dir). /git/status reports
		// them with isDir:true; the diff lists only real files.
		if f.IsDir {
			continue
		}
		abs, err := workspace.ResolveWithinRoot(cwd, f.Path)
		if err != nil {
			continue
		}
		entry, err := workspace.DiffEntry(r.Context(), cwd, ref, f.Path, abs)
		if err != nil {
			// Skip files we cannot build a diff for (e.g. unreadable), but
			// keep going for the rest.
			continue
		}
		entries = append(entries, entry)
	}
	writeJSON(w, http.StatusOK, entries)
}

// writeCommitFileDiff answers /git/diff?commit=<sha>&path=<rel>[&oldPath=<rel>]:
// one file of a commit against its first parent. The files are read from git
// objects, so they need not exist on disk; the paths are still traversal-checked.
func (s *Server) writeCommitFileDiff(w http.ResponseWriter, r *http.Request, cwd, commit string) {
	q := r.URL.Query()
	rel := strings.TrimSpace(q.Get("path"))
	if rel == "" {
		writeError(w, http.StatusBadRequest, "commit requires path")
		return
	}
	oldRel := strings.TrimSpace(q.Get("oldPath"))
	for _, p := range []string{rel, oldRel} {
		if p == "" {
			continue
		}
		if _, err := workspace.ResolveWithinRoot(cwd, p); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	sha, err := workspace.ResolveCommit(r.Context(), cwd, commit)
	if err != nil {
		writeWorkspaceError(w, err)
		return
	}
	entry, err := workspace.CommitDiffEntry(r.Context(), cwd, sha, rel, oldRel)
	if err != nil {
		writeWorkspaceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// handleWorkspaceGitLog returns one page of commit history, newest first.
// GET /v1/runtimes/{runtimeId}/git/log?head=<sha>&skip=<n>&limit=<n>
func (s *Server) handleWorkspaceGitLog(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeRead); !ok {
		return
	}
	cwd, ok := s.resolveWorkspaceCwd(w, r, r.PathValue("runtimeId"))
	if !ok {
		return
	}
	q := r.URL.Query()
	skip, limit := 0, 50
	var err error
	if v := q.Get("skip"); v != "" {
		if skip, err = strconv.Atoi(v); err != nil || skip < 0 {
			writeError(w, http.StatusBadRequest, "invalid skip")
			return
		}
	}
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = min(max(limit, 1), 200)
	}
	page, err := workspace.Log(r.Context(), cwd, strings.TrimSpace(q.Get("head")), skip, limit)
	if err != nil {
		writeWorkspaceError(w, err)
		return
	}
	if base := s.workspaceSvc.BaseRef(r.Context(), cwd); base != "HEAD" {
		page.BaseCommit = base
	}
	writeJSON(w, http.StatusOK, page)
}

// handleWorkspaceGitCommit returns the files a commit changed under cwd,
// compared with its first parent. GET /v1/runtimes/{runtimeId}/git/commits/{sha}
func (s *Server) handleWorkspaceGitCommit(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeRead); !ok {
		return
	}
	cwd, ok := s.resolveWorkspaceCwd(w, r, r.PathValue("runtimeId"))
	if !ok {
		return
	}
	sha, err := workspace.ResolveCommit(r.Context(), cwd, r.PathValue("sha"))
	if err != nil {
		writeWorkspaceError(w, err)
		return
	}
	changed, err := workspace.CommitChanges(r.Context(), cwd, sha)
	if err != nil {
		writeWorkspaceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sha": sha, "changed": changed})
}

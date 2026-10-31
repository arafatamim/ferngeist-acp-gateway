package api

import (
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
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
	"golang.org/x/sync/errgroup"
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

// fileListResponse is the payload for GET /v1/runtimes/{id}/files/list.
type fileListResponse struct {
	Files     []string `json:"files"`
	Truncated bool     `json:"truncated"`
}

// gitStatusResponse is the payload for GET /v1/runtimes/{id}/git/status.
type gitStatusResponse struct {
	Branch  string                  `json:"branch"`
	Ahead   int                     `json:"ahead"`
	Behind  int                     `json:"behind"`
	Changed []workspace.ChangedFile `json:"changed"`
	// Compare echoes the requested compare ref, and CompareCommit the sha it
	// resolved to. Both are absent unless an explicit, honoured ?compare= was
	// given, so a managed worktree's merge-base sha can never be mistaken for a
	// comparison.
	Compare       string `json:"compare,omitempty"`
	CompareCommit string `json:"compareCommit,omitempty"`
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

const maxWorkspaceFileBytes = 8 << 20

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
	data, err := workspace.ReadFileLimited(abs, maxWorkspaceFileBytes)
	if err != nil {
		s.logger.Error("read workspace file", "path", abs, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to read file")
		return
	}
	truncated := len(data) > maxWorkspaceFileBytes
	if truncated {
		data = data[:maxWorkspaceFileBytes]
	}
	uri := workspace.FileURI(abs)
	mimeType := workspace.MimeTypeFor(rel)
	var mimePtr *string
	if mimeType != "" {
		mimePtr = &mimeType
	}
	if workspace.IsBinary(data) || (strings.HasPrefix(mimeType, "image/") && !strings.HasPrefix(mimeType, "image/svg+xml")) {
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

// writeJSONGzip writes payload, gzipping the body when the request's
// Accept-Encoding includes gzip. Used by the file list and whole-tree diff, not
// a global middleware: the other endpoints' bodies are small and /files content
// is often already compressed. Vary is set on both paths for correct caching;
// the 404/422 error paths go through plain writeError on purpose, since error
// bodies are tiny.
func writeJSONGzip(w http.ResponseWriter, r *http.Request, status int, payload any) {
	w.Header().Set("Vary", "Accept-Encoding")
	if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		writeJSON(w, status, payload)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Encoding", "gzip")
	w.WriteHeader(status)
	gz := gzip.NewWriter(w)
	defer gz.Close()
	_ = json.NewEncoder(gz).Encode(payload)
}

// handleWorkspaceFileList lists a runtime's project files (tracked plus
// untracked, ignoring rules honoured). GET /v1/runtimes/{runtimeId}/files/list,
// shaped like handleWorkspaceFile: read scope, resolveWorkspaceCwd, then the
// workspace listing.
func (s *Server) handleWorkspaceFileList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeRead); !ok {
		return
	}
	cwd, ok := s.resolveWorkspaceCwd(w, r, r.PathValue("runtimeId"))
	if !ok {
		return
	}
	files, truncated, err := workspace.ListFiles(r.Context(), cwd, workspace.MaxFileListEntries)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSONGzip(w, r, http.StatusOK, fileListResponse{Files: files, Truncated: truncated})
}

// handleWorkspaceGitRefs lists the refs a client can compare against.
// GET /v1/runtimes/{runtimeId}/git/refs
func (s *Server) handleWorkspaceGitRefs(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeRead); !ok {
		return
	}
	cwd, ok := s.resolveWorkspaceCwd(w, r, r.PathValue("runtimeId"))
	if !ok {
		return
	}
	payload, err := workspace.Refs(r.Context(), cwd)
	if err != nil {
		writeWorkspaceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
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

	// Compare mode: an explicit ?compare= replaces HEAD (and a managed
	// worktree's base) with the target's tip for the file list, which comes
	// from the strict two-dot compare diff. The branch/ahead/behind header
	// still describes the checkout.
	resp := gitStatusResponse{}
	if compare := strings.TrimSpace(r.URL.Query().Get("compare")); compare != "" {
		sha, err := workspace.ResolveRef(r.Context(), cwd, compare)
		if err != nil {
			writeWorkspaceError(w, err)
			return
		}
		out, err := workspace.Status(r.Context(), cwd)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		changed, err := workspace.CompareChanges(r.Context(), cwd, sha)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		resp.Changed = changed
		resp.Compare, resp.CompareCommit = compare, sha
		parseBranchHeader(&resp, out)
		writeJSON(w, http.StatusOK, resp)
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

	resp.Changed = changed
	parseBranchHeader(&resp, out)
	writeJSON(w, http.StatusOK, resp)
}

// parseBranchHeader fills Branch/Ahead/Behind from `git status --branch`
// porcelain=v2 header lines.
func parseBranchHeader(resp *gitStatusResponse, out string) {
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

	// A compare ref replaces HEAD (or a managed worktree's base) as the old
	// side of every diff below: per-file and whole-tree alike, so `compare` is
	// never silently ignored. Commit mode above takes precedence and leaves it
	// unused.
	ref := s.workspaceSvc.BaseRef(r.Context(), cwd)
	compare := strings.TrimSpace(r.URL.Query().Get("compare"))
	if compare != "" {
		sha, err := workspace.ResolveRef(r.Context(), cwd, compare)
		if err != nil {
			writeWorkspaceError(w, err)
			return
		}
		ref = sha
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
		entry, err := workspace.DiffEntry(r.Context(), cwd, ref, rel, abs)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, entry)
		return
	}

	// Whole-tree: enumerate changed files and emit one entry per file that has
	// readable or deleted working content. In compare mode the set is the
	// strict two-dot one, so no phantom rows.
	var changed []workspace.ChangedFile
	var err error
	if compare != "" {
		changed, err = workspace.CompareChanges(r.Context(), cwd, ref)
	} else {
		_, changed, err = workspace.Changes(r.Context(), cwd, ref)
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	// One git show per file: run them in parallel (bounded, order kept) so a
	// large change set is not a serial chain of process spawns.
	slots := make([]*acp.ToolCallContentDiff, len(changed))
	var group errgroup.Group
	group.SetLimit(8)
	for i, f := range changed {
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
		group.Go(func() error {
			// Skip files we cannot build a diff for (e.g. unreadable), but
			// keep going for the rest.
			if entry, err := workspace.DiffEntry(r.Context(), cwd, ref, f.Path, abs); err == nil {
				slots[i] = &entry
			}
			return nil
		})
	}
	_ = group.Wait()
	entries := make([]acp.ToolCallContentDiff, 0, len(changed))
	for _, entry := range slots {
		if entry != nil {
			entries = append(entries, *entry)
		}
	}
	writeJSONGzip(w, r, http.StatusOK, entries)
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
// GET /v1/runtimes/{runtimeId}/git/log?head=<sha>&skip=<n>&limit=<n>&compare=<ref>&beyond=<n>
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
	beyond := 0
	if v := q.Get("beyond"); v != "" {
		if beyond, err = strconv.Atoi(v); err != nil || beyond < 0 {
			writeError(w, http.StatusBadRequest, "invalid beyond")
			return
		}
	}
	// Compare mode: ?compare= excludes that ref's commits from the walk, so the
	// page is the target..head range; workspace.Log validates the ref (a 400).
	page, err := workspace.Log(r.Context(), cwd, strings.TrimSpace(q.Get("head")), skip, limit, q.Get("compare"), beyond)
	if err != nil {
		writeWorkspaceError(w, err)
		return
	}
	if base := s.workspaceSvc.LogBase(r.Context(), cwd); base != "HEAD" {
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

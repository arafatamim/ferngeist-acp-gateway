package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/session"
	"github.com/coder/acp-go-sdk"
)

// maxFileReadBytes caps the size of a single file read by the workspace file
// endpoint. Larger files are truncated and the response flags truncated=true.
const maxFileReadBytes = 1 << 20 // 1 MiB

// gitCommandTimeout bounds every git invocation so a locked or hanging repo
// cannot stall the HTTP handler (the server WriteTimeout is 20s).
const gitCommandTimeout = 15 * time.Second

// errPathEscapesRoot is returned by resolveWithinRoot when a requested path is
// absolute, contains a traversal, or resolves through a symlink outside root.
var errPathEscapesRoot = errors.New("path escapes the agent's working directory")

// resolveWithinRoot resolves a relative path against root and returns an
// absolute path guaranteed to stay inside root. It rejects absolute paths,
// ".." traversal, and symlink escapes (both root and the joined path are
// eval-symlinked and re-checked). The caller must pass the cwd as root.
func resolveWithinRoot(root, rel string) (string, error) {
	if rel == "" {
		return "", errors.New("path is required")
	}
	if filepath.IsAbs(rel) {
		return "", errPathEscapesRoot
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	rootEval, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		rootEval = rootAbs
	}
	joined := filepath.Join(rootAbs, filepath.FromSlash(rel))
	if !subpathOf(joined, rootAbs) {
		return "", errPathEscapesRoot
	}
	// Resolve symlinks in the result if it exists (a dangling symlink's target
	// is untrusted even if the link itself parses inside root).
	joinedEval := joined
	if resolved, err := filepath.EvalSymlinks(joined); err == nil {
		joinedEval = resolved
	}
	if !subpathOf(joinedEval, rootEval) {
		return "", errPathEscapesRoot
	}
	return joined, nil
}

// subpathOf reports whether child == parent or child is lexically inside parent.
func subpathOf(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// gitChangedFile is one changed file from git status --porcelain, enriched
// with per-file line counts from git diff --numstat. Added/Removed are -1 for
// binary files (git reports "-" instead of a count) and for untracked files
// (git diff does not cover them; their line counts come from a raw file read).
type gitChangedFile struct {
	Path    string `json:"path"`
	Status  string `json:"status"` // single letter: M, A, D, R, U, etc.
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Binary  bool   `json:"binary"` // true when git reported "-" for the counts
	IsDir   bool   `json:"isDir"`  // true for submodules and collapsed untracked dirs
}

// parseGitStatus parses `git status --porcelain=v2` output into a list of
// changed files. The v2 format emits one `1 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <path>`
// line per tracked change and `2 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <X><score> <path>\t<origPath>`
// per rename/copy; the status is the X or Y of the XY pair (index status if
// non-'.', else worktree status). Rename/copy entries report the destination
// path (the field before the tab); the tab-separated origPath is the source.
// Untracked files are emitted as `? <path>` lines with status "?". Header
// lines (`#`, blank) are ignored. Paths are read as raw line suffixes (git
// runs with core.quotePath=false), so spaces inside a path parse correctly.
// Returns as many entries as it can parse.
func parseGitStatus(out string) []gitChangedFile {
	var files []gitChangedFile
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 2 {
			continue
		}
		switch {
		case line[0] == '?' && line[1] == ' ':
			// Untracked file: `? <path>`. With --untracked-files=all the path
			// is a file; without it Git collapses whole untracked directories
			// to a single `? <dir>/` entry, which is a directory.
			p := strings.TrimSpace(line[1:])
			files = append(files, gitChangedFile{Path: p, Status: "?", IsDir: strings.HasSuffix(p, "/")})
		case (line[0] == '1' || line[0] == '2') && line[1] == ' ':
			// Tracked change. Both formats share a fixed whitespace-separated
			// prefix (7 fields for format 1, 8 for format 2); the path is the
			// raw remainder of the line and may itself contain spaces
			// (core.quotePath=false), so it is not a whitespace token.
			prefix := 7
			if line[0] == '2' {
				prefix = 8
			}
			xy, rest, ok := afterFields(line[2:], 1)
			if !ok || rest == "" {
				continue
			}
			sub, rest, ok := afterFields(rest, 1)
			if !ok || rest == "" {
				continue
			}
			// Skip the remaining fixed fields (mH mI mW hH hI for format 1,
			// plus the rename score for format 2); the path is the raw
			// remainder and may contain spaces.
			_, rest, ok = afterFields(rest, prefix-2)
			if !ok || rest == "" {
				continue
			}
			path := rest
			if line[0] == '2' {
				// Rename/copy: `<dest>\t<orig>` — destination first.
				path, _, ok = strings.Cut(rest, "\t")
				if !ok || path == "" {
					continue
				}
			}
			status := string(xy[0])
			if status == "." {
				status = string(xy[1])
			}
			// sub is the abbreviated status per worktree: S..U marks a modified
			// submodule (a gitlink directory). A trailing slash marks a collapsed
			// untracked directory. Both are directories, not files.
			isDir := strings.HasSuffix(path, "/") || strings.HasPrefix(sub, "S")
			files = append(files, gitChangedFile{Path: path, Status: status, IsDir: isDir})
		}
	}
	return files
}

// afterFields removes the first n space-separated fields from s and returns
// the first field plus the raw remainder. It reports ok=false when s has
// fewer than n fields. The remainder is returned verbatim (spaces inside the
// path are preserved).
func afterFields(s string, n int) (first, rest string, ok bool) {
	first = s
	for i := range n {
		sp := strings.IndexByte(s, ' ')
		if sp < 0 {
			return "", "", false
		}
		if i == 0 {
			first = s[:sp]
		}
		s = s[sp+1:]
	}
	return first, s, true
}

// parseGitNumstat parses `git diff --numstat` output into a map keyed by
// repo-relative path. Each line is `<added>\t<deleted>\t<path>`. Binary files
// report "-" for both counts; the parser flags them with Binary=true and zero
// counts. Rename/copy entries render their path as the braced `{old => new}`
// form (e.g. `arch/{i386 => x86}/Makefile`), which does not match the
// destination path `git status --porcelain=v2` reports, so rename status
// entries do not get numstat counts. Returns as many entries as it can parse;
// unparseable lines are skipped.
func parseGitNumstat(out string) map[string]gitChangedFile {
	stats := make(map[string]gitChangedFile)
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 3 {
			continue
		}
		added, addedErr := strconv.Atoi(parts[0])
		removed, removedErr := strconv.Atoi(parts[1])
		path := parts[len(parts)-1]
		entry := gitChangedFile{Path: path}
		switch {
		case addedErr == nil && removedErr == nil:
			entry.Added, entry.Removed = added, removed
		default:
			// Binary file: git prints "-" for the counts.
			entry.Binary = true
		}
		stats[path] = entry
	}
	return stats
}

// countFileLines counts the lines in a file for untracked-file status entries.
// It returns the line count (0 for empty or unreadable files) and whether the
// file is binary (contains a NUL byte). Reading is capped at maxFileReadBytes
// so a huge untracked file cannot exhaust memory; line count is approximate
// beyond the cap.
func countFileLines(path string) (lines int, binary bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	buf := make([]byte, 32*1024)
	total := 0
	lastEndedWithNL := true
	for {
		n, err := f.Read(buf)
		if n > 0 {
			total += n
			if bytes.IndexByte(buf[:n], 0) >= 0 {
				return 0, true
			}
			lines += bytes.Count(buf[:n], []byte("\n"))
			lastEndedWithNL = buf[n-1] == '\n'
		}
		if err != nil {
			break
		}
		if total > maxFileReadBytes {
			break
		}
	}
	if !lastEndedWithNL {
		lines++
	}
	return lines, false
}

// runGit runs `git -C dir <args...>` with a 15s timeout and returns combined
// stdout. It returns an error wrapping the git stderr when git exits non-zero or
// the binary is missing.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitCommandTimeout)
	defer cancel()
	// core.quotePath=false keeps paths with spaces/non-ASCII unquoted and
	// unescaped in porcelain/numstat output, so they parse and open correctly.
	full := append([]string{"-c", "core.quotePath=false", "-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return string(out), nil
}

// runGitLimited runs a git command and returns at most max+1 bytes of stdout
// (plus stderr, which is small). Use it for subcommands whose output can be
// arbitrarily large (e.g. `git show HEAD:<path>`): like readFileLimited it
// streams the output through a limit instead of buffering the whole thing, so
// a huge blob cannot exhaust memory. stdout is always read to EOF, even past
// the cap, so the child can finish (or hit the gitCommandTimeout) and never
// block on a full pipe.
func runGitLimited(ctx context.Context, dir string, max int, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitCommandTimeout)
	defer cancel()
	// core.quotePath=false keeps paths with spaces/non-ASCII unquoted and
	// unescaped in porcelain/numstat output, so they parse and open correctly.
	full := append([]string{"-c", "core.quotePath=false", "-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), err)
	}
	out, readErr := io.ReadAll(io.LimitReader(stdout, int64(max)+1))
	waitErr := cmd.Wait()
	if waitErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = waitErr.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	if readErr != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), readErr)
	}
	return string(out), nil
}

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

// fileURI returns a file:// URI for an absolute path.
func fileURI(abs string) string {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	return u.String()
}

// mimeTypeFor returns a best-effort MIME type from a file path's extension,
// or "" when unknown.
func mimeTypeFor(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return ""
	}
	return mime.TypeByExtension(ext)
}

// isBinary reports whether data contains a NUL byte (the same heuristic git
// and countFileLines use to distinguish text from binary).
func isBinary(data []byte) bool {
	return bytes.IndexByte(data, 0) >= 0
}

// readFileLimited opens path and reads at most max+1 bytes. Callers detect
// truncation with len(data) > max. Unlike os.ReadFile, a file far larger than
// max is never buffered in full, bounding memory for arbitrarily large files.
func readFileLimited(path string, max int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return nil, err
	}
	return data, nil
}

// gitStatusResponse is the payload for GET /v1/runtimes/{id}/git/status.
type gitStatusResponse struct {
	Branch  string           `json:"branch"`
	Ahead   int              `json:"ahead"`
	Behind  int              `json:"behind"`
	Changed []gitChangedFile `json:"changed"`
}

// resolveWorkspaceCwd resolves a runtime's ACP project directory via the session
// layer, mapping session-layer errors to HTTP statuses. Returns the absolute
// cwd on success.
func (s *Server) resolveWorkspaceCwd(w http.ResponseWriter, r *http.Request, runtimeID string) (string, bool) {
	cwd, err := s.sessionSvc.WorkingDir(runtimeID)
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
	abs, err := resolveWithinRoot(cwd, rel)
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
	data, err := readFileLimited(abs, maxFileReadBytes)
	if err != nil {
		s.logger.Error("read workspace file", "path", abs, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to read file")
		return
	}
	truncated := len(data) > maxFileReadBytes
	if truncated {
		data = data[:maxFileReadBytes]
	}
	uri := fileURI(abs)
	mimeType := mimeTypeFor(rel)
	var mimePtr *string
	if mimeType != "" {
		mimePtr = &mimeType
	}
	if isBinary(data) {
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
	out, err := runGit(r.Context(), cwd, "status", "--porcelain=v2", "--branch", "--untracked-files=all")
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	changed := parseGitStatus(out)

	// Enrich tracked changes with per-file line counts from the diff against
	// HEAD — the same comparison /git/diff reports — so partially staged files
	// (both staged and unstaged edits) get counts for the whole HEAD → worktree
	// delta. Untracked files are not covered by git diff, so count their lines
	// directly from disk.
	numstat, err := runGit(r.Context(), cwd, "diff", "HEAD", "--numstat")
	if err != nil {
		// No HEAD yet (fresh repo with staged-but-uncommitted files): git diff
		// HEAD fails, so fall back to the staged diff (index vs the empty tree)
		// to still report real counts for tracked changes.
		numstat, err = runGit(r.Context(), cwd, "diff", "--cached", "--numstat")
	}
	if err == nil {
		stats := parseGitNumstat(numstat)
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
			added, binary := countFileLines(filepath.Join(cwd, filepath.FromSlash(changed[i].Path)))
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
	if rel := strings.TrimSpace(r.URL.Query().Get("path")); rel != "" {
		abs, err := resolveWithinRoot(cwd, rel)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// Submodules and directories are not diffable file content; reject them
		// with a clear 400 instead of letting git show/read fail into a 422.
		// Whole-tree diffs skip them via isDir; this is the single-file guard.
		if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
			writeError(w, http.StatusBadRequest, "path is a directory or submodule, not a file: "+rel)
			return
		}
		entry, err := s.gitDiffEntry(r.Context(), cwd, rel, abs)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, entry)
		return
	}

	// Whole-tree: enumerate changed files via git status and emit one entry
	// per file that has readable or deleted working content.
	out, err := runGit(r.Context(), cwd, "status", "--porcelain=v2", "--branch", "--untracked-files=all")
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	changed := parseGitStatus(out)
	entries := make([]acp.ToolCallContentDiff, 0, len(changed))
	for _, f := range changed {
		// Submodules and collapsed untracked dirs are directories, not files;
		// they cannot be diffed as file content (git show HEAD:<path> fails
		// for a gitlink, readFileLimited fails on a dir). /git/status reports
		// them with isDir:true; the diff lists only real files.
		if f.IsDir {
			continue
		}
		abs, err := resolveWithinRoot(cwd, f.Path)
		if err != nil {
			continue
		}
		entry, err := s.gitDiffEntry(r.Context(), cwd, f.Path, abs)
		if err != nil {
			// Skip files we cannot build a diff for (e.g. unreadable), but
			// keep going for the rest.
			continue
		}
		entries = append(entries, entry)
	}
	writeJSON(w, http.StatusOK, entries)
}

// gitDiffEntry builds one ToolCallContentDiff for a changed file. oldText is
// the committed version from git show HEAD:<path> (nil when that fails, e.g.
// new/untracked files or no HEAD). newText is the working-tree content, or ""
// for a deleted file (working copy gone but present in HEAD). The 1 MiB cap is
// applied to both texts; truncation is documented in docs/api.md.
func (s *Server) gitDiffEntry(ctx context.Context, cwd, rel, abs string) (acp.ToolCallContentDiff, error) {
	oldText, err := runGitLimited(ctx, cwd, maxFileReadBytes, "show", "HEAD:"+rel)
	var oldPtr *string
	if err == nil {
		oldPtr = truncateStringPtr(oldText, maxFileReadBytes)
	}
	newData, err := readFileLimited(abs, maxFileReadBytes)
	if err != nil {
		// Working copy gone. If the file exists in HEAD, treat it as a
		// deletion (oldText present, newText empty). Otherwise there is no
		// meaningful entry.
		if oldPtr != nil {
			return acp.ToolCallContentDiff{
				Path:    rel,
				OldText: oldPtr,
				NewText: "",
				Type:    "diff",
			}, nil
		}
		return acp.ToolCallContentDiff{}, err
	}
	if len(newData) > maxFileReadBytes {
		newData = newData[:maxFileReadBytes]
	}
	return acp.ToolCallContentDiff{
		Path:    rel,
		OldText: oldPtr,
		NewText: string(newData),
		Type:    "diff",
	}, nil
}

// truncateStringPtr returns a pointer to s truncated to max bytes, or nil when
// s is empty.
func truncateStringPtr(s string, max int) *string {
	if len(s) > max {
		s = s[:max]
	}
	if s == "" {
		return nil
	}
	return &s
}

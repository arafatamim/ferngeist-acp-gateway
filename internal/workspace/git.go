// Package workspace holds the gateway's git and filesystem logic: safe path
// resolution, git plumbing, changed-file parsing, and managed git worktrees.
// The HTTP layer keeps only request decoding and response encoding.
package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"
)

// MaxFileReadBytes caps the size of a single file read by the workspace file
// endpoint. Larger files are truncated and the response flags truncated=true.
const MaxFileReadBytes = 1 << 20 // 1 MiB

// GitCommandTimeout bounds every git invocation so a locked or hanging repo
// cannot stall the HTTP handler (the server WriteTimeout is 20s).
const GitCommandTimeout = 15 * time.Second

// ErrPathEscapesRoot is returned by ResolveWithinRoot when a requested path is
// absolute, contains a traversal, or resolves through a symlink outside root.
var ErrPathEscapesRoot = errors.New("path escapes the agent's working directory")

// ResolveWithinRoot resolves a relative path against root and returns an
// absolute path guaranteed to stay inside root. It rejects absolute paths,
// ".." traversal, and symlink escapes (both root and the joined path are
// eval-symlinked and re-checked). The caller must pass the cwd as root.
func ResolveWithinRoot(root, rel string) (string, error) {
	if rel == "" {
		return "", errors.New("path is required")
	}
	if filepath.IsAbs(rel) {
		return "", ErrPathEscapesRoot
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
	if !SubpathOf(joined, rootAbs) {
		return "", ErrPathEscapesRoot
	}
	// Resolve symlinks in the result if it exists (a dangling symlink's target
	// is untrusted even if the link itself parses inside root).
	joinedEval := joined
	if resolved, err := filepath.EvalSymlinks(joined); err == nil {
		joinedEval = resolved
	}
	if !SubpathOf(joinedEval, rootEval) {
		return "", ErrPathEscapesRoot
	}
	return joined, nil
}

// SubpathOf reports whether child == parent or child is lexically inside parent.
func SubpathOf(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// ChangedFile is one changed file from git status --porcelain, enriched with
// per-file line counts from git diff --numstat. Added/Removed are -1 for binary
// files (git reports "-" instead of a count) and for untracked files (git diff
// does not cover them; their line counts come from a raw file read).
type ChangedFile struct {
	Path    string `json:"path"`
	Status  string `json:"status"` // single letter: M, A, D, R, U, etc.
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Binary  bool   `json:"binary"`            // true when git reported "-" for the counts
	IsDir   bool   `json:"isDir"`             // true for submodules and collapsed untracked dirs
	OldPath string `json:"oldPath,omitempty"` // rename/copy source; set by CommitChanges only
}

// ParseGitStatus parses `git status --porcelain=v2` output into a list of
// changed files. The v2 format emits one `1 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <path>`
// line per tracked change and `2 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <X><score> <path>\t<origPath>`
// per rename/copy; the status is the X or Y of the XY pair (index status if
// non-'.', else worktree status). Rename/copy entries report the destination
// path (the field before the tab); the tab-separated origPath is the source.
// Unmerged (conflicted) entries are `u <XY> <sub> <m1> <m2> <m3> <mW> <h1> <h2> <h3> <path>`
// and report status "U" whatever the XY pair.
// Untracked files are emitted as `? <path>` lines with status "?". Header
// lines (`#`, blank) are ignored. Paths are read as raw line suffixes (git
// runs with core.quotePath=false), so spaces inside a path parse correctly.
// Returns as many entries as it can parse.
func ParseGitStatus(out string) []ChangedFile {
	var files []ChangedFile
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
			files = append(files, ChangedFile{Path: p, Status: "?", IsDir: strings.HasSuffix(p, "/")})
		case (line[0] == '1' || line[0] == '2' || line[0] == 'u') && line[1] == ' ':
			// Tracked change. All formats share a fixed whitespace-separated
			// prefix (7 fields for format 1, 8 for format 2, 9 for unmerged);
			// the path is the raw remainder of the line and may itself contain
			// spaces (core.quotePath=false), so it is not a whitespace token.
			prefix := 7
			switch line[0] {
			case '2':
				prefix = 8
			case 'u':
				prefix = 9
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
			if line[0] == 'u' {
				status = "U"
			}
			// sub is the abbreviated status per worktree: S..U marks a modified
			// submodule (a gitlink directory). A trailing slash marks a collapsed
			// untracked directory. Both are directories, not files.
			isDir := strings.HasSuffix(path, "/") || strings.HasPrefix(sub, "S")
			files = append(files, ChangedFile{Path: path, Status: status, IsDir: isDir})
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

// ParseGitNumstat parses `git diff --numstat` output into a map keyed by
// repo-relative path. Each line is `<added>\t<deleted>\t<path>`. Binary files
// report "-" for both counts; the parser flags them with Binary=true and zero
// counts. Rename/copy entries render their path as the braced `{old => new}`
// form (e.g. `arch/{i386 => x86}/Makefile`), which does not match the
// destination path `git status --porcelain=v2` reports, so rename status
// entries do not get numstat counts. Returns as many entries as it can parse;
// unparseable lines are skipped.
func ParseGitNumstat(out string) map[string]ChangedFile {
	stats := make(map[string]ChangedFile)
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
		entry := ChangedFile{Path: path}
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

// CountFileLines counts the lines in a file for untracked-file status entries.
// It returns the line count (0 for empty or unreadable files) and whether the
// file is binary (contains a NUL byte). Reading is capped at MaxFileReadBytes
// so a huge untracked file cannot exhaust memory; line count is approximate
// beyond the cap.
func CountFileLines(path string) (lines int, binary bool) {
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
		if total > MaxFileReadBytes {
			break
		}
	}
	if !lastEndedWithNL {
		lines++
	}
	return lines, false
}

// RunGit runs `git -C dir <args...>` with a 15s timeout and returns combined
// stdout. It returns an error wrapping the git stderr when git exits non-zero or
// the binary is missing.
func RunGit(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, GitCommandTimeout)
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

// RunGitLimited runs a git command and returns at most max+1 bytes of stdout
// (plus stderr, which is small). Use it for subcommands whose output can be
// arbitrarily large (e.g. `git show HEAD:<path>`): like ReadFileLimited it
// streams the output through a limit instead of buffering the whole thing, so
// a huge blob cannot exhaust memory. stdout is always read to EOF, even past
// the cap, so the child can finish (or hit the GitCommandTimeout) and never
// block on a full pipe.
func RunGitLimited(ctx context.Context, dir string, max int, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, GitCommandTimeout)
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

// Status runs `git status --porcelain=v2` scoped to cwd. The `-- .` pathspec
// keeps changes elsewhere in the repo out when the agent works in a
// subdirectory; v2 reports paths relative to cwd, which is what every consumer
// (ResolveWithinRoot, CountFileLines, numstat --relative) expects.
func Status(ctx context.Context, cwd string) (string, error) {
	return RunGit(ctx, cwd, "status", "--porcelain=v2", "--branch", "--untracked-files=all", "--", ".")
}

// Changes returns the porcelain status output and the changed files relative to
// ref. For ref "HEAD" that is just git status; for a managed worktree's base
// commit it also adds tracked files that differ from the base but are clean in
// the worktree (i.e. already committed by the agent).
func Changes(ctx context.Context, cwd, ref string) (string, []ChangedFile, error) {
	out, err := Status(ctx, cwd)
	if err != nil {
		return "", nil, err
	}
	changed := ParseGitStatus(out)
	if ref == "HEAD" {
		return out, changed, nil
	}
	nameStatus, err := RunGit(ctx, cwd, "diff", "--name-status", "--relative", ref, "--", ".")
	if err != nil {
		return "", nil, err
	}
	seen := make(map[string]bool, len(changed))
	for _, f := range changed {
		seen[f.Path] = true
	}
	for _, line := range strings.Split(nameStatus, "\n") {
		// `<status>\t<path>`, or `R<score>\t<old>\t<new>` for renames/copies.
		fields := strings.Split(line, "\t")
		if len(fields) < 2 || fields[0] == "" {
			continue
		}
		path := fields[len(fields)-1]
		if seen[path] {
			continue
		}
		seen[path] = true
		changed = append(changed, ChangedFile{Path: path, Status: fields[0][:1]})
	}
	return out, changed, nil
}

// DiffEntry builds one ACP ToolCallContentDiff for a changed file. oldText is
// the version at ref (HEAD, or a managed worktree's base) from git show (nil
// when that fails, e.g. new/untracked files or no HEAD). newText is the
// working-tree content, or "" for a deleted file (working copy gone but present
// in HEAD). The 1 MiB cap is applied to both texts; truncation is documented in
// docs/api.md.
func DiffEntry(ctx context.Context, cwd, ref, rel, abs string) (acp.ToolCallContentDiff, error) {
	// <ref>:<path> is repo-root relative; <ref>:./<path> resolves against cwd,
	// which is what rel is relative to.
	oldText, err := RunGitLimited(ctx, cwd, MaxFileReadBytes, "show", ref+":./"+rel)
	var oldPtr *string
	if err == nil {
		oldPtr = TruncateStringPtr(oldText, MaxFileReadBytes)
	}
	newData, err := ReadFileLimited(abs, MaxFileReadBytes)
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
	if len(newData) > MaxFileReadBytes {
		newData = newData[:MaxFileReadBytes]
	}
	return acp.ToolCallContentDiff{
		Path:    rel,
		OldText: oldPtr,
		NewText: string(newData),
		Type:    "diff",
	}, nil
}

// TruncateStringPtr returns a pointer to s truncated to max bytes, or nil when
// s is empty.
func TruncateStringPtr(s string, max int) *string {
	if len(s) > max {
		s = s[:max]
	}
	if s == "" {
		return nil
	}
	return &s
}

// FileURI returns a file:// URI for an absolute path.
func FileURI(abs string) string {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	return u.String()
}

// MimeTypeFor returns a best-effort MIME type from a file path's extension, or
// "" when unknown.
func MimeTypeFor(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return ""
	}
	return mime.TypeByExtension(ext)
}

// IsBinary reports whether data contains a NUL byte (the same heuristic git and
// CountFileLines use to distinguish text from binary).
func IsBinary(data []byte) bool {
	return bytes.IndexByte(data, 0) >= 0
}

// ReadFileLimited opens path and reads at most max+1 bytes. Callers detect
// truncation with len(data) > max. Unlike os.ReadFile, a file far larger than
// max is never buffered in full, bounding memory for arbitrarily large files.
func ReadFileLimited(path string, max int) ([]byte, error) {
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

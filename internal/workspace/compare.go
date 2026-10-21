package workspace

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
)

// validRefName reports whether name is a plausible ref name before it reaches
// git. It rejects what git would read as an option or as revision syntax
// rather than a ref name: an empty name, a leading "-", whitespace and control
// characters, "..", "@{", and the ref-name punctuation "~^:?*[\".
func validRefName(name string) bool {
	if name == "" || strings.HasPrefix(name, "-") {
		return false
	}
	for _, r := range name {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	if strings.Contains(name, "..") || strings.Contains(name, "@{") {
		return false
	}
	return !strings.ContainsAny(name, "~^:?*[\\")
}

// ResolveRef validates a client-supplied ref name and returns the full commit
// sha it names. It is the injection boundary for the compare query parameters,
// the same reason shaPattern exists for commit shas: a malformed name
// (option-like, whitespace, control characters, or git revision punctuation)
// and an unknown name are both a 400, but only inside a repository: a cwd that
// is not one is a 422, like the other workspace endpoints, since no name could
// resolve there.
func ResolveRef(ctx context.Context, cwd, name string) (string, error) {
	if !validRefName(name) {
		return "", errStatus(http.StatusBadRequest, "invalid ref: "+name)
	}
	commit, ok := commitOf(ctx, cwd, name)
	if !ok {
		// Only on the failure path, so a resolvable name costs no second process.
		if _, repoErr := RunGit(ctx, cwd, "rev-parse", "--git-dir"); repoErr != nil {
			return "", errStatus(http.StatusUnprocessableEntity, repoErr.Error())
		}
		return "", errStatus(http.StatusBadRequest, "unknown ref: "+name)
	}
	return commit, nil
}

// CompareChanges lists the files that differ between the working tree (with
// uncommitted work included) and ref's tip — a two-dot, tip-to-tip comparison,
// not a merge-base one. The path set and its status letters come from
// `git diff --name-status <ref>` rather than porcelain status, so a file that
// is dirty against HEAD but identical to ref is not listed (no phantom rows).
// Untracked files are appended as "?" with their line counts read from disk,
// and tracked counts come from `git diff <ref> --numstat`. The list is flat:
// there is no staged/unstaged grouping and Staged is always false.
//
// ref is expected to be a commit sha resolved by ResolveRef (the handler passes
// one), which is what keeps the comparison injection-safe.
func CompareChanges(ctx context.Context, cwd, ref string) ([]ChangedFile, error) {
	// -z keeps paths containing spaces or non-ASCII unquoted and unambiguous.
	others, err := RunGit(ctx, cwd, "ls-files", "-z", "--others", "--exclude-standard", "--", ".")
	if err != nil {
		return nil, err
	}
	var untrackedFiles []string
	untracked := map[string]bool{}
	for _, path := range splitNullPaths(others) {
		untracked[path] = true
		untrackedFiles = append(untrackedFiles, path)
	}

	out, err := RunGit(ctx, cwd, "diff", "--name-status", "--relative", ref, "--", ".")
	if err != nil {
		return nil, err
	}
	tracked := nameStatusChanges(out, map[string]bool{})
	changed := make([]ChangedFile, 0, len(tracked)+len(untrackedFiles))
	for _, f := range tracked {
		// A path the diff calls deleted but that sits untracked in the worktree
		// is not gone: git diff cannot see untracked files, so the "?" entry
		// below is the honest one. Keeping the D too would list the same path
		// twice and claim a file on disk was deleted.
		if f.Status == "D" && untracked[f.Path] {
			continue
		}
		changed = append(changed, f)
	}
	for _, path := range untrackedFiles {
		// ls-files reports an untracked nested repository as `dir/`.
		changed = append(changed, ChangedFile{Path: path, Status: "?", IsDir: strings.HasSuffix(path, "/")})
	}

	numstat, err := RunGit(ctx, cwd, "diff", ref, "--numstat", "--relative")
	if err != nil {
		return nil, err
	}
	stats := ParseGitNumstat(numstat)
	for i := range changed {
		f := &changed[i]
		switch f.Status {
		case "?":
			// Untracked entries are not covered by git diff; count from disk.
			if !f.IsDir {
				f.Added, f.Binary = CountFileLines(filepath.Join(cwd, filepath.FromSlash(f.Path)))
			}
		case "R":
			// Rename entries report their numstat path as the braced
			// `{old => new}` form, which never matches f.Path, and a pure
			// rename has 0/0 counts anyway.
		default:
			if s, ok := stats[f.Path]; ok {
				f.Added, f.Removed, f.Binary = s.Added, s.Removed, s.Binary
			}
		}
	}
	return changed, nil
}

// RefsPayload is the /git/refs response: the refs a client can compare
// against, plus the default target for labelling.
type RefsPayload struct {
	Default string `json:"default"`
	Refs    []Ref  `json:"refs"`
}

// Refs lists the repository's local branches, remote-tracking refs (with
// origin/HEAD-style aliases dropped) and tags for the compare picker, in
// for-each-ref order: heads, then remotes, then tags. The checked-out branch is
// marked IsHead; nothing is marked when HEAD is detached. A non-repo is a 422,
// like the other workspace endpoints.
func Refs(ctx context.Context, cwd string) (RefsPayload, error) {
	out, err := RunGit(ctx, cwd, "for-each-ref", "--format=%(refname)", "refs/heads", "refs/remotes", "refs/tags")
	if err != nil {
		return RefsPayload{}, errStatus(http.StatusUnprocessableEntity, err.Error())
	}
	head := strings.TrimSpace(gitOrEmpty(ctx, cwd, "symbolic-ref", "--quiet", "--short", "HEAD"))
	payload := RefsPayload{Refs: []Ref{}}
	for _, line := range strings.Split(out, "\n") {
		full := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(full, "refs/heads/"):
			name := strings.TrimPrefix(full, "refs/heads/")
			payload.Refs = append(payload.Refs, Ref{Name: name, Kind: "branch", IsHead: name == head})
		case strings.HasPrefix(full, "refs/remotes/"):
			name := strings.TrimPrefix(full, "refs/remotes/")
			if strings.HasSuffix(name, "/HEAD") {
				continue
			}
			payload.Refs = append(payload.Refs, Ref{Name: name, Kind: "remote"})
		case strings.HasPrefix(full, "refs/tags/"):
			payload.Refs = append(payload.Refs, Ref{Name: strings.TrimPrefix(full, "refs/tags/"), Kind: "tag"})
		}
	}
	payload.Default = defaultRef(ctx, cwd, payload.Refs)
	return payload, nil
}

// defaultRef picks the picker's labelling hint: origin/HEAD's branch when a
// local branch of that name exists, else the resolved short name (e.g.
// "origin/main"), else a local main/master, else "". The client preselects
// it as the compare target when the checkout is on another branch.
func defaultRef(ctx context.Context, cwd string, refs []Ref) string {
	local := make(map[string]bool, len(refs))
	for _, r := range refs {
		if r.Kind == "branch" {
			local[r.Name] = true
		}
	}
	if target := strings.TrimSpace(gitOrEmpty(ctx, cwd, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")); target != "" {
		if name, ok := strings.CutPrefix(target, "origin/"); ok && local[name] {
			return name
		}
		return target
	}
	for _, name := range []string{"main", "master"} {
		if local[name] {
			return name
		}
	}
	return ""
}

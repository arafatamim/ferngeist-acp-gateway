package workspace

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/coder/acp-go-sdk"
)

const (
	// maxLogBytes caps one /git/log page; the partial last record is dropped.
	maxLogBytes = 2 << 20
	// maxBodyBytes caps each commit body in a log page.
	maxBodyBytes = 4 << 10
	// maxUnpushedBytes caps the unpushed-commit listing (about 24k shas).
	maxUnpushedBytes = 1 << 20
)

// shaPattern rejects anything that is not a hex object name, which also rules
// out option injection ("-…") into the git command line.
var shaPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// Ref is one decoration on a commit.
type Ref struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"` // head, branch, remote, tag
	IsHead bool   `json:"isHead"`
}

// Commit is one entry of a /git/log page.
type Commit struct {
	SHA         string   `json:"sha"`
	Parents     []string `json:"parents"`
	Subject     string   `json:"subject"`
	Body        string   `json:"body"`
	AuthorName  string   `json:"authorName"`
	AuthorEmail string   `json:"authorEmail"`
	AuthorTime  int64    `json:"authorTime"`
	CommitTime  int64    `json:"commitTime"`
	Pushed      bool     `json:"pushed"`
	Refs        []Ref    `json:"refs"`
}

// LogPage is the /git/log response.
type LogPage struct {
	Head       string   `json:"head"`
	HasMore    bool     `json:"hasMore"`
	RemoteURL  string   `json:"remoteUrl"`
	BaseCommit string   `json:"baseCommit,omitempty"`
	Commits    []Commit `json:"commits"`
}

// ResolveCommit validates a client-supplied sha and returns the full object
// name of the commit it names. Malformed or unknown shas are a 400.
func ResolveCommit(ctx context.Context, cwd, sha string) (string, error) {
	if !shaPattern.MatchString(sha) {
		return "", errStatus(http.StatusBadRequest, "invalid commit sha")
	}
	out, err := RunGit(ctx, cwd, "rev-parse", "--verify", "--quiet", sha+"^{commit}")
	if err != nil {
		return "", errStatus(http.StatusBadRequest, "unknown commit: "+sha)
	}
	return strings.TrimSpace(out), nil
}

// Log returns up to limit commits reachable from head (HEAD when empty),
// skipping the first skip in topological order. The whole repo is walked, not
// just cwd: a pathspec would rewrite parents and break the client's graph.
func Log(ctx context.Context, cwd, head string, skip, limit int) (LogPage, error) {
	page := LogPage{Commits: []Commit{}}
	if head != "" {
		full, err := ResolveCommit(ctx, cwd, head)
		if err != nil {
			return page, err
		}
		head = full
	} else if out, err := RunGit(ctx, cwd, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err == nil {
		head = strings.TrimSpace(out)
	} else {
		// Empty repo or unborn branch; a non-repo still fails like /git/status.
		if _, err := RunGit(ctx, cwd, "rev-parse", "--git-dir"); err != nil {
			return page, errStatus(http.StatusUnprocessableEntity, err.Error())
		}
		return page, nil
	}
	// log.showSignature=true would print gpg text between records.
	args := []string{"-c", "log.showSignature=false", "log", "--no-color", "--topo-order", "--decorate=full",
		"--skip=" + strconv.Itoa(skip), "-n", strconv.Itoa(limit + 1),
		"--format=%H%x00%P%x00%an%x00%ae%x00%at%x00%ct%x00%D%x00%s%x00%b%x1e", head, "--"}
	out, err := RunGitLimited(ctx, cwd, maxLogBytes, args...)
	if err != nil {
		return page, errStatus(http.StatusUnprocessableEntity, err.Error())
	}
	commits, truncated := ParseGitLog(out, maxLogBytes)
	if len(commits) > limit {
		commits, truncated = commits[:limit], true
	}
	page.Head, page.Commits, page.HasMore = head, commits, truncated
	page.RemoteURL = StripRemoteCredentials(strings.TrimSpace(gitOrEmpty(ctx, cwd, "remote", "get-url", "origin")))
	markPushed(ctx, cwd, head, page.Commits)
	return page, nil
}

// gitOrEmpty returns git's output, or "" when the command fails. For optional
// lookups (origin URL, remote refs) where failure just means "none".
func gitOrEmpty(ctx context.Context, cwd string, args ...string) string {
	out, err := RunGit(ctx, cwd, args...)
	if err != nil {
		return ""
	}
	return out
}

// markPushed sets Pushed on every commit some origin/* branch contains. With
// no origin refs, or more unpushed commits than the cap, every commit stays
// unpushed: hiding the forge link is the safe failure.
func markPushed(ctx context.Context, cwd, head string, commits []Commit) {
	if len(commits) == 0 || strings.TrimSpace(gitOrEmpty(ctx, cwd, "for-each-ref", "--count=1", "refs/remotes/origin")) == "" {
		return
	}
	out, err := RunGitLimited(ctx, cwd, maxUnpushedBytes, "rev-list", head, "--not", "--remotes=origin")
	if err != nil || len(out) > maxUnpushedBytes {
		return
	}
	unpushed := make(map[string]bool)
	for _, sha := range strings.Fields(out) {
		unpushed[sha] = true
	}
	for i := range commits {
		commits[i].Pushed = !unpushed[commits[i].SHA]
	}
}

// ParseGitLog parses `git log --format=%H%x00%P%x00%an%x00%ae%x00%at%x00%ct%x00%D%x00%s%x00%b%x1e`
// output. When the output hit max bytes the last record may be cut, so it is
// dropped and truncated reports true.
func ParseGitLog(out string, max int) (commits []Commit, truncated bool) {
	records := strings.Split(out, "\x1e")
	if len(out) > max {
		records, truncated = records[:len(records)-1], true
	}
	for _, rec := range records {
		rec = strings.TrimPrefix(rec, "\n")
		if rec == "" {
			continue
		}
		// SplitN keeps a stray NUL in the body inside the body.
		f := strings.SplitN(rec, "\x00", 9)
		if len(f) < 9 {
			continue
		}
		at, _ := strconv.ParseInt(f[4], 10, 64)
		ct, _ := strconv.ParseInt(f[5], 10, 64)
		body := strings.TrimRight(f[8], " \t\r\n")
		if len(body) > maxBodyBytes {
			body = body[:maxBodyBytes]
		}
		commits = append(commits, Commit{
			SHA:         f[0],
			Parents:     append([]string{}, strings.Fields(f[1])...),
			AuthorName:  f[2],
			AuthorEmail: f[3],
			AuthorTime:  at,
			CommitTime:  ct,
			Refs:        ParseRefs(f[6]),
			Subject:     f[7],
			Body:        body,
		})
	}
	return commits, truncated
}

// ParseRefs parses a `%D` decoration list produced with --decorate=full.
func ParseRefs(d string) []Ref {
	refs := []Ref{}
	for _, part := range strings.Split(d, ", ") {
		isHead := false
		if rest, ok := strings.CutPrefix(part, "HEAD -> "); ok {
			part, isHead = rest, true
		}
		switch {
		case part == "HEAD":
			refs = append(refs, Ref{Name: "HEAD", Kind: "head"})
		case strings.HasPrefix(part, "refs/heads/"):
			refs = append(refs, Ref{Name: strings.TrimPrefix(part, "refs/heads/"), Kind: "branch", IsHead: isHead})
		case strings.HasPrefix(part, "refs/remotes/"):
			name := strings.TrimPrefix(part, "refs/remotes/")
			if strings.HasSuffix(name, "/HEAD") {
				continue
			}
			refs = append(refs, Ref{Name: name, Kind: "remote"})
		case strings.HasPrefix(part, "tag: refs/tags/"):
			refs = append(refs, Ref{Name: strings.TrimPrefix(part, "tag: refs/tags/"), Kind: "tag"})
		}
	}
	return refs
}

// StripRemoteCredentials removes userinfo from URL-form remotes so a token in
// an https remote never leaves the gateway. scp-like `git@host:x` is returned
// unchanged (the user part is not a secret).
func StripRemoteCredentials(remote string) string {
	if !strings.Contains(remote, "://") {
		return remote
	}
	u, err := url.Parse(remote)
	if err != nil {
		return ""
	}
	u.User = nil
	return u.String()
}

// CommitChanges lists the files sha changed relative to its first parent (all
// files for a root commit), scoped to cwd.
func CommitChanges(ctx context.Context, cwd, sha string) ([]ChangedFile, error) {
	args := []string{"diff-tree", "-r", "-z", "-M", "--raw", "--numstat", "--relative"}
	if _, err := RunGit(ctx, cwd, "rev-parse", "--verify", "--quiet", sha+"^1"); err == nil {
		args = append(args, sha+"^1", sha)
	} else {
		args = append(args, "--root", "--no-commit-id", sha)
	}
	// RunGitLimited keeps stderr (e.g. rename-limit warnings) out of the parsed output.
	// shortcut: past the cap the tail of the list or its counts are lost; page it if commits that big matter.
	out, err := RunGitLimited(ctx, cwd, maxLogBytes, append(args, "--", ".")...)
	if err != nil {
		return nil, errStatus(http.StatusUnprocessableEntity, err.Error())
	}
	return ParseDiffTree(out), nil
}

// ParseDiffTree parses `git diff-tree -z --raw --numstat` output. Raw entries
// (`:<om> <nm> <os> <ns> <status>\0<path>\0`, renames `R<n>\0<old>\0<new>\0`)
// come first and give status and modes; numstat entries
// (`<a>\t<r>\t<path>\0`, renames `<a>\t<r>\t\0<old>\0<new>\0`) give counts.
func ParseDiffTree(out string) []ChangedFile {
	files := []ChangedFile{}
	index := map[string]int{}
	tok := strings.Split(out, "\x00")
	for i := 0; i < len(tok); i++ {
		t := tok[i]
		if strings.HasPrefix(t, ":") {
			f := strings.Fields(t)
			if len(f) < 5 || i+1 >= len(tok) {
				continue
			}
			cf := ChangedFile{Status: f[4][:1], IsDir: f[0] == ":160000" || f[1] == "160000"}
			if cf.Status == "R" || cf.Status == "C" {
				if i+2 >= len(tok) {
					continue
				}
				cf.OldPath, cf.Path = tok[i+1], tok[i+2]
				i += 2
			} else {
				cf.Path = tok[i+1]
				i++
			}
			index[cf.Path] = len(files)
			files = append(files, cf)
			continue
		}
		parts := strings.SplitN(t, "\t", 3)
		if len(parts) < 3 {
			continue
		}
		path := parts[2]
		if path == "" && i+2 < len(tok) {
			path = tok[i+2]
			i += 2
		}
		j, ok := index[path]
		if !ok {
			continue
		}
		added, aerr := strconv.Atoi(parts[0])
		removed, rerr := strconv.Atoi(parts[1])
		if aerr != nil || rerr != nil {
			files[j].Binary = true
			continue
		}
		files[j].Added, files[j].Removed = added, removed
	}
	return files
}

// CommitDiffEntry builds the diff of one file in sha against its first parent.
// oldRel is the pre-rename path ("" when not renamed). oldText is nil for an
// added file or a root commit; newText is "" for a deleted file.
func CommitDiffEntry(ctx context.Context, cwd, sha, rel, oldRel string) (acp.ToolCallContentDiff, error) {
	if t, err := RunGit(ctx, cwd, "cat-file", "-t", sha+":./"+rel); err == nil && strings.TrimSpace(t) == "commit" {
		return acp.ToolCallContentDiff{}, errStatus(http.StatusBadRequest, "path is a submodule, not a file: "+rel)
	}
	if oldRel == "" {
		oldRel = rel
	}
	var oldPtr *string
	if oldText, err := RunGitLimited(ctx, cwd, MaxFileReadBytes, "show", sha+"^1:./"+oldRel); err == nil {
		oldPtr = TruncateStringPtr(oldText, MaxFileReadBytes)
	}
	newText, err := RunGitLimited(ctx, cwd, MaxFileReadBytes, "show", sha+":./"+rel)
	if err != nil {
		if oldPtr == nil {
			return acp.ToolCallContentDiff{}, errStatus(http.StatusUnprocessableEntity, "path not in commit: "+rel)
		}
		newText = ""
	}
	if len(newText) > MaxFileReadBytes {
		newText = newText[:MaxFileReadBytes]
	}
	return acp.ToolCallContentDiff{Path: rel, OldText: oldPtr, NewText: newText, Type: "diff"}, nil
}

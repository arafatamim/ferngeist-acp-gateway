package workspace

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseRefs(t *testing.T) {
	got := ParseRefs("HEAD -> refs/heads/main, refs/remotes/origin/main, refs/remotes/origin/HEAD, tag: refs/tags/v1, refs/heads/dev")
	want := []Ref{
		{Name: "main", Kind: "branch", IsHead: true},
		{Name: "origin/main", Kind: "remote"},
		{Name: "v1", Kind: "tag"},
		{Name: "dev", Kind: "branch"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("refs = %+v, want %+v", got, want)
	}
	if got := ParseRefs("HEAD"); !reflect.DeepEqual(got, []Ref{{Name: "HEAD", Kind: "head"}}) {
		t.Fatalf("detached = %+v", got)
	}
	if got := ParseRefs(""); len(got) != 0 {
		t.Fatalf("empty = %+v", got)
	}
}

func TestParseGitLog(t *testing.T) {
	rec := func(sha, body string) string {
		return sha + "\x00p1 p2\x00A B\x00a@b\x0010\x0020\x00\x00subj\x00" + body + "\x1e"
	}
	out := rec("s1", "line1\n\nline2\n\n") + "\n" + rec("s2", "") + "\n" + rec("s3", "has\x00nul") + "\n"
	got, truncated := ParseGitLog(out, len(out))
	if truncated || len(got) != 3 {
		t.Fatalf("got %d commits, truncated=%v", len(got), truncated)
	}
	c := got[0]
	if c.SHA != "s1" || c.Body != "line1\n\nline2" || c.Subject != "subj" || c.AuthorTime != 10 || c.CommitTime != 20 ||
		!reflect.DeepEqual(c.Parents, []string{"p1", "p2"}) || c.AuthorName != "A B" {
		t.Fatalf("first = %+v", c)
	}
	if got[1].SHA != "s2" || got[1].Body != "" || got[2].Body != "has\x00nul" {
		t.Fatalf("rest = %+v", got[1:])
	}
	// Output past the cap: the cut last record is dropped.
	cut := rec("s1", "") + "\n" + rec("s2", "")[:10]
	got, truncated = ParseGitLog(cut, len(cut)-1)
	if !truncated || len(got) != 1 || got[0].SHA != "s1" {
		t.Fatalf("truncated parse = %+v, %v", got, truncated)
	}
}

func TestParseDiffTree(t *testing.T) {
	z := "\x00"
	out := ":100644 000000 a b D" + z + "gone" + z +
		":100644 100644 a b R090" + z + "old" + z + "new" + z +
		":000000 100644 a b A" + z + "bin" + z +
		":160000 160000 a b M" + z + "sub" + z +
		"0\t3\tgone" + z + "2\t1\t" + z + "old" + z + "new" + z + "-\t-\tbin" + z + "1\t1\tsub" + z
	want := []ChangedFile{
		{Path: "gone", Status: "D", Removed: 3},
		{Path: "new", OldPath: "old", Status: "R", Added: 2, Removed: 1},
		{Path: "bin", Status: "A", Binary: true},
		{Path: "sub", Status: "M", Added: 1, Removed: 1, IsDir: true},
	}
	if got := ParseDiffTree(out); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestStripRemoteCredentials(t *testing.T) {
	for in, want := range map[string]string{
		"https://user:tok@github.com/o/r.git": "https://github.com/o/r.git",
		"ssh://git@host:22/o/r":               "ssh://host:22/o/r",
		"git@github.com:o/r.git":              "git@github.com:o/r.git",
		"/srv/repo.git":                       "/srv/repo.git",
		"":                                    "",
	} {
		if got := StripRemoteCredentials(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func historyRepo(t *testing.T) (dir string, git func(...string) string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir = t.TempDir()
	git = func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("config", "commit.gpgSign", "false")
	// Must not leak gpg output into the parsed log.
	git("config", "log.showSignature", "true")
	return dir, git
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func statusOf(err error) int {
	var e Error
	if errors.As(err, &e) {
		return e.StatusCode()
	}
	return 0
}

func TestLog(t *testing.T) {
	ctx := context.Background()
	dir, git := historyRepo(t)

	page, err := Log(ctx, dir, "", 0, 50)
	if err != nil || page.Head != "" || len(page.Commits) != 0 || page.HasMore {
		t.Fatalf("empty repo = %+v, %v", page, err)
	}

	for _, msg := range []string{"one", "two", "three"} {
		git("commit", "-q", "--allow-empty", "-m", msg, "-m", "body of "+msg)
	}
	git("tag", "v1", "HEAD~1")

	page, err = Log(ctx, dir, "", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if page.Head != git("rev-parse", "HEAD") || !page.HasMore || len(page.Commits) != 2 {
		t.Fatalf("page 1 = %+v", page)
	}
	if c := page.Commits[0]; c.Subject != "three" || c.Body != "body of three" ||
		!reflect.DeepEqual(c.Refs, []Ref{{Name: "main", Kind: "branch", IsHead: true}}) || c.Pushed {
		t.Fatalf("head commit = %+v", c)
	}
	if page.Commits[1].Refs[0].Name != "v1" || page.RemoteURL != "" {
		t.Fatalf("page 1 = %+v", page)
	}

	// A commit made between pages does not shift the pinned page 2.
	git("commit", "-q", "--allow-empty", "-m", "four")
	page2, err := Log(ctx, dir, page.Head[:7], 2, 2)
	if err != nil || page2.HasMore || len(page2.Commits) != 1 || page2.Commits[0].Subject != "one" || page2.Head != page.Head {
		t.Fatalf("page 2 = %+v, %v", page2, err)
	}
	// Exactly limit commits left: no more.
	if p, _ := Log(ctx, dir, page.Head, 1, 2); p.HasMore || len(p.Commits) != 2 {
		t.Fatalf("exact page = %+v", p)
	}

	for _, bad := range []string{"xyz", "abc", "-n1", strings.Repeat("0", 40)} {
		if _, err := Log(ctx, dir, bad, 0, 2); statusOf(err) != http.StatusBadRequest {
			t.Fatalf("head %q: err = %v, want 400", bad, err)
		}
	}
	if _, err := Log(ctx, t.TempDir(), "", 0, 2); statusOf(err) != http.StatusUnprocessableEntity {
		t.Fatalf("non-repo err = %v, want 422", err)
	}

	// Pushed: commits on origin/* are pushed, later local ones are not.
	origin := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "--bare", origin).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	git("remote", "add", "origin", origin)
	git("push", "-q", "origin", "main")
	git("commit", "-q", "--allow-empty", "-m", "five")
	page, err = Log(ctx, dir, "", 0, 50)
	if err != nil || page.RemoteURL != origin || page.Commits[0].Pushed || !page.Commits[1].Pushed {
		t.Fatalf("pushed page = %+v, %v", page, err)
	}
}

func TestCommitChangesAndDiff(t *testing.T) {
	ctx := context.Background()
	dir, git := historyRepo(t)
	write(t, filepath.Join(dir, "sub", "a.txt"), "1\n2\n3\n4\n5\n")
	write(t, filepath.Join(dir, "top.txt"), "top\n")
	git("add", ".")
	git("commit", "-q", "-m", "root")
	root := git("rev-parse", "HEAD")
	sub := filepath.Join(dir, "sub")

	files, err := CommitChanges(ctx, sub, root)
	if err != nil || !reflect.DeepEqual(files, []ChangedFile{{Path: "a.txt", Status: "A", Added: 5}}) {
		t.Fatalf("root commit files = %+v, %v", files, err)
	}
	d, err := CommitDiffEntry(ctx, sub, root, "a.txt", "")
	if err != nil || d.OldText != nil || d.NewText != "1\n2\n3\n4\n5\n" {
		t.Fatalf("root diff = %+v, %v", d, err)
	}

	git("mv", "sub/a.txt", "sub/b.txt")
	write(t, filepath.Join(sub, "b.txt"), "1\n2\n3\n4\n5\n6\n")
	git("commit", "-q", "-am", "rename")
	rename := git("rev-parse", "HEAD")
	files, err = CommitChanges(ctx, sub, rename)
	if err != nil || len(files) != 1 || files[0].Path != "b.txt" || files[0].OldPath != "a.txt" || files[0].Status != "R" || files[0].Added != 1 {
		t.Fatalf("rename files = %+v, %v", files, err)
	}
	d, err = CommitDiffEntry(ctx, sub, rename, "b.txt", "a.txt")
	if err != nil || d.OldText == nil || *d.OldText != "1\n2\n3\n4\n5\n" {
		t.Fatalf("rename diff = %+v, %v", d, err)
	}

	write(t, filepath.Join(sub, "keep.txt"), "") // keeps cwd alive once b.txt is gone
	git("rm", "-q", "sub/b.txt")
	git("commit", "-q", "-m", "delete")
	del := git("rev-parse", "HEAD")
	if d, err := CommitDiffEntry(ctx, sub, del, "b.txt", ""); err != nil || d.OldText == nil || d.NewText != "" {
		t.Fatalf("delete diff = %+v, %v", d, err)
	}

	// A commit outside cwd lists nothing under it.
	write(t, filepath.Join(dir, "top.txt"), "changed\n")
	git("commit", "-q", "-am", "outside")
	if files, err := CommitChanges(ctx, sub, git("rev-parse", "HEAD")); err != nil || len(files) != 0 {
		t.Fatalf("outside files = %+v, %v", files, err)
	}

	// A merge lists only what it brought in relative to its first parent.
	git("checkout", "-q", "-b", "side", root)
	write(t, filepath.Join(sub, "side.txt"), "s\n")
	git("add", "sub/side.txt")
	git("commit", "-q", "-m", "side")
	git("checkout", "-q", "main")
	git("merge", "-q", "--no-ff", "-m", "merge", "side")
	files, err = CommitChanges(ctx, sub, git("rev-parse", "HEAD"))
	if err != nil || !reflect.DeepEqual(files, []ChangedFile{{Path: "side.txt", Status: "A", Added: 1}}) {
		t.Fatalf("merge files = %+v, %v", files, err)
	}
}

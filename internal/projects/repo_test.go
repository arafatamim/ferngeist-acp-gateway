package projects

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestResolveRepoGroupsMainCheckoutAndLinkedWorktreeUnderOneRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(root, "main")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", main}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	mustMkdir(t, filepath.Join(main, "src"))
	git("init", "-q", "-b", "main")
	git("config", "commit.gpgSign", "false")
	git("commit", "-q", "--allow-empty", "-m", "init")
	linked := filepath.Join(main, ".worktrees", "email")
	git("worktree", "add", "-q", "-b", "email", linked)
	mustMkdir(t, filepath.Join(linked, "src"))
	plain := filepath.Join(root, "plain")
	mustMkdir(t, plain)
	link := filepath.Join(root, "link")
	if err := os.Symlink(linked, link); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, cwd, repo, worktree string
	}{
		{"main checkout subdir", filepath.Join(main, "src"), main, ""},
		{"linked worktree subdir", filepath.Join(linked, "src"), main, linked},
		{"symlink into linked worktree", link, main, linked},
		{"outside git", plain, plain, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveRepo(context.Background(), tc.cwd)
			if got.repo != tc.repo {
				t.Errorf("repo = %q, want %q", got.repo, tc.repo)
			}
			worktree := ""
			if got.worktree != nil {
				worktree = *got.worktree
			}
			if worktree != tc.worktree {
				t.Errorf("worktree = %q, want %q", worktree, tc.worktree)
			}
		})
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

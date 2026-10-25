package projects

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/workspace"
)

// repoInfo places a cwd: repo is the checkout that owns its git common dir (or
// the cwd itself outside git), worktree the linked worktree's top level, nil in
// the main checkout.
type repoInfo struct {
	repo     string
	worktree *string
}

// repoCache resolves each cwd once within a listing, shared by its agents.
type repoCache struct {
	mu     sync.Mutex
	byPath map[string]repoInfo
}

func (c *repoCache) resolve(ctx context.Context, cwd string) repoInfo {
	c.mu.Lock()
	info, ok := c.byPath[cwd]
	c.mu.Unlock()
	if ok {
		return info
	}
	info = resolveRepo(ctx, cwd)
	// A canceled request fails git, which would cache the cwd as outside git.
	if ctx.Err() == nil {
		c.mu.Lock()
		c.byPath[cwd] = info
		c.mu.Unlock()
	}
	return info
}

func resolveRepo(ctx context.Context, cwd string) repoInfo {
	dir := cwd
	if real, err := filepath.EvalSymlinks(cwd); err == nil {
		dir = real
	}
	out, err := workspace.RunGit(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir", "--git-dir", "--show-toplevel")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if err != nil || len(lines) != 3 {
		return repoInfo{repo: dir}
	}
	common, gitDir, top := lines[0], lines[1], lines[2]
	info := repoInfo{repo: common}
	if filepath.Base(common) == ".git" {
		info.repo = filepath.Dir(common)
	}
	if gitDir != common {
		info.worktree = &top
	}
	return info
}

// missingFolder confirms absence only; permission and cancellation errors do not
// authorize clients to remove cached data.
func missingFolder(ctx context.Context, path string) bool {
	if ctx.Err() != nil || path == "" {
		return false
	}
	_, err := os.Stat(path)
	return os.IsNotExist(err) && ctx.Err() == nil
}

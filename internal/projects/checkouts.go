package projects

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/workspace"
)

// CheckoutsLine ends GET /v1/projects: each repo's branch and linked worktrees.
type CheckoutsLine struct {
	Type         string     `json:"type"`
	Repos        []Checkout `json:"repos"`
	MissingRepos []string   `json:"missingRepos,omitempty"`
}

// Checkout is one repo's main checkout and its linked worktrees.
type Checkout struct {
	Repo      string     `json:"repo"`
	Branch    *string    `json:"branch"`
	Worktrees []Worktree `json:"worktrees"`
}

// Worktree is one linked worktree, by the id the /v1/worktrees routes take.
type Worktree struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Branch string `json:"branch"`
	Ahead  *int   `json:"ahead"`
	Dirty  *bool  `json:"dirty"`
}

// Checkouts covers every git repo in repos plus every repo the gateway knows
// with worktrees, so a repo with worktrees and no chats is still listed.
// A repo outside git is left out.
func (l *Lister) Checkouts(ctx context.Context, repos []string) CheckoutsLine {
	l.mu.Lock()
	repos = append(slices.Clone(repos), slices.Collect(maps.Keys(l.knownRepos))...)
	l.mu.Unlock()
	byRepo := map[string][]workspace.WorktreeStatus{}
	if known, err := l.workspace.List(ctx, ""); err == nil {
		for _, wt := range known {
			byRepo[wt.Repo] = append(byRepo[wt.Repo], wt)
		}
	}
	for _, repo := range repos {
		if missingFolder(ctx, repo) {
			continue
		}
		if _, ok := byRepo[repo]; ok {
			continue
		}
		// Naming the repo also makes the gateway remember it for later listings.
		if wts, err := l.workspace.List(ctx, repo); err == nil {
			byRepo[repo] = wts
		}
	}
	line := CheckoutsLine{Type: "checkouts", Repos: []Checkout{}}
	for _, repo := range slices.Compact(slices.Sorted(slices.Values(repos))) {
		if missingFolder(ctx, repo) {
			line.MissingRepos = append(line.MissingRepos, repo)
		}
	}
	for _, repo := range slices.Sorted(maps.Keys(byRepo)) {
		if missingFolder(ctx, repo) {
			continue
		}
		checkout := checkoutOf(ctx, repo, byRepo[repo])
		line.Repos = append(line.Repos, checkout)
	}
	return line
}

func checkoutOf(ctx context.Context, repo string, worktrees []workspace.WorktreeStatus) Checkout {
	checkout := Checkout{Repo: repo, Worktrees: []Worktree{}}
	// A detached HEAD has no branch.
	if out, err := workspace.RunGit(ctx, repo, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		branch := strings.TrimSpace(out)
		checkout.Branch = &branch
	}
	for _, wt := range worktrees {
		if missingFolder(ctx, wt.Path) {
			continue
		}
		checkout.Worktrees = append(checkout.Worktrees, Worktree{ID: wt.ID, Path: wt.Path, Branch: wt.Branch, Ahead: wt.Ahead, Dirty: wt.Dirty})
	}
	return checkout
}

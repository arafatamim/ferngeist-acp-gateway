package workspace

import (
	"context"
	"strings"
)

// parsePRRef inspects name for PR shorthand like "pr/123", "pull/123",
// "origin/pr/123", or "<remote>/pr/<id>".
func parsePRRef(name string) (remote, prID string, ok bool) {
	parts := strings.Split(name, "/")
	if len(parts) == 2 {
		if (parts[0] == "pr" || parts[0] == "pull") && isDigits(parts[1]) {
			return "origin", parts[1], true
		}
	} else if len(parts) == 3 {
		if (parts[1] == "pr" || parts[1] == "pull") && isDigits(parts[2]) {
			return parts[0], parts[2], true
		}
	}
	return "", "", false
}

// repoRemotes lists the repository's remote names.
func repoRemotes(ctx context.Context, top string) []string {
	out, err := RunGit(ctx, top, "remote")
	if err != nil {
		return nil
	}
	return strings.Fields(out)
}

// parseRemoteBranch splits base into a remote and a branch on it, as in
// "origin/feat-x", when base starts with the name of one of the repo's remotes.
func parseRemoteBranch(ctx context.Context, top, base string) (remote, branch string, ok bool) {
	for _, r := range repoRemotes(ctx, top) {
		if b, found := strings.CutPrefix(base, r+"/"); found && b != "" {
			return r, b, true
		}
	}
	return "", "", false
}

// fetchAndResolvePR fetches a pull request's head into
// refs/remotes/<remote>/pr/<id>, trying GitHub's refspec and then GitLab's.
// A failed fetch still resolves a ref an earlier fetch left behind.
func fetchAndResolvePR(ctx context.Context, top, remote, prID string) (string, string, error) {
	ref := remote + "/pr/" + prID
	dstRef := "refs/remotes/" + ref
	_, fetchErr := RunGitTimeout(ctx, CreateTimeout, top, "fetch", "--quiet", "--end-of-options", remote, "+refs/pull/"+prID+"/head:"+dstRef)
	if fetchErr != nil {
		if _, err := RunGitTimeout(ctx, CreateTimeout, top, "fetch", "--quiet", "--end-of-options", remote, "+refs/merge-requests/"+prID+"/head:"+dstRef); err == nil {
			fetchErr = nil
		}
	}
	if commit, ok := commitOf(ctx, top, dstRef); ok {
		return commit, ref, nil
	}
	if fetchErr != nil {
		return "", "", errStatus(422, "failed to fetch PR "+prID+" from "+remote+": "+fetchErr.Error())
	}
	return "", "", errStatus(422, "failed to resolve PR commit for "+prID)
}

// fetchAndResolveRemoteBranch fetches branch from remote into its
// remote-tracking ref and returns that ref's commit and short name. A failed
// fetch still resolves a tracking ref an earlier fetch left behind.
func fetchAndResolveRemoteBranch(ctx context.Context, top, remote, branch string) (string, string, error) {
	ref := remote + "/" + branch
	_, fetchErr := RunGitTimeout(ctx, CreateTimeout, top, "fetch", "--quiet", "--end-of-options", remote, "+refs/heads/"+branch+":refs/remotes/"+ref)
	if commit, ok := commitOf(ctx, top, "refs/remotes/"+ref); ok {
		return commit, ref, nil
	}
	if fetchErr != nil {
		return "", "", errStatus(422, "failed to fetch "+ref+": "+fetchErr.Error())
	}
	return "", "", errStatus(422, "failed to resolve ref "+ref)
}

// resolveBaseCommitAndRef resolves the base ref for a new worktree. If base
// names a pull request (e.g. "pr/123", "origin/pr/123") or a remote branch
// (e.g. "origin/feat-x") and is not a local branch, it fetches it from the
// remote first. It strictly
// validates the ref name to prevent option or revision injection.
func (s *Service) resolveBaseCommitAndRef(ctx context.Context, top, base string) (baseCommit, baseRef string, err error) {
	if base == "" {
		base = "HEAD"
	} else if !validRefName(base) {
		return "", "", errStatus(400, "invalid base: "+base)
	}

	// A local branch wins over PR and remote shorthand, as it does in git.
	if commit, ok := commitOf(ctx, top, "refs/heads/"+base); ok {
		return commit, base, nil
	}

	if remote, prID, ok := parsePRRef(base); ok {
		return fetchAndResolvePR(ctx, top, remote, prID)
	}

	if remote, branch, ok := parseRemoteBranch(ctx, top, base); ok {
		return fetchAndResolveRemoteBranch(ctx, top, remote, branch)
	}

	if commit, ok := commitOf(ctx, top, base); ok {
		return commit, baseRefOf(ctx, top, base), nil
	}

	return "", "", errStatus(422, "unknown base: "+base)
}

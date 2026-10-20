package workspace

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

// cleanWorktree finds a managed worktree for a finish operation and refuses
// (409) unless its folder exists and has nothing uncommitted: rebasing,
// merging or pushing only moves committed work, so uncommitted edits would
// either be lost or silently left behind.
func (s *Service) cleanWorktree(ctx context.Context, id string) (storage.WorktreeRecord, error) {
	rec, ok := s.find(ctx, func(rec storage.WorktreeRecord) bool { return rec.ID == id })
	if !ok {
		return rec, errStatus(404, "unknown worktree")
	}
	if _, err := os.Stat(rec.Path); err != nil {
		return rec, errStatus(409, "worktree folder is missing")
	}
	st, err := RunGit(ctx, rec.Path, "status", "--porcelain")
	if err != nil {
		s.logger.Error("worktree status", "path", rec.Path, "error", err)
		return rec, errStatus(500, "failed to check worktree status")
	}
	if strings.TrimSpace(st) != "" {
		return rec, errStatus(409, "worktree has uncommitted changes")
	}
	return rec, nil
}

// Rebase replays the worktree's commits onto its base ref, so work cut from a
// base that has since moved can land cleanly. A conflict aborts the rebase and
// leaves the branch as it was (409): resolving one needs a PC.
func (s *Service) Rebase(ctx context.Context, id string) (WorktreeStatus, error) {
	s.finishMu.Lock()
	defer s.finishMu.Unlock()
	rec, err := s.cleanWorktree(ctx, id)
	if err != nil {
		return WorktreeStatus{}, err
	}
	if rec.BaseRef == "" {
		return WorktreeStatus{}, errStatus(409, "worktree has no base ref to rebase onto")
	}
	if _, err := RunGit(ctx, rec.Path, "rebase", "--end-of-options", rec.BaseRef); err != nil {
		_, _ = RunGit(context.Background(), rec.Path, "rebase", "--abort")
		return WorktreeStatus{}, errStatus(409, "rebase onto "+rec.BaseRef+" failed and was aborted: "+err.Error())
	}
	return s.statusOf(ctx, rec), nil
}

// MergeRequest is the client-supplied shape of a worktree merge.
type MergeRequest struct {
	// Squash lands the branch as one commit; otherwise it must fast-forward.
	Squash bool
	// Message is the squash commit message; it defaults to "Merge <branch>".
	Message string
	// Delete removes the worktree (and its branch) once merged.
	Delete bool
}

// MergeResult reports what a merge did.
type MergeResult struct {
	// Commit is the base branch's head after the merge.
	Commit          string
	WorktreeDeleted bool
	BranchDeleted   bool
}

// Merge lands the worktree's branch on its base branch in the main checkout,
// which must have that branch checked out and be clean: the merge runs there.
// A fast-forward that is not possible (the base moved) or a squash that
// conflicts answers 409 and leaves both branches as they were.
func (s *Service) Merge(ctx context.Context, id string, req MergeRequest) (MergeResult, error) {
	s.finishMu.Lock()
	defer s.finishMu.Unlock()
	rec, err := s.cleanWorktree(ctx, id)
	if err != nil {
		return MergeResult{}, err
	}
	if rec.BaseRef == "" {
		return MergeResult{}, errStatus(409, "worktree has no base branch to merge into")
	}
	if _, err := RunGit(ctx, rec.Repo, "show-ref", "--verify", "--quiet", "refs/heads/"+rec.BaseRef); err != nil {
		return MergeResult{}, errStatus(409, "base "+rec.BaseRef+" is not a local branch")
	}
	if head, err := RunGit(ctx, rec.Repo, "symbolic-ref", "--quiet", "--short", "HEAD"); err != nil || strings.TrimSpace(head) != rec.BaseRef {
		return MergeResult{}, errStatus(409, "base branch "+rec.BaseRef+" is not checked out in the main checkout")
	}
	st, err := RunGit(ctx, rec.Repo, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		s.logger.Error("main checkout status", "repo", rec.Repo, "error", err)
		return MergeResult{}, errStatus(500, "failed to check the main checkout")
	}
	if strings.TrimSpace(st) != "" {
		return MergeResult{}, errStatus(409, "main checkout has uncommitted changes")
	}
	if ahead := s.statusOf(ctx, rec).Ahead; ahead != nil && *ahead == 0 {
		return MergeResult{}, errStatus(409, "nothing to merge")
	}
	if req.Delete {
		if err := s.inUse(rec); err != nil {
			return MergeResult{}, err
		}
	}

	if req.Squash {
		message := strings.TrimSpace(req.Message)
		if message == "" {
			message = "Merge " + rec.Branch
		}
		if _, err := RunGit(ctx, rec.Repo, "merge", "--squash", "--end-of-options", rec.Branch); err != nil {
			_, _ = RunGit(context.Background(), rec.Repo, "reset", "--merge")
			return MergeResult{}, errStatus(409, "squash merge failed and was undone: "+err.Error())
		}
		if _, err := RunGit(ctx, rec.Repo, "commit", "-m", message); err != nil {
			_, _ = RunGit(context.Background(), rec.Repo, "reset", "--merge")
			return MergeResult{}, errStatus(422, err.Error())
		}
	} else if _, err := RunGit(ctx, rec.Repo, "merge", "--ff-only", "--end-of-options", rec.Branch); err != nil {
		return MergeResult{}, errStatus(409, "cannot fast-forward "+rec.BaseRef+" (rebase first, or squash): "+err.Error())
	}

	var result MergeResult
	if commit, err := RunGit(ctx, rec.Repo, "rev-parse", "HEAD"); err == nil {
		result.Commit = strings.TrimSpace(commit)
	}
	if req.Delete {
		deleted, err := s.Delete(ctx, rec.ID, false)
		if err != nil {
			// The merge already landed; report the cleanup as not done.
			s.logger.Warn("delete merged worktree", "id", rec.ID, "error", err)
			return result, nil
		}
		result.WorktreeDeleted = true
		result.BranchDeleted = deleted
		// A squashed branch is never an ancestor of the base, so `branch -d`
		// kept it; its content is merged, so drop it.
		if req.Squash && !deleted {
			if _, err := RunGit(ctx, rec.Repo, "branch", "-D", rec.Branch); err == nil {
				result.BranchDeleted = true
			}
		}
	}
	return result, nil
}

// PushRequest is the client-supplied shape of a worktree push.
type PushRequest struct {
	// PR also opens a pull request with the gh CLI.
	PR bool
	// Title and Body fill the pull request; without a title gh fills both from
	// the branch's commits.
	Title string
	Body  string
}

// PushResult reports what a push did.
type PushResult struct {
	// PRURL is the opened pull request, "" when none was asked for.
	PRURL string
}

// Push publishes the worktree's branch to origin and, if asked, opens a pull
// request against the base branch. It never forces: a remote branch that has
// diverged answers 409 and is left alone.
// ponytail: push and gh each get RunGit's 15s, which can outlast the server's
// 20s write timeout together; make this async if large pushes need it.
func (s *Service) Push(ctx context.Context, id string, req PushRequest) (PushResult, error) {
	s.finishMu.Lock()
	defer s.finishMu.Unlock()
	rec, err := s.cleanWorktree(ctx, id)
	if err != nil {
		return PushResult{}, err
	}
	if req.PR {
		if _, err := exec.LookPath("gh"); err != nil {
			return PushResult{}, errStatus(422, "gh is not installed")
		}
	}
	if _, err := runTool(ctx, rec.Path, "git", "push", "--set-upstream", "origin", rec.Branch); err != nil {
		if strings.Contains(err.Error(), "rejected") {
			return PushResult{}, errStatus(409, "origin has commits this branch lacks; it was not overwritten: "+err.Error())
		}
		return PushResult{}, errStatus(422, err.Error())
	}
	if !req.PR {
		return PushResult{}, nil
	}
	args := []string{"pr", "create", "--head", rec.Branch}
	if rec.BaseRef != "" {
		if _, err := RunGit(ctx, rec.Repo, "show-ref", "--verify", "--quiet", "refs/heads/"+rec.BaseRef); err == nil {
			args = append(args, "--base", rec.BaseRef)
		}
	}
	if title := strings.TrimSpace(req.Title); title != "" {
		args = append(args, "--title", title, "--body", req.Body)
	} else {
		args = append(args, "--fill")
	}
	out, err := runTool(ctx, rec.Path, "gh", args...)
	if err != nil {
		return PushResult{}, errStatus(422, "pushed, but opening the pull request failed: "+err.Error())
	}
	return PushResult{PRURL: lastLine(out)}, nil
}

// runTool runs an external command in dir with RunGit's timeout and no
// credential prompts, returning stdout; failures carry its stderr.
func runTool(ctx context.Context, dir, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, GitCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GH_PROMPT_DISABLED=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s %s: %s", name, args[0], msg)
	}
	return string(out), nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

package storage

import (
	"context"
	"time"
)

// WorktreeRecord is a git worktree the gateway created for an agent to work
// in. BaseCommit is the commit the branch started from; the workspace git
// endpoints diff against it so committed agent work stays reviewable. BaseRef
// is the branch the worktree was cut from (empty for rows written before the
// v2 migration); the endpoints prefer merge-base against it so a rebased
// branch stops showing the base's newer upstream commits.
type WorktreeRecord struct {
	ID         string
	Repo       string
	Path       string
	Branch     string
	BaseCommit string
	BaseRef    string
	CreatedAt  time.Time
}

func (s *SQLiteStore) SaveWorktree(ctx context.Context, r WorktreeRecord) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO worktrees(worktree_id, repo, path, branch, base_commit, base_ref, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Repo, r.Path, r.Branch, r.BaseCommit, r.BaseRef, r.CreatedAt.UTC().Format(time.RFC3339Nano),
	)
	return err
}

func (s *SQLiteStore) ListWorktrees(ctx context.Context) ([]WorktreeRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT worktree_id, repo, path, branch, base_commit, base_ref, created_at FROM worktrees ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorktreeRecord
	for rows.Next() {
		var r WorktreeRecord
		var createdRaw string
		if err := rows.Scan(&r.ID, &r.Repo, &r.Path, &r.Branch, &r.BaseCommit, &r.BaseRef, &createdRaw); err != nil {
			return nil, err
		}
		if r.CreatedAt, err = time.Parse(time.RFC3339Nano, createdRaw); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) DeleteWorktree(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM worktrees WHERE worktree_id = ?`, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestWorktreeBaseRefRoundTrip saves and lists a record carrying the branch
// the worktree was cut from.
func TestWorktreeBaseRefRoundTrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "wt.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	rec := WorktreeRecord{
		ID: "wt1", Repo: "/r", Path: "/r/.worktrees/feat-x",
		Branch: "feat/x", BaseCommit: "abc123", BaseRef: "main",
		CreatedAt: time.Now().UTC().Truncate(time.Millisecond),
	}
	if err := store.SaveWorktree(ctx, rec); err != nil {
		t.Fatalf("SaveWorktree() error = %v", err)
	}
	list, err := store.ListWorktrees(ctx)
	if err != nil {
		t.Fatalf("ListWorktrees() error = %v", err)
	}
	if len(list) != 1 || list[0].BaseRef != "main" || list[0].BaseCommit != "abc123" {
		t.Fatalf("ListWorktrees() = %+v, want the saved row with BaseRef main", list)
	}
}

// TestMigrateAddsWorktreeBaseRef covers the v1→v2 upgrade: a worktrees table
// written before the base_ref column existed gains it (empty for old rows),
// and new rows can store it.
func TestMigrateAddsWorktreeBaseRef(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE worktrees (
		worktree_id TEXT PRIMARY KEY,
		repo TEXT NOT NULL,
		path TEXT NOT NULL,
		branch TEXT NOT NULL,
		base_commit TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create v1 worktrees: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO worktrees(worktree_id, repo, path, branch, base_commit, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"wt-old", "/r", "/r/.worktrees/old", "old", "deadbeef",
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert legacy worktree: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close v1 db: %v", err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	var version int
	if err := store.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	if version != latestMigrationVersion || version < 2 {
		t.Fatalf("user_version = %d, want at least 2", version)
	}
	list, err := store.ListWorktrees(ctx)
	if err != nil {
		t.Fatalf("ListWorktrees() error = %v", err)
	}
	if len(list) != 1 || list[0].ID != "wt-old" || list[0].BaseRef != "" {
		t.Fatalf("ListWorktrees() = %+v, want the legacy row with empty BaseRef", list)
	}
	if err := store.SaveWorktree(ctx, WorktreeRecord{
		ID: "wt-new", Repo: "/r", Path: "/r/.worktrees/new", Branch: "new",
		BaseCommit: "cafef00d", BaseRef: "main", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveWorktree() with BaseRef error = %v", err)
	}
}

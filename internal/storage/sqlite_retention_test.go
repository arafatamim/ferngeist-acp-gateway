package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// TestRuntimeFailuresBounded fails while runtime_failures has no DELETE path:
// every process exit appends a row forever. Retention must keep the newest
// records and drop the rest.
func TestRuntimeFailuresBounded(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "failure_retention.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	base := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	total := maxRuntimeFailureRecords + 50
	for i := 0; i < total; i++ {
		failedAt := base.Add(time.Duration(i) * time.Second)
		if err := store.SaveRuntimeFailure(ctx, RuntimeFailureRecord{
			RuntimeID:  fmt.Sprintf("run-%d", i),
			AgentID:    "mock-acp",
			AgentName:  "Mock ACP",
			LastError:  "boom",
			CreatedAt:  base,
			FailedAt:   failedAt,
			LogPreview: "[]",
		}); err != nil {
			t.Fatalf("SaveRuntimeFailure() error = %v", err)
		}
	}

	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime_failures`).Scan(&count); err != nil {
		t.Fatalf("COUNT(*) error = %v", err)
	}
	if count != maxRuntimeFailureRecords {
		t.Fatalf("COUNT(*) = %d, want %d (retention cap)", count, maxRuntimeFailureRecords)
	}

	// Newest survives (what Summary reads), oldest is gone.
	recent, err := store.ListRecentRuntimeFailures(ctx, 1)
	if err != nil {
		t.Fatalf("ListRecentRuntimeFailures() error = %v", err)
	}
	if len(recent) != 1 || recent[0].RuntimeID != fmt.Sprintf("run-%d", total-1) {
		t.Fatalf("newest record = %+v, want run-%d", recent, total-1)
	}
	var oldest string
	if err := store.db.QueryRowContext(ctx, `SELECT runtime_id FROM runtime_failures ORDER BY failed_at ASC LIMIT 1`).Scan(&oldest); err != nil {
		t.Fatalf("oldest query error = %v", err)
	}
	if oldest == "run-0" {
		t.Fatal("oldest record run-0 survives, want evicted by retention")
	}
}

// TestRuntimeFailuresFailedAtIndexed fails while the ORDER BY failed_at sort
// (and the retention subquery) runs without an index.
func TestRuntimeFailuresFailedAtIndexed(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "failure_index.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	var name string
	err = store.db.QueryRowContext(context.Background(),
		`SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'runtime_failures' AND sql LIKE '%failed_at%'`).Scan(&name)
	if err != nil {
		t.Fatalf("no index covering runtime_failures(failed_at): %v", err)
	}
}

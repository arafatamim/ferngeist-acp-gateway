package harness

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// hermesDB builds a state.db with the columns the native-session query reads.
func hermesDB(t *testing.T) (dir string) {
	t.Helper()
	dir = t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open state.db: %v", err)
	}
	defer func() { _ = db.Close() }() // The test owns the only writes.
	_, err = db.Exec(`
CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    source TEXT NOT NULL,
    created_source TEXT,
    session_key TEXT,
    model_config TEXT,
    started_at REAL NOT NULL,
    ended_at REAL,
    end_reason TEXT,
    message_count INTEGER DEFAULT 0,
    cwd TEXT,
    title TEXT,
    last_activity_at REAL,
    hidden INTEGER NOT NULL DEFAULT 0,
    archived INTEGER NOT NULL DEFAULT 0,
    parent_session_id TEXT
)`)
	if err != nil {
		t.Fatalf("create sessions: %v", err)
	}
	return dir
}

func insertHermesSession(t *testing.T, dir string, values map[string]any) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open state.db: %v", err)
	}
	defer func() { _ = db.Close() }() // Insert helper: one statement per call.
	columns := []string{"id", "source", "started_at"}
	args := []any{values["id"], values["source"], values["started_at"]}
	for column, value := range values {
		if slices.Contains(columns, column) {
			continue
		}
		columns = append(columns, column)
		args = append(args, value)
	}
	placeholders := ""
	for i := range columns {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
	}
	query := "INSERT INTO sessions (" + strings.Join(columns, ",") + ") VALUES (" + placeholders + ")"
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("insert %v: %v", values["id"], err)
	}
}

func TestHermesSessionsListTheNativeSurfacesAndSkipACPSurfaces(t *testing.T) {
	dir := hermesDB(t)
	insertHermesSession(t, dir, map[string]any{
		"id": "tui-1", "source": "tui", "started_at": 100, "message_count": 2,
		"cwd": "/work/a", "title": "TUI chat", "last_activity_at": 300,
	})
	insertHermesSession(t, dir, map[string]any{
		"id": "cli-1", "source": "local", "created_source": "cli", "started_at": 100,
		"message_count": 1, "cwd": "/work/b", "last_activity_at": 200,
	})
	insertHermesSession(t, dir, map[string]any{
		"id": "cwd-from-config", "source": "gui", "started_at": 100, "message_count": 1,
		"model_config": `{"cwd":"/work/c"}`, "last_activity_at": 150,
	})
	insertHermesSession(t, dir, map[string]any{
		"id": "acp-1", "source": "acp", "started_at": 100, "message_count": 1,
	})
	insertHermesSession(t, dir, map[string]any{
		"id": "editor-1", "source": "local", "created_source": "acp", "started_at": 100, "message_count": 1,
	})
	insertHermesSession(t, dir, map[string]any{
		"id": "tool-1", "source": "tool", "started_at": 100, "message_count": 1,
	})
	insertHermesSession(t, dir, map[string]any{
		"id": "hidden-1", "source": "tui", "started_at": 100, "message_count": 1, "hidden": 1,
	})
	insertHermesSession(t, dir, map[string]any{
		"id": "archived-1", "source": "tui", "started_at": 100, "message_count": 1, "archived": 1,
	})
	insertHermesSession(t, dir, map[string]any{
		"id": "empty-1", "source": "tui", "started_at": 100, "message_count": 0,
	})
	insertHermesSession(t, dir, map[string]any{
		"id": "delegate-1", "source": "tui", "started_at": 100, "message_count": 1,
		"model_config": `{"_delegate_from":"parent"}`,
	})

	got, err := hermesAt(dir).Sessions.Sessions(context.Background(), "")
	if err != nil {
		t.Fatalf("Sessions() error = %v", err)
	}
	ids := make([]string, len(got))
	for i, session := range got {
		ids[i] = session.ID
	}
	want := []string{"tui-1", "cli-1", "cwd-from-config"}
	if !slices.Equal(ids, want) {
		t.Fatalf("Sessions() ids = %v, want %v", ids, want)
	}
	if got[0].UpdatedAt != "1970-01-01T00:05:00Z" {
		t.Errorf("updatedAt = %q, want the last activity stamp", got[0].UpdatedAt)
	}
	if got[1].Title != "b" {
		t.Errorf("untitled chat title = %q, want the cwd name", got[1].Title)
	}
	if got[2].Cwd != "/work/c" {
		t.Errorf("model_config cwd = %q, want /work/c", got[2].Cwd)
	}
}

func TestHermesSessionsListOneRowPerCompressedConversation(t *testing.T) {
	dir := hermesDB(t)
	insertHermesSession(t, dir, map[string]any{
		"id": "root-1", "source": "tui", "started_at": 100, "ended_at": 200,
		"end_reason": "compression", "message_count": 2,
	})
	insertHermesSession(t, dir, map[string]any{
		"id": "tip-1", "source": "tui", "started_at": 200, "message_count": 1,
		"parent_session_id": "root-1",
	})
	insertHermesSession(t, dir, map[string]any{
		"id": "branch-1", "source": "tui", "started_at": 300, "message_count": 1,
		"parent_session_id": "root-1", "model_config": `{"_branched_from":"root-1"}`,
	})

	got, err := hermesAt(dir).Sessions.Sessions(context.Background(), "")
	if err != nil {
		t.Fatalf("Sessions() error = %v", err)
	}
	ids := make([]string, len(got))
	for i, session := range got {
		ids[i] = session.ID
	}
	want := []string{"branch-1", "root-1"}
	if !slices.Equal(ids, want) {
		t.Fatalf("Sessions() ids = %v, want %v", ids, want)
	}
}

func TestHermesSessionsFilterByCwd(t *testing.T) {
	dir := hermesDB(t)
	insertHermesSession(t, dir, map[string]any{
		"id": "a", "source": "tui", "started_at": 100, "message_count": 1, "cwd": "/work/a",
	})
	insertHermesSession(t, dir, map[string]any{
		"id": "b", "source": "tui", "started_at": 100, "message_count": 1, "cwd": "/work/b",
	})

	got, err := hermesAt(dir).Sessions.Sessions(context.Background(), "/work/a")
	if err != nil {
		t.Fatalf("Sessions() error = %v", err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("Sessions(/work/a) = %+v, want only a", got)
	}
}

func TestHermesSessionsMissingDatabaseIsEmpty(t *testing.T) {
	got, err := hermesAt(t.TempDir()).Sessions.Sessions(context.Background(), "")
	if err != nil {
		t.Fatalf("Sessions() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Sessions() = %+v, want none", got)
	}
}

func TestHermesSessionsListResetChildrenAsTheirOwnChats(t *testing.T) {
	dir := hermesDB(t)
	insertHermesSession(t, dir, map[string]any{
		"id": "old", "source": "tui", "started_at": 100, "ended_at": 200,
		"end_reason": "session_reset", "session_key": "route-a", "message_count": 2,
	})
	insertHermesSession(t, dir, map[string]any{
		"id": "reset", "source": "tui", "started_at": 200, "message_count": 1,
		"session_key": "route-a", "parent_session_id": "old",
	})

	got, err := hermesAt(dir).Sessions.Sessions(context.Background(), "")
	if err != nil {
		t.Fatalf("Sessions() error = %v", err)
	}
	ids := make([]string, len(got))
	for i, session := range got {
		ids[i] = session.ID
	}
	if want := []string{"reset", "old"}; !slices.Equal(ids, want) {
		t.Fatalf("Sessions() ids = %v, want %v", ids, want)
	}
}

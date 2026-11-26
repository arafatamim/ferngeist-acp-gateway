// Package storage provides a lightweight SQLite-backed persistence layer for the
// gateway's durable state: pairings, runtime metadata, gateway settings,
// acquired binaries, sessions, and inbound diagnostic logs. The store is
// intentionally small and local — the gateway is a daemon, not a distributed
// service.
//
// The implementation is split by area, one file per table group (pairing,
// runtimes, sessions, push, settings, binaries, custom agents, worktrees),
// with the schema in migrations.go. There is a single SQLiteStore type; the
// package deliberately has no repository interfaces.
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("record not found")

// SQLiteStore is the gateway's only durable state store. It is intentionally
// limited to pairings, gateway settings, runtime metadata, and recent failures.
type SQLiteStore struct {
	db *sql.DB
}

// Open initializes the SQLite database and applies numbered schema migrations
// before the gateway starts serving requests.
func Open(path string) (*SQLiteStore, error) {
	if err := protectDatabaseFiles(path); err != nil {
		return nil, fmt.Errorf("protect database files: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	// SQLite does not enforce foreign keys by default. Enable them so
	// ON DELETE CASCADE on session_inbound_log automatically removes
	// child rows when a session is deleted.
	if _, err := db.ExecContext(context.Background(), "PRAGMA foreign_keys=ON"); err != nil {
		// sql.Open is lazy: it already spawned the connection-opener
		// goroutine, so a bare return here leaks it (goleak-clean tests
		// and repeated open-fail loops would accumulate them).
		_ = db.Close()
		return nil, err
	}
	// WAL + busy_timeout: the inbound diagnostic path does one autocommit
	// INSERT per frame. In rollback-journal mode that is an fsync per frame;
	// WAL moves it to a sequential log append. busy_timeout avoids
	// SQLITE_BUSY flakes under the single-writer + readers contention.
	if _, err := db.ExecContext(context.Background(), "PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close() // Preserve the startup error.
		return nil, err
	}
	if _, err := db.ExecContext(context.Background(), "PRAGMA synchronous=NORMAL"); err != nil {
		_ = db.Close() // Preserve the startup error.
		return nil, err
	}
	if _, err := db.ExecContext(context.Background(), "PRAGMA busy_timeout=5000"); err != nil {
		_ = db.Close() // Preserve the startup error.
		return nil, err
	}

	store := &SQLiteStore{db: db}
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *SQLiteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func boolToSQLiteInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

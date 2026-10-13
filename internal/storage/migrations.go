package storage

import (
	"context"
	"database/sql"
	"fmt"
)

// migration is one numbered schema step. Steps are applied in version order and
// the database's PRAGMA user_version records the highest step applied, so each
// runs exactly once per database.
type migration struct {
	version int
	name    string
	up      func(ctx context.Context, tx *sql.Tx) error
}

// migrations is append-only: never renumber or edit a released migration, add a
// new one. Version 1 is the baseline: it creates the current schema on a fresh
// database AND converges databases written before user_version tracking existed
// (paired_devices without the retired-credential columns, runtime_failures
// without clean, or a lingering runtime_tokens table). Later steps can assume
// the v1 shape and use plain, unconditional DDL.
var migrations = []migration{
	{version: 1, name: "baseline", up: migrateBaseline},
	{version: 2, name: "worktree_base_ref", up: migrateWorktreeBaseRef},
}

// latestMigrationVersion is the schema version a fully migrated database reports.
var latestMigrationVersion = migrations[len(migrations)-1].version

// migrate applies every migration newer than the database's recorded
// user_version, each in its own transaction, then advances user_version. It is
// safe to call on every Open: on an up-to-date database it is a single read.
func (s *SQLiteStore) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for _, m := range migrations {
		if m.version <= version {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := m.up(ctx, tx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d (%s): %w", m.version, m.name, err)
		}
		// user_version cannot be parameterized; the value is a constant int
		// from the migrations table, never user input.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, m.version)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// baselineSchema is the full v1 schema. Stored as a package variable so the
// migration test can build the legacy shapes from it.
var baselineSchema = []string{
	`CREATE TABLE IF NOT EXISTS paired_devices (
		device_id TEXT PRIMARY KEY,
		device_name TEXT NOT NULL,
		token TEXT NOT NULL,
		expires_at TEXT NOT NULL,
		retired_token TEXT NOT NULL DEFAULT '',
		retired_until TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE TABLE IF NOT EXISTS paired_device_scopes (
		device_id TEXT PRIMARY KEY,
		scopes TEXT NOT NULL DEFAULT '[]'
	)`,
	`CREATE TABLE IF NOT EXISTS paired_device_proofs (
		device_id TEXT PRIMARY KEY,
		public_key TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE TABLE IF NOT EXISTS runtimes (
		runtime_id TEXT PRIMARY KEY,
		agent_id TEXT NOT NULL,
		agent_name TEXT NOT NULL,
		status TEXT NOT NULL,
		command TEXT NOT NULL,
		pid INTEGER NOT NULL,
		last_error TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS runtime_failures (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		runtime_id TEXT NOT NULL,
		agent_id TEXT NOT NULL,
		agent_name TEXT NOT NULL,
		last_error TEXT NOT NULL,
		created_at TEXT NOT NULL,
		failed_at TEXT NOT NULL,
		log_preview TEXT NOT NULL DEFAULT '',
		clean INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE IF NOT EXISTS gateway_settings (
		settings_key TEXT PRIMARY KEY,
		registry_url TEXT NOT NULL DEFAULT '',
		public_base_url TEXT NOT NULL DEFAULT '',
		enable_lan INTEGER NOT NULL DEFAULT 0,
		gateway_name TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE TABLE IF NOT EXISTS acquired_binaries (
		agent_id TEXT PRIMARY KEY,
		version TEXT NOT NULL DEFAULT '',
		path TEXT NOT NULL,
		archive_url TEXT NOT NULL DEFAULT '',
		installed_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS gateway_sessions (
		session_id TEXT PRIMARY KEY,
		runtime_id TEXT NOT NULL,
		device_id TEXT NOT NULL,
		agent_id TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'active',
		created_at TEXT NOT NULL,
		last_client_connect_at TEXT,
		last_client_disconnect_at TEXT,
		disconnected_since TEXT,
		metadata TEXT NOT NULL DEFAULT '{}'
	)`,
	`CREATE INDEX IF NOT EXISTS idx_sessions_runtime ON gateway_sessions(runtime_id)`,
	`CREATE INDEX IF NOT EXISTS idx_sessions_device ON gateway_sessions(device_id)`,
	`CREATE INDEX IF NOT EXISTS idx_sessions_status ON gateway_sessions(status)`,
	`CREATE TABLE IF NOT EXISTS session_inbound_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL,
		seq INTEGER NOT NULL,
		payload TEXT NOT NULL,
		recorded_at TEXT NOT NULL,
		FOREIGN KEY (session_id) REFERENCES gateway_sessions(session_id) ON DELETE CASCADE
	)`,
	`CREATE INDEX IF NOT EXISTS idx_inbound_session_seq ON session_inbound_log(session_id, seq)`,
	`CREATE INDEX IF NOT EXISTS idx_runtime_failures_failed_at ON runtime_failures(failed_at DESC)`,
	`CREATE TABLE IF NOT EXISTS device_push_tokens (
		device_id TEXT PRIMARY KEY,
		token TEXT NOT NULL,
		platform TEXT NOT NULL DEFAULT 'android',
		updated_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS gateway_identity (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		gateway_id TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS vapid_keys (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		public_key TEXT NOT NULL,
		private_key TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS custom_agents (
		agent_id TEXT PRIMARY KEY,
		display_name TEXT NOT NULL,
		command TEXT NOT NULL,
		args TEXT NOT NULL DEFAULT '[]',
		hint TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS worktrees (
		worktree_id TEXT PRIMARY KEY,
		repo TEXT NOT NULL,
		path TEXT NOT NULL,
		branch TEXT NOT NULL,
		base_commit TEXT NOT NULL,
		base_ref TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	)`,
}

// migrateBaseline creates the v1 schema and reconciles databases predating
// user_version tracking. CREATE TABLE IF NOT EXISTS is a no-op on an existing
// table, so columns added after a table first shipped are backfilled with
// ALTER only where PRAGMA table_info shows them missing.
func migrateBaseline(ctx context.Context, tx *sql.Tx) error {
	for _, statement := range baselineSchema {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}

	// runtime_tokens held one-time ACP bearer tokens for a validation path that
	// no longer exists; drop the table so upgraded databases stop carrying it.
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS runtime_tokens`); err != nil {
		return err
	}

	// runtime_failures gained the clean column after it first shipped.
	if err := ensureColumn(ctx, tx, "runtime_failures", "clean",
		`ALTER TABLE runtime_failures ADD COLUMN clean INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	// paired_devices gained the retired-credential columns after the first
	// releases shipped.
	for _, column := range []struct{ name, statement string }{
		{"retired_token", `ALTER TABLE paired_devices ADD COLUMN retired_token TEXT NOT NULL DEFAULT ''`},
		{"retired_until", `ALTER TABLE paired_devices ADD COLUMN retired_until TEXT NOT NULL DEFAULT ''`},
	} {
		if err := ensureColumn(ctx, tx, "paired_devices", column.name, column.statement); err != nil {
			return err
		}
	}

	// gateway_sessions.leaseholder recorded the stdio pipe lease, which no longer
	// exists. Dropping it makes the database unusable by v0.13 and earlier,
	// whose session queries name the column: downgrades are not supported.
	hasLeaseholder, err := hasColumn(ctx, tx, "gateway_sessions", "leaseholder")
	if err != nil {
		return err
	}
	if hasLeaseholder {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE gateway_sessions DROP COLUMN leaseholder`); err != nil {
			return err
		}
	}

	return nil
}

// migrateWorktreeBaseRef records the branch each managed worktree was cut
// from, so the workspace git endpoints can merge-base against it after the
// branch is rebased. Databases already at v1 gain the column with an empty
// default (old rows fall back to their fixed base commit); fresh databases
// already have it from the baseline schema above.
func migrateWorktreeBaseRef(ctx context.Context, tx *sql.Tx) error {
	return ensureColumn(ctx, tx, "worktrees", "base_ref",
		`ALTER TABLE worktrees ADD COLUMN base_ref TEXT NOT NULL DEFAULT ''`)
}

// ensureColumn adds a column only when it is missing. modernc.org/sqlite's
// bundled SQLite does not accept ADD COLUMN IF NOT EXISTS, so a fresh database
// (which already has the column from the CREATE above) must skip the ALTER.
func ensureColumn(ctx context.Context, tx *sql.Tx, table, column, addStatement string) error {
	present, err := hasColumn(ctx, tx, table, column)
	if err != nil || present {
		return err
	}
	_, err = tx.ExecContext(ctx, addStatement)
	return err
}

// hasColumn reports whether PRAGMA table_info lists column on table.
func hasColumn(ctx context.Context, tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid        int
			name       string
			colType    string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultVal, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

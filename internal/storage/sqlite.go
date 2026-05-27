// Package storage provides a lightweight SQLite-backed persistence layer for the
// gateway's durable state: pairings, runtime metadata, tokens, gateway settings,
// acquired binaries, sessions, and inbound diagnostic logs. The store is
// intentionally small and local — the gateway is a daemon, not a distributed
// service.
package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("record not found")

type PairingRecord struct {
	DeviceID       string
	DeviceName     string
	Token          string
	ExpiresAt      time.Time
	Scopes         []string
	ProofPublicKey string
}

type RuntimeRecord struct {
	RuntimeID string
	AgentID   string
	AgentName string
	Status    string
	Command   string
	PID       int
	LastError string
	CreatedAt time.Time
}

type RuntimeTokenRecord struct {
	RuntimeID string
	Token     string
	ExpiresAt time.Time
}

type RuntimeFailureRecord struct {
	RuntimeID  string
	AgentID    string
	AgentName  string
	LastError  string
	CreatedAt  time.Time
	FailedAt   time.Time
	LogPreview string
}

type GatewaySettingsRecord struct {
	RegistryURL   string
	PublicBaseURL string
	EnableLAN     bool
	GatewayName   string
}

type AcquiredBinaryRecord struct {
	AgentID     string
	Version     string
	Path        string
	ArchiveURL  string
	InstalledAt time.Time
}

// SessionRecord represents a stored resilient session row in gateway_sessions.
// Nullable time fields (LastClientConnectAt, LastClientDisconnectAt, DisconnectedSince)
// use pointers so SQLITE NULL maps to Go nil without parsing zero-value timestamps.
type SessionRecord struct {
	SessionID              string
	RuntimeID              string
	DeviceID               string
	AgentID                string
	Status                 string
	Leaseholder            string
	CreatedAt              time.Time
	LastClientConnectAt    *time.Time
	LastClientDisconnectAt *time.Time
	DisconnectedSince      *time.Time
	Metadata               string
}

// SQLiteStore is the gateway's only durable state store. It is intentionally
// limited to pairings, gateway settings, runtime metadata, runtime tokens, and
// recent failures.
type SQLiteStore struct {
	db *sql.DB
}

// Open initializes the SQLite database and applies idempotent schema
// migrations before the gateway starts serving requests.
func Open(path string) (*SQLiteStore, error) {
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

func (s *SQLiteStore) SavePairing(ctx context.Context, record PairingRecord) error {
	scopesJSON, err := json.Marshal(record.Scopes)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO paired_devices(device_id, device_name, token, expires_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(device_id) DO UPDATE SET
		   device_name = excluded.device_name,
		   token = excluded.token,
		   expires_at = excluded.expires_at`,
		record.DeviceID,
		record.DeviceName,
		record.Token,
		record.ExpiresAt.UTC().Format(time.RFC3339Nano),
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO paired_device_scopes(device_id, scopes)
		 VALUES (?, ?)
		 ON CONFLICT(device_id) DO UPDATE SET scopes = excluded.scopes`,
		record.DeviceID,
		string(scopesJSON),
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO paired_device_proofs(device_id, public_key)
		 VALUES (?, ?)
		 ON CONFLICT(device_id) DO UPDATE SET public_key = excluded.public_key`,
		record.DeviceID,
		record.ProofPublicKey,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) ListPairings(ctx context.Context) ([]PairingRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.device_id, p.device_name, p.token, p.expires_at, COALESCE(s.scopes, ''), COALESCE(k.public_key, '') FROM paired_devices p LEFT JOIN paired_device_scopes s ON s.device_id = p.device_id LEFT JOIN paired_device_proofs k ON k.device_id = p.device_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []PairingRecord
	for rows.Next() {
		var (
			record      PairingRecord
			expiresRaw  string
			scopesRaw   string
			proofKeyRaw string
		)
		if err := rows.Scan(&record.DeviceID, &record.DeviceName, &record.Token, &expiresRaw, &scopesRaw, &proofKeyRaw); err != nil {
			return nil, err
		}
		record.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiresRaw)
		if err != nil {
			return nil, err
		}
		if scopesRaw != "" {
			if err := json.Unmarshal([]byte(scopesRaw), &record.Scopes); err != nil {
				return nil, err
			}
		}
		record.ProofPublicKey = proofKeyRaw
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *SQLiteStore) DeletePairing(ctx context.Context, deviceID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `DELETE FROM paired_device_scopes WHERE device_id = ?`, deviceID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM paired_device_proofs WHERE device_id = ?`, deviceID)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM paired_devices WHERE device_id = ?`, deviceID)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// SaveRuntime persists the latest runtime snapshot rather than a full event
// history. This keeps the store small and restart recovery simple.
func (s *SQLiteStore) SaveRuntime(ctx context.Context, record RuntimeRecord) error {
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO runtimes(runtime_id, agent_id, agent_name, status, command, pid, last_error, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(runtime_id) DO UPDATE SET
		   agent_id = excluded.agent_id,
		   agent_name = excluded.agent_name,
		   status = excluded.status,
		   command = excluded.command,
		   pid = excluded.pid,
		   last_error = excluded.last_error,
		   created_at = excluded.created_at`,
		record.RuntimeID,
		record.AgentID,
		record.AgentName,
		record.Status,
		record.Command,
		record.PID,
		record.LastError,
		record.CreatedAt.UTC().Format(time.RFC3339Nano),
	)
	return err
}

func (s *SQLiteStore) SaveRuntimeToken(ctx context.Context, record RuntimeTokenRecord) error {
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO runtime_tokens(runtime_id, token, expires_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT(runtime_id) DO UPDATE SET
		   token = excluded.token,
		   expires_at = excluded.expires_at`,
		record.RuntimeID,
		record.Token,
		record.ExpiresAt.UTC().Format(time.RFC3339Nano),
	)
	return err
}

func (s *SQLiteStore) GetRuntimeToken(ctx context.Context, runtimeID string) (RuntimeTokenRecord, error) {
	row := s.db.QueryRowContext(ctx, `SELECT runtime_id, token, expires_at FROM runtime_tokens WHERE runtime_id = ?`, runtimeID)

	var (
		record     RuntimeTokenRecord
		expiresRaw string
	)
	if err := row.Scan(&record.RuntimeID, &record.Token, &expiresRaw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RuntimeTokenRecord{}, ErrNotFound
		}
		return RuntimeTokenRecord{}, err
	}

	expiresAt, err := time.Parse(time.RFC3339Nano, expiresRaw)
	if err != nil {
		return RuntimeTokenRecord{}, err
	}
	record.ExpiresAt = expiresAt
	return record, nil
}

func (s *SQLiteStore) DeleteRuntimeToken(ctx context.Context, runtimeID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM runtime_tokens WHERE runtime_id = ?`, runtimeID)
	return err
}

func (s *SQLiteStore) DeleteExpiredRuntimeTokens(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM runtime_tokens WHERE expires_at <= ?`, now.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *SQLiteStore) DeleteAllRuntimeTokens(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM runtime_tokens`)
	return err
}

func (s *SQLiteStore) SaveRuntimeFailure(ctx context.Context, record RuntimeFailureRecord) error {
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO runtime_failures(runtime_id, agent_id, agent_name, last_error, created_at, failed_at, log_preview)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		record.RuntimeID,
		record.AgentID,
		record.AgentName,
		record.LastError,
		record.CreatedAt.UTC().Format(time.RFC3339Nano),
		record.FailedAt.UTC().Format(time.RFC3339Nano),
		record.LogPreview,
	)
	return err
}

func (s *SQLiteStore) SaveGatewaySettings(ctx context.Context, record GatewaySettingsRecord) error {
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO gateway_settings(settings_key, registry_url, public_base_url, enable_lan, gateway_name)
		 VALUES ('default', ?, ?, ?, ?)
		 ON CONFLICT(settings_key) DO UPDATE SET
		   registry_url = excluded.registry_url,
		   public_base_url = excluded.public_base_url,
		   enable_lan = excluded.enable_lan,
		   gateway_name = excluded.gateway_name`,
		record.RegistryURL,
		record.PublicBaseURL,
		boolToSQLiteInt(record.EnableLAN),
		record.GatewayName,
	)
	return err
}

func (s *SQLiteStore) SaveAcquiredBinary(ctx context.Context, record AcquiredBinaryRecord) error {
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO acquired_binaries(agent_id, version, path, archive_url, installed_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(agent_id) DO UPDATE SET
		   version = excluded.version,
		   path = excluded.path,
		   archive_url = excluded.archive_url,
		   installed_at = excluded.installed_at`,
		record.AgentID,
		record.Version,
		record.Path,
		record.ArchiveURL,
		record.InstalledAt.UTC().Format(time.RFC3339Nano),
	)
	return err
}

func (s *SQLiteStore) GetAcquiredBinary(ctx context.Context, agentID string) (AcquiredBinaryRecord, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT agent_id, version, path, archive_url, installed_at
		 FROM acquired_binaries
		 WHERE agent_id = ?`,
		agentID,
	)

	var (
		record       AcquiredBinaryRecord
		installedRaw string
	)
	if err := row.Scan(&record.AgentID, &record.Version, &record.Path, &record.ArchiveURL, &installedRaw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AcquiredBinaryRecord{}, ErrNotFound
		}
		return AcquiredBinaryRecord{}, err
	}
	installedAt, err := time.Parse(time.RFC3339Nano, installedRaw)
	if err != nil {
		return AcquiredBinaryRecord{}, err
	}
	record.InstalledAt = installedAt
	return record, nil
}

func (s *SQLiteStore) GetGatewaySettings(ctx context.Context) (GatewaySettingsRecord, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT registry_url, public_base_url, enable_lan, gateway_name
		 FROM gateway_settings
		 WHERE settings_key = 'default'`,
	)

	var (
		record    GatewaySettingsRecord
		enableLAN int
	)
	if err := row.Scan(&record.RegistryURL, &record.PublicBaseURL, &enableLAN, &record.GatewayName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return GatewaySettingsRecord{}, ErrNotFound
		}
		return GatewaySettingsRecord{}, err
	}
	record.EnableLAN = enableLAN != 0
	return record, nil
}

// ListRecentRuntimeFailures returns persisted failure summaries for diagnostics.
// It deliberately stores a bounded log preview instead of full runtime logs.
func (s *SQLiteStore) ListRecentRuntimeFailures(ctx context.Context, limit int) ([]RuntimeFailureRecord, error) {
	if limit <= 0 {
		limit = 5
	}

	rows, err := s.db.QueryContext(
		ctx,
		`SELECT runtime_id, agent_id, agent_name, last_error, created_at, failed_at, log_preview
		 FROM runtime_failures
		 ORDER BY failed_at DESC
		 LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []RuntimeFailureRecord
	for rows.Next() {
		var (
			record     RuntimeFailureRecord
			createdRaw string
			failedRaw  string
		)
		if err := rows.Scan(
			&record.RuntimeID,
			&record.AgentID,
			&record.AgentName,
			&record.LastError,
			&createdRaw,
			&failedRaw,
			&record.LogPreview,
		); err != nil {
			return nil, err
		}
		record.CreatedAt, err = time.Parse(time.RFC3339Nano, createdRaw)
		if err != nil {
			return nil, err
		}
		record.FailedAt, err = time.Parse(time.RFC3339Nano, failedRaw)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// SaveSession inserts or updates a session record. Uses ON CONFLICT DO UPDATE
// so the same method works for both initial creation and incremental status updates.
func (s *SQLiteStore) SaveSession(ctx context.Context, record SessionRecord) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO gateway_sessions(session_id, runtime_id, device_id, agent_id, status, leaseholder, created_at, last_client_connect_at, last_client_disconnect_at, disconnected_since, metadata)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(session_id) DO UPDATE SET
		   status = excluded.status,
		   leaseholder = excluded.leaseholder,
		   last_client_connect_at = excluded.last_client_connect_at,
		   last_client_disconnect_at = excluded.last_client_disconnect_at,
		   disconnected_since = excluded.disconnected_since,
		   metadata = excluded.metadata`,
		record.SessionID, record.RuntimeID, record.DeviceID, record.AgentID, record.Status, record.Leaseholder,
		record.CreatedAt.UTC().Format(time.RFC3339Nano),
		timeToNullable(record.LastClientConnectAt),
		timeToNullable(record.LastClientDisconnectAt),
		timeToNullable(record.DisconnectedSince),
		record.Metadata,
	)
	return err
}

func scanSession(row *sql.Row) (SessionRecord, error) {
	var rec SessionRecord
	var createdRaw, connRaw, discRaw, sinceRaw string
	err := row.Scan(&rec.SessionID, &rec.RuntimeID, &rec.DeviceID, &rec.AgentID, &rec.Status, &rec.Leaseholder, &createdRaw, &connRaw, &discRaw, &sinceRaw, &rec.Metadata)
	if err != nil {
		return SessionRecord{}, err
	}
	rec.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdRaw)
	rec.LastClientConnectAt = nullableToTime(connRaw)
	rec.LastClientDisconnectAt = nullableToTime(discRaw)
	rec.DisconnectedSince = nullableToTime(sinceRaw)
	return rec, nil
}

// GetSession retrieves a single session record by its session ID.
// Returns ErrNotFound if no row exists.
func (s *SQLiteStore) GetSession(ctx context.Context, sessionID string) (SessionRecord, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT session_id, runtime_id, device_id, agent_id, status, leaseholder, created_at, last_client_connect_at, last_client_disconnect_at, disconnected_since, metadata
		 FROM gateway_sessions WHERE session_id = ?`, sessionID)
	rec, err := scanSession(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SessionRecord{}, ErrNotFound
		}
		return SessionRecord{}, err
	}
	return rec, nil
}

// DeleteSession removes a session and, via ON DELETE CASCADE, all its
// associated inbound diagnostic rows.
func (s *SQLiteStore) DeleteSession(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM gateway_sessions WHERE session_id = ?`, sessionID)
	return err
}

// ListSessionsByDevice returns all sessions owned by a device, ordered by
// creation time descending (newest first). Used to enforce per-device limits
// and for the sessions list endpoint.
func (s *SQLiteStore) ListSessionsByDevice(ctx context.Context, deviceID string) ([]SessionRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT session_id, runtime_id, device_id, agent_id, status, leaseholder, created_at, last_client_connect_at, last_client_disconnect_at, disconnected_since, metadata
		 FROM gateway_sessions WHERE device_id = ? ORDER BY created_at DESC`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []SessionRecord
	for rows.Next() {
		var rec SessionRecord
		var createdRaw, connRaw, discRaw, sinceRaw string
		if err := rows.Scan(&rec.SessionID, &rec.RuntimeID, &rec.DeviceID, &rec.AgentID, &rec.Status, &rec.Leaseholder, &createdRaw, &connRaw, &discRaw, &sinceRaw, &rec.Metadata); err != nil {
			return nil, err
		}
		rec.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdRaw)
		rec.LastClientConnectAt = nullableToTime(connRaw)
		rec.LastClientDisconnectAt = nullableToTime(discRaw)
		rec.DisconnectedSince = nullableToTime(sinceRaw)
		records = append(records, rec)
	}
	return records, rows.Err()
}

// ReconcileSessionsOnStartup transitions all sessions that were active or
// disconnected at the last shutdown to failed. On restart, the backing runtime
// processes no longer exist, so these sessions are unrecoverable.
func (s *SQLiteStore) ReconcileSessionsOnStartup(ctx context.Context) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx,
		`UPDATE gateway_sessions SET status = 'failed', last_client_disconnect_at = ? WHERE status IN ('active', 'disconnected')`,
		now)
	return err
}

// AppendInboundDiagnostic stores a client-to-agent message for audit/debugging.
// Written via a buffered channel so the hot path never blocks on SQLite I/O.
func (s *SQLiteStore) AppendInboundDiagnostic(ctx context.Context, sessionID string, seq int64, payload string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO session_inbound_log(session_id, seq, payload, recorded_at) VALUES (?, ?, ?, ?)`,
		sessionID, seq, payload, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *SQLiteStore) SaveFCMToken(ctx context.Context, deviceID, fcmToken string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO device_fcm_tokens(device_id, fcm_token, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(device_id) DO UPDATE SET fcm_token = excluded.fcm_token, updated_at = excluded.updated_at`,
		deviceID, fcmToken, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// migrate is intentionally append-only and idempotent because the gateway is a
// local daemon, not a service with a heavyweight migration framework.
func (s *SQLiteStore) migrate(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS paired_devices (
			device_id TEXT PRIMARY KEY,
			device_name TEXT NOT NULL,
			token TEXT NOT NULL,
			expires_at TEXT NOT NULL
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
		`CREATE TABLE IF NOT EXISTS runtime_tokens (
			runtime_id TEXT PRIMARY KEY,
			token TEXT NOT NULL,
			expires_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS runtime_failures (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			runtime_id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			agent_name TEXT NOT NULL,
			last_error TEXT NOT NULL,
			created_at TEXT NOT NULL,
			failed_at TEXT NOT NULL,
			log_preview TEXT NOT NULL DEFAULT ''
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
			leaseholder TEXT NOT NULL DEFAULT '',
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
		`CREATE TABLE IF NOT EXISTS device_fcm_tokens (
			device_id TEXT PRIMARY KEY,
			fcm_token TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
	}

	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}

	return nil
}

// timeToNullable formats a *time.Time for SQLite storage. Returns empty string
// for nil pointers so the column gets SQL NULL.
func timeToNullable(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// nullableToTime parses a SQLite text column back to *time.Time. Returns nil
// for empty strings (SQL NULL) or parse errors.
func nullableToTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil
	}
	return &t
}

func boolToSQLiteInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

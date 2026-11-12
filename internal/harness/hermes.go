package harness

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// hermesSessionLimit bounds one listing of Hermes's native chats.
const hermesSessionLimit = 200

// hermesSessionSelect reads the native chats Hermes's own picker shows: its CLI,
// TUI, desktop and web rows, with ACP threads, tool/one-shot runs, subagent
// runs, and compression continuations excluded. The lineage predicates mirror
// Hermes's own picker (`hermes_state_common._LISTABLE_CHILD_SQL`): a root, a
// branch child or a reset child is a conversation; a compression continuation
// is represented by its root.
const hermesSessionSelect = `
SELECT s.id,
       COALESCE(NULLIF(s.cwd, ''),
                CASE WHEN json_valid(s.model_config) THEN json_extract(s.model_config, '$.cwd') END,
                '') AS cwd,
       COALESCE(s.title, '') AS title,
       COALESCE(s.last_activity_at, s.started_at) AS updated
FROM sessions s
WHERE s.message_count > 0
  AND s.hidden = 0
  AND s.archived = 0
  AND s.source IN ('cli', 'tui', 'gui', 'desktop', 'web', 'local')
  AND COALESCE(s.created_source, '') <> 'acp'
  AND (
        s.parent_session_id IS NULL
     OR (json_valid(s.model_config) AND json_extract(s.model_config, '$._branched_from') IS NOT NULL)
     OR (json_valid(s.model_config) AND json_extract(s.model_config, '$._reset_from') IS NOT NULL)
     OR EXISTS (SELECT 1 FROM sessions p
                WHERE p.id = s.parent_session_id
                  AND p.end_reason = 'branched'
                  AND s.started_at >= p.ended_at)
     OR EXISTS (SELECT 1 FROM sessions p
                WHERE p.id = s.parent_session_id
                  AND p.end_reason IN ('session_reset', 'session_switch', 'idle', 'daily',
                                       'suspended', 'resume_pending_expired')
                  AND s.session_key IS NOT NULL AND s.session_key <> ''
                  AND s.session_key = p.session_key)
  )
  AND NOT (json_valid(s.model_config) AND json_extract(s.model_config, '$._delegate_from') IS NOT NULL)`

// Hermes lists the chats the Hermes CLI, TUI and desktop/web clients created.
// Its ACP adapter answers session/list from an ACP-only surface filter, so those
// chats are invisible to an ACP client even though Hermes's owner resumes them
// on session/load. The profile is only needed while that filter stands: if the
// adapter starts listing the native surfaces itself, drop it.
func Hermes() Profile {
	return hermesAt(hermesHome())
}

func hermesAt(home string) Profile {
	return Profile{ID: "hermes", Sessions: hermesSessions{path: filepath.Join(home, "state.db")}}
}

func hermesHome() string {
	if home := os.Getenv("HERMES_HOME"); home != "" {
		return home
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".hermes")
}

type hermesSessions struct {
	path string
}

// Sessions reads the native chats read-only from Hermes's store. Hermes owns
// that schema, so this is best-effort: a missing or unreadable store yields no
// rows rather than an error the client would see.
//
// shortcut: a compressed conversation is listed with its root's title and
// last-activity stamp, not Hermes's projected tip; opening it still resumes the
// tip.
func (h hermesSessions) Sessions(ctx context.Context, cwd string) ([]Session, error) {
	if h.path == "" {
		return nil, nil
	}
	if _, err := os.Stat(h.path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(h.path), RawQuery: "mode=ro&_pragma=busy_timeout(1000)"}
	if filepath.VolumeName(h.path) != "" && !strings.HasPrefix(uri.Path, "/") {
		uri.Path = "/" + uri.Path
	}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }() // Read-only: nothing to flush.
	db.SetMaxOpenConns(1)

	query, args := hermesSessionQuery(cwd)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() // The scan loop owns the only error worth reporting.

	var sessions []Session
	for rows.Next() {
		var (
			id      string
			dir     string
			title   string
			updated sql.NullFloat64
		)
		if err := rows.Scan(&id, &dir, &title, &updated); err != nil {
			return nil, err
		}
		if title == "" {
			title = hermesSessionTitle(dir)
		}
		sessions = append(sessions, Session{
			ID:        id,
			Cwd:       dir,
			Title:     title,
			UpdatedAt: hermesUpdatedAt(updated),
		})
	}
	return sessions, rows.Err()
}

// hermesSessionQuery adds the optional folder filter and the row cap.
func hermesSessionQuery(cwd string) (string, []any) {
	query, args := hermesSessionSelect, []any{}
	if cwd != "" {
		query += `
  AND COALESCE(NULLIF(s.cwd, ''),
               CASE WHEN json_valid(s.model_config) THEN json_extract(s.model_config, '$.cwd') END,
               '') = ?`
		args = append(args, cwd)
	}
	query += `
ORDER BY updated DESC
LIMIT ?`
	return query, append(args, hermesSessionLimit)
}

// hermesSessionTitle is Hermes's last title fallback: the cwd's own name, then
// a generic label for a chat outside any folder.
func hermesSessionTitle(cwd string) string {
	if base := cwd[strings.LastIndexAny(cwd, `/\`)+1:]; base != "" && base != "." {
		return base
	}
	return "New thread"
}

func hermesUpdatedAt(updated sql.NullFloat64) string {
	if !updated.Valid {
		return ""
	}
	return time.Unix(int64(updated.Float64), 0).UTC().Format(time.RFC3339)
}

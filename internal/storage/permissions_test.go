package storage

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOpenKeepsNewDatabaseFilesPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX permissions")
	}
	for _, format := range []string{"path", "uri"} {
		t.Run(format, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gateway state.db")
			dsn := path
			if format == "uri" {
				dsn = fileURI(path, "mode=rwc")
			}
			store, err := Open(dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			assertDatabaseModes(t, path, 0o600)
		})
	}
}

func TestOpenRestrictsLegacyDatabaseAndSidecars(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX permissions")
	}
	path := filepath.Join(t.TempDir(), "gateway.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	for _, query := range []string{"PRAGMA journal_mode=WAL", "CREATE TABLE legacy (name TEXT)", "INSERT INTO legacy VALUES ('preserved')"} {
		if _, err := legacy.ExecContext(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Chmod(path+suffix, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	assertDatabaseModes(t, path, 0o600)
	var name string
	if err := store.db.QueryRowContext(t.Context(), "SELECT name FROM legacy").Scan(&name); err != nil || name != "preserved" {
		t.Errorf("legacy data = %q, error = %v", name, err)
	}
}

func TestOpenLeavesMemoryDatabaseOffDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "named.db")
	for _, dsn := range []string{":memory:", "file::memory:", fileURI(path, "mode=memory"), fileURI(path, "vfs=memdb"), path + "?vfs=memdb"} {
		store, err := Open(dsn)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("memory DSN touched disk: %v", err)
	}
}

func TestOpenRejectsInvalidModeWithoutCreatingDatabase(t *testing.T) {
	for _, options := range []string{"mode=invalid", "mode="} {
		t.Run(options, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			if store, err := Open(fileURI(path, options)); err == nil {
				store.Close()
				t.Error("Open succeeded with an invalid SQLite mode")
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid SQLite mode touched disk: %v", err)
			}
		})
	}
}

func fileURI(path, query string) string {
	u := url.URL{Scheme: "file", Path: "/" + strings.TrimPrefix(filepath.ToSlash(path), "/"), RawQuery: query}
	return u.String()
}

func TestOpenRejectsLinksWithoutChangingTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX permissions")
	}
	for _, tc := range []struct {
		name string
		link func(string, string) error
	}{
		{"gateway.db", os.Symlink},
		{"gateway.db-wal", os.Symlink},
		{"gateway.db-shm", os.Symlink},
		{"gateway.db-journal", os.Link},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "other.db")
			if err := os.WriteFile(target, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(target, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := tc.link(target, filepath.Join(dir, tc.name)); err != nil {
				t.Fatal(err)
			}
			if store, err := Open(filepath.Join(dir, "gateway.db")); err == nil {
				store.Close()
				t.Error("Open succeeded with a linked database file")
			}
			info, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != 0o644 {
				t.Errorf("link target mode = %04o, want 0644", got)
			}
		})
	}
}

func assertDatabaseModes(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %04o, want %04o", filepath.Base(path+suffix), got, want)
		}
	}
}

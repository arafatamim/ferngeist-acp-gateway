//go:build unix

package storage

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"syscall"
)

const databaseFileMode = 0o600

func protectDatabaseFiles(dsn string) error {
	path, create, err := databaseFilePath(dsn)
	if err != nil || path == "" {
		return err
	}
	// Pre-create the empty database before SQLite opens it. SQLite inherits
	// those permissions when it creates the journal, WAL and shared memory.
	if create {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, databaseFileMode)
		if err == nil {
			err = file.Close()
		}
		if err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	if err := restrictDatabaseFile(path); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := restrictDatabaseFile(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func databaseFilePath(dsn string) (path string, create bool, err error) {
	if !strings.HasPrefix(dsn, "file:") {
		// The driver treats a question mark after the first byte as options,
		// even for a filename without a file: scheme.
		return plainDatabaseFilePath(dsn)
	}
	uri, err := url.Parse(dsn)
	if err != nil {
		return "", false, err
	}
	if uri.User != nil || uri.Host != "" && uri.Host != "localhost" {
		return "", false, errors.New("invalid SQLite file URI authority")
	}
	path = uri.Path
	if uri.Opaque != "" {
		path, err = url.PathUnescape(uri.Opaque)
		if err != nil {
			return "", false, err
		}
	}
	if strings.ContainsRune(path, 0) || strings.Contains(uri.RawQuery, "%00") {
		return "", false, errors.New("SQLite file URI contains an escaped NUL")
	}
	query, err := url.ParseQuery(uri.RawQuery)
	if err != nil {
		return "", false, err
	}
	if len(query["mode"]) > 1 {
		return "", false, errors.New("SQLite file URI contains multiple mode options")
	}
	mode := query.Get("mode")
	if mode == "" && query.Has("mode") {
		return "", false, errors.New("empty SQLite file mode")
	}
	if mode == "memory" || path == ":memory:" || query.Get("vfs") == "memdb" {
		return "", false, nil
	}
	switch mode {
	case "ro", "rw":
		return path, false, nil
	case "", "rwc":
		return path, true, nil
	default:
		return "", false, fmt.Errorf("unknown SQLite file mode %q", mode)
	}
}

func plainDatabaseFilePath(dsn string) (string, bool, error) {
	path, rawQuery, _ := strings.Cut(dsn, "?")
	if path == "" {
		return dsn, true, nil
	}
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", false, err
	}
	if path == ":memory:" || query.Get("vfs") == "memdb" {
		return "", false, nil
	}
	return path, true, nil
}

func restrictDatabaseFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !isSingleLinkDatabase(info) {
		return fmt.Errorf("database file must be a regular file with one link: %s", path)
	}
	if info.Mode().Perm()&^databaseFileMode == 0 {
		return nil
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }() // No writes to the file contents.
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(info, opened) || !isSingleLinkDatabase(opened) {
		return fmt.Errorf("database file changed while opening: %s", path)
	}
	return file.Chmod(opened.Mode().Perm() & databaseFileMode)
}

func isSingleLinkDatabase(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode().IsRegular() && stat.Nlink == 1
}

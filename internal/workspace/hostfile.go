package workspace

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxHostFileBytes bounds the full file size, including metadata-only requests.
const MaxHostFileBytes = 512 << 20

// ValidateHostFilePath checks the supplied host spelling without accessing files.
func ValidateHostFilePath(path string) error {
	if path == "" || len(path) > 4096 || !utf8.ValidString(path) ||
		!filepath.IsAbs(path) || filepath.Clean(path) != path ||
		strings.ContainsFunc(path, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) {
		return errStatus(http.StatusBadRequest, "invalid file path")
	}
	return validateHostFilePlatformPath(path)
}

// OpenHostFile opens a current regular file, returning its checked size and
// identity. The caller owns the file; this does not freeze its contents.
func OpenHostFile(ctx context.Context, path string) (*os.File, os.FileInfo, error) {
	if err := ValidateHostFilePath(path); err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return openHostFile(path)
}

func checkedHostFile(f *os.File, before os.FileInfo) (*os.File, os.FileInfo, error) {
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, hostFileError(err)
	}
	if !info.Mode().IsRegular() || !os.SameFile(before, info) {
		f.Close()
		return nil, nil, errStatus(http.StatusConflict, "file changed while opening")
	}
	if info.Size() > MaxHostFileBytes {
		f.Close()
		return nil, nil, errStatus(http.StatusRequestEntityTooLarge, "file exceeds 512 MiB")
	}
	return f, info, nil
}

func hostFileError(err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return errStatus(http.StatusNotFound, "file not found")
	case errors.Is(err, os.ErrPermission):
		return errStatus(http.StatusForbidden, "file access denied")
	default:
		return errStatus(http.StatusInternalServerError, "could not open file")
	}
}

// HostFileVersion identifies an open file's current version from its identity,
// size and modification/change times, or returns "" when they are unavailable.
// Metadata cannot prove identical bytes, so callers must treat it as weak.
func HostFileVersion(f *os.File) string {
	return hostFileVersion(f)
}

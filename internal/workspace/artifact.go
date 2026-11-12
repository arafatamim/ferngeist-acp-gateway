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

// MaxArtifactBytes bounds the full file size, including metadata-only requests.
const MaxArtifactBytes = 512 << 20

// ValidateArtifactPath checks the supplied host spelling without accessing files.
func ValidateArtifactPath(path string) error {
	if path == "" || len(path) > 4096 || !utf8.ValidString(path) ||
		!filepath.IsAbs(path) || filepath.Clean(path) != path ||
		strings.ContainsFunc(path, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) {
		return errStatus(http.StatusBadRequest, "invalid artifact path")
	}
	return validateArtifactPlatformPath(path)
}

// OpenArtifact opens a current regular file, returning its checked size and
// identity. The caller owns the file; this does not freeze its contents.
func OpenArtifact(ctx context.Context, path string) (*os.File, os.FileInfo, error) {
	if err := ValidateArtifactPath(path); err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return openArtifact(path)
}

func checkedArtifact(f *os.File, before os.FileInfo) (*os.File, os.FileInfo, error) {
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, artifactFileError(err)
	}
	if !info.Mode().IsRegular() || !os.SameFile(before, info) {
		f.Close()
		return nil, nil, errStatus(http.StatusConflict, "artifact changed while opening")
	}
	if info.Size() > MaxArtifactBytes {
		f.Close()
		return nil, nil, errStatus(http.StatusRequestEntityTooLarge, "artifact exceeds 512 MiB")
	}
	return f, info, nil
}

func artifactFileError(err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return errStatus(http.StatusNotFound, "artifact not found")
	case errors.Is(err, os.ErrPermission):
		return errStatus(http.StatusForbidden, "artifact access denied")
	default:
		return errStatus(http.StatusInternalServerError, "could not open artifact")
	}
}

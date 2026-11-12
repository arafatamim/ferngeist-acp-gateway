package workspace

import (
	"context"
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
	return nil
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

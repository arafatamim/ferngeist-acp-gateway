//go:build !linux && !darwin && !windows

package workspace

import (
	"net/http"
	"path/filepath"
	"testing"
)

func TestHostFileUnsupportedPlatformRefusesBeforeFileLookup(t *testing.T) {
	_, _, err := OpenHostFile(t.Context(), filepath.Join(t.TempDir(), "does-not-exist"))
	wantWorkspaceStatus(t, err, http.StatusServiceUnavailable)
}

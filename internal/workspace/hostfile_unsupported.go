//go:build !linux && !darwin && !windows

package workspace

import (
	"net/http"
	"os"
)

func openHostFile(string) (*os.File, os.FileInfo, error) {
	return nil, nil, errStatus(http.StatusServiceUnavailable, "file retrieval is unsupported on this platform")
}

func validateHostFilePlatformPath(string) error {
	return nil
}

func hostFileVersion(*os.File) string { return "" }

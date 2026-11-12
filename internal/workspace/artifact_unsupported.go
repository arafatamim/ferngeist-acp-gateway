//go:build !linux && !darwin

package workspace

import (
	"net/http"
	"os"
)

func openArtifact(string) (*os.File, os.FileInfo, error) {
	return nil, nil, errStatus(http.StatusServiceUnavailable, "artifact retrieval is unsupported on this platform")
}

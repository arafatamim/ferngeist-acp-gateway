//go:build !linux || android

package service

import "io"

// RunOpenRCStateWorker is unused on platforms without the OpenRC manager.
func RunOpenRCStateWorker([]string, io.Reader, io.Writer) (bool, error) {
	return false, nil
}

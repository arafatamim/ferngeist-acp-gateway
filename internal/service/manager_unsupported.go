//go:build !linux && !windows && !darwin

package service

import (
	"fmt"
	goruntime "runtime"
)

func newOSManager() Manager {
	return unsupportedManager{
		err: fmt.Errorf("%w: %s", ErrServiceUnsupportedOS, goruntime.GOOS),
	}
}

// init keeps darwinServiceTarget non-nil on OSes without a darwin-tagged file.
func init() {
	darwinServiceTarget = func() string { return "" }
}

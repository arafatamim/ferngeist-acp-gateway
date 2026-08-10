//go:build android

package service

import (
	"fmt"
	goruntime "runtime"
)

// Android (Termux) has no systemd/launchd/Task Scheduler, so daemon service
// management is unsupported — `daemon install`/`start`/`stop` etc. all report
// ErrServiceUnsupportedOS, and `update` falls back to a direct binary swap.
// NOTE: Go treats GOOS=android as satisfying the `linux` build tag
// (go/build: "if GOOS=android, files with GOOS=linux are also matched"), so
// manager_linux.go MUST stay `//go:build linux && !android` for this file to
// win on android.
func newOSManager() Manager {
	return unsupportedManager{
		err: fmt.Errorf("%w: %s", ErrServiceUnsupportedOS, goruntime.GOOS),
	}
}

// init keeps darwinServiceTarget non-nil on android.
func init() {
	darwinServiceTarget = func() string { return "" }
}

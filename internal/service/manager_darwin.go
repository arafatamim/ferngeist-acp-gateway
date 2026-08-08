//go:build darwin

package service

import (
	"os"
	"strconv"
)

// darwinUID is the current user's uid as a string, used for gui/<uid>/<label>
// service targets. Computed once at package init.
var darwinUID = strconv.Itoa(os.Getuid())

func newOSManager() Manager {
	return &darwinManager{}
}

// init points the common darwinServiceTarget at the real launchd target so the
// control methods (which live in manager_darwin_common.go, compiled on every
// OS) act on the current user's per-user LaunchAgent.
func init() {
	darwinServiceTarget = func() string { return "gui/" + darwinUID + "/" + darwinLabel }
}

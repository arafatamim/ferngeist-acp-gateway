package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sync/atomic"
	"time"
)

// exitBinaryReplaced is the daemon's status after its binary was upgraded in
// place. Non-zero so systemd (Restart=on-failure) restarts it, as launchd
// (KeepAlive) does for any exit, onto the new binary.
const exitBinaryReplaced = 75

// daemonBinaryPath is the path the service manager started the daemon
// through. argv[0] keeps Homebrew's opt link, which os.Executable resolves
// to the versioned keg on Linux.
func daemonBinaryPath() string {
	if filepath.IsAbs(os.Args[0]) {
		return os.Args[0]
	}
	exe, _ := os.Executable()
	return exe
}

// watchBinary returns a context that is cancelled once path names a
// different file than at start: a package upgrade renames a new binary over
// it, and brew moves its opt link to the new keg. replaced reports whether
// that happened. A missing file (uninstalled) is ignored, since a restart
// would only fail. Off on Windows, whose scheduled task does not restart the
// daemon and whose `update` restarts it itself.
func watchBinary(parent context.Context, path string, interval time.Duration) (ctx context.Context, replaced func() bool) {
	var fired atomic.Bool
	start, err := os.Stat(path)
	if err != nil || goruntime.GOOS == "windows" {
		return parent, fired.Load
	}
	ctx, cancel := context.WithCancel(parent)
	go func() {
		defer cancel()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if now, err := os.Stat(path); err == nil && !os.SameFile(start, now) {
					fmt.Fprintf(os.Stderr, "%s was replaced; exiting so the service restarts on the new binary\n", path)
					fired.Store(true)
					return
				}
			}
		}
	}()
	return ctx, fired.Load
}

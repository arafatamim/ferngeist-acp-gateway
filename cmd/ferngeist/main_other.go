//go:build !windows

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// configureConsoleLifecycle is a no-op on non-Windows platforms: there is no
// console-control-event mechanism equivalent to Windows CTRL_CLOSE.
func configureConsoleLifecycle() {}

// waitForSignal returns a context cancelled by SIGINT/SIGTERM.
func waitForSignal() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

//go:build windows

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// SetConsoleCtrlHandler isn't exposed by syscall or x/sys/windows, so call it
// directly through kernel32. The callback runs on a dedicated thread when the
// console delivers CTRL_C, CTRL_BREAK, CTRL_CLOSE, CTRL_LOGOFF or CTRL_SHUTDOWN.
var procSetConsoleCtrlHandler = syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleCtrlHandler")

// consoleCtrlHandler receives console control events. Returning TRUE for
// CTRL_CLOSE keeps the process alive when its console window is closed; every
// other event falls through so Go's os/signal still sees CTRL_C/CTRL_BREAK.
// Keep the function simple: no allocations, no calls into the runtime that
// could deadlock a console-handler thread.
func consoleCtrlHandler(ctrlType uint32) uintptr {
	if ctrlType == syscall.CTRL_CLOSE_EVENT {
		return 1 // TRUE: ignore, daemon must survive console close
	}
	return 0 // FALSE: let the default handler / os/signal take it
}

// configureConsoleLifecycle makes the daemon survive console close events.
// A service-launched daemon inherits the wrapper's console (CreateNoWindow
// suppresses the window but does not detach), so closing that console would
// otherwise deliver CTRL_CLOSE and kill the daemon. Only CTRL_CLOSE is
// swallowed; interactive Ctrl+C still cancels the context via os/signal.
func configureConsoleLifecycle() {
	r1, _, err := procSetConsoleCtrlHandler.Call(
		syscall.NewCallback(consoleCtrlHandler), // the handler
		1,                                       // add = TRUE (install)
	)
	if r1 == 0 {
		// Not fatal: the wrapper's conhost --headless already avoids a visible
		// console. Log via stderr so an interactive `daemon run` still sees it.
		println("ferngeist: SetConsoleCtrlHandler failed:", err)
	}
}

// waitForSignal returns a context cancelled by Ctrl+C/SIGTERM. os/signal
// receives CTRL_C/CTRL_BREAK events normally; CTRL_CLOSE is swallowed above.
func waitForSignal() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

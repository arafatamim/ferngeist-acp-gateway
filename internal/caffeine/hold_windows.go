package caffeine

import (
	"fmt"
	"runtime"
	"syscall"
)

const (
	esContinuous     = 0x80000000
	esSystemRequired = 0x00000001
)

var setThreadExecutionState = syscall.NewLazyDLL("kernel32.dll").NewProc("SetThreadExecutionState")

// hold pins one OS thread for the inhibitor's lifetime: the execution state
// belongs to the thread that set it.
func hold() (func(), error) {
	errc := make(chan error, 1)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if r, _, err := setThreadExecutionState.Call(esContinuous | esSystemRequired); r == 0 {
			errc <- fmt.Errorf("SetThreadExecutionState: %w", err)
			return
		}
		errc <- nil
		<-stop
		_, _, _ = setThreadExecutionState.Call(esContinuous) // clearing cannot fail in a way we could act on
	}()
	if err := <-errc; err != nil {
		<-done
		return nil, err
	}
	return func() {
		close(stop)
		<-done
	}, nil
}

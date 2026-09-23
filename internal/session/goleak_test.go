package session

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package's test run if any goroutine (pump, writer,
// reaper, keepalive, proxy) is still alive after all tests complete.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

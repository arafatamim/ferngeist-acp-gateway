package projects

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package's test run if a listing leaves a goroutine
// (agent goroutine, ACP connection reader, process wait) behind.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

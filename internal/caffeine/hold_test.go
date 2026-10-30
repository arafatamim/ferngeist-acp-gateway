package caffeine

import (
	"os"
	"runtime"
	"testing"
	"time"
)

// TestHoldTakesAndReleasesInhibitor takes a real OS inhibitor, so it runs only
// on GitHub runners: on a dev machine it would briefly pin the host awake.
func TestHoldTakesAndReleasesInhibitor(t *testing.T) {
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Skip("takes a real sleep inhibitor; runs only on GitHub Actions")
	}
	release, err := hold()
	if err != nil && runtime.GOOS == "linux" {
		// The runner user has no logind session, so polkit may refuse; the refusal
		// still proves hold reports it instead of hanging.
		t.Skipf("inhibitor refused on this runner: %v", err)
	}
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	released := make(chan struct{})
	go func() {
		release()
		close(released)
	}()
	select {
	case <-released:
	case <-time.After(10 * time.Second):
		t.Fatal("release did not return")
	}
}

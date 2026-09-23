package catalog

import (
	"sync"
	"testing"
)

// TestConcurrentListGetRefresh hammers the catalog from many goroutines the
// way concurrent HTTP handlers do (List on GET, Get on POST /start). Without
// synchronization on Service state, -race flags the Refresh write vs List/Get
// reads (and write-vs-write). Must be -race-clean.
func TestConcurrentListGetRefresh(t *testing.T) {
	svc := NewWithBaseDir(t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				_ = svc.List()
				_, _ = svc.Get("mock-acp")
				_, _ = svc.Get("no-such-agent")
				if i == 0 {
					svc.Refresh()
				}
			}
		}(i)
	}
	wg.Wait()
}

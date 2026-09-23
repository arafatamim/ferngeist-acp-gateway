package session

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

func openInboundTestStore(t *testing.T) *storage.SQLiteStore {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "inbound_test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestInboundWriterSendAfterStop fails with "panic: send on closed channel"
// while stop() bare-closes the channel raced by LogInbound after Shutdown.
func TestInboundWriterSendAfterStop(t *testing.T) {
	w := newInboundWriter(openInboundTestStore(t))
	w.stop()
	if w.send(inboundDiagnostic{SessionID: "s", Seq: 1, Payload: "p"}) {
		t.Fatal("send after stop = true, want false (dropped)")
	}
}

// TestInboundWriterDoubleStop fails with "panic: close of closed channel"
// while stop() is not idempotent (double Shutdown path).
func TestInboundWriterDoubleStop(t *testing.T) {
	w := newInboundWriter(openInboundTestStore(t))
	w.stop()
	w.stop()
}

// TestInboundWriterConcurrentSendStop hammers the Shutdown interleaving:
// senders in flight while the channel is closed must drop, never panic.
func TestInboundWriterConcurrentSendStop(t *testing.T) {
	w := newInboundWriter(openInboundTestStore(t))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = w.send(inboundDiagnostic{SessionID: "s", Seq: int64(j), Payload: "p"})
			}
		}(i)
	}
	w.stop()
	wg.Wait()
	w.stop()
}

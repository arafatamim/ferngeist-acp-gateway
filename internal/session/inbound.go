package session

import (
	"context"
	"sync"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

// inboundChanSize sets the buffered channel capacity for inbound diagnostic
// writes. On overflow, frames are silently dropped — the hot path never
// blocks on audit SQLite I/O. 256 entries provides ample headroom for
// bursty client activity.
const inboundChanSize = 256

// inboundDiagnostic carries a single client-to-agent frame for async storage.
type inboundDiagnostic struct {
	SessionID string
	Seq       int64
	Payload   string
}

// inboundWriter provides an async, non-blocking path for persisting client-to-agent
// frames to SQLite. It uses a buffered channel dispatched by a single drain goroutine.
// The hot path calls send() with a non-blocking select — if the channel is full
// the frame is silently dropped so the hot path never blocks on audit I/O.
type inboundWriter struct {
	store *storage.SQLiteStore
	ch    chan inboundDiagnostic

	// mu guards closed so a send racing stop()/Shutdown can never touch a
	// closed channel. A closed writer drops (returns false), like overflow.
	mu     sync.Mutex
	closed bool
}

func newInboundWriter(store *storage.SQLiteStore) *inboundWriter {
	w := &inboundWriter{
		store: store,
		ch:    make(chan inboundDiagnostic, inboundChanSize),
	}
	go w.drain(context.Background())
	return w
}

func (w *inboundWriter) send(d inboundDiagnostic) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return false
	}
	select {
	case w.ch <- d:
		return true
	default:
		return false
	}
}

func (w *inboundWriter) drain(ctx context.Context) {
	for d := range w.ch {
		_ = w.store.AppendInboundDiagnostic(ctx, d.SessionID, d.Seq, d.Payload)
	}
}

func (w *inboundWriter) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.closed = true
	close(w.ch)
}

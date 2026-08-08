package session

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
	"github.com/coder/websocket"
)

// TestPumpDrainNotBlockedBySlowClient reproduces the freeze observed in
// production: a connected client that stops reading must not stall the stdout
// drain loop. Before the fix, handleStdoutLine wrote to the client inline; a
// client that stopped reading blocked the write (up to acpWebSocketWriteTimeout
// per frame), stalling agent stdout draining and blocking takeover (clientMu).
//
// Assertion: after the agent emits N frames, the drain loop must consume all
// of them promptly even though the client never reads. Each consumed frame
// advances lastStdoutAt; if the loop stalls on a client write, lastStdoutAt
// advances at most once and then freezes.
func TestPumpDrainNotBlockedBySlowClient(t *testing.T) {
	serverCh := make(chan *websocket.Conn, 1)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, aErr := websocket.Accept(w, r, nil)
		if aErr != nil {
			return
		}
		serverCh <- c
	}))
	defer s.Close()

	wsURL := "ws://" + s.Listener.Addr().String() + "/"
	_, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	sc := <-serverCh
	if sc == nil {
		t.Fatal("server connection not established")
	}
	defer sc.Close(websocket.StatusNormalClosure, "")

	// Never read from sc — the client is alive but slow/not reading.

	r, w := io.Pipe()
	pump := &StdioPump{
		pipes: &runtime.LeasedPipes{
			Stdin:  nopWriteCloser{},
			Stdout: r,
		},
		runtimeID: "rt-slow-client",
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	gen := pump.Attach()
	if !pump.Bind(sc, gen) {
		t.Fatal("Bind after Attach should succeed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pump.StdoutDrainLoop(ctx)

	// Write frames spaced apart so each drain-loop iteration is observable.
	const frames = 5
	marker := time.Now()
	for i := 0; i < frames; i++ {
		_, _ = w.Write([]byte("frame\n"))
		time.Sleep(150 * time.Millisecond)
	}

	// After the writes, the drain loop must have consumed the final frame —
	// lastStdoutAt must advance to near the end of the write burst within a
	// bounded time. A stalled loop (client write blocking) would consume only
	// the first frame and then freeze well before the burst ends.
	burstEnd := marker.Add(time.Duration(frames) * 150 * time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pump.lastStdoutMu.Lock()
		ts := pump.lastStdoutAt
		pump.lastStdoutMu.Unlock()
		if !ts.IsZero() && ts.After(burstEnd.Add(-300*time.Millisecond)) {
			return // pass — the loop drained through the burst
		}
		time.Sleep(50 * time.Millisecond)
	}
	pump.lastStdoutMu.Lock()
	ts := pump.lastStdoutAt
	pump.lastStdoutMu.Unlock()
	t.Fatalf("drain loop stalled: lastStdoutAt=%v, burstEnd=%v (client write blocking)", ts, burstEnd)
}

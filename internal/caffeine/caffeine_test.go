package caffeine

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func TestInhibitorFollowsBusyAndDropsOnShutdown(t *testing.T) {
	var busy atomic.Bool
	busy.Store(true)
	held, released := make(chan struct{}, 1), make(chan struct{}, 1)
	fakeHold := func() (func(), error) {
		held <- struct{}{}
		return func() { released <- struct{}{} }, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		run(ctx, tick, busy.Load, fakeHold, slog.New(slog.DiscardHandler))
	}()

	wait := func(ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
		}
	}
	wait(held, "hold while busy")
	busy.Store(false)
	tick <- time.Now()
	wait(released, "release once idle")
	busy.Store(true)
	tick <- time.Now()
	wait(held, "hold again when busy")
	cancel()
	wait(released, "release on shutdown")
	wait(done, "run to return")
}

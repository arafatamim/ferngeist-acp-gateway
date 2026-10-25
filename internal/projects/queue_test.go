package projects

import (
	"context"
	"testing"
	"time"
)

func TestQueuedListingIsRemovedWhenItsLastRequesterLeaves(t *testing.T) {
	lister := newMockLister(t)()
	for range cap(lister.slots) {
		lister.slots <- struct{}{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		lister.List(ctx, []string{"mock-acp"}, func(AgentLine) {})
	}()
	waitForRequesters(t, lister, 1)
	cancel()
	<-done
	lister.mu.Lock()
	queued := len(lister.pending)
	lister.mu.Unlock()
	if queued != 0 {
		t.Errorf("abandoned listing still occupies the queue")
	}
	waitForListings(t, lister)
	for range cap(lister.slots) {
		<-lister.slots
	}
	line := lister.agentLine(context.Background(), "mock-acp")
	if line.Status != StatusOK || len(line.Chats) != 2 {
		t.Fatalf("retry = %+v, want a complete listing", line)
	}
}

func TestCloseDrainsListingsStillWaitingForAdmission(t *testing.T) {
	lister := newMockLister(t)()
	for range cap(lister.slots) {
		lister.slots <- struct{}{}
	}
	done := make(chan AgentLine, 1)
	go func() { done <- lister.agentLine(context.Background(), "mock-acp") }()
	waitForRequesters(t, lister, 1)
	closed := make(chan struct{})
	go func() {
		lister.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not drain queued work")
	}
	if line := <-done; line.Status != StatusFailed {
		t.Fatalf("closed listing = %+v, want cancellation", line)
	}
	for range cap(lister.slots) {
		<-lister.slots
	}
}

func waitForListings(t *testing.T, lister *Lister) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		lister.listings.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("abandoned work did not release queue resources")
	}
}

func TestQueuedListingContinuesForItsRemainingRequester(t *testing.T) {
	lister := newMockLister(t)()
	for range cap(lister.slots) {
		lister.slots <- struct{}{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan AgentLine, 1)
	second := make(chan AgentLine, 1)
	go func() { first <- lister.agentLine(ctx, "mock-acp") }()
	waitForRequesters(t, lister, 1)
	go func() { second <- lister.agentLine(context.Background(), "mock-acp") }()
	waitForRequesters(t, lister, 2)
	cancel()
	<-first
	waitForRequesters(t, lister, 1)
	for range cap(lister.slots) {
		<-lister.slots
	}
	select {
	case line := <-second:
		if line.Status != StatusOK || len(line.Chats) != 2 {
			t.Fatalf("remaining requester = %+v, want complete listing", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("remaining requester never completed")
	}
}

func waitForRequesters(t *testing.T, lister *Lister, want int) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		lister.mu.Lock()
		call := lister.pending["mock-acp"]
		got := 0
		if call != nil {
			got = call.waiters
		}
		lister.mu.Unlock()
		if got == want {
			return
		}
		select {
		case <-tick.C:
		case <-timer.C:
			t.Fatalf("queued requesters = %d, want %d", got, want)
		}
	}
}

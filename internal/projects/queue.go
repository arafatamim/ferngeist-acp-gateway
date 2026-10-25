package projects

import (
	"context"
	"time"
)

type listingCall struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	started bool
	line    AgentLine
}

// agentLine answers from the cache, else joins the agent's one in-flight
// listing, so concurrent devices start it once.
func (l *Lister) agentLine(ctx context.Context, id string) AgentLine {
	failed := AgentLine{Type: "agent", AgentID: id, Chats: []Chat{}}
	if err := ctx.Err(); err != nil {
		return failed.failed(StatusFailed, err)
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return failed.failed(StatusFailed, context.Canceled)
	}
	if cached, ok := l.cached[id]; ok && time.Since(cached.at) < cacheTTL {
		l.mu.Unlock()
		return cached.line
	}
	call := l.pending[id]
	if call == nil {
		workCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		call = &listingCall{done: make(chan struct{}), cancel: cancel}
		l.pending[id] = call
		l.listings.Go(func() { l.runListing(workCtx, id, call) })
	}
	call.waiters++
	l.mu.Unlock()
	defer l.releaseRequester(id, call)
	select {
	case <-call.done:
		return call.line
	case <-ctx.Done():
		return failed.failed(StatusFailed, ctx.Err())
	}
}

func (l *Lister) releaseRequester(id string, call *listingCall) {
	l.mu.Lock()
	defer l.mu.Unlock()
	call.waiters--
	// Once started, other devices can still reuse the answer and warm process.
	if call.waiters == 0 && !call.started {
		call.cancel()
		if l.pending[id] == call {
			delete(l.pending, id)
		}
	}
}

func (l *Lister) runListing(ctx context.Context, id string, call *listingCall) {
	defer call.cancel()
	line := AgentLine{Type: "agent", AgentID: id, Chats: []Chat{}}
	if l.admitListing(ctx, call) {
		line = l.listAgent(ctx, id)
		<-l.slots
	} else {
		line = line.failed(StatusFailed, context.Canceled)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pending[id] == call {
		delete(l.pending, id)
		if !l.closed && ctx.Err() == nil {
			l.cached[id] = cachedLine{line: line, at: time.Now()}
		}
	}
	call.line = line
	close(call.done)
}

func (l *Lister) admitListing(ctx context.Context, call *listingCall) bool {
	select {
	case l.slots <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if ctx.Err() != nil || l.closed || call.waiters == 0 {
		<-l.slots
		return false
	}
	call.started = true
	return true
}

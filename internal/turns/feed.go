package turns

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"time"
)

const (
	// feedWindow is the coalescing cadence. A poke that finds no sample
	// scheduled starts one this far ahead; later pokes do not move it. Samples
	// therefore start at most once per window: intermediate states can be
	// skipped, but a change is published within one window of its mutation.
	feedWindow = 100 * time.Millisecond

	// feedBackstop re-samples a quiet feed while a stream is open. It is the
	// convergence bound for a mutation site that forgot to poke: a missed signal
	// delays a change by this long instead of leaving it stale indefinitely.
	feedBackstop = 30 * time.Second

	// maxSubscriptions caps streams daemon-wide; maxStreamsPerDevice
	// caps one device's, its oldest superseded by the next one that readies.
	maxSubscriptions    = 256
	maxStreamsPerDevice = 4

	// maxQueuedChanges bounds an active subscription's backlog. One more
	// sample than this cannot be queued, and that subscription alone is closed
	// as slow.
	maxQueuedChanges = 16
)

var (
	// ErrBusy is returned when the daemon already holds maxSubscriptions.
	ErrBusy = errors.New("turn feed is at capacity")
	// ErrSlowConsumer closes a subscription that could not keep up.
	ErrSlowConsumer = errors.New("turn subscription fell behind")
	// ErrSuperseded closes a device's oldest stream when it opens too many.
	ErrSuperseded = errors.New("turn stream superseded by a newer one")
	// ErrStopped ends every subscription when the feed stops.
	ErrStopped = errors.New("turn feed stopped")
)

// Snapshot is every known turn, running or finished, at one instant.
type Snapshot struct {
	ObservedAt time.Time `json:"observedAt"`
	Turns      []Status  `json:"turns"`
}

// Key identifies a turn entry on the wire, as `changes.removed` carries it.
type Key struct {
	AgentID          string `json:"agentId"`
	GatewaySessionID string `json:"gatewaySessionId"`
	SessionID        string `json:"sessionId"`
}

// Change is one sample's difference from what was last published.
type Change struct {
	// ObservedAt is when the sample that produced this change began, the same
	// value a snapshot carries.
	ObservedAt time.Time
	Upserts    []Status
	Removed    []Key
}

// Feed publishes the turn snapshot to stream subscribers as it changes.
//
// It never trusts individual events: a poke means "something may have changed",
// so the feed re-samples the same snapshot GET /v1/turns serves and diffs it
// against what it last published. That is what makes a poke that races the
// mutation it reports harmless: the sample sees the settled state.
//
// Run is the only goroutine that touches subscribers or the last published
// sample. The zero value is not ready; use NewFeed.
type Feed struct {
	snapshot func() Snapshot

	// changed is the poke signal, capacity one: a poke only schedules a sample.
	changed chan struct{}
	// reqs carries Subscribe, Ready and Close to Run.
	reqs chan feedRequest
	// done is closed when Run returns, unblocking any request awaiting service.
	done chan struct{}

	// Written only by Run.
	last           []Status
	lastObservedAt time.Time
	subs           map[*Subscription]struct{}
	byDevice       map[string][]*Subscription

	stats feedStats
}

// feedStats are the counters Stats reports, updated from several
// goroutines.
type feedStats struct {
	subscribers  atomic.Int64
	samples      atomic.Uint64
	pokes        atomic.Uint64
	lastSampleAt atomic.Int64
}

// FeedStats is the feed's health snapshot: a LastSampleAt that stops
// advancing while Subscribers is non-zero means Run is not sampling.
type FeedStats struct {
	Subscribers  int       `json:"subscribers"`
	Samples      uint64    `json:"samples"`
	Pokes        uint64    `json:"pokes"`
	LastSampleAt time.Time `json:"lastSampleAt"`
}

// Subscription is one stream's view of the feed. Its channel closes when
// the subscription ends; Err then says why.
type Subscription struct {
	feed     *Feed
	deviceID string
	changes  chan Change

	// Written only by Run.
	pending bool
	active  bool
	// base is the snapshot handed to a subscription that has not readied yet;
	// its catch-up is the difference from base to the latest sample. Neither a
	// queue nor a message per sample, so a slow snapshot write cannot build a
	// backlog.
	base []Status
	err  error
}

// NewFeed returns a feed sampling snapshots from snapshot. It serves nothing
// until its owner calls Run.
func NewFeed(snapshot func() Snapshot) *Feed {
	return &Feed{
		snapshot: snapshot,
		changed:  make(chan struct{}, 1),
		reqs:     make(chan feedRequest),
		done:     make(chan struct{}),
		subs:     make(map[*Subscription]struct{}),
		byDevice: make(map[string][]*Subscription),
	}
}

// Changed reports that the turn snapshot may have changed. It never blocks and
// is safe to call under any lock, including a mutation site's.
func (f *Feed) Changed() {
	select {
	case f.changed <- struct{}{}:
	default:
	}
}

// Run serves the feed until ctx is cancelled: it samples one coalescing window
// after a poke, re-samples every backstop interval while a stream is open, and
// serves Subscribe, Ready and Close. It closes every subscription with
// ErrStopped before returning.
func (f *Feed) Run(ctx context.Context) {
	defer close(f.done)
	backstop := time.NewTicker(feedBackstop)
	defer backstop.Stop()

	// window is non-nil while a sample is scheduled. Later pokes leave it
	// alone, so the cadence is fixed rather than a debounce.
	var window <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			for sub := range f.subs {
				f.end(sub, ErrStopped)
			}
			return
		case req := <-f.reqs:
			req.serve(f)
		case <-f.changed:
			f.stats.pokes.Add(1)
			if len(f.subs) == 0 {
				// Nothing can read a sample, so do not take one and forget
				// what was last published.
				f.last, f.lastObservedAt = nil, time.Time{}
				continue
			}
			if window == nil {
				window = time.After(feedWindow)
			}
		case <-window:
			window = nil
			f.sample()
		case <-backstop.C:
			if len(f.subs) > 0 {
				f.sample()
			}
		}
	}
}

// Stats reports the feed's counters.
func (f *Feed) Stats() FeedStats {
	st := FeedStats{
		Subscribers: int(f.stats.subscribers.Load()),
		Samples:     f.stats.samples.Load(),
		Pokes:       f.stats.pokes.Load(),
	}
	if nanos := f.stats.lastSampleAt.Load(); nanos != 0 {
		st.LastSampleAt = time.Unix(0, nanos).UTC()
	}
	return st
}

// Subscribe reserves a slot, samples, and returns the sample as the caller's
// snapshot. The subscription starts pending (not subject to the queue bound)
// until Ready, which the caller calls once the snapshot write has succeeded.
func (f *Feed) Subscribe(ctx context.Context, deviceID string) (Snapshot, *Subscription, error) {
	req := &subscribeRequest{ctx: ctx, deviceID: deviceID, reply: make(chan subscribeResult, 1)}
	select {
	case f.reqs <- req:
	case <-f.done:
		return Snapshot{}, nil, ErrStopped
	case <-ctx.Done():
		return Snapshot{}, nil, ctx.Err()
	}
	select {
	case res := <-req.reply:
		return res.snapshot, res.sub, res.err
	case <-f.done:
		return Snapshot{}, nil, ErrStopped
	case <-ctx.Done():
		// Run may have served the request already: take its answer so the
		// reservation is not left behind, then report the cancellation.
		select {
		case res := <-req.reply:
			if res.sub != nil {
				res.sub.Close()
			}
		case <-f.done:
		}
		return Snapshot{}, nil, ctx.Err()
	}
}

// Changes is the stream of turn changes, closed when the subscription ends.
func (s *Subscription) Changes() <-chan Change { return s.changes }

// Err reports why the subscription ended; it is meaningful once Changes has
// closed. A caller that closed the subscription itself sees nil.
func (s *Subscription) Err() error { return s.err }

// Ready moves the subscription from pending to active, after its snapshot
// write. It delivers the consolidated catch-up first and then applies the
// per-device limit: the device's oldest stream is superseded if a fifth one
// readies.
func (s *Subscription) Ready() {
	req := readyRequest{sub: s, ack: make(chan struct{})}
	select {
	case s.feed.reqs <- req:
	case <-s.feed.done:
		return
	}
	select {
	case <-req.ack:
	case <-s.feed.done:
	}
}

// Close ends the subscription, freeing its device and global slots at once. It
// is idempotent and safe from any return path of a stream handler.
func (s *Subscription) Close() {
	req := closeRequest{sub: s, ack: make(chan struct{})}
	select {
	case s.feed.reqs <- req:
	case <-s.feed.done:
		return
	}
	select {
	case <-req.ack:
	case <-s.feed.done:
	}
}

// sample takes one snapshot and publishes its difference from the last one to
// every active subscription. An empty difference publishes nothing. Run-only.
func (f *Feed) sample() Snapshot {
	snap := f.snapshot()
	change := diffTurns(f.last, snap)
	f.last, f.lastObservedAt = snap.Turns, snap.ObservedAt
	f.stats.samples.Add(1)
	f.stats.lastSampleAt.Store(snap.ObservedAt.UnixNano())
	if len(change.Upserts) == 0 && len(change.Removed) == 0 {
		return snap
	}
	for sub := range f.subs {
		if !sub.active {
			continue
		}
		select {
		case sub.changes <- change:
		default:
			f.end(sub, ErrSlowConsumer)
		}
	}
	return snap
}

// end closes a subscription and frees its slots. Idempotent; Run-only.
func (f *Feed) end(sub *Subscription, err error) {
	if _, ok := f.subs[sub]; !ok {
		return
	}
	delete(f.subs, sub)
	f.stats.subscribers.Add(-1)
	if sub.active {
		f.byDevice[sub.deviceID] = slices.DeleteFunc(f.byDevice[sub.deviceID],
			func(s *Subscription) bool { return s == sub })
		if len(f.byDevice[sub.deviceID]) == 0 {
			delete(f.byDevice, sub.deviceID)
		}
	}
	sub.pending, sub.active, sub.base = false, false, nil
	sub.err = err
	// Drop the backlog so the reader sees the close next: a slow or superseded
	// client would otherwise be sent up to maxQueuedChanges frames first.
	// Non-blocking: the reader may take the last one concurrently.
drain:
	for {
		select {
		case <-sub.changes:
		default:
			break drain
		}
	}
	close(sub.changes)
	if len(f.subs) == 0 {
		f.last, f.lastObservedAt = nil, time.Time{}
	}
}

// diffTurns reports how next differs from the last published snapshot. Upserts
// keep the snapshot's order and removed keys keep the previous snapshot's,
// which is the same order, so both arrays are sorted like GET /v1/turns.
func diffTurns(last []Status, next Snapshot) Change {
	change := Change{ObservedAt: next.ObservedAt}
	previous := make(map[Key]Status, len(last))
	for _, st := range last {
		previous[keyOf(st)] = st
	}
	present := make(map[Key]bool, len(next.Turns))
	for _, st := range next.Turns {
		k := keyOf(st)
		present[k] = true
		if old, ok := previous[k]; !ok || !sameStatus(old, st) {
			change.Upserts = append(change.Upserts, st)
		}
	}
	for _, st := range last {
		if k := keyOf(st); !present[k] {
			change.Removed = append(change.Removed, k)
		}
	}
	return change
}

func keyOf(st Status) Key {
	return Key{AgentID: st.AgentID, GatewaySessionID: st.GatewaySessionID, SessionID: st.SessionID}
}

// sameStatus compares every member. An attention change alone is a difference:
// the client's entry is replaced with the new attention.
func sameStatus(a, b Status) bool {
	return a.AgentID == b.AgentID && a.SessionID == b.SessionID &&
		a.GatewaySessionID == b.GatewaySessionID && a.RuntimeID == b.RuntimeID &&
		a.State == b.State && a.StopReason == b.StopReason &&
		a.StartedAt.Equal(b.StartedAt) && a.ChangedAt.Equal(b.ChangedAt) &&
		slices.Equal(a.Attention, b.Attention)
}

// feedRequest is one operation Run serves in order with its samples.
type feedRequest interface {
	serve(*Feed)
}

type subscribeResult struct {
	snapshot Snapshot
	sub      *Subscription
	err      error
}

type subscribeRequest struct {
	ctx      context.Context
	deviceID string
	reply    chan subscribeResult
}

func (r *subscribeRequest) serve(f *Feed) {
	if r.ctx.Err() != nil {
		r.reply <- subscribeResult{err: r.ctx.Err()}
		return
	}
	if len(f.subs) >= maxSubscriptions {
		r.reply <- subscribeResult{err: ErrBusy}
		return
	}
	// The sample is the caller's snapshot and, at the same time, the diff
	// existing subscribers must not miss. One goroutine does both, so no
	// change can fall between a subscriber's snapshot and its first message.
	snap := f.sample()
	sub := &Subscription{
		feed:     f,
		deviceID: r.deviceID,
		changes:  make(chan Change, maxQueuedChanges),
		pending:  true,
		base:     snap.Turns,
	}
	f.subs[sub] = struct{}{}
	f.stats.subscribers.Add(1)
	r.reply <- subscribeResult{snapshot: snap, sub: sub}
}

type readyRequest struct {
	sub *Subscription
	ack chan struct{}
}

func (r readyRequest) serve(f *Feed) {
	defer close(r.ack)
	sub := r.sub
	if _, ok := f.subs[sub]; !ok || !sub.pending {
		return
	}
	// The catch-up is the cumulative difference from the snapshot this
	// subscription was handed, so it holds one message however many samples
	// ran while the snapshot was being written.
	change := diffTurns(sub.base, Snapshot{ObservedAt: f.lastObservedAt, Turns: f.last})
	sub.base, sub.pending, sub.active = nil, false, true
	if len(change.Upserts) > 0 || len(change.Removed) > 0 {
		sub.changes <- change
	}
	f.byDevice[sub.deviceID] = append(f.byDevice[sub.deviceID], sub)
	if streams := f.byDevice[sub.deviceID]; len(streams) > maxStreamsPerDevice {
		f.end(streams[0], ErrSuperseded)
	}
}

type closeRequest struct {
	sub *Subscription
	ack chan struct{}
}

func (r closeRequest) serve(f *Feed) {
	defer close(r.ack)
	f.end(r.sub, nil)
}

package turns

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// turnSource is a scripted Snapshot source: a test sets the entries the
// feed samples.
type turnSource struct {
	mu      sync.Mutex
	entries []Status
}

func newTurnSource(entries ...Status) *turnSource {
	return &turnSource{entries: entries}
}

func (s *turnSource) snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Snapshot{ObservedAt: time.Now().UTC(), Turns: slices.Clone(s.entries)}
}

func (s *turnSource) set(entries ...Status) {
	s.mu.Lock()
	s.entries = entries
	s.mu.Unlock()
}

func statusOf(agentID, gatewaySessionID, sessionID, state string) Status {
	at := time.Unix(1_700_000_000, 0).UTC()
	return Status{
		AgentID:          agentID,
		SessionID:        sessionID,
		GatewaySessionID: gatewaySessionID,
		RuntimeID:        "rt_" + gatewaySessionID,
		State:            state,
		Attention:        []string{},
		StartedAt:        at,
		ChangedAt:        at,
	}
}

// startFeed runs a feed over src and stops it with the test.
func startFeed(t *testing.T, src *turnSource) *Feed {
	t.Helper()
	feed := NewFeed(src.snapshot)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		feed.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return feed
}

// subscribeReady takes a subscription and marks its snapshot written, then
// closes it with the test.
func subscribeReady(t *testing.T, feed *Feed, deviceID string) *Subscription {
	t.Helper()
	_, sub, err := feed.Subscribe(context.Background(), deviceID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	sub.Ready()
	t.Cleanup(sub.Close)
	return sub
}

// nextChange waits for the next change, failing the test if none arrives.
func nextChange(t *testing.T, sub *Subscription) Change {
	t.Helper()
	select {
	case change, ok := <-sub.Changes():
		if !ok {
			t.Fatalf("subscription ended: %v", sub.Err())
		}
		return change
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a turn change")
		return Change{}
	}
}

// expectNoChange asserts that sub publishes nothing for d.
func expectNoChange(t *testing.T, sub *Subscription, d time.Duration) {
	t.Helper()
	select {
	case change, ok := <-sub.Changes():
		if !ok {
			t.Fatalf("subscription ended: %v", sub.Err())
		}
		t.Fatalf("unexpected change: %+v", change)
	case <-time.After(d):
	}
}

// drainSubscription reads until the subscription's channel closes and returns
// why it ended.
func drainSubscription(t *testing.T, sub *Subscription) error {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-sub.Changes():
			if !ok {
				return sub.Err()
			}
		case <-timeout:
			t.Fatal("subscription did not end")
		}
	}
}

// waitForSamples waits until the feed has taken n samples.
func waitForSamples(t *testing.T, feed *Feed, n uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for feed.Stats().Samples < n {
		if time.Now().After(deadline) {
			t.Fatalf("samples = %d, want at least %d", feed.Stats().Samples, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// Pokes inside one coalescing window produce exactly one message carrying every
// change; the cadence is fixed, not restarted per poke.
func TestFeedCoalescesMutationsIntoOneMessage(t *testing.T) {
	src := newTurnSource()
	feed := startFeed(t, src)
	sub := subscribeReady(t, feed, "dev")

	src.set(statusOf("agent", "gs_a", "ses_a", StateRunning))
	feed.Changed()
	src.set(statusOf("agent", "gs_a", "ses_a", StateRunning),
		statusOf("agent", "gs_a", "ses_b", StateRunning))
	feed.Changed()

	change := nextChange(t, sub)
	if len(change.Upserts) != 2 || len(change.Removed) != 0 {
		t.Fatalf("change = %+v, want both turns in one message", change)
	}
	if got := feed.Stats().Samples; got != 2 {
		t.Fatalf("samples = %d, want one sample for the subscription and one for the pokes", got)
	}
	expectNoChange(t, sub, 300*time.Millisecond)
	if got := feed.Stats().Samples; got != 2 {
		t.Fatalf("samples = %d after the window, want still 2", got)
	}
}

// A poke that finds nothing changed publishes nothing, and the next real change
// still arrives.
func TestFeedPublishesNothingForANoOpSample(t *testing.T) {
	src := newTurnSource(statusOf("agent", "gs_a", "ses_a", StateRunning))
	feed := startFeed(t, src)
	sub := subscribeReady(t, feed, "dev")

	feed.Changed()
	expectNoChange(t, sub, 300*time.Millisecond)
	if got := feed.Stats().Samples; got != 2 {
		t.Fatalf("samples = %d, want the no-op sample to have run", got)
	}

	src.set(statusOf("agent", "gs_a", "ses_a", StateSucceeded))
	feed.Changed()
	change := nextChange(t, sub)
	if len(change.Upserts) != 1 || change.Upserts[0].State != StateSucceeded {
		t.Fatalf("change = %+v, want the finished turn", change)
	}
}

// An attention-only change is an upsert: the entry's times and state are
// identical.
func TestFeedUpsertsAnAttentionOnlyChange(t *testing.T) {
	running := statusOf("agent", "gs_a", "ses_a", StateRunning)
	src := newTurnSource(running)
	feed := startFeed(t, src)
	sub := subscribeReady(t, feed, "dev")

	flagged := running
	flagged.Attention = []string{AttentionPermission}
	src.set(flagged)
	feed.Changed()

	change := nextChange(t, sub)
	if len(change.Upserts) != 1 || len(change.Removed) != 0 {
		t.Fatalf("change = %+v, want one upsert", change)
	}
	if got := change.Upserts[0].Attention; !slices.Equal(got, []string{AttentionPermission}) {
		t.Fatalf("attention = %v, want [permission]", got)
	}
}

// An entry that vanishes from the source is reported by key.
func TestFeedReportsAVanishedEntryAsRemoved(t *testing.T) {
	src := newTurnSource(statusOf("agent", "gs_a", "ses_a", StateRunning))
	feed := startFeed(t, src)
	sub := subscribeReady(t, feed, "dev")

	src.set()
	feed.Changed()

	change := nextChange(t, sub)
	want := Key{AgentID: "agent", GatewaySessionID: "gs_a", SessionID: "ses_a"}
	if len(change.Removed) != 1 || change.Removed[0] != want || len(change.Upserts) != 0 {
		t.Fatalf("change = %+v, want removed %+v", change, want)
	}
}

// A subscriber that joins while another is streaming starts from a snapshot and
// then receives the change the other saw, with no gap and no duplicate.
func TestFeedIsContinuousFromSnapshotToChanges(t *testing.T) {
	first := statusOf("agent", "gs_a", "ses_a", StateRunning)
	second := statusOf("agent", "gs_a", "ses_a", StateSucceeded)
	src := newTurnSource(first)
	feed := startFeed(t, src)
	watcher := subscribeReady(t, feed, "dev-watcher")

	// The new subscription's snapshot is taken while ses_a still runs.
	_, joining, err := feed.Subscribe(context.Background(), "dev-joining")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(joining.Close)

	// The turn finishes before the joining client has written its snapshot.
	src.set(second)
	feed.Changed()
	waitForSamples(t, feed, 3)
	joining.Ready()

	// The joining stream's first change is the one the watcher also received.
	got := nextChange(t, joining)
	if len(got.Upserts) != 1 || got.Upserts[0].State != StateSucceeded {
		t.Fatalf("joining change = %+v, want the finished turn", got)
	}
	watcherChange := nextChange(t, watcher)
	if len(watcherChange.Upserts) != 1 || watcherChange.Upserts[0].State != StateSucceeded {
		t.Fatalf("watcher change = %+v, want the finished turn", watcherChange)
	}
	expectNoChange(t, joining, 300*time.Millisecond)
}

// A subscription that never reads is closed as a slow consumer without the
// other subscriber missing a message.
func TestFeedDropsASlowSubscriberWithoutStallingOthers(t *testing.T) {
	src := newTurnSource()
	feed := startFeed(t, src)
	fast := subscribeReady(t, feed, "dev")
	_, slow, err := feed.Subscribe(context.Background(), "dev-slow")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	slow.Ready()

	received := make(chan Change, 64)
	go func() {
		for change := range fast.Changes() {
			received <- change
		}
	}()

	// One distinct change per window, more than the queue holds.
	for i := 0; i <= maxQueuedChanges; i++ {
		before := feed.Stats().Samples
		src.set(statusOf("agent", "gs_a", "ses_a", StateRunning),
			statusOf("agent", "gs_a", fmt.Sprintf("ses_%02d", i), StateRunning))
		feed.Changed()
		waitForSamples(t, feed, before+1)
	}
	deadline := time.Now().Add(5 * time.Second)
	for feed.Stats().Subscribers != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	// Ending drops the backlog, so the slow client is sent the close next
	// instead of maxQueuedChanges stale frames first.
	if n := len(slow.Changes()); n != 0 {
		t.Fatalf("ended slow subscription still holds %d changes", n)
	}
	if err := drainSubscription(t, slow); !errors.Is(err, ErrSlowConsumer) {
		t.Fatalf("slow subscription ended with %v, want %v", err, ErrSlowConsumer)
	}
	deadline = time.Now().Add(5 * time.Second)
	for len(received) < maxQueuedChanges+1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := len(received); got != maxQueuedChanges+1 {
		t.Fatalf("fast subscriber received %d changes, want %d", got, maxQueuedChanges+1)
	}
	if got := feed.Stats().Subscribers; got != 1 {
		t.Fatalf("subscribers = %d, want the slow one freed", got)
	}
}

// A pending subscription consolidates its backlog: however many samples run
// before Ready, it holds one catch-up, not one message per sample.
func TestFeedConsolidatesAPendingSubscribersBacklog(t *testing.T) {
	src := newTurnSource(statusOf("agent", "gs_a", "ses_a", StateRunning))
	feed := startFeed(t, src)
	_, pending, err := feed.Subscribe(context.Background(), "dev")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(pending.Close)

	for i := 0; i < 5; i++ {
		running := statusOf("agent", "gs_a", "ses_a", StateRunning)
		running.ChangedAt = running.ChangedAt.Add(time.Duration(i) * time.Second)
		src.set(running)
		feed.Changed()
		waitForSamples(t, feed, uint64(i+2))
	}
	select {
	case change := <-pending.Changes():
		t.Fatalf("pending subscription queued %+v before Ready", change)
	default:
	}

	pending.Ready()
	change := nextChange(t, pending)
	want := statusOf("agent", "gs_a", "ses_a", StateRunning)
	want.ChangedAt = want.ChangedAt.Add(4 * time.Second)
	if len(change.Upserts) != 1 || !change.Upserts[0].ChangedAt.Equal(want.ChangedAt) {
		t.Fatalf("catch-up = %+v, want one upsert of the final sample", change)
	}
	expectNoChange(t, pending, 300*time.Millisecond)
}

// A device's fifth stream supersedes its oldest, and a subscription that never
// readies evicts nobody.
func TestFeedSupersedesADevicesOldestStreamPastTheLimit(t *testing.T) {
	feed := startFeed(t, newTurnSource())

	subs := make([]*Subscription, maxStreamsPerDevice)
	for i := range subs {
		subs[i] = subscribeReady(t, feed, "dev")
	}
	oldest := subs[0]

	_, fifth, err := feed.Subscribe(context.Background(), "dev")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(fifth.Close)
	for _, sub := range subs {
		expectNoChange(t, sub, 50*time.Millisecond)
	}

	fifth.Ready()
	if err := drainSubscription(t, oldest); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("oldest stream ended with %v, want %v", err, ErrSuperseded)
	}
	for _, sub := range subs[1:] {
		expectNoChange(t, sub, 50*time.Millisecond)
	}
}

// The daemon-wide cap refuses the 257th subscription without disturbing the
// 256 that hold it.
func TestFeedRefusesThe257thSubscription(t *testing.T) {
	feed := startFeed(t, newTurnSource())

	subs := make([]*Subscription, 0, maxSubscriptions)
	for i := 0; i < maxSubscriptions; i++ {
		subs = append(subs, subscribeReady(t, feed, fmt.Sprintf("dev-%03d", i)))
	}

	if _, _, err := feed.Subscribe(context.Background(), "dev-overflow"); !errors.Is(err, ErrBusy) {
		t.Fatalf("257th Subscribe = %v, want %v", err, ErrBusy)
	}
	if got := feed.Stats().Subscribers; got != maxSubscriptions {
		t.Fatalf("subscribers = %d, want %d", got, maxSubscriptions)
	}
	for _, sub := range []*Subscription{subs[0], subs[maxSubscriptions/2], subs[len(subs)-1]} {
		expectNoChange(t, sub, 10*time.Millisecond)
	}
}

// Stopping the feed ends every subscription with ErrStopped, and a Subscribe
// after stop returns instead of parking.
func TestFeedStopEndsEverySubscription(t *testing.T) {
	src := newTurnSource()
	feed := NewFeed(src.snapshot)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		feed.Run(ctx)
	}()

	_, sub, err := feed.Subscribe(context.Background(), "dev")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	sub.Ready()

	cancel()
	<-stopped
	if err := drainSubscription(t, sub); !errors.Is(err, ErrStopped) {
		t.Fatalf("subscription ended with %v, want %v", err, ErrStopped)
	}
	if _, _, err := feed.Subscribe(context.Background(), "dev"); !errors.Is(err, ErrStopped) {
		t.Fatalf("Subscribe after stop = %v, want %v", err, ErrStopped)
	}
}

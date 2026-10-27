package turns

import (
	"fmt"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

func TestOutcomeTreatsOnlyEndTurnAsSuccess(t *testing.T) {
	for _, tc := range []struct {
		errored bool
		reason  acp.StopReason
		want    string
	}{
		{false, acp.StopReasonEndTurn, StateSucceeded},
		{false, acp.StopReasonCancelled, StateCancelled},
		{false, acp.StopReasonMaxTokens, StateFailed},
		{false, acp.StopReasonMaxTurnRequests, StateFailed},
		{false, acp.StopReasonRefusal, StateFailed},
		{false, "", StateUnknown},
		{false, "paused", StateUnknown},
		{true, "", StateFailed},
		{true, acp.StopReasonEndTurn, StateFailed},
	} {
		if got := Outcome(tc.errored, tc.reason); got != tc.want {
			t.Errorf("Outcome(%v, %q) = %q, want %q", tc.errored, tc.reason, got, tc.want)
		}
	}
}

func TestRecordKeepsTheLaterStartedTurnWhateverTheArrivalOrder(t *testing.T) {
	t0 := time.Unix(100, 0)
	older := Status{AgentID: "a", SessionID: "s", GatewaySessionID: "g", State: StateFailed, StartedAt: t0}
	newer := older
	newer.State, newer.StartedAt = StateSucceeded, t0.Add(time.Second)

	for name, order := range map[string][]Status{"in order": {older, newer}, "late older reply": {newer, older}} {
		s := NewStore()
		for _, st := range order {
			s.Record(st)
		}
		got := s.Snapshot(nil)
		if len(got) != 1 || got[0].State != StateSucceeded {
			t.Errorf("%s: snapshot = %+v, want only the newer success", name, got)
		}
	}
}

func TestRecordEvictsTheOldestOutcomeAtTheCap(t *testing.T) {
	s := NewStore()
	base := time.Unix(1000, 0)
	for i := range maxOutcomes {
		// Two outcomes share the oldest time; the key breaks the tie.
		changed := base.Add(time.Duration(i) * time.Second)
		if i == 1 {
			changed = base
		}
		s.Record(Status{AgentID: "a", SessionID: fmt.Sprintf("s%05d", i), ChangedAt: changed})
	}
	s.Record(Status{AgentID: "a", SessionID: "z-new", ChangedAt: base.Add(time.Hour)})

	got := s.Snapshot(nil)
	if len(got) != maxOutcomes {
		t.Fatalf("store holds %d outcomes, want cap %d", len(got), maxOutcomes)
	}
	if got[0].SessionID != "s00001" {
		t.Fatalf("first remaining = %q, want s00000 evicted and s00001 kept", got[0].SessionID)
	}
}

func TestSnapshotLetsARunningTurnHideItsChatsOutcomeAndSorts(t *testing.T) {
	s := NewStore()
	s.Record(Status{AgentID: "b", SessionID: "x", GatewaySessionID: "g1", State: StateSucceeded})
	s.Record(Status{AgentID: "a", SessionID: "y", GatewaySessionID: "g2", State: StateFailed})
	s.Record(Status{AgentID: "a", SessionID: "y", GatewaySessionID: "g1", State: StateCancelled})

	got := s.Snapshot([]Status{{AgentID: "b", SessionID: "x", GatewaySessionID: "g1", State: StateRunning, Attention: []string{AttentionPermission}}})
	want := []string{"a/y/g1/cancelled", "a/y/g2/failed", "b/x/g1/running"}
	if len(got) != len(want) {
		t.Fatalf("snapshot = %+v", got)
	}
	for i, st := range got {
		if line := st.AgentID + "/" + st.SessionID + "/" + st.GatewaySessionID + "/" + st.State; line != want[i] {
			t.Errorf("snapshot[%d] = %s, want %s", i, line, want[i])
		}
		if st.State != StateRunning && (st.Attention == nil || len(st.Attention) != 0) {
			t.Errorf("stored outcome attention = %v, want []", st.Attention)
		}
	}
	if s.Snapshot(nil) == nil {
		t.Fatal("empty-live snapshot must not be nil")
	}
}

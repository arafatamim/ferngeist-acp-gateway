package session

import (
	"testing"
	"time"
)

func TestFormatPushDuration(t *testing.T) {
	cases := map[time.Duration]string{
		3 * time.Second:                "3s",
		12 * time.Second:               "12s",
		59 * time.Second:               "59s",
		4 * time.Minute:                "4m",
		4*time.Minute + 30*time.Second: "4m",
		65 * time.Minute:               "1h 5m",
		time.Hour:                      "1h",
	}
	for d, want := range cases {
		if got := formatPushDuration(d); got != want {
			t.Errorf("formatPushDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

// turnCompletePush runs the notification stage for one prompt result and
// reports the pushed bodies.
func turnCompletePush(stopReason string, startedAt time.Time) []PushEvent {
	p := newRecoveryPump()
	var fired []PushEvent
	p.onPushNotification = func(e PushEvent) { fired = append(fired, e) }
	line := `{"jsonrpc":"2.0","id":1,"result":{"stopReason":"` + stopReason + `"}}`
	probe, ok := parseFrameProbe([]byte(line))
	p.notifyFrame(&frame{
		line:   line,
		raw:    []byte(line),
		probe:  probe,
		ok:     ok,
		origin: pendingRequest{method: "session/prompt", startedAt: startedAt},
		out:    []string{line},
	})
	return fired
}

func TestNotifyTurnCompleteIncludesDuration(t *testing.T) {
	fired := turnCompletePush("end_turn", time.Now().Add(-4*time.Minute))
	if len(fired) != 1 || fired[0].Body != "Your agent finished in 4m." {
		t.Fatalf("long turn push = %+v, want finished-in-4m body", fired)
	}
}

func TestNotifyTurnCompleteLeavesShortTurnsAlone(t *testing.T) {
	for _, startedAt := range []time.Time{time.Now().Add(-3 * time.Second), {}} {
		fired := turnCompletePush("end_turn", startedAt)
		if len(fired) != 1 || fired[0].Body != "Your agent has finished processing." {
			t.Fatalf("short turn push = %+v, want legacy body", fired)
		}
	}
}

func TestNotifyTurnCompleteSkipsCancelled(t *testing.T) {
	if fired := turnCompletePush("cancelled", time.Now().Add(-4*time.Minute)); len(fired) != 0 {
		t.Fatalf("cancelled turn pushed %+v, want nothing", fired)
	}
}

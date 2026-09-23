package api

import (
	"fmt"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/config"
)

func testRateLimiterConfig() config.Config {
	return config.Config{
		PairingBurstPerIP:      2,
		PairingBurstGlobal:     10000,
		PairingStartRefill:     time.Second,
		PairingCompleteRefill:  time.Second,
		ListenAddr:             "127.0.0.1:0",
	}
}

// TestStaleIPBucketsAreSwept fails while per-IP buckets are never evicted:
// two IPs seen minutes apart must not both occupy the map.
func TestStaleIPBucketsAreSwept(t *testing.T) {
	l := newPairingRateLimiter(testRateLimiterConfig())
	t0 := time.Now().UTC()
	if !l.allow("10.0.0.1", true, t0) {
		t.Fatal("first allow = false, want true")
	}
	if got := len(l.ipStartBuckets); got != 1 {
		t.Fatalf("len(ipStartBuckets) = %d, want 1", got)
	}
	// Idle far past the full-refill window (2 tokens x 1s) and the sweep
	// interval: the stale entry must be gone after the next request.
	later := t0.Add(2 * time.Minute)
	if !l.allow("10.0.0.2", true, later) {
		t.Fatal("second allow = false, want true")
	}
	if got := len(l.ipStartBuckets); got != 1 {
		t.Fatalf("len(ipStartBuckets) = %d after sweep, want 1 (stale entry evicted)", got)
	}
}

// TestStaleCompleteBucketsAreSwept covers the /pair/complete map symmetrically.
func TestStaleCompleteBucketsAreSwept(t *testing.T) {
	l := newPairingRateLimiter(testRateLimiterConfig())
	t0 := time.Now().UTC()
	if !l.allow("10.0.0.1", false, t0) {
		t.Fatal("first allow = false, want true")
	}
	later := t0.Add(2 * time.Minute)
	if !l.allow("10.0.0.2", false, later) {
		t.Fatal("second allow = false, want true")
	}
	if got := len(l.ipDoneBuckets); got != 1 {
		t.Fatalf("len(ipDoneBuckets) = %d after sweep, want 1 (stale entry evicted)", got)
	}
}

// TestEvictedBucketComesBackFull verifies eviction is rate-limit neutral: an
// idle bucket would have refilled to capacity anyway, so a returning IP must
// get a full burst after its entry was swept.
func TestEvictedBucketComesBackFull(t *testing.T) {
	l := newPairingRateLimiter(testRateLimiterConfig())
	t0 := time.Now().UTC()
	if !l.allow("10.0.0.1", true, t0) || !l.allow("10.0.0.1", true, t0) {
		t.Fatal("burst of 2 should be allowed")
	}
	if l.allow("10.0.0.1", true, t0) {
		t.Fatal("third immediate request should be rate-limited")
	}
	later := t0.Add(2 * time.Minute)
	if !l.allow("10.0.0.1", true, later) {
		t.Fatal("returning IP after idle window should get a fresh burst")
	}
}

// TestIPBucketCountIsCapped bounds memory under a distinct-IP flood inside a
// single sweep window (all entries fresh, so sweeping alone cannot help).
func TestIPBucketCountIsCapped(t *testing.T) {
	l := newPairingRateLimiter(testRateLimiterConfig())
	t0 := time.Now().UTC()
	total := pairingMaxIPBuckets + 500
	for i := 0; i < total; i++ {
		l.allow(fmt.Sprintf("10.1.%d.%d", i>>8, i&0xff), true, t0)
		l.allow(fmt.Sprintf("10.1.%d.%d", i>>8, i&0xff), false, t0)
	}
	if got := len(l.ipStartBuckets); got > pairingMaxIPBuckets {
		t.Fatalf("len(ipStartBuckets) = %d, want <= %d", got, pairingMaxIPBuckets)
	}
	if got := len(l.ipDoneBuckets); got > pairingMaxIPBuckets {
		t.Fatalf("len(ipDoneBuckets) = %d, want <= %d", got, pairingMaxIPBuckets)
	}
}

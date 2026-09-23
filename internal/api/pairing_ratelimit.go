package api

import (
	"sync"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/config"
)

// tokenBucket implements a simple token bucket rate limiter with continuous
// refill. Tokens are refilled based on elapsed time since the last access,
// up to a maximum capacity.
type tokenBucket struct {
	tokens float64
	last   time.Time
}

// pairingRateLimiter protects the pairing endpoints from brute-force attacks
// by maintaining separate token buckets for:
//   - Per-IP start requests (pairStart)
//   - Per-IP complete requests (pairComplete)
//   - Global start requests (across all IPs)
//   - Global complete requests (across all IPs)
//
// This allows burst tolerance for legitimate use while throttling sustained abuse.
type pairingRateLimiter struct {
	mu             sync.Mutex
	ipStartBuckets map[string]tokenBucket // per-IP buckets for /pair/start
	ipDoneBuckets  map[string]tokenBucket // per-IP buckets for /pair/complete
	globalStart    tokenBucket            // global bucket for /pair/start
	globalDone     tokenBucket            // global bucket for /pair/complete
	burstPerIP     int                    // max tokens per IP bucket
	globalBurst    int                    // max tokens for global bucket
	startRefill    time.Duration          // refill interval for start buckets
	completeRefill time.Duration          // refill interval for complete buckets
	lastSweep      time.Time              // last amortized sweep of idle per-IP buckets
}

// newPairingRateLimiter creates a rate limiter with configuration-aware defaults.
// Zero or negative config values fall back to compiled-in constants.
func newPairingRateLimiter(cfg config.Config) *pairingRateLimiter {
	now := time.Now().UTC()
	burstPerIP := cfg.PairingBurstPerIP
	if burstPerIP <= 0 {
		burstPerIP = pairingBurstPerIP
	}
	globalBurst := cfg.PairingBurstGlobal
	if globalBurst <= 0 {
		globalBurst = pairingBurstGlobal
	}
	startRefill := cfg.PairingStartRefill
	if startRefill <= 0 {
		startRefill = pairingStartRefill
	}
	completeRefill := cfg.PairingCompleteRefill
	if completeRefill <= 0 {
		completeRefill = pairingCompleteRefill
	}
	return &pairingRateLimiter{
		ipStartBuckets: make(map[string]tokenBucket),
		ipDoneBuckets:  make(map[string]tokenBucket),
		globalStart:    tokenBucket{tokens: float64(globalBurst), last: now},
		globalDone:     tokenBucket{tokens: float64(globalBurst), last: now},
		burstPerIP:     burstPerIP,
		globalBurst:    globalBurst,
		startRefill:    startRefill,
		completeRefill: completeRefill,
		lastSweep:      now,
	}
}

// allow checks whether a pairing request should be permitted. It consumes one
// token from both the per-IP bucket and the global bucket for the relevant
// phase (start vs complete). Both must have tokens available.
func (l *pairingRateLimiter) allow(ip string, isStart bool, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ip == "" {
		ip = "unknown"
	}
	now = now.UTC()
	l.maybeSweep(now)
	if isStart {
		bucket, store := l.loadBucket(l.ipStartBuckets, ip, now)
		if !takeToken(&bucket, now, l.burstPerIP, l.startRefill) {
			if store {
				l.ipStartBuckets[ip] = bucket
			}
			return false
		}
		if store {
			l.ipStartBuckets[ip] = bucket
		}
		return takeToken(&l.globalStart, now, l.globalBurst, l.startRefill)
	}
	bucket, store := l.loadBucket(l.ipDoneBuckets, ip, now)
	if !takeToken(&bucket, now, l.burstPerIP, l.completeRefill) {
		if store {
			l.ipDoneBuckets[ip] = bucket
		}
		return false
	}
	if store {
		l.ipDoneBuckets[ip] = bucket
	}
	return takeToken(&l.globalDone, now, l.globalBurst, l.completeRefill)
}

// loadBucket returns the per-IP bucket for ip, or a zero (full-after-refill)
// bucket for a new key. store reports whether the caller must write the
// bucket back: when the map is at pairingMaxIPBuckets and the key is new, an
// idle-entry sweep runs first, and if the map is still full the key is left
// untracked (the global bucket is still enforced) so attacker-influenced keys
// cannot grow the map without bound.
func (l *pairingRateLimiter) loadBucket(buckets map[string]tokenBucket, ip string, now time.Time) (tokenBucket, bool) {
	if b, ok := buckets[ip]; ok {
		return b, true
	}
	if len(buckets) >= pairingMaxIPBuckets {
		l.sweepBuckets(now)
		if b, ok := buckets[ip]; ok {
			return b, true
		}
		if len(buckets) >= pairingMaxIPBuckets {
			return tokenBucket{}, false
		}
	}
	return tokenBucket{}, true
}

// maybeSweep evicts idle per-IP buckets at most every pairingBucketSweepEvery.
// Amortized (no goroutine, no per-request O(n) scan): steady-state memory is
// bounded to IPs seen since the last sweep plus one sweep interval of churn.
func (l *pairingRateLimiter) maybeSweep(now time.Time) {
	if now.Sub(l.lastSweep) < pairingBucketSweepEvery {
		return
	}
	l.lastSweep = now
	l.sweepBuckets(now)
}

// sweepBuckets deletes per-IP buckets idle longer than twice their full-refill
// window (capacity x refill interval). Such a bucket would refill to capacity
// on next use anyway, so eviction is rate-limit neutral — the next request
// simply starts from a full bucket again.
func (l *pairingRateLimiter) sweepBuckets(now time.Time) {
	if ttl := bucketIdleTTL(l.burstPerIP, l.startRefill); ttl > 0 {
		for ip, b := range l.ipStartBuckets {
			if now.Sub(b.last) >= ttl {
				delete(l.ipStartBuckets, ip)
			}
		}
	}
	if ttl := bucketIdleTTL(l.burstPerIP, l.completeRefill); ttl > 0 {
		for ip, b := range l.ipDoneBuckets {
			if now.Sub(b.last) >= ttl {
				delete(l.ipDoneBuckets, ip)
			}
		}
	}
}

// bucketIdleTTL returns the idle duration after which a bucket is guaranteed
// full and therefore safe to drop. Zero means never expire: without refill,
// deleting would mint fresh tokens and weaken the limiter.
func bucketIdleTTL(capacity int, refillEvery time.Duration) time.Duration {
	if capacity <= 0 || refillEvery <= 0 {
		return 0
	}
	return 2 * time.Duration(capacity) * refillEvery
}

// takeToken attempts to consume one token from the bucket. If the bucket is
// empty, it refills based on elapsed time before checking again. Returns false
// if no token is available even after refill.
func takeToken(bucket *tokenBucket, now time.Time, capacity int, refillEvery time.Duration) bool {
	if bucket.last.IsZero() {
		bucket.last = now
		bucket.tokens = float64(capacity)
	}
	if refillEvery > 0 {
		elapsed := now.Sub(bucket.last)
		if elapsed > 0 {
			bucket.tokens += elapsed.Seconds() / refillEvery.Seconds()
			if bucket.tokens > float64(capacity) {
				bucket.tokens = float64(capacity)
			}
			bucket.last = now
		}
	}
	if bucket.tokens < 1 {
		return false
	}
	bucket.tokens -= 1
	return true
}

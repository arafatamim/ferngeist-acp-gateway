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
	if isStart {
		bucket := l.ipStartBuckets[ip]
		if !takeToken(&bucket, now, l.burstPerIP, l.startRefill) {
			l.ipStartBuckets[ip] = bucket
			return false
		}
		l.ipStartBuckets[ip] = bucket
		return takeToken(&l.globalStart, now, l.globalBurst, l.startRefill)
	}
	bucket := l.ipDoneBuckets[ip]
	if !takeToken(&bucket, now, l.burstPerIP, l.completeRefill) {
		l.ipDoneBuckets[ip] = bucket
		return false
	}
	l.ipDoneBuckets[ip] = bucket
	return takeToken(&l.globalDone, now, l.globalBurst, l.completeRefill)
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

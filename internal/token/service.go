package token

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// attachClaim represents a single-use token for session reconnection.
type attachClaim struct {
	SessionID string
	DeviceID  string
	ExpiresAt time.Time
}

const (
	// attachTokenSweepEvery bounds how often Mint/Validate scan for expired
	// tokens; expiry itself is governed by each claim's TTL.
	attachTokenSweepEvery = time.Minute
	// maxAttachTokens hard-bounds the map under a mint flood inside one
	// sweep window. Overflow evicts the soonest-expiring entry first.
	maxAttachTokens = 4096
)

// Service mints and validates single-use attach tokens for session reconnection.
type Service struct {
	logger       *slog.Logger
	mu           sync.Mutex
	now          func() time.Time
	attachTokens map[string]attachClaim
	lastSweep    time.Time
}

// New creates a new attach token service.
func New(logger *slog.Logger) *Service {
	return &Service{
		logger:       logger.With("component", "token"),
		now:          time.Now,
		attachTokens: make(map[string]attachClaim),
	}
}

// Mint creates a single-use attach token bound to a session and device.
// Returns an error if the random source fails (extremely unlikely).
func (s *Service) Mint(sessionID, deviceID string, ttl time.Duration) (string, error) {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", fmt.Errorf("generate attach token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	s.maybeSweepLocked(now)
	if len(s.attachTokens) >= maxAttachTokens {
		// Fresh flood: sweep first, then evict the soonest-expiring entry
		// so the map stays bounded without refusing new mints.
		s.sweepLocked(now)
		if len(s.attachTokens) >= maxAttachTokens {
			s.evictOldestLocked()
		}
	}
	s.attachTokens[token] = attachClaim{
		SessionID: sessionID,
		DeviceID:  deviceID,
		ExpiresAt: now.Add(ttl),
	}
	return token, nil
}

// Validate verifies and consumes a single-use attach token.
func (s *Service) Validate(token string) (sessionID, deviceID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maybeSweepLocked(s.now().UTC())
	claim, ok := s.attachTokens[token]
	if !ok {
		return "", "", ErrTokenInvalid
	}
	if s.now().UTC().After(claim.ExpiresAt) {
		delete(s.attachTokens, token)
		return "", "", ErrTokenExpired
	}
	delete(s.attachTokens, token) // consume single-use
	return claim.SessionID, claim.DeviceID, nil
}

// maybeSweepLocked evicts expired tokens at most every attachTokenSweepEvery.
// Amortized (no goroutine, no per-call O(n) scan): memory is bounded to live
// tokens plus one sweep interval of dead ones. Caller holds s.mu.
func (s *Service) maybeSweepLocked(now time.Time) {
	if now.Sub(s.lastSweep) < attachTokenSweepEvery {
		return
	}
	s.lastSweep = now
	s.sweepLocked(now)
}

// sweepLocked deletes every token past its TTL. Caller holds s.mu.
func (s *Service) sweepLocked(now time.Time) {
	for token, claim := range s.attachTokens {
		if now.After(claim.ExpiresAt) {
			delete(s.attachTokens, token)
		}
	}
}

// evictOldestLocked removes the soonest-expiring token to hold the cap under
// a fresh-token flood. Caller holds s.mu.
func (s *Service) evictOldestLocked() {
	oldest := ""
	var oldestExp time.Time
	first := true
	for token, claim := range s.attachTokens {
		if first || claim.ExpiresAt.Before(oldestExp) {
			oldest, oldestExp, first = token, claim.ExpiresAt, false
		}
	}
	if !first {
		delete(s.attachTokens, oldest)
	}
}

// ClearAll removes all in-memory attach tokens.
func (s *Service) ClearAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attachTokens = make(map[string]attachClaim)
}

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

// Service mints and validates single-use attach tokens for session reconnection.
type Service struct {
	logger       *slog.Logger
	mu           sync.Mutex
	now          func() time.Time
	attachTokens map[string]attachClaim
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
	s.attachTokens[token] = attachClaim{
		SessionID: sessionID,
		DeviceID:  deviceID,
		ExpiresAt: s.now().UTC().Add(ttl),
	}
	return token, nil
}

// Validate verifies and consumes a single-use attach token.
func (s *Service) Validate(token string) (sessionID, deviceID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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

// ClearAll removes all in-memory attach tokens.
func (s *Service) ClearAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attachTokens = make(map[string]attachClaim)
}

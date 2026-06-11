package token

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

// TestMintReturnsValidatableToken verifies that Mint produces a 64-char hex token
// that can be validated to recover session and device IDs.
func TestMintReturnsValidatableToken(t *testing.T) {
	svc := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }

	token, err := svc.Mint("session-1", "device-1", time.Minute)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	if token == "" {
		t.Fatal("Mint() returned empty token")
	}
	if len(token) != 64 {
		t.Fatalf("token length = %d, want 64", len(token))
	}

	sessionID, deviceID, err := svc.Validate(token)
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if sessionID != "session-1" {
		t.Fatalf("sessionID = %q, want %q", sessionID, "session-1")
	}
	if deviceID != "device-1" {
		t.Fatalf("deviceID = %q, want %q", deviceID, "device-1")
	}
}

// TestValidateConsumesToken verifies that an attach token can only be validated once;
// subsequent attempts are rejected.
func TestValidateConsumesToken(t *testing.T) {
	svc := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }

	token, err := svc.Mint("session-1", "device-1", time.Minute)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}

	if _, _, err := svc.Validate(token); err != nil {
		t.Fatalf("first Validate() error = %v", err)
	}
	if _, _, err := svc.Validate(token); err != ErrTokenInvalid {
		t.Fatalf("second Validate() error = %v, want %v", err, ErrTokenInvalid)
	}
}

// TestValidateRejectsExpiredToken verifies that an attach token minted with a
// negative TTL is rejected as expired.
func TestValidateRejectsExpiredToken(t *testing.T) {
	svc := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }

	token, err := svc.Mint("session-1", "device-1", -time.Minute)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}

	if _, _, err := svc.Validate(token); err != ErrTokenExpired {
		t.Fatalf("Validate() error = %v, want %v", err, ErrTokenExpired)
	}
}

// TestValidateRejectsInvalidToken verifies that a garbage token string is rejected.
func TestValidateRejectsInvalidToken(t *testing.T) {
	svc := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }

	if _, _, err := svc.Validate("not-a-real-token"); err != ErrTokenInvalid {
		t.Fatalf("Validate() error = %v, want %v", err, ErrTokenInvalid)
	}
}

// TestValidateRejectsEmptyToken verifies that an empty token string is rejected.
func TestValidateRejectsEmptyToken(t *testing.T) {
	svc := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }

	if _, _, err := svc.Validate(""); err != ErrTokenInvalid {
		t.Fatalf("Validate() error = %v, want %v", err, ErrTokenInvalid)
	}
}

// TestClearAllInvalidatesTokens verifies that ClearAll invalidates all minted tokens.
func TestClearAllInvalidatesTokens(t *testing.T) {
	svc := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }

	token, err := svc.Mint("session-1", "device-1", time.Minute)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	svc.ClearAll()

	if _, _, err := svc.Validate(token); err != ErrTokenInvalid {
		t.Fatalf("Validate() after ClearAll error = %v, want %v", err, ErrTokenInvalid)
	}
}

// TestMintReturnsNonEmptyTokens verifies that Mint returns non-empty tokens for
// multiple sessions.
func TestMintReturnsNonEmptyTokens(t *testing.T) {
	svc := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }

	token1, err := svc.Mint("session-1", "device-1", time.Minute)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	token2, err := svc.Mint("session-2", "device-2", time.Minute)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}

	if token1 == "" {
		t.Fatal("Mint() returned empty token1")
	}
	if token2 == "" {
		t.Fatal("Mint() returned empty token2")
	}
}

// TestMintProducesUniqueTokens verifies that Mint produces distinct tokens for
// different sessions.
func TestMintProducesUniqueTokens(t *testing.T) {
	svc := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }

	token1, err := svc.Mint("session-1", "device-1", time.Minute)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	token2, err := svc.Mint("session-2", "device-2", time.Minute)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}

	if token1 == token2 {
		t.Fatal("Mint() returned identical tokens")
	}
}

// TestMintContainsCorrectClaims verifies that each attach token encodes the
// correct session and device IDs.
func TestMintContainsCorrectClaims(t *testing.T) {
	svc := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }

	token1, err := svc.Mint("session-1", "device-1", time.Minute)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	token2, err := svc.Mint("session-2", "device-2", time.Minute)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}

	sessionID, deviceID, err := svc.Validate(token1)
	if err != nil {
		t.Fatalf("token1 Validate() error = %v", err)
	}
	if sessionID != "session-1" {
		t.Fatalf("token1 sessionID = %q, want %q", sessionID, "session-1")
	}
	if deviceID != "device-1" {
		t.Fatalf("token1 deviceID = %q, want %q", deviceID, "device-1")
	}

	sessionID, deviceID, err = svc.Validate(token2)
	if err != nil {
		t.Fatalf("token2 Validate() error = %v", err)
	}
	if sessionID != "session-2" {
		t.Fatalf("token2 sessionID = %q, want %q", sessionID, "session-2")
	}
	if deviceID != "device-2" {
		t.Fatalf("token2 deviceID = %q, want %q", deviceID, "device-2")
	}
}

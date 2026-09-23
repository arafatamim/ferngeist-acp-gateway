package token

import (
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"
)

func newTestServiceAt(now time.Time) (*Service, *time.Time) {
	svc := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	current := now
	svc.now = func() time.Time { return current }
	return svc, &current
}

// TestExpiredTokensSwept fails while expired-but-never-presented tokens linger:
// minting after the TTL + sweep interval must not grow the map.
func TestExpiredTokensSwept(t *testing.T) {
	t0 := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	svc, current := newTestServiceAt(t0)

	if _, err := svc.Mint("s1", "d1", time.Minute); err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	*current = t0.Add(2 * time.Minute) // past TTL (1m) and sweep interval
	if _, err := svc.Mint("s2", "d1", time.Minute); err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	svc.mu.Lock()
	n := len(svc.attachTokens)
	svc.mu.Unlock()
	if n != 1 {
		t.Fatalf("len(attachTokens) = %d, want 1 (expired entry swept)", n)
	}
}

// TestValidateSweepsExpiredTokens covers the Validate-triggered sweep path.
func TestValidateSweepsExpiredTokens(t *testing.T) {
	t0 := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	svc, current := newTestServiceAt(t0)

	if _, err := svc.Mint("s1", "d1", time.Minute); err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	*current = t0.Add(2 * time.Minute)
	if _, _, err := svc.Validate("never-minted"); err != ErrTokenInvalid {
		t.Fatalf("Validate() error = %v, want %v", err, ErrTokenInvalid)
	}
	svc.mu.Lock()
	n := len(svc.attachTokens)
	svc.mu.Unlock()
	if n != 0 {
		t.Fatalf("len(attachTokens) = %d, want 0 (expired entry swept)", n)
	}
}

// TestAttachTokenCountIsCapped bounds memory under a mint flood inside one
// sweep window (all entries fresh, so sweeping alone cannot help).
func TestAttachTokenCountIsCapped(t *testing.T) {
	t0 := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	svc, _ := newTestServiceAt(t0)

	for i := 0; i < maxAttachTokens+500; i++ {
		if _, err := svc.Mint(fmt.Sprintf("s%d", i), "d1", time.Minute); err != nil {
			t.Fatalf("Mint() error = %v", err)
		}
	}
	svc.mu.Lock()
	n := len(svc.attachTokens)
	svc.mu.Unlock()
	if n > maxAttachTokens {
		t.Fatalf("len(attachTokens) = %d, want <= %d", n, maxAttachTokens)
	}
}

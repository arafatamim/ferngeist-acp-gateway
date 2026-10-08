package pairing

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

// pairTestDevice pairs a device against a bare service (no store).
func pairTestDevice(t *testing.T, service *Service, name string) Credential {
	t.Helper()
	challenge, err := service.StartPairingWithLocalApproval()
	if err != nil {
		t.Fatalf("StartPairingWithLocalApproval() error = %v", err)
	}
	credential, err := service.CompletePairing(challenge.ID, challenge.Code, name)
	if err != nil {
		t.Fatalf("CompletePairing() error = %v", err)
	}
	return credential
}

// A refresh whose response never reached the client must stay recoverable: the
// client still holds the token it had, so the gateway exchanges it for the live
// one instead of answering "credential invalid" — the one failure the app acts
// on by deleting the gateway and its agent bindings.
func TestRefreshExchangesRotatedTokenWithinOverlap(t *testing.T) {
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	service := NewServiceWithOptions(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Options{CredentialTTL: 24 * time.Hour})
	service.now = func() time.Time { return now }
	credential := pairTestDevice(t, service, "Pixel 9")

	service.now = func() time.Time { return now.Add(2 * time.Hour) }
	first, err := service.RefreshCredential(credential.Token)
	if err != nil {
		t.Fatalf("RefreshCredential() error = %v", err)
	}
	if first.Token == credential.Token {
		t.Fatal("refresh should rotate the token")
	}

	// The client never saw that response and retries with the token it has.
	retry, err := service.RefreshCredential(credential.Token)
	if err != nil {
		t.Fatalf("RefreshCredential(rotated token) error = %v", err)
	}
	if retry.Token != first.Token {
		t.Fatalf("retry token = %q, want the live token %q", retry.Token, first.Token)
	}
	if retry.DeviceID != credential.DeviceID {
		t.Fatalf("DeviceID = %q, want %q", retry.DeviceID, credential.DeviceID)
	}
}

// A retry that re-issues the same credential must not extend the expiry: the
// caller converges on the live token, it does not get a fresh lease out of it.
func TestRefreshOfRotatedTokenDoesNotExtendExpiry(t *testing.T) {
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	service := NewServiceWithOptions(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Options{CredentialTTL: 24 * time.Hour})
	service.now = func() time.Time { return now }
	credential := pairTestDevice(t, service, "Pixel 9")

	service.now = func() time.Time { return now.Add(2 * time.Hour) }
	first, err := service.RefreshCredential(credential.Token)
	if err != nil {
		t.Fatalf("RefreshCredential() error = %v", err)
	}

	service.now = func() time.Time { return now.Add(3 * time.Hour) }
	retry, err := service.RefreshCredential(credential.Token)
	if err != nil {
		t.Fatalf("RefreshCredential(rotated token) error = %v", err)
	}
	if !retry.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatalf("ExpiresAt = %v, want the live credential's %v", retry.ExpiresAt, first.ExpiresAt)
	}
}

// The overlap is a recovery window, not a second live credential.
func TestRotatedTokenStopsExchangingAfterOverlap(t *testing.T) {
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	service := NewServiceWithOptions(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Options{CredentialTTL: 90 * 24 * time.Hour})
	service.now = func() time.Time { return now }
	credential := pairTestDevice(t, service, "Pixel 9")

	service.now = func() time.Time { return now.Add(2 * time.Hour) }
	if _, err := service.RefreshCredential(credential.Token); err != nil {
		t.Fatalf("RefreshCredential() error = %v", err)
	}

	service.now = func() time.Time { return now.Add(2*time.Hour + defaultRotationOverlap + time.Minute) }
	if _, err := service.RefreshCredential(credential.Token); err != ErrCredentialInvalid {
		t.Fatalf("RefreshCredential(rotated token past overlap) error = %v, want %v", err, ErrCredentialInvalid)
	}
}

// A rotated token recovers a device; it must not authenticate anything else.
func TestRotatedTokenDoesNotAuthenticateApiCalls(t *testing.T) {
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	service := NewServiceWithOptions(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Options{CredentialTTL: 24 * time.Hour})
	service.now = func() time.Time { return now }
	credential := pairTestDevice(t, service, "Pixel 9")

	service.now = func() time.Time { return now.Add(2 * time.Hour) }
	refreshed, err := service.RefreshCredential(credential.Token)
	if err != nil {
		t.Fatalf("RefreshCredential() error = %v", err)
	}

	if _, err := service.ValidateCredential(credential.Token); err != ErrCredentialInvalid {
		t.Fatalf("ValidateCredential(rotated token) error = %v, want %v", err, ErrCredentialInvalid)
	}
	if _, err := service.ValidateCredential(refreshed.Token); err != nil {
		t.Fatalf("ValidateCredential(live token) error = %v", err)
	}
}

// Revoking a device must also drop the token it rotated away, or a revoked
// device could keep exchanging its retired credential for a new one.
func TestRevokedDeviceStopsExchangingRotatedToken(t *testing.T) {
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	service := NewServiceWithOptions(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Options{CredentialTTL: 24 * time.Hour})
	service.now = func() time.Time { return now }
	credential := pairTestDevice(t, service, "Pixel 9")

	service.now = func() time.Time { return now.Add(2 * time.Hour) }
	if _, err := service.RefreshCredential(credential.Token); err != nil {
		t.Fatalf("RefreshCredential() error = %v", err)
	}
	if _, err := service.RevokeDevice(credential.DeviceID); err != nil {
		t.Fatalf("RevokeDevice() error = %v", err)
	}

	if _, err := service.RefreshCredential(credential.Token); err != ErrCredentialInvalid {
		t.Fatalf("RefreshCredential(revoked device) error = %v, want %v", err, ErrCredentialInvalid)
	}
}

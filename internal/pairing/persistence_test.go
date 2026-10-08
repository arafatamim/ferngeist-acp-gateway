package pairing

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

func TestServiceLoadsPersistedCredentials(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	defer store.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Now().UTC().Add(-1 * time.Hour)

	service := NewService(logger, store)
	service.now = func() time.Time { return now }

	challenge, err := service.StartPairingWithLocalApproval()
	if err != nil {
		t.Fatalf("StartPairingWithLocalApproval() error = %v", err)
	}

	credential, err := service.CompletePairingWithProofKey(challenge.ID, challenge.Code, "Pixel 9", "proof-key")
	if err != nil {
		t.Fatalf("CompletePairing() error = %v", err)
	}
	pairings, err := store.ListPairings(context.Background())
	if err != nil {
		t.Fatalf("ListPairings() error = %v", err)
	}
	if len(pairings) != 1 {
		t.Fatalf("len(pairings) = %d, want 1", len(pairings))
	}
	if !isHashedCredentialToken(pairings[0].Token) {
		t.Fatalf("stored token = %q, want hashed token", pairings[0].Token)
	}

	reloaded := NewService(logger, store)
	reloaded.now = func() time.Time { return now }

	validated, err := reloaded.ValidateCredential(credential.Token)
	if err != nil {
		t.Fatalf("ValidateCredential() error = %v", err)
	}
	if validated.DeviceName != "Pixel 9" {
		t.Fatalf("DeviceName = %q, want %q", validated.DeviceName, "Pixel 9")
	}
	if !validated.HasScope(ScopeRead) || !validated.HasScope(ScopeControl) {
		t.Fatalf("validated scopes = %v, want baseline scopes", validated.Scopes)
	}
	if validated.ProofPublicKey != "proof-key" {
		t.Fatalf("ProofPublicKey = %q, want %q", validated.ProofPublicKey, "proof-key")
	}
}

func TestRevokedCredentialDoesNotReloadAfterRestart(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	defer store.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Now().UTC().Add(-1 * time.Hour)

	service := NewService(logger, store)
	service.now = func() time.Time { return now }

	challenge, err := service.StartPairingWithLocalApproval()
	if err != nil {
		t.Fatalf("StartPairingWithLocalApproval() error = %v", err)
	}

	credential, err := service.CompletePairing(challenge.ID, challenge.Code, "Pixel 9")
	if err != nil {
		t.Fatalf("CompletePairing() error = %v", err)
	}

	if _, err := service.RevokeDevice(credential.DeviceID); err != nil {
		t.Fatalf("RevokeDevice() error = %v", err)
	}

	reloaded := NewService(logger, store)
	reloaded.now = func() time.Time { return now }

	if _, err := reloaded.ValidateCredential(credential.Token); err != ErrCredentialInvalid {
		t.Fatalf("ValidateCredential() error = %v, want %v", err, ErrCredentialInvalid)
	}
}

func TestRefreshedCredentialReloadsAfterRestart(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	defer store.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Now().UTC().Add(-1 * time.Hour)

	service := NewServiceWithOptions(logger, store, Options{CredentialTTL: 24 * time.Hour})
	service.now = func() time.Time { return now }

	challenge, err := service.StartPairingWithLocalApproval()
	if err != nil {
		t.Fatalf("StartPairingWithLocalApproval() error = %v", err)
	}
	credential, err := service.CompletePairing(challenge.ID, challenge.Code, "Pixel 9")
	if err != nil {
		t.Fatalf("CompletePairing() error = %v", err)
	}

	service.now = func() time.Time { return now.Add(2 * time.Hour) }
	refreshed, err := service.RefreshCredential(credential.Token)
	if err != nil {
		t.Fatalf("RefreshCredential() error = %v", err)
	}

	reloaded := NewServiceWithOptions(logger, store, Options{CredentialTTL: 24 * time.Hour})
	reloaded.now = func() time.Time { return now.Add(2 * time.Hour) }

	if _, err := reloaded.ValidateCredential(credential.Token); err != ErrCredentialInvalid {
		t.Fatalf("ValidateCredential(old token) error = %v, want %v", err, ErrCredentialInvalid)
	}
	validated, err := reloaded.ValidateCredential(refreshed.Token)
	if err != nil {
		t.Fatalf("ValidateCredential(refreshed token) error = %v", err)
	}
	if validated.DeviceID != refreshed.DeviceID {
		t.Fatalf("DeviceID = %q, want %q", validated.DeviceID, refreshed.DeviceID)
	}
}

func TestSoftExpiredCredentialReloadsWithinGrace(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	defer store.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Base the clock far enough in the past that the persisted row is already
	// past ExpiresAt when the reloaded service constructs (which loads with the
	// real clock), yet still within the grace window.
	now := time.Now().UTC().Add(-2 * time.Hour)

	service := NewServiceWithOptions(logger, store, Options{CredentialTTL: time.Hour, GracePeriod: 24 * time.Hour})
	service.now = func() time.Time { return now }

	challenge, err := service.StartPairingWithLocalApproval()
	if err != nil {
		t.Fatalf("StartPairingWithLocalApproval() error = %v", err)
	}
	credential, err := service.CompletePairing(challenge.ID, challenge.Code, "Pixel 9")
	if err != nil {
		t.Fatalf("CompletePairing() error = %v", err)
	}

	// Move past expiry but within grace, soft-expire via validation.
	service.now = func() time.Time { return now.Add(2 * time.Hour) }
	if _, err := service.ValidateCredential(credential.Token); err != ErrCredentialExpired {
		t.Fatalf("ValidateCredential() error = %v, want %v", err, ErrCredentialExpired)
	}

	// Reload: the soft-expired row should load back with ExpiredAt derived from ExpiresAt.
	reloaded := NewServiceWithOptions(logger, store, Options{CredentialTTL: time.Hour, GracePeriod: 24 * time.Hour})
	reloaded.now = func() time.Time { return now.Add(2 * time.Hour) }

	cred, ok := reloaded.credentials[credential.DeviceID]
	if !ok {
		t.Fatal("soft-expired credential should reload within grace window")
	}
	if cred.ExpiredAt == nil {
		t.Fatal("reloaded soft-expired credential should have ExpiredAt set")
	}
	if _, err := reloaded.ValidateCredential(credential.Token); err != ErrCredentialExpired {
		t.Fatalf("ValidateCredential() error = %v, want %v", err, ErrCredentialExpired)
	}
}

// The overlap has to survive a restart: a daemon that restarts between a lost
// refresh response and the client's retry would otherwise re-brick the device.
// The store only holds hashes, so the reloaded service mints a fresh token
// instead of echoing the one it cannot know.
func TestRotatedTokenReloadsAfterRestart(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	defer store.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Now().UTC().Add(-1 * time.Hour)

	service := NewServiceWithOptions(logger, store, Options{CredentialTTL: 24 * time.Hour})
	service.now = func() time.Time { return now }

	challenge, err := service.StartPairingWithLocalApproval()
	if err != nil {
		t.Fatalf("StartPairingWithLocalApproval() error = %v", err)
	}
	credential, err := service.CompletePairing(challenge.ID, challenge.Code, "Pixel 9")
	if err != nil {
		t.Fatalf("CompletePairing() error = %v", err)
	}

	service.now = func() time.Time { return now.Add(2 * time.Hour) }
	lost, err := service.RefreshCredential(credential.Token)
	if err != nil {
		t.Fatalf("RefreshCredential() error = %v", err)
	}

	reloaded := NewServiceWithOptions(logger, store, Options{CredentialTTL: 24 * time.Hour})
	reloaded.now = func() time.Time { return now.Add(2 * time.Hour) }

	recovered, err := reloaded.RefreshCredential(credential.Token)
	if err != nil {
		t.Fatalf("RefreshCredential(rotated token) after reload error = %v", err)
	}
	if recovered.DeviceID != credential.DeviceID {
		t.Fatalf("DeviceID = %q, want %q", recovered.DeviceID, credential.DeviceID)
	}
	if recovered.Token == credential.Token {
		t.Fatal("a reloaded service cannot echo the lost token; it must mint a fresh one")
	}
	if _, err := reloaded.ValidateCredential(lost.Token); err != ErrCredentialInvalid {
		t.Fatalf("ValidateCredential(token from the lost response) error = %v, want %v", err, ErrCredentialInvalid)
	}
	if _, err := reloaded.ValidateCredential(recovered.Token); err != nil {
		t.Fatalf("ValidateCredential(recovered token) error = %v", err)
	}
}

// A refresh whose rotation cannot be persisted must leave the old credential in
// place. Handing the client a token the store never recorded is what strands a
// device in the unrecoverable "credential invalid" state on the next restart.
func TestRefreshPersistFailureKeepsOldTokenValid(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Now().UTC().Add(-1 * time.Hour)

	service := NewServiceWithOptions(logger, store, Options{CredentialTTL: 24 * time.Hour})
	service.now = func() time.Time { return now }

	challenge, err := service.StartPairingWithLocalApproval()
	if err != nil {
		t.Fatalf("StartPairingWithLocalApproval() error = %v", err)
	}
	credential, err := service.CompletePairing(challenge.ID, challenge.Code, "Pixel 9")
	if err != nil {
		t.Fatalf("CompletePairing() error = %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("store.Close() error = %v", err)
	}

	service.now = func() time.Time { return now.Add(2 * time.Hour) }
	if _, err := service.RefreshCredential(credential.Token); err == nil {
		t.Fatal("RefreshCredential() error = nil, want the persist failure to surface")
	}

	if _, err := service.ValidateCredential(credential.Token); err != nil {
		t.Fatalf("ValidateCredential(old token) error = %v, want the credential to stay live", err)
	}
}

// Recovering after a restart mints a token the client may also never receive.
// The token it still holds must stay exchangeable across a second restart.
func TestRotatedTokenSurvivesSecondRestart(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	defer store.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Now().UTC().Add(-1 * time.Hour)
	later := func() time.Time { return now.Add(2 * time.Hour) }

	service := NewServiceWithOptions(logger, store, Options{CredentialTTL: 24 * time.Hour})
	service.now = func() time.Time { return now }
	challenge, err := service.StartPairingWithLocalApproval()
	if err != nil {
		t.Fatalf("StartPairingWithLocalApproval() error = %v", err)
	}
	credential, err := service.CompletePairing(challenge.ID, challenge.Code, "Pixel 9")
	if err != nil {
		t.Fatalf("CompletePairing() error = %v", err)
	}

	service.now = later
	if _, err := service.RefreshCredential(credential.Token); err != nil {
		t.Fatalf("RefreshCredential() error = %v", err)
	}

	// First restart: the retry mints a fresh token whose response is lost too.
	first := NewServiceWithOptions(logger, store, Options{CredentialTTL: 24 * time.Hour})
	first.now = later
	if _, err := first.RefreshCredential(credential.Token); err != nil {
		t.Fatalf("RefreshCredential() after first restart error = %v", err)
	}

	second := NewServiceWithOptions(logger, store, Options{CredentialTTL: 24 * time.Hour})
	second.now = later
	recovered, err := second.RefreshCredential(credential.Token)
	if err != nil {
		t.Fatalf("RefreshCredential() after second restart error = %v", err)
	}
	if _, err := second.ValidateCredential(recovered.Token); err != nil {
		t.Fatalf("ValidateCredential(recovered token) error = %v", err)
	}
}

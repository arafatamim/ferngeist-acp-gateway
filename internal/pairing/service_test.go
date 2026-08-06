package pairing

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestCompletePairingSuccess(t *testing.T) {
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	service := NewService(slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	service.now = func() time.Time { return now }

	challenge, err := service.StartPairingWithLocalApproval()
	if err != nil {
		t.Fatalf("StartPairingWithLocalApproval() error = %v", err)
	}

	credential, err := service.CompletePairing(challenge.ID, challenge.Code, "Pixel 9")
	if err != nil {
		t.Fatalf("CompletePairing() error = %v", err)
	}

	if credential.DeviceName != "Pixel 9" {
		t.Fatalf("DeviceName = %q, want %q", credential.DeviceName, "Pixel 9")
	}
	if credential.DeviceID == "" {
		t.Fatal("DeviceID should not be empty")
	}
	if credential.Token == "" {
		t.Fatal("Token should not be empty")
	}
	if !credential.HasScope(ScopeRead) || !credential.HasScope(ScopeControl) {
		t.Fatalf("credential scopes = %v, want baseline read/control", credential.Scopes)
	}
	if count := service.ActiveDeviceCount(); count != 1 {
		t.Fatalf("ActiveDeviceCount() = %d, want 1", count)
	}
}

func TestCompletePairingRejectsWrongCode(t *testing.T) {
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	service := NewService(slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	service.now = func() time.Time { return now }

	challenge, err := service.StartPairingWithLocalApproval()
	if err != nil {
		t.Fatalf("StartPairingWithLocalApproval() error = %v", err)
	}

	_, err = service.CompletePairing(challenge.ID, "000000", "Pixel 9")
	if err != ErrCodeMismatch {
		t.Fatalf("CompletePairing() error = %v, want %v", err, ErrCodeMismatch)
	}
}

func TestCompletePairingRejectsExpiredChallenge(t *testing.T) {
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	service := NewService(slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	service.now = func() time.Time { return now }

	challenge, err := service.StartPairingWithLocalApproval()
	if err != nil {
		t.Fatalf("StartPairingWithLocalApproval() error = %v", err)
	}

	service.now = func() time.Time { return now.Add(defaultChallengeTTL + time.Second) }

	_, err = service.CompletePairing(challenge.ID, challenge.Code, "Pixel 9")
	if err != ErrChallengeExpired {
		t.Fatalf("CompletePairing() error = %v, want %v", err, ErrChallengeExpired)
	}
}

func TestCompletePairingAllowsCodeOnlyLookup(t *testing.T) {
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	service := NewService(slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	service.now = func() time.Time { return now }

	challenge, err := service.StartPairingWithLocalApproval()
	if err != nil {
		t.Fatalf("StartPairingWithLocalApproval() error = %v", err)
	}

	credential, err := service.CompletePairing("", challenge.Code, "Pixel 9")
	if err != nil {
		t.Fatalf("CompletePairing() error = %v", err)
	}
	if credential.DeviceName != "Pixel 9" {
		t.Fatalf("DeviceName = %q, want %q", credential.DeviceName, "Pixel 9")
	}
}

func TestValidateCredentialSuccess(t *testing.T) {
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	service := NewService(slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	service.now = func() time.Time { return now }

	challenge, err := service.StartPairingWithLocalApproval()
	if err != nil {
		t.Fatalf("StartPairingWithLocalApproval() error = %v", err)
	}

	credential, err := service.CompletePairing(challenge.ID, challenge.Code, "Pixel 9")
	if err != nil {
		t.Fatalf("CompletePairing() error = %v", err)
	}

	validated, err := service.ValidateCredential(credential.Token)
	if err != nil {
		t.Fatalf("ValidateCredential() error = %v", err)
	}
	if validated.DeviceID != credential.DeviceID {
		t.Fatalf("DeviceID = %q, want %q", validated.DeviceID, credential.DeviceID)
	}
}

func TestCancelChallengeMarksChallengeCancelled(t *testing.T) {
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	service := NewService(slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	service.now = func() time.Time { return now }

	challenge, err := service.StartPairingWithLocalApproval()
	if err != nil {
		t.Fatalf("StartPairingWithLocalApproval() error = %v", err)
	}

	status, err := service.CancelChallenge(challenge.ID)
	if err != nil {
		t.Fatalf("CancelChallenge() error = %v", err)
	}
	if status.State != ChallengeStateCancelled {
		t.Fatalf("State = %q, want %q", status.State, ChallengeStateCancelled)
	}
}

func TestRevokeDeviceRemovesCredential(t *testing.T) {
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	service := NewService(slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
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
	if _, err := service.ValidateCredential(credential.Token); err != ErrCredentialInvalid {
		t.Fatalf("ValidateCredential() error = %v, want %v", err, ErrCredentialInvalid)
	}
}

func TestStartPairingRequiresLocalApproval(t *testing.T) {
	service := NewService(slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	_, err := service.StartPairing()
	if err != ErrPairingNotArmed {
		t.Fatalf("StartPairing() error = %v, want %v", err, ErrPairingNotArmed)
	}
}

func TestLocalApprovalExpires(t *testing.T) {
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	service := NewService(slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	service.now = func() time.Time { return now }

	challenge, err := service.StartPairingWithLocalApproval()
	if err != nil {
		t.Fatalf("StartPairingWithLocalApproval() error = %v", err)
	}

	_, err = service.CompletePairing(challenge.ID, challenge.Code, "Pixel 9")
	if err != nil {
		t.Fatalf("CompletePairing() error = %v", err)
	}

	_, err = service.StartPairing()
	if err != ErrPairingNotArmed {
		t.Fatalf("StartPairing() error = %v, want %v", err, ErrPairingNotArmed)
	}
}

func TestNewServiceWithOptionsGrantsElevatedScopesAndTTL(t *testing.T) {
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	service := NewServiceWithOptions(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Options{
		CredentialTTL:          24 * time.Hour,
		AllowDiagnosticsExport: true,
		AllowRuntimeRestartEnv: true,
	})
	service.now = func() time.Time { return now }

	challenge, err := service.StartPairingWithLocalApproval()
	if err != nil {
		t.Fatalf("StartPairingWithLocalApproval() error = %v", err)
	}
	credential, err := service.CompletePairing(challenge.ID, challenge.Code, "Pixel 9")
	if err != nil {
		t.Fatalf("CompletePairing() error = %v", err)
	}
	if credential.ExpiresAt != now.Add(24*time.Hour) {
		t.Fatalf("ExpiresAt = %v, want %v", credential.ExpiresAt, now.Add(24*time.Hour))
	}
	if !credential.HasScope(ScopeDiagnosticsExport) {
		t.Fatalf("credential scopes = %v, want diagnostics export", credential.Scopes)
	}
	if !credential.HasScope(ScopeRuntimeRestartEnv) {
		t.Fatalf("credential scopes = %v, want runtime restart env", credential.Scopes)
	}
}

func TestCompletePairingWithProofKeyStoresBinding(t *testing.T) {
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	service := NewService(slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	service.now = func() time.Time { return now }

	challenge, err := service.StartPairingWithLocalApproval()
	if err != nil {
		t.Fatalf("StartPairingWithLocalApproval() error = %v", err)
	}
	credential, err := service.CompletePairingWithProofKey(challenge.ID, challenge.Code, "Pixel 9", "proof-key")
	if err != nil {
		t.Fatalf("CompletePairingWithProofKey() error = %v", err)
	}
	if credential.ProofPublicKey != "proof-key" {
		t.Fatalf("ProofPublicKey = %q, want %q", credential.ProofPublicKey, "proof-key")
	}
}

func TestRefreshCredentialRotatesTokenAndExtendsExpiry(t *testing.T) {
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	service := NewServiceWithOptions(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Options{CredentialTTL: 24 * time.Hour})
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
	if refreshed.DeviceID != credential.DeviceID {
		t.Fatalf("DeviceID = %q, want %q", refreshed.DeviceID, credential.DeviceID)
	}
	if refreshed.Token == credential.Token {
		t.Fatal("refresh should rotate the token")
	}
	if refreshed.ExpiresAt != now.Add(26*time.Hour) {
		t.Fatalf("ExpiresAt = %v, want %v", refreshed.ExpiresAt, now.Add(26*time.Hour))
	}
	if _, err := service.ValidateCredential(credential.Token); err != ErrCredentialInvalid {
		t.Fatalf("ValidateCredential(old token) error = %v, want %v", err, ErrCredentialInvalid)
	}
	validated, err := service.ValidateCredential(refreshed.Token)
	if err != nil {
		t.Fatalf("ValidateCredential(refreshed token) error = %v", err)
	}
	if validated.DeviceID != credential.DeviceID {
		t.Fatalf("validated DeviceID = %q, want %q", validated.DeviceID, credential.DeviceID)
	}
	if len(validated.Scopes) != len(credential.Scopes) {
		t.Fatalf("validated scopes = %v, want %v", validated.Scopes, credential.Scopes)
	}
}

func TestRefreshCredentialRejectsExpiredTokenWhenGraceDisabled(t *testing.T) {
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	service := NewServiceWithOptions(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Options{CredentialTTL: time.Hour, GracePeriod: 0})
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
	_, err = service.RefreshCredential(credential.Token)
	if err != ErrCredentialExpired {
		t.Fatalf("RefreshCredential() error = %v, want %v", err, ErrCredentialExpired)
	}
}

func TestRefreshCredentialSucceedsWithinGraceAfterExpiry(t *testing.T) {
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	service := NewServiceWithOptions(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Options{CredentialTTL: 24 * time.Hour, GracePeriod: 90 * 24 * time.Hour})
	service.now = func() time.Time { return now }

	challenge, err := service.StartPairingWithLocalApproval()
	if err != nil {
		t.Fatalf("StartPairingWithLocalApproval() error = %v", err)
	}
	credential, err := service.CompletePairing(challenge.ID, challenge.Code, "Pixel 9")
	if err != nil {
		t.Fatalf("CompletePairing() error = %v", err)
	}

	// Expire it (soft).
	service.now = func() time.Time { return now.Add(48 * time.Hour) }
	if _, err := service.ValidateCredential(credential.Token); err != ErrCredentialExpired {
		t.Fatalf("ValidateCredential() error = %v, want %v", err, ErrCredentialExpired)
	}

	// Refresh within grace: new token, window slides, ExpiredAt cleared.
	refreshed, err := service.RefreshCredential(credential.Token)
	if err != nil {
		t.Fatalf("RefreshCredential() error = %v", err)
	}
	if refreshed.Token == credential.Token {
		t.Fatal("refresh should rotate the token")
	}
	if refreshed.ExpiresAt != now.Add(48*time.Hour).Add(24*time.Hour) {
		t.Fatalf("ExpiresAt = %v, want %v", refreshed.ExpiresAt, now.Add(48*time.Hour).Add(24*time.Hour))
	}
	if refreshed.ExpiredAt != nil {
		t.Fatal("refresh should clear the soft-expire marker")
	}
	stored := service.credentials[credential.DeviceID]
	if stored.ExpiredAt != nil {
		t.Fatal("stored credential ExpiredAt should be cleared after refresh")
	}
	if _, err := service.ValidateCredential(refreshed.Token); err != nil {
		t.Fatalf("ValidateCredential(refreshed token) error = %v", err)
	}
	if _, err := service.ValidateCredential(credential.Token); err != ErrCredentialInvalid {
		t.Fatalf("ValidateCredential(old token) error = %v, want %v", err, ErrCredentialInvalid)
	}
}

func TestRefreshCredentialRejectsPastGrace(t *testing.T) {
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	service := NewServiceWithOptions(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Options{CredentialTTL: 24 * time.Hour, GracePeriod: 24 * time.Hour})
	service.now = func() time.Time { return now }

	challenge, err := service.StartPairingWithLocalApproval()
	if err != nil {
		t.Fatalf("StartPairingWithLocalApproval() error = %v", err)
	}
	credential, err := service.CompletePairing(challenge.ID, challenge.Code, "Pixel 9")
	if err != nil {
		t.Fatalf("CompletePairing() error = %v", err)
	}

	service.now = func() time.Time { return now.Add(50 * time.Hour) } // 26h past expiry, grace=24h
	_, err = service.RefreshCredential(credential.Token)
	if err != ErrCredentialGraceExpired {
		t.Fatalf("RefreshCredential() error = %v, want %v", err, ErrCredentialGraceExpired)
	}
	if _, ok := service.credentials[credential.DeviceID]; ok {
		t.Fatal("past-grace refresh should reap the credential")
	}
}

func TestRefreshOnOneDeviceLeavesAnotherDeviceCredentialUntouched(t *testing.T) {
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	service := NewServiceWithOptions(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Options{CredentialTTL: 24 * time.Hour, GracePeriod: 90 * 24 * time.Hour})
	service.now = func() time.Time { return now }

	pair := func(name string) Credential {
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

	deviceA := pair("Phone A")
	deviceB := pair("Phone B")

	service.now = func() time.Time { return now.Add(2 * time.Hour) }
	refreshedA, err := service.RefreshCredential(deviceA.Token)
	if err != nil {
		t.Fatalf("RefreshCredential(A) error = %v", err)
	}
	if refreshedA.DeviceID != deviceA.DeviceID {
		t.Fatalf("refreshed DeviceID = %q, want %q", refreshedA.DeviceID, deviceA.DeviceID)
	}

	// B's credential is completely untouched.
	if _, err := service.ValidateCredential(deviceB.Token); err != nil {
		t.Fatalf("ValidateCredential(B) error = %v, want nil", err)
	}
	validatedB, err := service.ValidateCredential(deviceB.Token)
	if err != nil {
		t.Fatalf("ValidateCredential(B) error = %v", err)
	}
	if validatedB.DeviceID != deviceB.DeviceID {
		t.Fatalf("validated DeviceID = %q, want %q", validatedB.DeviceID, deviceB.DeviceID)
	}
	if validatedB.Token == "" || validatedB.Token != deviceB.Token {
		t.Fatal("device B token should be unchanged")
	}
	if _, err := service.ValidateCredential(deviceA.Token); err != ErrCredentialInvalid {
		t.Fatalf("ValidateCredential(A old token) error = %v, want %v", err, ErrCredentialInvalid)
	}
}

func TestNewServiceWithOptionsSetsGracePeriod(t *testing.T) {
	service := NewServiceWithOptions(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Options{
		CredentialTTL: 24 * time.Hour,
		GracePeriod:   90 * 24 * time.Hour,
	})
	if service.gracePeriod != 90*24*time.Hour {
		t.Fatalf("gracePeriod = %v, want %v", service.gracePeriod, 90*24*time.Hour)
	}
}

func TestNewServiceWithOptionsDefaultsGracePeriod(t *testing.T) {
	// Only negative values coerce to the default grace period; zero disables grace.
	service := NewServiceWithOptions(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Options{GracePeriod: -1})
	if service.gracePeriod != defaultGracePeriod {
		t.Fatalf("gracePeriod = %v, want default %v", service.gracePeriod, defaultGracePeriod)
	}
}

func TestNewServiceWithOptionsZeroGracePeriodDisablesGrace(t *testing.T) {
	service := NewServiceWithOptions(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Options{})
	if service.gracePeriod != 0 {
		t.Fatalf("gracePeriod = %v, want 0 (grace disabled)", service.gracePeriod)
	}
}

func TestValidateCredentialSoftExpiresInsteadOfDeleting(t *testing.T) {
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	service := NewServiceWithOptions(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Options{CredentialTTL: time.Hour, GracePeriod: 24 * time.Hour})
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
	_, err = service.ValidateCredential(credential.Token)
	if err != ErrCredentialExpired {
		t.Fatalf("ValidateCredential() error = %v, want %v", err, ErrCredentialExpired)
	}
	if _, ok := service.credentials[credential.DeviceID]; !ok {
		t.Fatal("soft-expired credential should remain in memory")
	}
	if service.credentials[credential.DeviceID].ExpiredAt == nil {
		t.Fatal("ExpiredAt should be set after soft-expire")
	}
}

func TestPruneDeletesOnlyPastGrace(t *testing.T) {
	now := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	service := NewServiceWithOptions(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Options{CredentialTTL: time.Hour, GracePeriod: 24 * time.Hour})
	service.now = func() time.Time { return now }

	challenge, err := service.StartPairingWithLocalApproval()
	if err != nil {
		t.Fatalf("StartPairingWithLocalApproval() error = %v", err)
	}
	credential, err := service.CompletePairing(challenge.ID, challenge.Code, "Pixel 9")
	if err != nil {
		t.Fatalf("CompletePairing() error = %v", err)
	}

	// 2h after expiry: within grace (24h) -> keep.
	service.now = func() time.Time { return now.Add(2 * time.Hour) }
	if _, err := service.ValidateCredential(credential.Token); err != ErrCredentialExpired {
		t.Fatalf("ValidateCredential() error = %v, want %v", err, ErrCredentialExpired)
	}
	service.pruneExpiredCredentialsLocked(now.Add(2 * time.Hour))
	if _, ok := service.credentials[credential.DeviceID]; !ok {
		t.Fatal("credential should survive within grace window")
	}

	// 26h after pairing: 25h past expiry, past the 24h grace -> reap.
	service.now = func() time.Time { return now.Add(26 * time.Hour) }
	service.pruneExpiredCredentialsLocked(now.Add(26 * time.Hour))
	if _, ok := service.credentials[credential.DeviceID]; ok {
		t.Fatal("credential should be reaped past grace window")
	}
}

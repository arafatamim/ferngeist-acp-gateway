package pairing

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

const (
	defaultChallengeTTL = 2 * time.Minute
	defaultArmTTL       = 2 * time.Minute
	defaultTokenTTL     = 7 * 24 * time.Hour
	defaultGracePeriod  = 90 * 24 * time.Hour
	challengeHistoryTTL = 10 * time.Minute
	codeLength          = 6

	// defaultRotationOverlap is how long a token stays exchangeable after a
	// refresh rotated it away. Rotation is otherwise instant and irreversible, so
	// a lost response (or two refreshes racing) would leave the client holding a
	// token the gateway only answers with "credential invalid" — the one state
	// the grace recovery does not cover, and the one the app reacts to by
	// dropping the gateway and its bindings. The client refreshes on launch and
	// on push and treats a token within 24h of expiry as due, so one missed
	// response is retried well inside this window.
	defaultRotationOverlap = 48 * time.Hour
)

var (
	ErrChallengeNotFound      = errors.New("pairing challenge not found")
	ErrChallengeExpired       = errors.New("pairing challenge expired")
	ErrChallengeAmbiguous     = errors.New("pairing challenge is ambiguous")
	ErrCodeMismatch           = errors.New("pairing code mismatch")
	ErrInvalidDeviceName      = errors.New("device name is required")
	ErrDeviceNotFound         = errors.New("paired device not found")
	ErrCredentialMissing      = errors.New("gateway credential missing")
	ErrCredentialInvalid      = errors.New("gateway credential invalid")
	ErrCredentialExpired      = errors.New("gateway credential expired")
	ErrCredentialGraceExpired = errors.New("gateway credential expired beyond grace period")
	ErrCredentialScope        = errors.New("gateway credential does not allow this operation")
	ErrPairingNotArmed        = errors.New("pairing requires local approval")
)

const (
	ScopeRead              = "gateway.read"
	ScopeControl           = "gateway.control"
	ScopeDiagnosticsExport = "gateway.diagnostics.export"
	ScopeRuntimeRestartEnv = "gateway.runtime.restart_env"
)

type ChallengeState string

const (
	ChallengeStateActive    ChallengeState = "active"
	ChallengeStateCompleted ChallengeState = "completed"
	ChallengeStateCancelled ChallengeState = "cancelled"
	ChallengeStateExpired   ChallengeState = "expired"
)

type Challenge struct {
	ID        string    `json:"id"`
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type Credential struct {
	DeviceID       string    `json:"deviceId"`
	DeviceName     string    `json:"deviceName"`
	Token          string    `json:"token"`
	TokenHash      string    `json:"-"`
	ExpiresAt      time.Time `json:"expiresAt"`
	Scopes         []string  `json:"scopes,omitempty"`
	ProofPublicKey string    `json:"proofPublicKey,omitempty"`
	// ExpiredAt records when the credential's access token lapsed. A nil value
	// means the credential is live. Non-nil marks a soft-expired credential that
	// is still recoverable via refresh within the grace window.
	ExpiredAt *time.Time `json:"expiredAt,omitempty"`
}

type CompletedDevice struct {
	DeviceID   string    `json:"deviceId"`
	DeviceName string    `json:"deviceName"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

type ChallengeStatus struct {
	ID              string           `json:"id"`
	Code            string           `json:"code"`
	ExpiresAt       time.Time        `json:"expiresAt"`
	State           ChallengeState   `json:"state"`
	CompletedDevice *CompletedDevice `json:"completedDevice,omitempty"`
}

type challengeRecord struct {
	challenge       Challenge
	state           ChallengeState
	completedDevice *CompletedDevice
	stateChangedAt  time.Time
}

// Service manages gateway-local trust bootstrap. Pairing challenges are
// short-lived and in-memory; issued device credentials can be reloaded from
// SQLite so the gateway survives restarts.
type Service struct {
	logger      *slog.Logger
	mu          sync.Mutex
	now         func() time.Time
	armTTL      time.Duration
	tokenTTL    time.Duration
	gracePeriod time.Duration
	baseScopes  []string
	activeID    string
	armedUntil  time.Time
	challenges  map[string]challengeRecord
	credentials map[string]Credential
	// byTokenHash indexes TokenHash -> deviceID so Validate is O(1):
	// one SHA-256 per request instead of one per device.
	byTokenHash map[string]string
	// retired indexes the credential hashes recent refreshes rotated away, so a
	// client that never received its new token can still exchange the old one.
	// Retired hashes authorize nothing by themselves: only the refresh endpoint
	// consults them, and only after proof-of-possession.
	retired map[string]retiredToken
	store   *storage.SQLiteStore
}

// retiredToken is a credential hash that a refresh rotated away, and the
// deadline until which the gateway will still exchange it for the live token.
type retiredToken struct {
	deviceID string
	until    time.Time
}

type Options struct {
	ArmTTL        time.Duration
	CredentialTTL time.Duration
	// GracePeriod is how long an expired credential stays recoverable via
	// refresh. A value <= 0 restores hard-delete-on-expiry (no grace).
	GracePeriod            time.Duration
	AllowDiagnosticsExport bool
	AllowRuntimeRestartEnv bool
}

func NewService(logger *slog.Logger, store *storage.SQLiteStore) *Service {
	return NewServiceWithOptions(logger, store, Options{})
}

func NewServiceWithOptions(logger *slog.Logger, store *storage.SQLiteStore, options Options) *Service {
	armTTL := options.ArmTTL
	if armTTL <= 0 {
		armTTL = defaultArmTTL
	}
	tokenTTL := options.CredentialTTL
	if tokenTTL <= 0 {
		tokenTTL = defaultTokenTTL
	}
	gracePeriod := options.GracePeriod
	if gracePeriod < 0 {
		gracePeriod = defaultGracePeriod
	}
	service := &Service{
		logger:      logger.With("component", "pairing"),
		now:         time.Now,
		armTTL:      armTTL,
		tokenTTL:    tokenTTL,
		gracePeriod: gracePeriod,
		baseScopes:  defaultCredentialScopes(options.AllowDiagnosticsExport, options.AllowRuntimeRestartEnv),
		challenges:  make(map[string]challengeRecord),
		credentials: make(map[string]Credential),
		byTokenHash: make(map[string]string),
		retired:     make(map[string]retiredToken),
		store:       store,
	}
	service.loadPersistedCredentials()
	return service
}

// SetClockForTesting replaces the time provider. It exists so tests outside
// the pairing package (internal/api) can drive expiry-based behavior
// deterministically; production code never calls it.
func (s *Service) SetClockForTesting(now func() time.Time) {
	s.now = now
}

func (s *Service) StartPairing() (Challenge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now().UTC()
	s.pruneExpiredLocked(now)
	if status, ok := s.activeChallengeLocked(); ok {
		return status.challenge, nil
	}
	if !s.isArmedLocked(now) {
		return Challenge{}, ErrPairingNotArmed
	}

	challenge := Challenge{
		ID:        randomToken(18),
		Code:      randomCode(codeLength),
		ExpiresAt: now.Add(defaultChallengeTTL),
	}
	s.activeID = challenge.ID
	s.challenges[challenge.ID] = challengeRecord{
		challenge:      challenge,
		state:          ChallengeStateActive,
		stateChangedAt: now,
	}
	return challenge, nil
}

// StartPairingWithLocalApproval opens a short pairing window and then starts
// (or returns) the active challenge. Intended for trusted local control paths.
func (s *Service) StartPairingWithLocalApproval() (Challenge, error) {
	s.mu.Lock()
	now := s.now().UTC()
	s.armedUntil = now.Add(s.armTTL)
	s.mu.Unlock()
	return s.StartPairing()
}

func (s *Service) ActiveChallenge() (ChallengeStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneExpiredLocked(s.now().UTC())
	record, ok := s.activeChallengeLocked()
	if !ok {
		return ChallengeStatus{}, false
	}
	return record.toStatus(), true
}

func (s *Service) GetChallengeStatus(challengeID string) (ChallengeStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneExpiredLocked(s.now().UTC())
	record, ok := s.challenges[challengeID]
	if !ok {
		return ChallengeStatus{}, ErrChallengeNotFound
	}
	return record.toStatus(), nil
}

func (s *Service) CancelChallenge(challengeID string) (ChallengeStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now().UTC()
	s.pruneExpiredLocked(now)

	if challengeID == "" {
		challengeID = s.activeID
	}
	if challengeID == "" {
		return ChallengeStatus{}, ErrChallengeNotFound
	}

	record, ok := s.challenges[challengeID]
	if !ok {
		return ChallengeStatus{}, ErrChallengeNotFound
	}
	if record.state == ChallengeStateActive {
		record.state = ChallengeStateCancelled
		record.stateChangedAt = now
		s.challenges[challengeID] = record
		if s.activeID == challengeID {
			s.activeID = ""
		}
	}
	return record.toStatus(), nil
}

// CompletePairing exchanges a valid short-lived challenge for a longer-lived
// device credential. Clients may provide either a challenge ID plus code, or a
// code alone when the gateway has displayed a short code separately from the QR
// payload. Code-only completion succeeds only when that code resolves to a
// single active challenge.
func (s *Service) CompletePairing(challengeID, code, deviceName string) (Credential, error) {
	return s.CompletePairingWithProofKey(challengeID, code, deviceName, "")
}

func (s *Service) CompletePairingWithProofKey(challengeID, code, deviceName, proofPublicKey string) (Credential, error) {
	if deviceName == "" {
		return Credential{}, ErrInvalidDeviceName
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now().UTC()
	s.pruneExpiredCredentialsLocked(now)

	challengeID, challenge, err := s.resolveChallengeLocked(challengeID, now)
	if err != nil {
		return Credential{}, err
	}
	if code != challenge.challenge.Code {
		return Credential{}, ErrCodeMismatch
	}

	credential := Credential{
		DeviceID:       randomToken(18),
		DeviceName:     deviceName,
		Token:          randomToken(32),
		ExpiresAt:      now.Add(s.tokenTTL),
		Scopes:         slices.Clone(s.baseScopes),
		ProofPublicKey: proofPublicKey,
	}
	credential.TokenHash = hashCredentialToken(credential.Token)
	s.credentials[credential.DeviceID] = credential
	s.indexCredentialLocked(credential)
	if s.store != nil {
		if err := s.store.SavePairing(context.Background(), storage.PairingRecord{
			DeviceID:       credential.DeviceID,
			DeviceName:     credential.DeviceName,
			Token:          credential.TokenHash,
			ExpiresAt:      credential.ExpiresAt,
			Scopes:         credential.Scopes,
			ProofPublicKey: credential.ProofPublicKey,
		}); err != nil {
			s.logger.Error("persist pairing failed", "error", err)
		}
	}
	challenge.state = ChallengeStateCompleted
	challenge.stateChangedAt = now
	challenge.completedDevice = &CompletedDevice{
		DeviceID:   credential.DeviceID,
		DeviceName: credential.DeviceName,
		ExpiresAt:  credential.ExpiresAt,
	}
	s.challenges[challengeID] = challenge
	if s.activeID == challengeID {
		s.activeID = ""
	}
	s.armedUntil = time.Time{}
	return credential, nil
}

func (s *Service) resolveChallengeLocked(challengeID string, now time.Time) (string, challengeRecord, error) {
	if challengeID != "" {
		challenge, ok := s.challenges[challengeID]
		if !ok {
			return "", challengeRecord{}, ErrChallengeNotFound
		}
		if challenge.state != ChallengeStateActive {
			if challenge.state == ChallengeStateExpired {
				return "", challengeRecord{}, ErrChallengeExpired
			}
			return "", challengeRecord{}, ErrChallengeNotFound
		}
		if now.After(challenge.challenge.ExpiresAt) {
			s.expireChallengeLocked(challengeID, challenge)
			return "", challengeRecord{}, ErrChallengeExpired
		}
		return challengeID, challenge, nil
	}

	record, ok := s.activeChallengeLocked()
	if !ok {
		return "", challengeRecord{}, ErrChallengeNotFound
	}
	if now.After(record.challenge.ExpiresAt) {
		s.expireChallengeLocked(record.challenge.ID, record)
		return "", challengeRecord{}, ErrChallengeExpired
	}
	return record.challenge.ID, record, nil
}

func (s *Service) ActiveDeviceCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneExpiredCredentialsLocked(s.now().UTC())
	return len(s.credentials)
}

// ValidateCredential is a simple token lookup because gateway-issued device
// credentials are already random opaque tokens scoped to this daemon.
// O(1) via the TokenHash index: one SHA-256 per call, no per-device scan.
func (s *Service) ValidateCredential(token string) (Credential, error) {
	if token == "" {
		return Credential{}, ErrCredentialMissing
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now().UTC()

	if id, ok := s.byTokenHash[hashCredentialToken(token)]; ok {
		credential := s.credentials[id]
		if now.After(credential.ExpiresAt) {
			if s.gracePeriod <= 0 {
				s.deleteCredentialLocked(id)
				return Credential{}, ErrCredentialExpired
			}
			expiredAt := now
			credential.ExpiredAt = &expiredAt
			s.credentials[id] = credential
			return Credential{}, ErrCredentialExpired
		}
		return credential, nil
	}
	// Legacy fallback: unhashed tokens (pre-migration) have no index entry.
	for id, credential := range s.credentials {
		if credential.TokenHash == "" && credential.Token == token {
			if now.After(credential.ExpiresAt) {
				if s.gracePeriod <= 0 {
					s.deleteCredentialLocked(id)
					return Credential{}, ErrCredentialExpired
				}
				expiredAt := now
				credential.ExpiredAt = &expiredAt
				s.credentials[id] = credential
				return Credential{}, ErrCredentialExpired
			}
			return credential, nil
		}
	}

	s.pruneExpiredCredentialsLocked(now)
	return Credential{}, ErrCredentialInvalid
}

func (s *Service) RefreshCredential(token string) (Credential, error) {
	if token == "" {
		return Credential{}, ErrCredentialMissing
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now().UTC()
	id, ok := s.byTokenHash[hashCredentialToken(token)]
	if !ok {
		// Legacy unhashed fallback.
		for legacyID, credential := range s.credentials {
			if credential.TokenHash == "" && credential.Token == token {
				id, ok = legacyID, true
				break
			}
		}
	}
	// presented is the retired entry the caller came in on, if any. It is the
	// token the client actually holds, so it is what must survive a restart.
	var presented *retiredToken
	if !ok {
		// The caller holds a token this gateway already rotated away: its refresh
		// response was lost, or two refreshes raced. Hand back the live credential
		// when its secret is still known rather than answering "credential
		// invalid", which is the one failure the client cannot recover from.
		if retiredID, retired := s.retiredDeviceLocked(token, now); retired {
			entry := s.retired[hashCredentialToken(token)]
			presented = &entry
			if current, exists := s.credentials[retiredID]; exists && current.Token != "" && now.Before(current.ExpiresAt) {
				return current, nil
			}
			id, ok = retiredID, true
		}
	}
	if !ok {
		s.pruneExpiredCredentialsLocked(now)
		return Credential{}, ErrCredentialInvalid
	}
	credential := s.credentials[id]
	if now.After(credential.ExpiresAt) {
		if s.gracePeriod <= 0 {
			s.deleteCredentialLocked(id)
			return Credential{}, ErrCredentialExpired
		}
		if now.Sub(credential.ExpiresAt) > s.gracePeriod {
			s.deleteCredentialLocked(id)
			return Credential{}, ErrCredentialGraceExpired
		}
	}

	oldHash := credential.TokenHash
	newToken := randomToken(32)
	newHash := hashCredentialToken(newToken)
	expiresAt := now.Add(s.tokenTTL)

	// Persist before the rotation becomes visible. A token the store never
	// recorded is a token that disappears on the next restart, and the store is
	// authoritative for what survives one. Failing here leaves the old credential
	// in place — it still works, so the client can simply retry.
	if s.store != nil {
		record := storage.PairingRecord{
			DeviceID:       credential.DeviceID,
			DeviceName:     credential.DeviceName,
			Token:          newHash,
			ExpiresAt:      expiresAt,
			Scopes:         credential.Scopes,
			ProofPublicKey: credential.ProofPublicKey,
		}
		switch {
		case presented != nil:
			// The store keeps one retired hash per device. The live hash being
			// replaced here was never delivered (the client came back with the
			// retired one), so persist the token the client holds, under its
			// original deadline so repeated retries cannot stretch the window.
			record.RetiredToken = hashCredentialToken(token)
			record.RetiredUntil = presented.until
		case oldHash != "":
			record.RetiredToken = oldHash
			record.RetiredUntil = now.Add(defaultRotationOverlap)
		}
		if err := s.store.SavePairing(context.Background(), record); err != nil {
			s.logger.Error("persist refreshed pairing failed", "device_id", credential.DeviceID, "error", err)
			return Credential{}, err
		}
	}

	credential.Token = newToken
	credential.TokenHash = newHash
	credential.ExpiresAt = expiresAt
	credential.ExpiredAt = nil
	s.credentials[id] = credential
	if oldHash != "" && oldHash != credential.TokenHash {
		delete(s.byTokenHash, oldHash)
		s.retired[oldHash] = retiredToken{deviceID: credential.DeviceID, until: now.Add(defaultRotationOverlap)}
	}
	s.pruneRetiredLocked(now)
	s.indexCredentialLocked(credential)
	return credential, nil
}

// LookupCredentialByToken returns the credential matching the token regardless
// of expiry state, without mutating it. Used by the refresh endpoint to
// authorize a proof-of-possession for a possibly-expired credential, and for a
// token a refresh rotated away within the overlap window — the caller that lost
// the refresh response still holds that one. Normal API calls go through
// ValidateCredential and never see retired tokens.
func (s *Service) LookupCredentialByToken(token string) (Credential, error) {
	if token == "" {
		return Credential{}, ErrCredentialMissing
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.byTokenHash[hashCredentialToken(token)]; ok {
		if credential, ok := s.credentials[id]; ok {
			return credential, nil
		}
	}
	for _, credential := range s.credentials {
		if credential.TokenHash == "" && credential.Token == token {
			return credential, nil
		}
	}
	if deviceID, ok := s.retiredDeviceLocked(token, s.now().UTC()); ok {
		if credential, ok := s.credentials[deviceID]; ok {
			return credential, nil
		}
	}
	return Credential{}, ErrCredentialInvalid
}

func (c Credential) HasScope(scope string) bool {
	return slices.Contains(c.Scopes, scope)
}

func (c Credential) RequireScope(scope string) error {
	if c.HasScope(scope) {
		return nil
	}
	return ErrCredentialScope
}

func (s *Service) ListDevices() []Credential {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneExpiredCredentialsLocked(s.now().UTC())
	devices := make([]Credential, 0, len(s.credentials))
	for _, credential := range s.credentials {
		devices = append(devices, credential)
	}
	sort.Slice(devices, func(i, j int) bool {
		if devices[i].DeviceName == devices[j].DeviceName {
			return devices[i].DeviceID < devices[j].DeviceID
		}
		return devices[i].DeviceName < devices[j].DeviceName
	})
	return devices
}

func (s *Service) RevokeDevice(deviceID string) (Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	credential, ok := s.credentials[deviceID]
	if !ok {
		return Credential{}, ErrDeviceNotFound
	}
	s.deleteCredentialLocked(deviceID)
	return credential, nil
}

func (s *Service) pruneExpiredLocked(now time.Time) {
	for id, record := range s.challenges {
		switch record.state {
		case ChallengeStateActive:
			if now.After(record.challenge.ExpiresAt) {
				s.expireChallengeLocked(id, record)
			}
		default:
			if now.Sub(record.stateChangedAt) > challengeHistoryTTL {
				delete(s.challenges, id)
			}
		}
	}
	s.pruneExpiredCredentialsLocked(now)
}

func (s *Service) pruneExpiredCredentialsLocked(now time.Time) {
	for id, credential := range s.credentials {
		if s.shouldReapCredential(credential, now) {
			s.deleteCredentialLocked(id)
		}
	}
	s.pruneRetiredLocked(now)
}

func (s *Service) pruneRetiredLocked(now time.Time) {
	for hash, entry := range s.retired {
		if now.After(entry.until) {
			delete(s.retired, hash)
		}
	}
}

// retiredDeviceLocked resolves a token that a refresh rotated away recently.
// It only names the device the caller is holding a token for; the caller must
// still prove possession of that device's key before anything is issued.
// Caller holds s.mu.
func (s *Service) retiredDeviceLocked(token string, now time.Time) (string, bool) {
	entry, ok := s.retired[hashCredentialToken(token)]
	if !ok || now.After(entry.until) {
		return "", false
	}
	if _, ok := s.credentials[entry.deviceID]; !ok {
		return "", false
	}
	return entry.deviceID, true
}

// shouldReapCredential reports whether a credential should be hard-deleted:
// immediately on expiry when grace is disabled, or once the grace window
// (measured from the credential's ExpiresAt) has elapsed.
func (s *Service) shouldReapCredential(credential Credential, now time.Time) bool {
	if !now.After(credential.ExpiresAt) {
		return false
	}
	if s.gracePeriod <= 0 {
		return true
	}
	return now.Sub(credential.ExpiresAt) > s.gracePeriod
}

func (s *Service) deleteCredentialLocked(deviceID string) {
	if cred, ok := s.credentials[deviceID]; ok && cred.TokenHash != "" {
		if mapped, ok := s.byTokenHash[cred.TokenHash]; ok && mapped == deviceID {
			delete(s.byTokenHash, cred.TokenHash)
		}
	}
	delete(s.credentials, deviceID)
	// A revoked device must not keep exchanging whatever it rotated away.
	for hash, entry := range s.retired {
		if entry.deviceID == deviceID {
			delete(s.retired, hash)
		}
	}
	if s.store == nil {
		return
	}
	if err := s.store.DeletePairing(context.Background(), deviceID); err != nil && !errors.Is(err, storage.ErrNotFound) {
		s.logger.Error("delete pairing failed", "device_id", deviceID, "error", err)
	}
}

// indexCredentialLocked records TokenHash -> deviceID. Caller holds s.mu.
func (s *Service) indexCredentialLocked(c Credential) {
	if c.TokenHash != "" {
		s.byTokenHash[c.TokenHash] = c.DeviceID
	}
}

func (s *Service) activeChallengeLocked() (challengeRecord, bool) {
	if s.activeID == "" {
		return challengeRecord{}, false
	}
	record, ok := s.challenges[s.activeID]
	if !ok || record.state != ChallengeStateActive {
		s.activeID = ""
		return challengeRecord{}, false
	}
	return record, true
}

func (s *Service) isArmedLocked(now time.Time) bool {
	if s.armedUntil.IsZero() {
		return false
	}
	if now.After(s.armedUntil) {
		s.armedUntil = time.Time{}
		return false
	}
	return true
}

func (s *Service) expireChallengeLocked(challengeID string, record challengeRecord) {
	record.state = ChallengeStateExpired
	record.stateChangedAt = record.challenge.ExpiresAt
	s.challenges[challengeID] = record
	if s.activeID == challengeID {
		s.activeID = ""
	}
}

func (r challengeRecord) toStatus() ChallengeStatus {
	return ChallengeStatus{
		ID:              r.challenge.ID,
		Code:            r.challenge.Code,
		ExpiresAt:       r.challenge.ExpiresAt,
		State:           r.state,
		CompletedDevice: r.completedDevice,
	}
}

// loadPersistedCredentials restores still-valid gateway credentials so paired
// devices are not forced to re-pair after every daemon restart.
func (s *Service) loadPersistedCredentials() {
	if s.store == nil {
		return
	}

	records, err := s.store.ListPairings(context.Background())
	if err != nil {
		s.logger.Error("load persisted pairings failed", "error", err)
		return
	}

	now := s.now().UTC()
	for _, record := range records {
		if s.shouldReapPersistedCredential(record, now) {
			if err := s.store.DeletePairing(context.Background(), record.DeviceID); err != nil && !errors.Is(err, storage.ErrNotFound) {
				s.logger.Error("delete expired pairing failed", "device_id", record.DeviceID, "error", err)
			}
			continue
		}
		credential := Credential{
			DeviceID:       record.DeviceID,
			DeviceName:     record.DeviceName,
			TokenHash:      storedCredentialHash(record.Token),
			ExpiresAt:      record.ExpiresAt,
			Scopes:         fallbackScopes(record.Scopes, s.baseScopes),
			ProofPublicKey: record.ProofPublicKey,
		}
		if now.After(record.ExpiresAt) && s.gracePeriod > 0 {
			// Expired but still within the grace window: mark it soft-expired so
			// a proof-validated refresh can recover it later.
			expiredAt := record.ExpiresAt
			credential.ExpiredAt = &expiredAt
		}
		s.credentials[record.DeviceID] = credential
		s.indexCredentialLocked(credential)
		if record.RetiredToken != "" && now.Before(record.RetiredUntil) {
			s.retired[record.RetiredToken] = retiredToken{deviceID: record.DeviceID, until: record.RetiredUntil}
		}
		if !isHashedCredentialToken(record.Token) && s.store != nil {
			if err := s.store.SavePairing(context.Background(), storage.PairingRecord{
				DeviceID:       record.DeviceID,
				DeviceName:     record.DeviceName,
				Token:          storedCredentialHash(record.Token),
				ExpiresAt:      record.ExpiresAt,
				Scopes:         fallbackScopes(record.Scopes, s.baseScopes),
				ProofPublicKey: record.ProofPublicKey,
				RetiredToken:   record.RetiredToken,
				RetiredUntil:   record.RetiredUntil,
			}); err != nil {
				s.logger.Error("upgrade pairing token hash failed", "device_id", record.DeviceID, "error", err)
			}
		}
	}
}

// shouldReapPersistedCredential mirrors shouldReapCredential for stored rows:
// when grace is disabled, any row past ExpiresAt is reaped; otherwise only
// rows whose expiry is more than gracePeriod in the past.
func (s *Service) shouldReapPersistedCredential(record storage.PairingRecord, now time.Time) bool {
	if s.gracePeriod <= 0 {
		return now.After(record.ExpiresAt)
	}
	if !now.After(record.ExpiresAt) {
		return false
	}
	return now.Sub(record.ExpiresAt) > s.gracePeriod
}

func defaultCredentialScopes(allowDiagnosticsExport, allowRuntimeRestartEnv bool) []string {
	scopes := []string{ScopeRead, ScopeControl}
	if allowDiagnosticsExport {
		scopes = append(scopes, ScopeDiagnosticsExport)
	}
	if allowRuntimeRestartEnv {
		scopes = append(scopes, ScopeRuntimeRestartEnv)
	}
	return scopes
}

func fallbackScopes(scopes []string, fallback []string) []string {
	if len(scopes) > 0 {
		return slices.Clone(scopes)
	}
	return slices.Clone(fallback)
}

const credentialHashPrefix = "sha256:"

func credentialMatchesToken(credential Credential, token string) bool {
	if token == "" {
		return false
	}
	if credential.TokenHash != "" {
		return credential.TokenHash == hashCredentialToken(token)
	}
	return credential.Token == token
}

func hashCredentialToken(token string) string {
	digest := sha256.Sum256([]byte(token))
	return credentialHashPrefix + base64.RawURLEncoding.EncodeToString(digest[:])
}

func isHashedCredentialToken(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), credentialHashPrefix)
}

func storedCredentialHash(stored string) string {
	if isHashedCredentialToken(stored) {
		return strings.TrimSpace(stored)
	}
	return hashCredentialToken(stored)
}

func randomToken(byteLen int) string {
	buf := make([]byte, byteLen)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Errorf("pairing token generation failed: %w", err))
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func randomCode(length int) string {
	if length <= 0 {
		return ""
	}
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Errorf("pairing code generation failed: %w", err))
	}

	for i := range buf {
		buf[i] = '0' + (buf[i] % 10)
	}
	return string(buf)
}

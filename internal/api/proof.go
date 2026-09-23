package api

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
)

// verifyCredentialProof validates a proof-of-possession signature attached to
// an authenticated request. The proof mechanism prevents credential theft by
// requiring the client to prove it holds the private key corresponding to the
// public key registered during pairing.
//
// The verification process:
//  1. Extract timestamp, nonce, and signature from HTTP headers
//  2. Validate timestamp is within the allowed skew window (prevents old requests)
//  3. Check nonce against the replay tracker (prevents duplicate requests)
//  4. Compute SHA-256 hash of the request body
//  5. Build a canonical proof message from method, path, token, timestamp, nonce, body hash
//  6. Verify the ECDSA signature against the credential's registered public key
//
// Returns nil if the credential has no proof public key registered (legacy mode)
// or if all verification steps pass.
func (s *Server) verifyCredentialProof(r *http.Request, rawToken string, credential pairing.Credential) error {
	if strings.TrimSpace(credential.ProofPublicKey) == "" {
		return nil
	}
	if r == nil {
		return errors.New("gateway credential proof required")
	}
	timestampText := strings.TrimSpace(r.Header.Get(proofHeaderTimestamp))
	nonce := strings.TrimSpace(r.Header.Get(proofHeaderNonce))
	signatureText := strings.TrimSpace(r.Header.Get(proofHeaderSignature))
	if timestampText == "" || nonce == "" || signatureText == "" {
		return errors.New("gateway credential proof required")
	}
	timestampUnix, err := strconv.ParseInt(timestampText, 10, 64)
	if err != nil {
		return errors.New("gateway credential proof invalid")
	}
	now := s.now().UTC()
	timestamp := time.Unix(timestampUnix, 0).UTC()
	if timestamp.Before(now.Add(-proofSkewWindow)) || timestamp.After(now.Add(proofSkewWindow)) {
		return errors.New("gateway credential proof expired")
	}
	nonceKey := credential.DeviceID + ":" + nonce
	if s.proofNonces != nil && s.proofNonces.seen(nonceKey, now) {
		return errors.New("gateway credential proof replayed")
	}
	bodyHash, err := requestBodyHash(r, jsonBodyLimit)
	if err != nil {
		return errors.New("gateway credential proof invalid")
	}
	message := buildProofMessage(r.Method, requestPathWithRawQuery(r), rawToken, timestampText, nonce, bodyHash)
	publicKey, err := cachedProofPublicKey(credential.ProofPublicKey)
	if err != nil {
		return errors.New("gateway credential proof invalid")
	}
	signature, err := decodeProofBase64(signatureText)
	if err != nil {
		return errors.New("gateway credential proof invalid")
	}
	digest := sha256.Sum256([]byte(message))
	if !ecdsa.VerifyASN1(publicKey, digest[:], signature) {
		return errors.New("gateway credential proof invalid")
	}
	if s.proofNonces != nil && !s.proofNonces.use(nonceKey, now) {
		return errors.New("gateway credential proof replayed")
	}
	return nil
}

// buildProofMessage constructs the canonical message that is signed by the client.
// The domain separator prefix ensures signatures cannot be reused outside this protocol.
func buildProofMessage(method, pathWithQuery, token, timestamp, nonce, bodyHash string) string {
	return strings.Join([]string{
		proofDomain,
		strings.ToUpper(strings.TrimSpace(method)),
		pathWithQuery,
		token,
		timestamp,
		nonce,
		bodyHash,
	}, "\n")
}

// requestPathWithRawQuery returns the full request path including query string
// for inclusion in the proof message. Unlike requestPath(), this does NOT
// sanitize sensitive query parameters.
func requestPathWithRawQuery(r *http.Request) string {
	if r == nil || r.URL == nil {
		return "/"
	}
	if r.URL.RawQuery == "" {
		return r.URL.Path
	}
	return r.URL.Path + "?" + r.URL.RawQuery
}

// requestBodyHash reads and hashes the request body for inclusion in the proof.
// It restores the body after reading so downstream handlers can still access it.
// The maxBytes limit prevents memory exhaustion from oversized bodies.
func requestBodyHash(r *http.Request, maxBytes int64) (string, error) {
	if r == nil || r.Body == nil {
		return hashProofBody(nil), nil
	}
	reader := io.Reader(r.Body)
	if maxBytes > 0 {
		reader = io.LimitReader(r.Body, maxBytes+1)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	if maxBytes > 0 && int64(len(body)) > maxBytes {
		return "", errors.New("proof body too large")
	}
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	return hashProofBody(body), nil
}

// hashProofBody computes a URL-safe base64-encoded SHA-256 hash of the body.
func hashProofBody(body []byte) string {
	digest := sha256.Sum256(body)
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// parseProofPublicKey decodes and parses an ECDSA public key from base64-encoded
// PKIX/DER format. The key is used to verify credential proof signatures.
func parseProofPublicKey(encoded string) (*ecdsa.PublicKey, error) {
	decoded, err := decodeProofBase64(encoded)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParsePKIXPublicKey(decoded)
	if err != nil {
		return nil, err
	}
	publicKey, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("unexpected proof public key type")
	}
	return publicKey, nil
}

// decodeProofBase64 attempts to decode a base64 value, trying both raw URL
// encoding (no padding) and standard encoding (with padding) for compatibility.
func decodeProofBase64(value string) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err == nil {
		return decoded, nil
	}
	return base64.StdEncoding.DecodeString(strings.TrimSpace(value))
}

// proofNonceTracker prevents replay attacks by tracking used nonces per device
// and rejecting any nonce that has been seen within the replay window.
type proofNonceTracker struct {
	mu        sync.Mutex
	nonces    map[string]time.Time
	lastSweep time.Time
}

// newProofNonceTracker creates an empty nonce tracker.
func newProofNonceTracker() *proofNonceTracker {
	return &proofNonceTracker{nonces: make(map[string]time.Time)}
}

// use checks whether a nonce has been used before. If not, it records the
// nonce with an expiry time of now + proofReplayWindow. Expired nonces are
// swept at most once per minute (amortized): a full-map scan on every request
// is O(request-rate x window) and attacker-amplifiable. Returns false if the
// nonce is a replay.
func (t *proofNonceTracker) use(key string, now time.Time) bool {
	if key == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if now.Sub(t.lastSweep) >= time.Minute {
		t.lastSweep = now
		for existingKey, expiresAt := range t.nonces {
			if !expiresAt.After(now) {
				delete(t.nonces, existingKey)
			}
		}
		// Hard cap: a flood of distinct nonces within one sweep window must
		// not grow the map without bound.
		const maxProofNonces = 16384
		if len(t.nonces) > maxProofNonces {
			// Evict arbitrary oldest-expired-ish entries; correctness holds
			// because eviction only widens the replay window slightly.
			for existingKey := range t.nonces {
				delete(t.nonces, existingKey)
				if len(t.nonces) <= maxProofNonces {
					break
				}
			}
		}
	}
	if expiresAt, ok := t.nonces[key]; ok && expiresAt.After(now) {
		return false
	}
	t.nonces[key] = now.Add(proofReplayWindow)
	return true
}

// seen reports whether a nonce was already consumed without mutating state.
// Used to fail fast before the ECDSA verify; the nonce is consumed by use()
// only after a valid signature.
func (t *proofNonceTracker) seen(key string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	expiresAt, ok := t.nonces[key]
	return ok && expiresAt.After(now)
}

// proofKeyCache memoizes x509 parses keyed by the encoded key itself
// (content-addressed, so no invalidation needed): one parse per unique
// device key instead of one per request.
var proofKeyCache = struct {
	sync.RWMutex
	keys map[string]*ecdsa.PublicKey
}{keys: make(map[string]*ecdsa.PublicKey)}

func cachedProofPublicKey(encoded string) (*ecdsa.PublicKey, error) {
	proofKeyCache.RLock()
	if key, ok := proofKeyCache.keys[encoded]; ok {
		proofKeyCache.RUnlock()
		return key, nil
	}
	proofKeyCache.RUnlock()
	parsed, err := parseProofPublicKey(encoded)
	if err != nil {
		return nil, err
	}
	proofKeyCache.Lock()
	proofKeyCache.keys[encoded] = parsed
	proofKeyCache.Unlock()
	return parsed, nil
}

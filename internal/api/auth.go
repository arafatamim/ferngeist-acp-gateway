package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
)

// Auth rejection codes. They are contract: clients decide whether to unpair on
// these, never on the message text.
const (
	proofRequired             = "proof_required"
	proofInvalid              = "proof_invalid"
	proofExpired              = "proof_expired"
	proofReplayed             = "proof_replayed"
	credentialMissing         = "credential_missing"
	credentialInvalid         = "credential_invalid"
	credentialExpired         = "credential_expired"
	credentialGraceExpired    = "credential_grace_expired"
	credentialScope           = "credential_scope"
	legacyCredentialsDisabled = "legacy_credentials_disabled"
)

var errLegacyCredentialsDisabled = &authError{code: legacyCredentialsDisabled, message: "legacy bearer credentials are disabled"}

// authError is an auth rejection that carries its stable code.
type authError struct {
	code    string
	message string
}

func (e *authError) Error() string { return e.message }

func authErrorCode(err error) string {
	var authErr *authError
	switch {
	case errors.As(err, &authErr):
		return authErr.code
	case errors.Is(err, pairing.ErrCredentialMissing):
		return credentialMissing
	case errors.Is(err, pairing.ErrCredentialInvalid):
		return credentialInvalid
	case errors.Is(err, pairing.ErrCredentialExpired):
		return credentialExpired
	case errors.Is(err, pairing.ErrCredentialGraceExpired):
		return credentialGraceExpired
	case errors.Is(err, pairing.ErrCredentialScope):
		return credentialScope
	}
	return ""
}

// writeAuthError writes an auth rejection with its code and the gateway's
// clock, so a client whose proof failed on clock skew can correct its offset
// from this response and retry.
func (s *Server) writeAuthError(w http.ResponseWriter, status int, err error) {
	code := authErrorCode(err)
	s.logger.Warn("auth rejected", "code", code, "status", status, "error", err)
	now := s.now().UTC()
	writeJSON(w, status, errorResponse{Error: err.Error(), Code: code, ServerTime: &now})
}

func (s *Server) handleAuthRefresh(w http.ResponseWriter, r *http.Request) {
	rawToken := bearerToken(r)
	credential, err := s.pairing.LookupCredentialByToken(rawToken)
	if err != nil {
		s.writeAuthError(w, http.StatusUnauthorized, err)
		return
	}
	if strings.TrimSpace(credential.ProofPublicKey) == "" && !s.cfg.AllowLegacyBearerCredentials {
		s.writeAuthError(w, http.StatusUnauthorized, errLegacyCredentialsDisabled)
		return
	}
	// A credential may be past its bearer-validity gate (soft-expired) and still
	// refresh within the grace window; proof-of-possession authorizes that path.
	if err := s.verifyCredentialProof(r, rawToken, credential); err != nil {
		s.writeAuthError(w, http.StatusUnauthorized, err)
		return
	}
	refreshed, err := s.pairing.RefreshCredential(rawToken)
	if err != nil {
		switch {
		case errors.Is(err, pairing.ErrCredentialMissing), errors.Is(err, pairing.ErrCredentialInvalid), errors.Is(err, pairing.ErrCredentialExpired), errors.Is(err, pairing.ErrCredentialGraceExpired):
			s.writeAuthError(w, http.StatusUnauthorized, err)
		default:
			writeError(w, http.StatusInternalServerError, "failed to refresh gateway credential")
		}
		return
	}
	writeJSON(w, http.StatusOK, pairCompleteResponse{
		DeviceID:   refreshed.DeviceID,
		DeviceName: refreshed.DeviceName,
		Token:      refreshed.Token,
		ExpiresAt:  refreshed.ExpiresAt,
		Scopes:     refreshed.Scopes,
		// Lets clients paired before pair/complete returned it backfill the id
		// their push deep-links resolve against.
		GatewayID: s.cfg.GatewayID,
	})
}

// requireGatewayCredential is the common auth gate for the control API. Pairing
// bootstrap and public status stay outside this path by design.
func (s *Server) requireGatewayCredential(w http.ResponseWriter, r *http.Request) (pairing.Credential, bool) {
	if r == nil {
		s.writeAuthError(w, http.StatusUnauthorized, pairing.ErrCredentialMissing)
		return pairing.Credential{}, false
	}

	rawToken := bearerToken(r)
	credential, err := s.pairing.ValidateCredential(rawToken)
	if err != nil {
		s.writeAuthError(w, http.StatusUnauthorized, err)
		return pairing.Credential{}, false
	}
	if strings.TrimSpace(credential.ProofPublicKey) == "" && !s.cfg.AllowLegacyBearerCredentials {
		s.writeAuthError(w, http.StatusUnauthorized, errLegacyCredentialsDisabled)
		return pairing.Credential{}, false
	}
	if err := s.verifyCredentialProof(r, rawToken, credential); err != nil {
		s.writeAuthError(w, http.StatusUnauthorized, err)
		return pairing.Credential{}, false
	}
	return credential, true
}

func (s *Server) requireGatewayScope(w http.ResponseWriter, r *http.Request, scope string) (pairing.Credential, bool) {
	credential, ok := s.requireGatewayCredential(w, r)
	if !ok {
		return pairing.Credential{}, false
	}
	if err := credential.RequireScope(scope); err != nil {
		s.writeAuthError(w, http.StatusForbidden, err)
		return pairing.Credential{}, false
	}
	return credential, true
}

func (s *Server) requireHostAccess(w http.ResponseWriter, r *http.Request) (pairing.Credential, bool) {
	credential, ok := s.requireGatewayScope(w, r, pairing.ScopeRead)
	if !ok {
		return credential, false
	}
	// Control permits agents to access host files and run commands. Git
	// can also consult executable configuration in a selected checkout.
	if err := credential.RequireScope(pairing.ScopeControl); err != nil {
		s.writeAuthError(w, http.StatusForbidden, err)
		return credential, false
	}
	return credential, true
}

// requireScope wraps a handler with the credential + scope gate, so the same
// handler can be registered on both the public mux (wrapped, device-authed) and
// the localhost admin mux (bare, trusted).
func (s *Server) requireScope(scope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.requireGatewayScope(w, r, scope); !ok {
			return
		}
		next(w, r)
	}
}

func bearerToken(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if auth == "" {
		return ""
	}

	prefix := "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(auth, prefix))
}

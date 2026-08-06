package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
)

func (s *Server) handleAuthRefresh(w http.ResponseWriter, r *http.Request) {
	rawToken := bearerToken(r)
	credential, err := s.pairing.LookupCredentialByToken(rawToken)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if strings.TrimSpace(credential.ProofPublicKey) == "" && !s.cfg.AllowLegacyBearerCredentials {
		writeError(w, http.StatusUnauthorized, "legacy bearer credentials are disabled")
		return
	}
	// A credential may be past its bearer-validity gate (soft-expired) and still
	// refresh within the grace window; proof-of-possession authorizes that path.
	if err := s.verifyCredentialProof(r, rawToken, credential); err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	refreshed, err := s.pairing.RefreshCredential(rawToken)
	if err != nil {
		switch {
		case errors.Is(err, pairing.ErrCredentialMissing), errors.Is(err, pairing.ErrCredentialInvalid), errors.Is(err, pairing.ErrCredentialExpired), errors.Is(err, pairing.ErrCredentialGraceExpired):
			writeError(w, http.StatusUnauthorized, err.Error())
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
	})
}

// requireGatewayCredential is the common auth gate for the control API. Pairing
// bootstrap and public status stay outside this path by design.
func (s *Server) requireGatewayCredential(w http.ResponseWriter, r *http.Request) (pairing.Credential, bool) {
	if r == nil {
		writeError(w, http.StatusUnauthorized, pairing.ErrCredentialMissing.Error())
		return pairing.Credential{}, false
	}

	rawToken := bearerToken(r)
	credential, err := s.pairing.ValidateCredential(rawToken)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return pairing.Credential{}, false
	}
	if strings.TrimSpace(credential.ProofPublicKey) == "" && !s.cfg.AllowLegacyBearerCredentials {
		writeError(w, http.StatusUnauthorized, "legacy bearer credentials are disabled")
		return pairing.Credential{}, false
	}
	if err := s.verifyCredentialProof(r, rawToken, credential); err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
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
		writeError(w, http.StatusForbidden, err.Error())
		return pairing.Credential{}, false
	}
	return credential, true
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

package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/push"
)

// registerPushTokenRequest is the device's push-token registration body. The
// device identity is taken from the authenticated credential, never the body.
// A Web Push client sends its subscription (PushSubscription.toJSON() shape);
// any other platform sends an opaque token.
type registerPushTokenRequest struct {
	Token        string             `json:"token"`
	Platform     string             `json:"platform"`
	Subscription *push.Subscription `json:"subscription"`
}

// pushConfigResponse tells a client what it needs to subscribe for pushes.
type pushConfigResponse struct {
	// VAPIDPublicKey is the base64url, uncompressed P-256 key the client's
	// subscription must be created with.
	VAPIDPublicKey string `json:"vapidPublicKey"`
}

func (s *Server) handlePushConfig(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayCredential(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, pushConfigResponse{VAPIDPublicKey: s.cfg.VAPIDPublicKey})
}

// handleRegisterPushToken upserts the calling device's push token. It is
// idempotent: the client re-POSTs the same token across restarts and whenever the
// token rotates, once per paired gateway. The platform is stored as the routing
// key the push dispatcher uses to select a delivery provider.
func (s *Server) handleRegisterPushToken(w http.ResponseWriter, r *http.Request) {
	credential, ok := s.requireGatewayCredential(w, r)
	if !ok {
		return
	}

	var body registerPushTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	err := s.pushSvc.Register(r.Context(), credential.DeviceID, push.Registration{
		Token: body.Token, Platform: body.Platform, Subscription: body.Subscription,
	})
	if err != nil {
		var invalid push.RegistrationError
		if errors.As(err, &invalid) {
			http.Error(w, invalid.Error(), http.StatusBadRequest)
			return
		}
		s.logger.Error("failed to register push token", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

package api

import (
	"encoding/json"
	"net/http"
	"strings"
)

// registerPushTokenRequest is the device's push-token registration body. The
// device identity is taken from the authenticated credential, never the body —
// the body carries only the token and the platform it was issued for.
type registerPushTokenRequest struct {
	Token    string `json:"token"`
	Platform string `json:"platform"`
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
	token := strings.TrimSpace(body.Token)
	if token == "" {
		http.Error(w, "token is required", http.StatusBadRequest)
		return
	}
	// platform is part of the contract but default it rather than reject: a
	// well-formed token we can store should never 4xx (the client retries 4xx
	// indefinitely). Older/other clients that omit it are treated as Android.
	platform := strings.TrimSpace(body.Platform)
	if platform == "" {
		s.logger.Warn("push token registered with empty platform, defaulting to android; client may be outdated",
			"device_id", credential.DeviceID)
		platform = "android"
	}

	if err := s.store.SaveDevicePushToken(r.Context(), credential.DeviceID, token, platform); err != nil {
		s.logger.Error("failed to save push token", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

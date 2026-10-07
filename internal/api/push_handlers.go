package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

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
	if body.Subscription != nil {
		s.registerWebPushSubscription(w, r, credential.DeviceID, *body.Subscription)
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

// registerWebPushSubscription stores a Web Push subscription as the device's
// token. The endpoint is where the gateway will POST, so it must be https.
func (s *Server) registerWebPushSubscription(w http.ResponseWriter, r *http.Request, deviceID string, sub push.Subscription) {
	endpoint, err := url.Parse(sub.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" {
		http.Error(w, "subscription.endpoint must be an https URL", http.StatusBadRequest)
		return
	}
	if sub.Keys.P256dh == "" || sub.Keys.Auth == "" {
		http.Error(w, "subscription.keys.p256dh and subscription.keys.auth are required", http.StatusBadRequest)
		return
	}
	token, err := json.Marshal(sub)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.store.SaveDevicePushToken(r.Context(), deviceID, string(token), push.PlatformWebPush); err != nil {
		s.logger.Error("failed to save push subscription", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

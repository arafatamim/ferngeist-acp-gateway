package api

import (
	"encoding/json"
	"net/http"
)

type registerFCMRequest struct {
	FCMToken string `json:"fcmToken"`
}

func (s *Server) handleRegisterFCMToken(w http.ResponseWriter, r *http.Request) {
	credential, ok := s.requireGatewayCredential(w, r)
	if !ok {
		return
	}

	var body registerFCMRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.FCMToken == "" {
		http.Error(w, "fcmToken is required", http.StatusBadRequest)
		return
	}

	if err := s.store.SaveFCMToken(r.Context(), credential.DeviceID, body.FCMToken); err != nil {
		s.logger.Error("failed to save FCM token", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

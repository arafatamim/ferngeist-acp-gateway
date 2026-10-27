package api

import (
	"net/http"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/session"
)

// turnsResponse is the GET /v1/turns body. GatewayStartedAt marks the daemon
// lifetime the outcomes belong to: they do not survive a restart.
type turnsResponse struct {
	GatewayStartedAt time.Time `json:"gatewayStartedAt"`
	session.TurnSnapshot
}

// handleTurns serves the cross-device status of every known turn from memory;
// it never touches an agent process or the listing path.
func (s *Server) handleTurns(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeRead); !ok {
		return
	}
	if s.sessionSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "session service not available")
		return
	}
	writeJSON(w, http.StatusOK, turnsResponse{GatewayStartedAt: s.startedAt, TurnSnapshot: s.sessionSvc.TurnSnapshot()})
}

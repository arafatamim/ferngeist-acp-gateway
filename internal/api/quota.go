package api

import (
	"net/http"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
)

func (s *Server) handleQuotas(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeRead); !ok {
		return
	}
	if s.quotas == nil {
		writeError(w, http.StatusServiceUnavailable, "quotas unavailable")
		return
	}
	data, err := s.quotas(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "quotas unavailable")
		return
	}
	writeJSON(w, http.StatusOK, data)
}

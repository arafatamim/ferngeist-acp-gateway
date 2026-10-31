package api

import (
	"errors"
	"io/fs"
	"net/http"
	"os/exec"

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
		if errors.Is(err, exec.ErrNotFound) {
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "quotas unavailable", Code: "codexbar_not_found"})
			return
		}
		if errors.Is(err, fs.ErrInvalid) {
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "quotas unavailable", Code: "codexbar_invalid_response"})
			return
		}
		writeError(w, http.StatusServiceUnavailable, "quotas unavailable")
		return
	}
	writeJSON(w, http.StatusOK, data)
}

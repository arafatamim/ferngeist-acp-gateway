package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/catalog"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/customagents"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

// The custom-agent write policy (and its bounds) lives in customagents; the API
// documents the same limits, so it re-states the two it advertises.
const (
	maxCustomAgents  = customagents.MaxAgents
	maxCustomNameLen = customagents.MaxNameLen
)

// customAgentRequest is the client-supplied shape of a custom agent. Everything
// else (id, protocol, launch policy) is derived server-side.
type customAgentRequest struct {
	DisplayName string   `json:"displayName"`
	Command     string   `json:"command"`
	Args        []string `json:"args,omitempty"`
	Hint        string   `json:"hint,omitempty"`
}

type customAgentDeleteResponse struct {
	Deleted string `json:"deleted"`
}

func (s *Server) handleCustomAgentCreate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeControl); !ok {
		return
	}
	request, ok := decodeCustomAgentRequest(w, r)
	if !ok {
		return
	}
	agent, err := s.customAgents().Create(r.Context(), customagents.Create{
		DisplayName: request.DisplayName,
		Command:     request.Command,
		Args:        request.Args,
		Hint:        request.Hint,
	})
	if err != nil {
		writeCustomAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, agent)
}

// handleCustomAgentUpdate edits an existing custom agent in place. The id is
// immutable: it was derived from the original display name and clients key
// stored sessions/runtimes by it.
func (s *Server) handleCustomAgentUpdate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeControl); !ok {
		return
	}
	request, ok := decodeCustomAgentRequest(w, r)
	if !ok {
		return
	}
	var update customagents.Update
	// Empty displayName/command means "unchanged"; nil args means "unchanged",
	// an empty slice clears them; hint is always overwritten.
	if trimmed := strings.TrimSpace(request.DisplayName); trimmed != "" {
		update.DisplayName = &trimmed
	}
	if trimmed := strings.TrimSpace(request.Command); trimmed != "" {
		update.Command = &trimmed
	}
	if request.Args != nil {
		update.Args = request.Args
		update.ArgsSet = true
	}
	hint := request.Hint
	update.Hint = &hint

	agent, err := s.customAgents().Update(r.Context(), r.PathValue("id"), update)
	if err != nil {
		writeCustomAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

func (s *Server) handleCustomAgentDelete(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeControl); !ok {
		return
	}
	id := r.PathValue("id")
	if err := s.customAgents().Delete(r.Context(), id); err != nil {
		writeCustomAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, customAgentDeleteResponse{Deleted: id})
}

func decodeCustomAgentRequest(w http.ResponseWriter, r *http.Request) (customAgentRequest, bool) {
	var request customAgentRequest
	r.Body = http.MaxBytesReader(w, r.Body, jsonBodyLimit)
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return customAgentRequest{}, false
	}
	return request, true
}

func writeCustomAgentError(w http.ResponseWriter, err error) {
	var serviceError customagents.Error
	switch {
	case errors.As(err, &serviceError):
		writeError(w, serviceError.StatusCode(), serviceError.Error())
	case errors.Is(err, storage.ErrNotFound):
		writeError(w, http.StatusNotFound, "unknown custom agent")
	case errors.Is(err, catalog.ErrAgentNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "custom agent operation failed")
	}
}

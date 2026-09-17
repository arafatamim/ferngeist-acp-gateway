package api

import (
	"net/http"
	"strings"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/customagents"
)

// Admin transport for custom agents. The admin API listens on localhost only,
// so these handlers carry no scope gate: they are trusted exactly like the
// other adminMux handlers (pairing, devices). They mirror the public handlers
// in custom_agents.go and share their decode, service and error mapping, so the
// status codes cannot drift between the two transports.

// handleAdminCustomAgentList returns the full catalog (embedded, registry and
// custom) in the same envelope /v1/agents uses, so clients parse one shape. It
// serves the catalog view without the live runtime merge: the host CLI lists
// what can be launched, not what is currently running.
func (s *Server) handleAdminCustomAgentList(w http.ResponseWriter, _ *http.Request) {
	agents := s.customAgents().List()
	response := make([]agentRuntimeState, 0, len(agents))
	for _, agent := range agents {
		response = append(response, agentRuntimeState{Agent: agent})
	}
	writeJSON(w, http.StatusOK, agentsResponse{Agents: response})
}

func (s *Server) handleAdminCustomAgentCreate(w http.ResponseWriter, r *http.Request) {
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

// handleAdminCustomAgentUpdate edits an existing custom agent in place. The id
// is immutable, so the CLI keeps addressing the same agent it created.
func (s *Server) handleAdminCustomAgentUpdate(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) handleAdminCustomAgentDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.customAgents().Delete(r.Context(), id); err != nil {
		writeCustomAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, customAgentDeleteResponse{Deleted: id})
}

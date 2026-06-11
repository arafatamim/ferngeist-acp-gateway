package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/catalog"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
)

// agentsResponse wraps the list of agents with their live runtime state.
type agentsResponse struct {
	Agents []agentRuntimeState `json:"agents"`
}

// agentRuntimeState merges static catalog metadata with live runtime status
// so clients can determine which agents are currently running.
type agentRuntimeState struct {
	catalog.Agent
	Running       bool   `json:"running"`
	RuntimeID     string `json:"runtimeId,omitempty"`
	RuntimeStatus string `json:"runtimeStatus,omitempty"`
}

// runtimesResponse wraps the list of managed ACP runtimes.
type runtimesResponse struct {
	Runtimes []runtime.Runtime `json:"runtimes"`
}

// runtimeStartResponse is returned when an agent runtime is successfully started.
type runtimeStartResponse struct {
	Runtime runtime.Runtime `json:"runtime"`
}

// runtimeStopResponse is returned when an agent runtime is successfully stopped.
type runtimeStopResponse struct {
	Runtime runtime.Runtime `json:"runtime"`
}

// runtimeConnectResponse provides the WebSocket connection details and short-lived
// bearer token for establishing an ACP session with a running runtime.
type runtimeConnectResponse struct {
	RuntimeID      string    `json:"runtimeId"`
	Protocol       string    `json:"protocol"`
	Scheme         string    `json:"scheme"`
	Host           string    `json:"host"`
	WebSocketURL   string    `json:"websocketUrl"`
	WebSocketPath  string    `json:"websocketPath"`
	BearerToken    string    `json:"bearerToken"`
	TokenExpiresAt time.Time `json:"tokenExpiresAt"`
	SessionID      string    `json:"sessionId,omitempty"`
	AttachToken    string    `json:"attachToken,omitempty"`
}

// runtimeRestartRequest optionally carries environment variable overrides
// for the restarted runtime.
type runtimeRestartRequest struct {
	Env map[string]string `json:"env"`
}

// runtimeLogsResponse contains the buffered log entries for a specific runtime.
type runtimeLogsResponse struct {
	RuntimeID string             `json:"runtimeId"`
	Logs      []runtime.LogEntry `json:"logs"`
}

// runtimeCounts aggregates the current state of all managed runtimes.
type runtimeCounts struct {
	Starting    int `json:"starting"`
	Total       int `json:"total"`
	Running     int `json:"running"`
	Stopping    int `json:"stopping"`
	Stopped     int `json:"stopped"`
	Failed      int `json:"failed"`
	CircuitOpen int `json:"circuitOpen"` // circuit breaker tripped, no new starts attempted
}

// handleAgents merges static catalog data with live runtime state so clients do
// not need to reconstruct gateway policy from multiple endpoints.
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeRead); !ok {
		return
	}

	runtimes := s.runtime.List()
	runtimeByAgent := make(map[string]runtime.Runtime, len(runtimes))
	for _, runtimeInfo := range runtimes {
		runtimeByAgent[runtimeInfo.AgentID] = runtimeInfo
	}

	agents := s.catalog.List()
	response := make([]agentRuntimeState, 0, len(agents))
	for _, agent := range agents {
		state := agentRuntimeState{Agent: agent}
		if runtimeInfo, ok := runtimeByAgent[agent.ID]; ok {
			state.Running = runtimeInfo.Status == "running"
			state.RuntimeID = runtimeInfo.ID
			state.RuntimeStatus = runtimeInfo.Status
		}
		response = append(response, state)
	}

	writeJSON(w, http.StatusOK, agentsResponse{Agents: response})
}

func (s *Server) handleRuntimes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeRead); !ok {
		return
	}

	writeJSON(w, http.StatusOK, runtimesResponse{Runtimes: s.runtime.List()})
}

func (s *Server) handleRuntimeLogs(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeRead); !ok {
		return
	}

	runtimeID := r.PathValue("runtimeId")
	logs, err := s.runtime.Logs(runtimeID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, runtimeLogsResponse{
		RuntimeID: runtimeID,
		Logs:      logs,
	})
}

func (s *Server) handleAgentStart(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeControl); !ok {
		return
	}

	agentID := r.PathValue("agentId")
	agent, err := s.catalog.Get(agentID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	runtimeInfo, err := s.runtime.Start(agent)
	if err != nil {
		switch {
		case errors.Is(err, runtime.ErrAgentNotDetected), errors.Is(err, runtime.ErrUnsupportedLaunch), errors.Is(err, runtime.ErrRemoteStartNotAllowed):
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, runtime.ErrExecutableNotFound):
			writeError(w, http.StatusFailedDependency, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "failed to start runtime")
		}
		return
	}
	writeJSON(w, http.StatusOK, runtimeStartResponse{Runtime: runtimeInfo})
}

func (s *Server) handleAgentStop(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeControl); !ok {
		return
	}

	agentID := r.PathValue("agentId")
	runtimeInfo, err := s.runtime.StopByAgentID(agentID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	s.gateway.Revoke(runtimeInfo.ID)
	writeJSON(w, http.StatusOK, runtimeStopResponse{Runtime: runtimeInfo})
}

// handleRuntimeConnect converts a running runtime into a short-lived ACP
// connection descriptor. The gateway token is registered here so the WebSocket
// endpoint can stay stateless apart from token validation.
func (s *Server) handleRuntimeConnect(w http.ResponseWriter, r *http.Request) {
	credential, ok := s.requireGatewayScope(w, r, pairing.ScopeControl)
	if !ok {
		return
	}

	runtimeID := r.PathValue("runtimeId")
	descriptor, err := s.runtime.Connect(runtimeID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	s.gateway.Register(descriptor)
	resp := connectResponseFromDescriptor(r, descriptor)

	// If the client requested a resilient session, attach the session ID and a
	// fresh attach token to the connect response. Reuse an existing session for
	// this runtime when one is still alive (the common reconnect-after-app-kill
	// case): the agent process and the runtime lease are still held by it, so
	// creating a second session would fail with ErrRuntimeLeaseHeld and leave the
	// client without session credentials.
	if r.Body != nil && r.ContentLength > 0 {
		var body struct {
			SessionMode string `json:"sessionMode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil && body.SessionMode == "resilient" && s.sessionSvc != nil {
			if existingID, ok := s.sessionSvc.FindReconnectableByRuntime(runtimeID, credential.DeviceID); ok {
				attachToken, err := s.sessionSvc.Resume(r.Context(), existingID, credential.DeviceID)
				if err != nil {
					s.logger.Warn("failed to resume existing resilient session", "error", err)
				} else {
					resp.SessionID = existingID
					resp.AttachToken = attachToken
				}
			} else {
				sess, attachToken, err := s.sessionSvc.Create(r.Context(), runtimeID, credential.DeviceID, descriptor.AgentID)
				if err != nil {
					s.logger.Warn("failed to create resilient session", "error", err)
				} else {
					resp.SessionID = sess.ID
					resp.AttachToken = attachToken
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleRuntimeRestart performs an atomic restart of a runtime: it stops the
// existing runtime (revoking its gateway token), starts a fresh instance with
// optional environment overrides, and issues a new connection descriptor.
// The env scope check ensures only credentials with runtimeRestartEnv scope
// can pass custom environment variables.
func (s *Server) handleRuntimeRestart(w http.ResponseWriter, r *http.Request) {
	credential, ok := s.requireGatewayScope(w, r, pairing.ScopeControl)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, jsonBodyLimit)
	runtimeID := r.PathValue("runtimeId")
	var request runtimeRestartRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	if len(request.Env) > 0 {
		if err := credential.RequireScope(pairing.ScopeRuntimeRestartEnv); err != nil {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
	}

	restarted, err := s.runtime.Restart(runtimeID, request.Env)
	if err != nil {
		s.gateway.Revoke(runtimeID)
		s.runtime.AppendLog(runtimeID, "gateway", "runtime restart failed: "+err.Error())
		switch {
		case errors.Is(err, runtime.ErrRuntimeNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, runtime.ErrRuntimeNotRunning):
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, runtime.ErrExecutableNotFound):
			writeError(w, http.StatusFailedDependency, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "failed to restart runtime")
		}
		return
	}

	s.gateway.Revoke(runtimeID)
	descriptor, err := s.runtime.Connect(restarted.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "restarted runtime is not connectable")
		return
	}
	s.gateway.Register(descriptor)
	writeJSON(w, http.StatusOK, connectResponseFromDescriptor(r, descriptor))
}

func connectResponseFromDescriptor(r *http.Request, descriptor runtime.ConnectDescriptor) runtimeConnectResponse {
	return runtimeConnectResponse{
		RuntimeID:      descriptor.RuntimeID,
		Protocol:       descriptor.Protocol,
		Scheme:         websocketScheme(r),
		Host:           websocketHostWithPath(r, descriptor.WebSocketPath),
		WebSocketURL:   absoluteWebSocketURL(r, descriptor.WebSocketPath),
		WebSocketPath:  descriptor.WebSocketPath,
		BearerToken:    descriptor.BearerToken,
		TokenExpiresAt: descriptor.TokenExpiresAt,
	}
}

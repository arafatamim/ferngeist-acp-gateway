package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/catalog"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/session"
)

// agentsResponse wraps the list of agents with their live runtime state.
type agentsResponse struct {
	Agents []agentRuntimeState `json:"agents"`
}

// agentRuntimeState merges static catalog metadata with live runtime status
// so clients can determine which agents are currently running.
type agentRuntimeState struct {
	catalog.Agent
	Running       bool              `json:"running"`
	RuntimeID     string            `json:"runtimeId,omitempty"`
	RuntimeStatus string            `json:"runtimeStatus,omitempty"`
	Runtimes      []runtime.Runtime `json:"runtimes,omitempty"`
}

// runtimesResponse wraps the list of managed ACP runtimes.
type runtimesResponse struct {
	Runtimes []runtime.Runtime `json:"runtimes"`
}

// runtimeStopResponse is returned when an agent's runtimes are successfully stopped.
type runtimeStopResponse struct {
	Runtime runtime.Runtime `json:"runtime"`
}

// runtimeConnectResponse provides the WebSocket connection details for
// establishing an ACP session with a running runtime.
type runtimeConnectResponse struct {
	RuntimeID     string `json:"runtimeId"`
	Protocol      string `json:"protocol"`
	Scheme        string `json:"scheme"`
	Host          string `json:"host"`
	WebSocketURL  string `json:"websocketUrl"`
	WebSocketPath string `json:"websocketPath"`
	SessionID     string `json:"sessionId,omitempty"`
	AttachToken   string `json:"attachToken,omitempty"`
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
	Starting int `json:"starting"`
	Total    int `json:"total"`
	Running  int `json:"running"`
	Stopped  int `json:"stopped"`
	Failed   int `json:"failed"`
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
	runtimeByAgent := make(map[string][]runtime.Runtime, len(runtimes))
	for _, runtimeInfo := range runtimes {
		runtimeByAgent[runtimeInfo.AgentID] = append(runtimeByAgent[runtimeInfo.AgentID], runtimeInfo)
	}

	agents := s.catalog.List()
	response := make([]agentRuntimeState, 0, len(agents))
	for _, agent := range agents {
		state := agentRuntimeState{Agent: agent}
		agentRuntimes := runtimeByAgent[agent.ID]
		state.Runtimes = agentRuntimes
		if len(agentRuntimes) > 0 {
			newest := agentRuntimes[0]
			state.Running = newest.Status == "running"
			state.RuntimeID = newest.ID
			state.RuntimeStatus = newest.Status
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

// handleAgentConnect starts the selected agent and creates the resilient
// session that owns its process, returning the WebSocket connection details.
// Connecting starts the agent: there is no separate start step.
func (s *Server) handleAgentConnect(w http.ResponseWriter, r *http.Request) {
	credential, ok := s.requireGatewayScope(w, r, pairing.ScopeControl)
	if !ok {
		return
	}
	if s.sessionSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "session service not available")
		return
	}

	agent, err := s.catalog.Get(r.PathValue("agentId"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	sess, attachToken, err := s.sessionSvc.Create(r.Context(), agent, credential.DeviceID, nil)
	if err != nil {
		writeAgentLaunchError(w, err)
		return
	}

	descriptor, err := s.runtime.Connect(sess.RuntimeID)
	if err != nil {
		s.writeStartupExit(w, r, sess, credential.DeviceID)
		return
	}
	resp := connectResponseFromDescriptor(r, descriptor)
	resp.SessionID = sess.ID
	resp.AttachToken = attachToken
	writeJSON(w, http.StatusOK, resp)
}

// writeAgentLaunchError maps the session service's launch failures onto the
// agent-control HTTP contract.
func writeAgentLaunchError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runtime.ErrAgentNotDetected),
		errors.Is(err, runtime.ErrUnsupportedLaunch),
		errors.Is(err, runtime.ErrRemoteStartNotAllowed),
		errors.Is(err, session.ErrSessionLimitReached):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, runtime.ErrExecutableNotFound):
		writeError(w, http.StatusFailedDependency, err.Error())
	case errors.Is(err, session.ErrSessionNotActive):
		writeError(w, http.StatusServiceUnavailable, "session service is shutting down")
	default:
		writeError(w, http.StatusInternalServerError, "failed to start agent")
	}
}

// writeStartupExit reports an agent that exited before its connect response
// was built, with the exit error when there was one. The exit watcher may
// already have reclaimed the session; otherwise it is closed here.
func (s *Server) writeStartupExit(w http.ResponseWriter, r *http.Request, sess *session.Session, deviceID string) {
	_ = s.sessionSvc.Close(r.Context(), sess.ID, deviceID)
	message := "agent exited during startup"
	if rt, err := s.runtime.Get(sess.RuntimeID); err == nil && rt.LastError != "" {
		message += ": " + rt.LastError
	}
	writeError(w, http.StatusBadGateway, message)
}

// handleAgentStop stops every runtime the calling device has for the agent
// (each is owned by one session) and reports the newest stopped runtime.
func (s *Server) handleAgentStop(w http.ResponseWriter, r *http.Request) {
	credential, ok := s.requireGatewayScope(w, r, pairing.ScopeControl)
	if !ok {
		return
	}
	if s.sessionSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "session service not available")
		return
	}

	runtimeInfo, err := s.sessionSvc.StopByAgent(r.Context(), r.PathValue("agentId"), credential.DeviceID)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			writeError(w, http.StatusNotFound, "no runtimes for agent")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to stop agent")
		return
	}

	writeJSON(w, http.StatusOK, runtimeStopResponse{Runtime: runtimeInfo})
}

// handleRuntimeRestart restarts an agent: it launches a fresh agent process and
// session with the requested environment, then closes the session that owned
// the runtime. Only the device that owns the runtime may restart it. The env
// scope check ensures only credentials with runtimeRestartEnv scope can pass
// custom environment variables.
func (s *Server) handleRuntimeRestart(w http.ResponseWriter, r *http.Request) {
	credential, ok := s.requireGatewayScope(w, r, pairing.ScopeControl)
	if !ok {
		return
	}
	if s.sessionSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "session service not available")
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
			s.writeAuthError(w, http.StatusForbidden, err)
			return
		}
	}

	runtimeInfo, err := s.runtime.Get(runtimeID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if runtimeInfo.Status != runtime.StatusRunning {
		writeError(w, http.StatusConflict, runtime.ErrRuntimeNotRunning.Error())
		return
	}
	agent, err := s.catalog.Get(runtimeInfo.AgentID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	sess, attachToken, err := s.sessionSvc.Restart(r.Context(), runtimeID, credential.DeviceID, agent, request.Env)
	switch {
	case errors.Is(err, session.ErrSessionNotFound):
		writeError(w, http.StatusConflict, "runtime has no session to restart")
		return
	case errors.Is(err, session.ErrDeviceMismatch):
		writeError(w, http.StatusForbidden, "runtime belongs to another device")
		return
	case err != nil:
		writeAgentLaunchError(w, err)
		return
	}

	descriptor, err := s.runtime.Connect(sess.RuntimeID)
	if err != nil {
		s.writeStartupExit(w, r, sess, credential.DeviceID)
		return
	}
	resp := connectResponseFromDescriptor(r, descriptor)
	resp.SessionID = sess.ID
	resp.AttachToken = attachToken
	writeJSON(w, http.StatusOK, resp)
}

func connectResponseFromDescriptor(r *http.Request, descriptor runtime.ConnectDescriptor) runtimeConnectResponse {
	return runtimeConnectResponse{
		RuntimeID:     descriptor.RuntimeID,
		Protocol:      descriptor.Protocol,
		Scheme:        websocketScheme(r),
		Host:          websocketHostWithPath(r, descriptor.WebSocketPath),
		WebSocketURL:  absoluteWebSocketURL(r, descriptor.WebSocketPath),
		WebSocketPath: descriptor.WebSocketPath,
	}
}

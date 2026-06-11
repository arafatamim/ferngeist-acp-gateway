package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/session"
	"github.com/coder/websocket"
)

func (s *Server) registerSessionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/sessions/{sessionId}/resume", s.handleSessionResume)
	mux.HandleFunc("GET /v1/sessions", s.handleSessionList)
	mux.HandleFunc("DELETE /v1/sessions/{sessionId}", s.handleSessionClose)
	mux.HandleFunc("GET /v1/acp/{runtimeId}", s.handleACPWebSocket)
}

// sessionResumeResponse is returned by POST /v1/sessions/{id}/resume.
// AttachToken is a single-use token the client passes as a query param on
// the WebSocket reconnect.
type sessionResumeResponse struct {
	AttachToken string `json:"attachToken"`
}

// sessionListResponse is returned by GET /v1/sessions. Lists all sessions
// owned by the authenticated device.
type sessionListResponse struct {
	Sessions []session.SessionSummary `json:"sessions"`
}

// handleSessionResume mints a new attach token for reconnecting to an existing
// session. Validates that the session belongs to the authenticated device and
// is in a reconnectable state (active or disconnected).
func (s *Server) handleSessionResume(w http.ResponseWriter, r *http.Request) {
	credential, ok := s.requireGatewayCredential(w, r)
	if !ok {
		return
	}
	if s.sessionSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "session service not available")
		return
	}
	sessionID := r.PathValue("sessionId")
	attachToken, err := s.sessionSvc.Resume(r.Context(), sessionID, credential.DeviceID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sessionResumeResponse{AttachToken: attachToken})
}

// handleSessionList returns all sessions owned by the authenticated device.
func (s *Server) handleSessionList(w http.ResponseWriter, r *http.Request) {
	credential, ok := s.requireGatewayCredential(w, r)
	if !ok {
		return
	}
	if s.sessionSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "session service not available")
		return
	}
	sessions, err := s.sessionSvc.ListByDevice(r.Context(), credential.DeviceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sessionListResponse{Sessions: sessions})
}

// handleSessionClose terminates a session and its backing runtime. Validates
// that the session belongs to the authenticated device before closing.
func (s *Server) handleSessionClose(w http.ResponseWriter, r *http.Request) {
	credential, ok := s.requireGatewayCredential(w, r)
	if !ok {
		return
	}
	if s.sessionSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "session service not available")
		return
	}
	sessionID := r.PathValue("sessionId")
	if err := s.sessionSvc.Close(r.Context(), sessionID, credential.DeviceID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleACPWebSocket handles resilient session WebSocket connections.
// Clients must supply ?sessionId=<id>&attachToken=<token> query parameters,
// obtained via POST /v1/runtimes/{id}/connect with sessionMode:"resilient".
func (s *Server) handleACPWebSocket(w http.ResponseWriter, r *http.Request) {
	runtimeID := r.PathValue("runtimeId")
	sessionID := r.URL.Query().Get("sessionId")
	attachToken := r.URL.Query().Get("attachToken")

	if sessionID == "" || attachToken == "" {
		writeError(w, http.StatusBadRequest, "sessionId and attachToken query parameters are required")
		return
	}

	if s.sessionSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "session service not available")
		return
	}

	s.handleSessionWebSocket(w, r, runtimeID, sessionID, attachToken)
}

// handleSessionWebSocket handles reconnection to an existing resilient session.
// Flow: validate attach token (taking over any stale connection) → upgrade to
// WebSocket → bind pump client → proxy. A new valid attach always supersedes the
// previous connection rather than being rejected, so a client whose socket died
// (e.g. the app was killed) can always reconnect. The client is responsible for
// calling session/load on the agent for context restoration after reconnection.
func (s *Server) handleSessionWebSocket(w http.ResponseWriter, r *http.Request, runtimeID, sessionID, attachToken string) {
	runtimeIDResult, gen, err := s.sessionSvc.AttachClient(r.Context(), sessionID, attachToken)
	if err != nil {
		s.logger.Warn("attach client failed", "error", err)
		if errors.Is(err, session.ErrAttachTokenInvalid) {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	if runtimeIDResult != runtimeID {
		s.sessionSvc.DetachClient(sessionID, gen)
		http.Error(w, "runtime ID does not match session", http.StatusBadRequest)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		s.logger.Warn("websocket accept failed", "error", err)
		s.sessionSvc.DetachClient(sessionID, gen)
		return
	}
	defer conn.CloseNow()
	defer s.sessionSvc.DetachClient(sessionID, gen)
	conn.SetReadLimit(acpWebSocketReadLimit)

	// Bind this connection to the session pump. If a newer attach raced ahead and
	// already superseded us, bail out and let the deferred (no-op) detach clean up.
	if !s.sessionSvc.BindConn(sessionID, conn, gen) {
		return
	}

	pump, err := s.sessionSvc.GetPump(sessionID)
	if err != nil {
		s.logger.Warn("pump not found", "error", err)
		return
	}

	// Detect a dead peer that never sent a close frame (half-open socket): ping
	// periodically and close the conn on failure so the read loop unblocks and
	// the session is released instead of lingering as falsely "connected".
	pingCtx, stopPing := context.WithCancel(context.Background())
	defer stopPing()
	go keepAliveWebSocket(pingCtx, conn, s.logger)

	// Intercept a duplicate `initialize` from a reconnecting client: the agent is
	// already initialized, so replay the cached response instead of forwarding a
	// second handshake that a strict agent may reject by exiting.
	writeToAgent := func(payload []byte) error {
		if pump.MaybeReplayInitialize(payload) {
			return nil
		}
		return pump.WriteToAgent(payload)
	}

	done := make(chan error, 1)
	go proxyWebSocketToStdio(conn, writeToAgent, func() {}, runtimeID, s.runtime.AppendLog,
		func(payload []byte) {
			s.sessionSvc.LogInbound(sessionID, string(payload))
		}, done)

	<-done
}

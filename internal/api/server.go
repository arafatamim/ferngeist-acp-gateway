// Package api provides the HTTP control plane and ACP bridge for the Ferngeist
// gateway daemon. It exposes two servers:
//
//   - A public API for paired devices (pairing, agent control, ACP WebSocket)
//   - An admin API bound to localhost for local management and diagnostics
//
// The API stays intentionally small: a handful of control endpoints and a
// single ACP WebSocket path that bridges client connections to stdio-based
// ACP agent processes.
package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/catalog"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/config"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/customagents"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/discovery"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/logging"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/projects"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/push"
	acpregistry "github.com/arafatamim/ferngeist-acp-gateway/internal/registry"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/remote"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/session"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/workspace"
)

// Server is the main HTTP server that wires together all gateway subsystems into
// a unified control plane. It manages two HTTP servers:
//   - httpServer: the public-facing API for paired mobile/desktop clients
//   - adminServer: a localhost-bound admin interface for local management
//
// The server handles pairing orchestration, agent lifecycle management, ACP
// protocol bridging via WebSocket, and diagnostic export. All requests are
// logged with structured metadata for troubleshooting.
type Server struct {
	httpServer  *http.Server   // public API server
	adminServer *http.Server   // localhost admin server
	adminMux    *http.ServeMux // admin routes, before the local-request guard
	logger      *slog.Logger
	cfg         config.Config
	build       config.BuildInfo // version/commit info baked at compile time
	startedAt   time.Time        // used for uptime calculation
	now         func() time.Time // injectable clock for testability

	catalog    *catalog.Service
	runtime    *runtime.Supervisor
	sessionSvc *session.RuntimeSession
	projects   *projects.Lister
	pushSvc    *push.Dispatcher
	pairing    *pairing.Service
	discovery  *discovery.Service
	logs       *logging.Service
	registry   registryStatusProvider

	// customagentsSvc is the write policy behind /v1/agents/custom*, shared with
	// the admin API.
	customagentsSvc *customagents.Service

	// workspaceSvc owns the worktree and workspace git/filesystem operations.
	// It is composed by the daemon with persistence, clock and active chats.
	workspaceSvc *workspace.Service

	// remoteSetup, when set, returns the daemon's live remote-access
	// provisioning state (auth pending, setup blockers) for status snapshots.
	remoteSetup func() *remote.RemoteSetupSnapshot

	// publicURL is the live provisioned remote-access public URL, set when
	// asynchronous provisioning finishes after NewServer. Guards the
	// boot-time cfg.PublicBaseURL snapshot for status reporting.
	publicURL   string
	publicURLMu sync.Mutex

	rateLimiter *pairingRateLimiter    // protects pairing endpoints from abuse
	attempts    *pairingAttemptTracker // tracks failed pairing attempts for lockout
	proofNonces *proofNonceTracker     // prevents replay of credential proofs
}

// ProtocolVersion identifies the current gateway-to-client protocol version.
// Clients use this to detect compatibility mismatches. The daemon also emits it
// in its mDNS discovery TXT records, so bump both consumers together by
// referencing this constant.
//
// v2: connecting starts the agent — POST /v1/agents/{id}/connect replaces
// POST /v1/agents/{id}/start and POST /v1/runtimes/{id}/connect, whose
// responses the client must read. See docs/changelog.md.
const ProtocolVersion = "v2"

// Security and request limits.
const (
	// acpWebSocketReadLimit caps a single ACP message (client->agent) at 16 MiB.
	// ACP frames are newline-delimited JSON with no protocol-defined size bound;
	// client frames (initialize, session/load requests, prompts) are small, so
	// this is a DoS guard, not a functional limit. It was raised from 1 MiB
	// because inbound frames can legitimately carry large payloads and the old
	// value silently closed the socket (StatusMessageTooBig) with no log.
	acpWebSocketReadLimit    = 16 * 1024 * 1024
	acpWebSocketWriteTimeout = 30 * time.Second          // write deadline per WebSocket frame — keep in sync with session/pump.go:acpWebSocketWriteTimeout
	jsonBodyLimit            = int64(16 * 1024)          // max JSON request body size
	pairingMaxAttempts       = 5                         // failures before temporary lockout
	pairingLockoutWindow     = 2 * time.Minute           // cooldown period after max attempts
	pairingStartRefill       = 5 * time.Second           // token bucket refill interval for /pair/start
	pairingCompleteRefill    = 2 * time.Second           // token bucket refill interval for /pair/complete
	pairingBurstPerIP        = 5                         // burst allowance per source IP
	pairingBurstGlobal       = 30                        // global burst allowance across all IPs
	pairingBucketSweepEvery  = time.Minute               // amortized sweep interval for idle per-IP buckets
	pairingMaxIPBuckets      = 4096                      // hard cap per per-IP bucket map (flood bound)
	proofSkewWindow          = 5 * time.Minute           // allowed clock drift for proof timestamps
	proofReplayWindow        = 10 * time.Minute          // nonce validity window to prevent replay
	proofDomain              = "FERNGEIST-HTTP-PROOF-V1" // domain separator for proof signatures
)

// HTTP header names used for proof-of-possession credential verification.
const (
	proofHeaderTimestamp = "X-Ferngeist-Proof-Timestamp" // Unix timestamp of the proof
	proofHeaderNonce     = "X-Ferngeist-Proof-Nonce"     // random nonce to prevent replay
	proofHeaderSignature = "X-Ferngeist-Proof-Signature" // ECDSA signature over the proof message
)

// errorResponse is the standard JSON error envelope returned on API errors.
type errorResponse struct {
	Error string `json:"error"`
	// Code and ServerTime are set only on auth rejections; see writeAuthError.
	Code       string     `json:"code,omitempty"`
	ServerTime *time.Time `json:"serverTime,omitempty"`
}

// customAgents returns the custom-agent write policy shared with the admin API.
func (s *Server) customAgents() *customagents.Service {
	return s.customagentsSvc
}

// statusResponse is returned by the public /v1/status endpoint with a summary
// of gateway health, configuration, and runtime state.
type statusResponse struct {
	Name              string             `json:"name"`
	Version           string             `json:"version"`
	Build             config.BuildInfo   `json:"build"`
	ProtocolVersion   string             `json:"protocolVersion"`
	StartedAt         time.Time          `json:"startedAt"`
	ServerTime        time.Time          `json:"serverTime"` // lets clients offset proof timestamps by their clock skew
	UptimeSeconds     int64              `json:"uptimeSeconds"`
	ListenAddr        string             `json:"listenAddr"`
	LANEnabled        bool               `json:"lanEnabled"`
	PairedDeviceCount int                `json:"pairedDeviceCount"`
	Discovery         discovery.Snapshot `json:"discovery"`
	Remote            remoteStatus       `json:"remote"`
	Registry          acpregistry.Status `json:"registry"`
	RuntimeCounts     runtimeCounts      `json:"runtimeCounts"`
}

// remoteStatus describes the gateway's remote access configuration as detected
// from the PublicBaseURL setting (tailscale, cloudflare tunnel, manual proxy, etc).
type remoteStatus struct {
	Configured   bool   `json:"configured"`
	Mode         string `json:"mode,omitempty"`  // e.g. "tailscale", "cloudflare_tunnel", "lan_direct"
	Scope        string `json:"scope,omitempty"` // "public", "private", or "local"
	Healthy      bool   `json:"healthy"`
	Warning      string `json:"warning,omitempty"`
	PublicURL    string `json:"publicUrl,omitempty"`
	AuthRequired bool   `json:"authRequired,omitempty"`
	AuthURL      string `json:"authUrl,omitempty"`
}

// adminStatusResponse is the comprehensive status returned by the admin
// endpoint: the public status plus the admin listener address, pairing target
// info and active challenge state.
type adminStatusResponse struct {
	statusResponse
	AdminListenAddr string                 `json:"adminListenAddr"`
	PairingTarget   adminPairingTargetInfo `json:"pairingTarget"`
	ActivePairing   *adminPairingResponse  `json:"activePairing,omitempty"`
}

// adminPairingTargetInfo describes how a mobile client can reach this gateway
// for pairing (scheme + host), or why pairing is unavailable.
type adminPairingTargetInfo struct {
	Reachable bool   `json:"reachable"`
	Scheme    string `json:"scheme,omitempty"`
	Host      string `json:"host,omitempty"`
	Error     string `json:"error,omitempty"`
}

type registryStatusProvider interface {
	Status() acpregistry.Status
}

// Deps is the set of configuration and subsystems NewServer wires together.
type Deps struct {
	Config       config.Config
	Build        config.BuildInfo
	Logger       *slog.Logger
	Catalog      *catalog.Service
	Runtime      *runtime.Supervisor
	Pairing      *pairing.Service
	Discovery    *discovery.Service
	Logs         *logging.Service
	Registry     registryStatusProvider
	CustomAgents *customagents.Service
	Workspace    *workspace.Service
	Session      *session.RuntimeSession
	Projects     *projects.Lister
	Push         *push.Dispatcher
}

// NewServer wires the gateway's control plane and ACP bridge into one HTTP
// server. The API stays intentionally small: a handful of control endpoints and
// a single ACP WebSocket path.
func NewServer(deps Deps) *Server {
	cfg := normalizeSecurityConfig(deps.Config)
	server := &Server{
		logger:          deps.Logger.With("component", "api"),
		cfg:             cfg,
		build:           normalizeBuildInfo(deps.Build),
		startedAt:       time.Now().UTC(),
		now:             func() time.Time { return time.Now().UTC() },
		catalog:         deps.Catalog,
		runtime:         deps.Runtime,
		pairing:         deps.Pairing,
		discovery:       deps.Discovery,
		logs:            deps.Logs,
		registry:        deps.Registry,
		rateLimiter:     newPairingRateLimiter(cfg),
		attempts:        newPairingAttemptTracker(cfg),
		proofNonces:     newProofNonceTracker(),
		customagentsSvc: deps.CustomAgents,
		workspaceSvc:    deps.Workspace,
		sessionSvc:      deps.Session,
		projects:        deps.Projects,
		pushSvc:         deps.Push,
	}

	mux, adminMux := server.routes()

	adminAddr := strings.TrimSpace(cfg.AdminListenAddr)
	if adminAddr == "" {
		adminAddr = "127.0.0.1:0"
	}

	server.httpServer = &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           server.withRequestLogging(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	server.adminMux = adminMux
	server.adminServer = &http.Server{
		Addr:              adminAddr,
		Handler:           server.withRequestLogging(requireLocalAdminRequest(adminMux)),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	return server
}

func (s *Server) routes() (*http.ServeMux, *http.ServeMux) {
	mux := http.NewServeMux()
	adminMux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/status", s.handleStatus)
	mux.HandleFunc("/v1/agents", s.handleAgents)
	mux.HandleFunc("GET /v1/turns", s.handleTurns)
	mux.HandleFunc("GET /v1/diagnostics/summary", s.handleDiagnosticsSummary)
	mux.HandleFunc("GET /v1/diagnostics/export", s.handleDiagnosticsExport)
	mux.HandleFunc("POST /v1/auth/refresh", s.handleAuthRefresh)
	mux.HandleFunc("/v1/pair/start", s.handlePairStart)
	mux.HandleFunc("/v1/pair/complete", s.handlePairComplete)
	mux.HandleFunc("GET /v1/pair/status/{challengeId}", s.handlePairStatus)
	mux.HandleFunc("/v1/runtimes", s.handleRuntimes)
	mux.HandleFunc("GET /v1/runtimes/{runtimeId}/logs", s.handleRuntimeLogs)
	mux.HandleFunc("POST /v1/agents/custom", s.requireScope(pairing.ScopeControl, s.handleCustomAgentCreate))
	mux.HandleFunc("PUT /v1/agents/custom/{id}", s.requireScope(pairing.ScopeControl, s.handleCustomAgentUpdate))
	mux.HandleFunc("DELETE /v1/agents/custom/{id}", s.requireScope(pairing.ScopeControl, s.handleCustomAgentDelete))
	mux.HandleFunc("POST /v1/agents/{agentId}/connect", s.handleAgentConnect)
	mux.HandleFunc("POST /v1/agents/{agentId}/stop", s.handleAgentStop)
	mux.HandleFunc("POST /v1/runtimes/{runtimeId}/restart", s.handleRuntimeRestart)
	mux.HandleFunc("GET /v1/runtimes/{runtimeId}/files", s.handleWorkspaceFile)
	mux.HandleFunc("GET /v1/runtimes/{runtimeId}/files/list", s.handleWorkspaceFileList)
	mux.HandleFunc("GET /v1/runtimes/{runtimeId}/git/refs", s.handleWorkspaceGitRefs)
	mux.HandleFunc("GET /v1/runtimes/{runtimeId}/git/status", s.handleWorkspaceGitStatus)
	mux.HandleFunc("GET /v1/runtimes/{runtimeId}/git/diff", s.handleWorkspaceGitDiff)
	mux.HandleFunc("GET /v1/runtimes/{runtimeId}/git/log", s.handleWorkspaceGitLog)
	mux.HandleFunc("GET /v1/runtimes/{runtimeId}/git/commits/{sha}", s.handleWorkspaceGitCommit)
	mux.HandleFunc("POST /v1/worktrees", s.handleWorktreeCreate)
	mux.HandleFunc("GET /v1/worktrees", s.handleWorktreeList)
	mux.HandleFunc("GET /v1/worktrees/refs", s.handleWorktreeRefs)
	mux.HandleFunc("GET /v1/worktrees/{worktreeId}", s.handleWorktreeGet)
	mux.HandleFunc("DELETE /v1/worktrees/{worktreeId}", s.handleWorktreeDelete)
	mux.HandleFunc("POST /v1/worktrees/{worktreeId}/rebase", s.handleWorktreeRebase)
	mux.HandleFunc("POST /v1/worktrees/{worktreeId}/merge", s.handleWorktreeMerge)
	mux.HandleFunc("POST /v1/worktrees/{worktreeId}/push", s.handleWorktreePush)
	mux.HandleFunc("POST /v1/paths/missing", s.handlePathsMissing)
	s.registerSessionRoutes(mux)
	mux.HandleFunc("GET /v1/projects", s.handleProjects)
	mux.HandleFunc("POST /v1/devices/push-token", s.handleRegisterPushToken)
	mux.HandleFunc("GET /v1/devices/push-config", s.handlePushConfig)
	adminMux.HandleFunc("GET /admin/v1/status", s.handleAdminStatus)
	adminMux.HandleFunc("POST /admin/v1/pairings/start", s.handleAdminPairingStart)
	adminMux.HandleFunc("GET /admin/v1/pairings/{challengeId}", s.handleAdminPairingStatus)
	adminMux.HandleFunc("DELETE /admin/v1/pairings/{challengeId}", s.handleAdminPairingCancel)
	adminMux.HandleFunc("GET /admin/v1/devices", s.handleAdminDevices)
	adminMux.HandleFunc("DELETE /admin/v1/devices/{deviceId}", s.handleAdminDeviceRevoke)
	adminMux.HandleFunc("GET /admin/v1/agents", s.handleAdminCustomAgentList)
	adminMux.HandleFunc("POST /admin/v1/agents/custom", s.handleCustomAgentCreate)
	adminMux.HandleFunc("PUT /admin/v1/agents/custom/{id}", s.handleCustomAgentUpdate)
	adminMux.HandleFunc("DELETE /admin/v1/agents/custom/{id}", s.handleCustomAgentDelete)

	return mux, adminMux
}

// normalizeSecurityConfig applies safe security defaults based on whether the
// gateway is running in public mode.
//
// In public mode, proof-of-possession is enforced and legacy bearer-only
// credentials stay disabled. In private/local mode, legacy bearer credentials
// remain allowed unless the caller has explicitly chosen a stricter setup.
// normalizeSecurityConfig applies safe defaults when the gateway is exposed
// publicly. Public mode is detected from PublicBaseURL because that is the
// signal that the daemon is expected to be reachable beyond localhost.
func normalizeSecurityConfig(cfg config.Config) config.Config {
	publicMode := strings.TrimSpace(cfg.PublicBaseURL) != ""
	if publicMode {
		if !cfg.RequireProofOfPossession {
			cfg.RequireProofOfPossession = true
		}
		if !cfg.AllowLegacyBearerCredentials {
			cfg.AllowLegacyBearerCredentials = false
		}
		return cfg
	}
	if !cfg.RequireProofOfPossession && !cfg.AllowLegacyBearerCredentials {
		// Keep local development simple unless the caller opted into stricter auth.
		cfg.AllowLegacyBearerCredentials = true
	}
	return cfg
}

// ListenAndServe starts both the public and admin HTTP servers.
func (s *Server) ListenAndServe() error {
	s.workspaceSvc.Prune(context.Background())
	errCh := make(chan error, 2)
	go func() {
		s.logger.Info("api server listening", slog.String("addr", s.httpServer.Addr))
		errCh <- s.httpServer.ListenAndServe()
	}()
	go func() {
		s.logger.Info("admin api listening", slog.String("addr", s.adminServer.Addr))
		errCh <- s.adminServer.ListenAndServe()
	}()

	var firstErr error
	for i := 0; i < 2; i++ {
		err := <-errCh
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			continue
		}
		if firstErr == nil {
			firstErr = err
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = s.Shutdown(shutdownCtx)
			cancel()
		}
	}
	return firstErr
}

// Shutdown stops both HTTP servers, then the project lister's warm agents.
func (s *Server) Shutdown(ctx context.Context) error {
	err := errors.Join(s.httpServer.Shutdown(ctx), s.adminServer.Shutdown(ctx))
	// After the drain, so no listing parks a process once its pool is stopped.
	if s.projects != nil {
		s.projects.Close()
	}
	return err
}

// Handler returns the public API handler.
func (s *Server) Handler() http.Handler {
	return s.httpServer.Handler
}

// AdminHandler returns the admin API mux without the listener's request
// logging and local-request guard (requireLocalAdminRequest); tests that need
// the guard go through adminServer.Handler.
func (s *Server) AdminHandler() http.Handler {
	return s.adminMux
}

// requireLocalAdminRequest protects the unauthenticated admin API from web
// pages. A browser can reach 127.0.0.1 from any site, either by DNS rebinding
// (the request then carries the attacker's hostname in Host) or by a cross-site
// form/fetch POST (the request then carries an Origin header). The CLI client
// sends neither a hostname nor an Origin, so both are refused.
func requireLocalAdminRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" || !isIPOrLocalhost(r.Host) {
			writeError(w, http.StatusForbidden, "admin API only accepts local, non-browser requests")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isIPOrLocalhost reports whether a Host header names an IP literal or
// localhost. DNS rebinding needs a resolvable name, so any other hostname is
// refused.
func isIPOrLocalhost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return strings.EqualFold(host, "localhost") || net.ParseIP(host) != nil
}

// withRequestLogging records one structured log entry per HTTP request so the
// gateway can diagnose pairing, launch, and ACP handoff traffic from stdout or
// the rolling gateway log file.
func (s *Server) withRequestLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedAt := time.Now()
		wrapped := &loggingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(wrapped, r)
		s.logger.Info(
			"http request",
			slog.String("method", r.Method),
			slog.String("path", requestPath(r)),
			slog.Int("status", wrapped.statusCode),
			slog.Int("bytes", wrapped.bytesWritten),
			slog.Duration("duration", time.Since(startedAt)),
			slog.String("remote_addr", r.RemoteAddr),
		)
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode   int
	bytesWritten int
}

func (w *loggingResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *loggingResponseWriter) Write(p []byte) (int, error) {
	written, err := w.ResponseWriter.Write(p)
	w.bytesWritten += written
	return written, err
}

// Unwrap lets http.ResponseController reach the underlying connection.
func (w *loggingResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *loggingResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *loggingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	return hijacker.Hijack()
}

func (w *loggingResponseWriter) Push(target string, opts *http.PushOptions) error {
	pusher, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, opts)
}

// requestPath returns a log-safe request path and query string.
// Sensitive query values are redacted before logging.
func requestPath(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	if r.URL.RawQuery == "" {
		return r.URL.Path
	}
	return r.URL.Path + "?" + sanitizeRawQuery(r.URL.RawQuery)
}

// sanitizeRawQuery replaces sensitive values with a fixed placeholder so logs
// do not leak bearer tokens or other credentials.
func sanitizeRawQuery(rawQuery string) string {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return rawQuery
	}
	for key := range values {
		if isSensitiveQueryKey(key) {
			values.Set(key, "redacted")
		}
	}
	return values.Encode()
}

// isSensitiveQueryKey marks query parameters that should not be logged verbatim.
func isSensitiveQueryKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "access_token", "token", "authorization":
		return true
	default:
		return false
	}
}

// SetRemoteSetup wires a reader for the daemon's live remote-access
// provisioning state (auth pending, setup blockers). Nil-safe: status simply
// reports no pending setup when unset.
func (s *Server) SetRemoteSetup(remoteSetup func() *remote.RemoteSetupSnapshot) {
	s.remoteSetup = remoteSetup
}

// SetPublicURL sets the live remote-access public URL. Remote provisioning
// can finish asynchronously (interactive Tailscale login) after NewServer
// snapshotted the config, so remoteStatus must consult this live value rather
// than the boot-time cfg.PublicBaseURL. Thread-safe; empty clears it.
func (s *Server) SetPublicURL(publicURL string) {
	s.publicURLMu.Lock()
	defer s.publicURLMu.Unlock()
	s.publicURL = strings.TrimSpace(publicURL)
}

// publicBaseURL returns the effective public base URL: the live provisioned
// value when set, otherwise the boot-time config value.
func (s *Server) publicBaseURL() string {
	s.publicURLMu.Lock()
	defer s.publicURLMu.Unlock()
	if s.publicURL != "" {
		return s.publicURL
	}
	return strings.TrimSpace(s.cfg.PublicBaseURL)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, s.statusSnapshot(false))
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorResponse{Error: message})
}

func (s *Server) statusSnapshot(includePublicURL bool) statusResponse {
	summary := s.runtime.Summary()
	now := s.now()
	return statusResponse{
		Name:              s.gatewayDisplayName(),
		Version:           s.build.Version,
		Build:             s.build,
		ProtocolVersion:   ProtocolVersion,
		StartedAt:         s.startedAt,
		ServerTime:        now.UTC(),
		UptimeSeconds:     uptimeSeconds(s.startedAt, now),
		ListenAddr:        s.cfg.ListenAddr,
		LANEnabled:        s.cfg.EnableLAN,
		PairedDeviceCount: s.pairing.ActiveDeviceCount(),
		Discovery:         s.discovery.Snapshot(),
		Remote:            s.remoteStatus(includePublicURL),
		Registry:          s.registryStatus(),
		RuntimeCounts: runtimeCounts{
			Starting: summary.Starting,
			Total:    summary.Total,
			Running:  summary.Running,
			Stopped:  summary.Stopped,
			Failed:   summary.Failed,
		},
	}
}

func (s *Server) gatewayDisplayName() string {
	if name := strings.TrimSpace(s.cfg.GatewayName); name != "" {
		return name
	}
	return "ferngeist-gateway"
}

// remoteStatus builds a description of how this gateway is reachable from outside.
// It classifies the connection mode (tailscale, cloudflare tunnel, LAN, local)
// and detects the network scope (public vs private) for security warnings.
func (s *Server) remoteStatus(includePublicURL bool) remoteStatus {
	publicBaseURL := s.publicBaseURL()
	status := remoteStatus{
		Configured: publicBaseURL != "",
		Healthy:    true,
	}

	switch {
	case publicBaseURL != "":
		status.Mode, status.Scope, status.Warning, status.Healthy = classifyRemoteURL(publicBaseURL)
		if warning := s.remoteSecurityWarning(status.Scope); warning != "" {
			status.Warning = strings.TrimSpace(strings.Join([]string{status.Warning, warning}, "; "))
		}
		if includePublicURL {
			status.PublicURL = publicBaseURL
		}
	case s.cfg.EnableLAN:
		status.Mode = "lan_direct"
		status.Scope = "lan"
	default:
		status.Mode = "local_only"
		status.Scope = "local"
	}

	// Overlay the daemon's live provisioning state: an interactive Tailscale
	// login still pending, or a setup blocker. This is what lets `daemon
	// status` surface the login link instead of forcing a log-file dig.
	if s.remoteSetup != nil {
		if snap := s.remoteSetup(); snap != nil {
			if snap.AuthRequired {
				status.Mode = "tsnet"
				if s.cfg.TailscalePrivate {
					status.Scope = "private"
				} else {
					status.Scope = "public"
				}
				status.Healthy = false
				status.AuthRequired = true
				status.AuthURL = snap.AuthURL
			}
			if hint := strings.TrimSpace(snap.Hint); hint != "" {
				status.Warning = strings.TrimSpace(strings.Join([]string{status.Warning, hint}, "; "))
			}
			if lastErr := strings.TrimSpace(snap.LastErr); lastErr != "" {
				status.Warning = strings.TrimSpace(strings.Join([]string{status.Warning, "setup issue: " + lastErr}, "; "))
			}
		}
	}

	return status
}

// remoteSecurityWarning returns security warnings for the current remote mode.
// Public mode should require proof-of-possession and should not allow legacy
// bearer credentials.
func (s *Server) remoteSecurityWarning(scope string) string {
	if strings.TrimSpace(scope) != "public" {
		return ""
	}
	warnings := make([]string, 0, 2)
	if !s.cfg.RequireProofOfPossession {
		warnings = append(warnings, "public mode should require proof-of-possession pairing")
	}
	if s.cfg.AllowLegacyBearerCredentials {
		warnings = append(warnings, "legacy bearer credentials are still allowed")
	}
	return strings.Join(warnings, "; ")
}

// classifyRemoteURL inspects a public base URL to determine the remote access
// mode and network scope. It recognizes Tailscale (.ts.net), Cloudflare tunnels
// (.trycloudflare.com), and private hostnames (.local, .internal, .lan).
// It also flags non-HTTPS URLs as unhealthy.
func classifyRemoteURL(raw string) (mode, scope, warning string, healthy bool) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "configured", "unknown", "public base URL is invalid", false
	}

	host := strings.ToLower(parsed.Hostname())
	healthy = true

	switch {
	case strings.HasSuffix(host, ".ts.net") || strings.Contains(host, "tailscale"):
		mode = "tailscale"
		scope = "private"
	case strings.HasSuffix(host, ".trycloudflare.com") || strings.Contains(host, "cloudflare"):
		mode = "cloudflare_tunnel"
		scope = "public"
	case isPrivateHostname(host):
		mode = "manual_reverse_proxy"
		scope = "private"
	default:
		mode = "manual_reverse_proxy"
		scope = "public"
	}

	if parsed.Scheme != "https" {
		warning = "remote access should use HTTPS"
	}

	return mode, scope, warning, healthy
}

// isPrivateHostname returns true for hostnames that are clearly internal
// (localhost, .local, .internal, .lan suffixes).
func isPrivateHostname(host string) bool {
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	if strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".lan") {
		return true
	}
	return false
}

func normalizeBuildInfo(build config.BuildInfo) config.BuildInfo {
	if strings.TrimSpace(build.Version) == "" {
		build.Version = "dev"
	}
	return build
}

func uptimeSeconds(startedAt, now time.Time) int64 {
	if startedAt.IsZero() || now.Before(startedAt) {
		return 0
	}
	return int64(now.Sub(startedAt).Seconds())
}

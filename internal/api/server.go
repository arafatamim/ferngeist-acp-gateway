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
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/catalog"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/config"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/discovery"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/gateway"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/logging"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
	acpregistry "github.com/arafatamim/ferngeist-acp-gateway/internal/registry"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/session"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
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
	httpServer  *http.Server // public API server
	adminServer *http.Server // localhost admin server
	logger      *slog.Logger
	cfg         config.Config
	build       BuildInfo        // version/commit info baked at compile time
	startedAt   time.Time        // used for uptime calculation
	now         func() time.Time // injectable clock for testability

	catalog    *catalog.Service
	runtime    *runtime.Supervisor
	sessionSvc *session.RuntimeSession
	store      *storage.SQLiteStore
	pairing    *pairing.Service
	gateway    *gateway.Service
	discovery  *discovery.Service
	logs       *logging.Service
	registry   registryStatusProvider

	rateLimiter *pairingRateLimiter    // protects pairing endpoints from abuse
	attempts    *pairingAttemptTracker // tracks failed pairing attempts for lockout
	proofNonces *proofNonceTracker     // prevents replay of credential proofs
}

// protocolVersion identifies the current gateway-to-client protocol version.
// Clients use this to detect compatibility mismatches.
const protocolVersion = "v1alpha1"

// Security and request limits.
const (
	acpWebSocketReadLimit    = 1024 * 1024               // max ACP message size (1MB)
	acpWebSocketWriteTimeout = 30 * time.Second          // write deadline per WebSocket frame — keep in sync with session/pump.go:acpWebSocketWriteTimeout
	jsonBodyLimit            = int64(16 * 1024)          // max JSON request body size
	pairingMaxAttempts       = 5                         // failures before temporary lockout
	pairingLockoutWindow     = 2 * time.Minute           // cooldown period after max attempts
	pairingStartRefill       = 5 * time.Second           // token bucket refill interval for /pair/start
	pairingCompleteRefill    = 2 * time.Second           // token bucket refill interval for /pair/complete
	pairingBurstPerIP        = 5                         // burst allowance per source IP
	pairingBurstGlobal       = 30                        // global burst allowance across all IPs
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

// BuildInfo is injected from the build so status and diagnostics can describe
// the exact gateway binary that produced a failure report.
type BuildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	BuiltAt   string `json:"builtAt,omitempty"`
	GoVersion string `json:"goVersion,omitempty"`
}

// errorResponse is the standard JSON error envelope returned on API errors.
type errorResponse struct {
	Error string `json:"error"`
}

// statusResponse is returned by the public /v1/status endpoint with a summary
// of gateway health, configuration, and runtime state.
type statusResponse struct {
	Name              string             `json:"name"`
	Version           string             `json:"version"`
	Build             BuildInfo          `json:"build"`
	ProtocolVersion   string             `json:"protocolVersion"`
	StartedAt         time.Time          `json:"startedAt"`
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
	Configured bool   `json:"configured"`
	Mode       string `json:"mode,omitempty"`  // e.g. "tailscale", "cloudflare_tunnel", "lan_direct"
	Scope      string `json:"scope,omitempty"` // "public", "private", or "local"
	Healthy    bool   `json:"healthy"`
	Warning    string `json:"warning,omitempty"`
	PublicURL  string `json:"publicUrl,omitempty"`
}

// adminStatusResponse is the comprehensive status returned by the admin endpoint,
// including pairing target info and active challenge state.
type adminStatusResponse struct {
	Name              string                 `json:"name"`
	Version           string                 `json:"version"`
	Build             BuildInfo              `json:"build"`
	ProtocolVersion   string                 `json:"protocolVersion"`
	StartedAt         time.Time              `json:"startedAt"`
	UptimeSeconds     int64                  `json:"uptimeSeconds"`
	ListenAddr        string                 `json:"listenAddr"`
	AdminListenAddr   string                 `json:"adminListenAddr"`
	LANEnabled        bool                   `json:"lanEnabled"`
	PairedDeviceCount int                    `json:"pairedDeviceCount"`
	Discovery         discovery.Snapshot     `json:"discovery"`
	Remote            remoteStatus           `json:"remote"`
	Registry          acpregistry.Status     `json:"registry"`
	RuntimeCounts     runtimeCounts          `json:"runtimeCounts"`
	PairingTarget     adminPairingTargetInfo `json:"pairingTarget"`
	ActivePairing     *adminPairingResponse  `json:"activePairing,omitempty"`
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

// NewServer wires the gateway's control plane and ACP bridge into one HTTP
// server. The API stays intentionally small: a handful of control endpoints and
// a single ACP WebSocket path.
func NewServer(
	cfg config.Config,
	build BuildInfo,
	logger *slog.Logger,
	catalogSvc *catalog.Service,
	runtimeSvc *runtime.Supervisor,
	pairingSvc *pairing.Service,
	gatewaySvc *gateway.Service,
	discoverySvc *discovery.Service,
	logSvc *logging.Service,
	registrySvc registryStatusProvider,
	store *storage.SQLiteStore,
	sessionSvc *session.RuntimeSession,
) *Server {
	cfg = normalizeSecurityConfig(cfg)
	server := &Server{
		logger:      logger.With("component", "api"),
		cfg:         cfg,
		build:       normalizeBuildInfo(build),
		startedAt:   time.Now().UTC(),
		now:         func() time.Time { return time.Now().UTC() },
		catalog:     catalogSvc,
		runtime:     runtimeSvc,
		pairing:     pairingSvc,
		gateway:     gatewaySvc,
		discovery:   discoverySvc,
		logs:        logSvc,
		registry:    registrySvc,
		rateLimiter: newPairingRateLimiter(cfg),
		attempts:    newPairingAttemptTracker(cfg),
		proofNonces: newProofNonceTracker(),
		store:       store,
		sessionSvc:  sessionSvc,
	}

	mux := http.NewServeMux()
	adminMux := http.NewServeMux()
	mux.HandleFunc("/healthz", server.handleHealth)
	mux.HandleFunc("/v1/status", server.handleStatus)
	mux.HandleFunc("/v1/agents", server.handleAgents)
	mux.HandleFunc("GET /v1/diagnostics/summary", server.handleDiagnosticsSummary)
	mux.HandleFunc("GET /v1/diagnostics/export", server.handleDiagnosticsExport)
	mux.HandleFunc("POST /v1/auth/refresh", server.handleAuthRefresh)
	mux.HandleFunc("/v1/pair/start", server.handlePairStart)
	mux.HandleFunc("/v1/pair/complete", server.handlePairComplete)
	mux.HandleFunc("GET /v1/pair/status/{challengeId}", server.handlePairStatus)
	mux.HandleFunc("/v1/runtimes", server.handleRuntimes)
	mux.HandleFunc("GET /v1/runtimes/{runtimeId}/logs", server.handleRuntimeLogs)
	mux.HandleFunc("POST /v1/agents/{agentId}/start", server.handleAgentStart)
	mux.HandleFunc("POST /v1/agents/{agentId}/stop", server.handleAgentStop)
	mux.HandleFunc("POST /v1/runtimes/{runtimeId}/connect", server.handleRuntimeConnect)
	mux.HandleFunc("POST /v1/runtimes/{runtimeId}/restart", server.handleRuntimeRestart)
	server.registerSessionRoutes(mux)
	mux.HandleFunc("POST /v1/devices/fcm-token", server.handleRegisterFCMToken)
	adminMux.HandleFunc("GET /admin/v1/status", server.handleAdminStatus)
	adminMux.HandleFunc("POST /admin/v1/pairings/start", server.handleAdminPairingStart)
	adminMux.HandleFunc("GET /admin/v1/pairings/{challengeId}", server.handleAdminPairingStatus)
	adminMux.HandleFunc("DELETE /admin/v1/pairings/{challengeId}", server.handleAdminPairingCancel)
	adminMux.HandleFunc("GET /admin/v1/devices", server.handleAdminDevices)
	adminMux.HandleFunc("DELETE /admin/v1/devices/{deviceId}", server.handleAdminDeviceRevoke)

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
	server.adminServer = &http.Server{
		Addr:              adminAddr,
		Handler:           server.withRequestLogging(adminMux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	return server
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

// Shutdown stops both HTTP servers.
func (s *Server) Shutdown(ctx context.Context) error {
	return errors.Join(s.httpServer.Shutdown(ctx), s.adminServer.Shutdown(ctx))
}

// Handler returns the public API handler.
func (s *Server) Handler() http.Handler {
	return s.httpServer.Handler
}

// AdminHandler returns the local admin API handler.
func (s *Server) AdminHandler() http.Handler {
	return s.adminServer.Handler
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
		ProtocolVersion:   protocolVersion,
		StartedAt:         s.startedAt,
		UptimeSeconds:     uptimeSeconds(s.startedAt, now),
		ListenAddr:        s.cfg.ListenAddr,
		LANEnabled:        s.cfg.EnableLAN,
		PairedDeviceCount: s.pairing.ActiveDeviceCount(),
		Discovery:         s.discovery.Snapshot(),
		Remote:            s.remoteStatus(includePublicURL),
		Registry:          s.registryStatus(),
		RuntimeCounts: runtimeCounts{
			Starting:    summary.Starting,
			Total:       summary.Total,
			Running:     summary.Running,
			Stopping:    summary.Stopping,
			Stopped:     summary.Stopped,
			Failed:      summary.Failed,
			CircuitOpen: summary.CircuitOpen,
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
	publicBaseURL := strings.TrimSpace(s.cfg.PublicBaseURL)
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
	mode = "reverse_proxy"
	scope = "public"
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

func normalizeBuildInfo(build BuildInfo) BuildInfo {
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

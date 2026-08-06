package api

import (
	"net/http"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/discovery"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
	acpregistry "github.com/arafatamim/ferngeist-acp-gateway/internal/registry"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
)

// diagnosticsSummaryResponse provides a compact health overview for monitoring.
type diagnosticsSummaryResponse struct {
	Runtime runtime.Summary `json:"runtime"`
}

// diagnosticsExportResponse is the full diagnostic bundle exported for bug reports.
// It includes gateway metadata, runtime state, and bounded logs from all sources.
type diagnosticsExportResponse struct {
	GeneratedAt time.Time                     `json:"generatedAt"`
	Gateway     diagnosticsGatewaySnapshot    `json:"gateway"`
	Runtime     runtime.Summary               `json:"runtime"`
	Runtimes    []runtime.Runtime             `json:"runtimes"`
	RuntimeLogs map[string][]runtime.LogEntry `json:"runtimeLogs"`
	GatewayLogs []string                      `json:"gatewayLogs"`
}

// diagnosticsGatewaySnapshot captures the gateway daemon's own configuration and
// state at the moment of diagnostic export.
type diagnosticsGatewaySnapshot struct {
	Name            string             `json:"name"`
	Version         string             `json:"version"`
	Build           BuildInfo          `json:"build"`
	ProtocolVersion string             `json:"protocolVersion"`
	StartedAt       time.Time          `json:"startedAt"`
	UptimeSeconds   int64              `json:"uptimeSeconds"`
	ListenAddr      string             `json:"listenAddr"`
	LANEnabled      bool               `json:"lanEnabled"`
	GatewayName     string             `json:"gatewayName"`
	LogDir          string             `json:"logDir"`
	StateDBPath     string             `json:"stateDbPath"`
	Discovery       discovery.Snapshot `json:"discovery"`
	Remote          remoteStatus       `json:"remote"`
	Registry        acpregistry.Status `json:"registry"`
}

func (s *Server) handleDiagnosticsSummary(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeRead); !ok {
		return
	}

	writeJSON(w, http.StatusOK, diagnosticsSummaryResponse{
		Runtime: s.runtime.Summary(),
	})
}

// handleDiagnosticsExport produces a compact bug-report bundle. It includes
// gateway metadata, active runtime state, and bounded logs, but intentionally
// does not try to become a transcript export format.
func (s *Server) handleDiagnosticsExport(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayScope(w, r, pairing.ScopeDiagnosticsExport); !ok {
		return
	}

	runtimes := s.runtime.List()
	runtimeLogs := make(map[string][]runtime.LogEntry, len(runtimes))
	for _, runtimeInfo := range runtimes {
		logs, err := s.runtime.Logs(runtimeInfo.ID)
		if err != nil {
			continue
		}
		runtimeLogs[runtimeInfo.ID] = logs
	}

	gatewayLogs, err := s.tailGatewayLogs(200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read gateway logs")
		return
	}

	writeJSON(w, http.StatusOK, diagnosticsExportResponse{
		GeneratedAt: s.now(),
		Gateway: diagnosticsGatewaySnapshot{
			Name:            s.gatewayDisplayName(),
			Version:         s.build.Version,
			Build:           s.build,
			ProtocolVersion: ProtocolVersion,
			StartedAt:       s.startedAt,
			UptimeSeconds:   uptimeSeconds(s.startedAt, s.now()),
			ListenAddr:      s.cfg.ListenAddr,
			LANEnabled:      s.cfg.EnableLAN,
			GatewayName:     s.cfg.GatewayName,
			LogDir:          s.cfg.LogDir,
			StateDBPath:     s.cfg.StateDBPath,
			Discovery:       s.discovery.Snapshot(),
			Remote:          s.remoteStatus(true),
			Registry:        s.registryStatus(),
		},
		Runtime:     s.runtime.Summary(),
		Runtimes:    runtimes,
		RuntimeLogs: runtimeLogs,
		GatewayLogs: gatewayLogs,
	})
}

func (s *Server) tailGatewayLogs(limit int) ([]string, error) {
	if s.logs == nil {
		return nil, nil
	}
	return s.logs.TailLines(limit)
}

func (s *Server) registryStatus() acpregistry.Status {
	if s.registry == nil {
		return acpregistry.Status{State: "disabled"}
	}
	return s.registry.Status()
}

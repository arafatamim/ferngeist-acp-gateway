package push

import (
	"context"
	"log/slog"
)

// LogProvider is a Provider that logs notifications instead of delivering them.
// It is the fallback when no real transport is configured (e.g. local/dev runs
// with no Firebase credentials), so the push path stays exercised end-to-end
// without a push backend.
type LogProvider struct {
	l *slog.Logger
}

func NewLogProvider(logger *slog.Logger) *LogProvider {
	if logger == nil {
		logger = slog.Default()
	}
	return &LogProvider{l: logger}
}

func (p *LogProvider) Send(ctx context.Context, token string, n Notification) error {
	p.l.Info("[push] notification (log-only, not delivered)",
		"title", n.Title,
		"body", n.Body,
		"category", n.Category,
		"server_id", n.ServerID,
		"session_id", n.SessionID,
	)
	return nil
}

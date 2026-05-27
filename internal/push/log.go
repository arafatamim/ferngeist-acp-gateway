package push

import (
	"context"
	"log/slog"
)

type LogPushService struct {
	l *slog.Logger
}

func NewLogPushService(logger *slog.Logger) *LogPushService {
	return &LogPushService{l: logger}
}

func (s *LogPushService) logger() *slog.Logger {
	if s.l == nil {
		return slog.Default()
	}
	return s.l
}

func (s *LogPushService) SendNotification(ctx context.Context, deviceID, title, body string, data map[string]string) error {
	attrs := []any{
		"device_id", deviceID,
		"title", title,
		"body", body,
		"data", data,
	}
	s.logger().Info("[push] notification sent", attrs...)
	return nil
}


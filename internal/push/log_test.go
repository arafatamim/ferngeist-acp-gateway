package push

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func TestSendNotificationReturnsNil(t *testing.T) {
	svc := NewLogPushService(slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := svc.SendNotification(context.Background(), "device-1", "Hello", "Test body", map[string]string{"key": "val"})
	if err != nil {
		t.Fatalf("SendNotification() error = %v", err)
	}
}

func TestNilLoggerDoesNotPanic(t *testing.T) {
	svc := &LogPushService{}
	err := svc.SendNotification(context.Background(), "device-1", "Hello", "Test body", map[string]string{"key": "val"})
	if err != nil {
		t.Fatalf("SendNotification() with nil logger error = %v", err)
	}
}

func TestSendNotificationReturnsNilWithNilDataMap(t *testing.T) {
	svc := NewLogPushService(slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := svc.SendNotification(context.Background(), "device-1", "Hello", "Test body", nil)
	if err != nil {
		t.Fatalf("SendNotification() with nil data error = %v", err)
	}
}

func TestSendNotificationReturnsNilWithEmptyStrings(t *testing.T) {
	svc := NewLogPushService(slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := svc.SendNotification(context.Background(), "", "", "", map[string]string{})
	if err != nil {
		t.Fatalf("SendNotification() with empty strings error = %v", err)
	}
}

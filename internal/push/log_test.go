package push

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func TestLogProviderSendReturnsNil(t *testing.T) {
	p := NewLogProvider(slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := p.Send(context.Background(), "device-token", Notification{
		Title: "Hello", Body: "Test body", Category: CategoryTurnComplete, SessionID: "sess-1",
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
}

func TestLogProviderNilLoggerDoesNotPanic(t *testing.T) {
	p := NewLogProvider(nil)
	if err := p.Send(context.Background(), "device-token", Notification{Title: "Hello"}); err != nil {
		t.Fatalf("Send() with nil logger error = %v", err)
	}
}

func TestLogProviderSendReturnsNilWithEmptyNotification(t *testing.T) {
	p := NewLogProvider(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := p.Send(context.Background(), "", Notification{}); err != nil {
		t.Fatalf("Send() with empty notification error = %v", err)
	}
}

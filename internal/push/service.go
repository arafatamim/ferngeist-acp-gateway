package push

import "context"

type PushService interface {
	SendNotification(ctx context.Context, deviceID, title, body string, data map[string]string) error
}

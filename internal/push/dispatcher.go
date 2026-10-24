package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
)

// TokenStore resolves a device's push token and platform and lets the dispatcher
// register tokens and evict tokens reported as permanently invalid.
// Satisfied by *storage.SQLiteStore.
type TokenStore interface {
	// GetDevicePushToken returns the device's token and platform, or ("","",nil)
	// if none is registered.
	GetDevicePushToken(ctx context.Context, deviceID string) (token, platform string, err error)
	// DeleteDevicePushToken removes a device's token after a provider reports it dead.
	DeleteDevicePushToken(ctx context.Context, deviceID string) error
	// SaveDevicePushToken registers or replaces a device token and routing platform.
	SaveDevicePushToken(ctx context.Context, deviceID, token, platform string) error
}

// Dispatcher is the platform-neutral PushService. It resolves a device to its
// registered (token, platform), routes delivery to the provider registered for
// that platform, and evicts tokens providers report as unregistered. Adding a new
// platform is a matter of registering another Provider — neither this dispatcher
// nor the session layer changes.
type Dispatcher struct {
	store     TokenStore
	providers map[string]Provider
	l         *slog.Logger
}

// NewDispatcher builds a dispatcher routing each platform to its provider. The
// providers map is keyed by the platform string stored at token registration
// (e.g. "android").
func NewDispatcher(store TokenStore, providers map[string]Provider, logger *slog.Logger) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Dispatcher{store: store, providers: providers, l: logger}
}

// Notify is best-effort: a device with no registered token is skipped silently, a
// device on a platform with no configured provider is logged and skipped, and a
// dead token is evicted rather than returned as an error. Only genuine,
// retryable delivery failures are returned to the caller.
func (d *Dispatcher) Notify(ctx context.Context, deviceID string, n Notification) error {
	token, platform, err := d.store.GetDevicePushToken(ctx, deviceID)
	if err != nil {
		return fmt.Errorf("lookup push token for device %s: %w", deviceID, err)
	}
	if token == "" {
		d.l.Debug("[push] no token for device, skipping", "device_id", deviceID)
		return nil
	}

	provider, ok := d.providers[platform]
	if !ok {
		d.l.Warn("[push] no provider for platform, skipping", "device_id", deviceID, "platform", platform)
		return nil
	}

	err = provider.Send(ctx, token, n)
	if errors.Is(err, ErrTokenUnregistered) {
		if delErr := d.store.DeleteDevicePushToken(ctx, deviceID); delErr != nil {
			d.l.Warn("[push] failed to evict dead token", "device_id", deviceID, "error", delErr)
		} else {
			d.l.Info("[push] evicted unregistered token", "device_id", deviceID, "platform", platform)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("deliver push to device %s via %s: %w", deviceID, platform, err)
	}
	d.l.Debug("[push] notification delivered", "device_id", deviceID, "platform", platform, "title", n.Title)
	return nil
}

// Registration is a device token or a Web Push subscription, whose platform
// is inferred from the subscription when present.
type Registration struct {
	Token        string
	Platform     string
	Subscription *Subscription
}

// RegistrationError rejects invalid client-supplied push registration data.
type RegistrationError string

func (e RegistrationError) Error() string { return string(e) }

// Register stores a token for the authenticated device, replacing any prior one.
// A Web Push subscription becomes the token; its endpoint must be HTTPS because
// it is where the gateway will POST notifications.
func (d *Dispatcher) Register(ctx context.Context, deviceID string, in Registration) error {
	token, platform := strings.TrimSpace(in.Token), strings.TrimSpace(in.Platform)
	if in.Subscription != nil {
		endpoint, err := url.Parse(in.Subscription.Endpoint)
		if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" {
			return RegistrationError("subscription.endpoint must be an https URL")
		}
		if in.Subscription.Keys.P256dh == "" || in.Subscription.Keys.Auth == "" {
			return RegistrationError("subscription.keys.p256dh and subscription.keys.auth are required")
		}
		data, err := json.Marshal(in.Subscription)
		if err != nil {
			return fmt.Errorf("marshal push subscription: %w", err)
		}
		token, platform = string(data), PlatformWebPush
	} else {
		if token == "" {
			return RegistrationError("token is required")
		}
		// platform is part of the contract but default it rather than reject: a
		// well-formed token we can store should never 4xx (the client retries 4xx
		// indefinitely). Older/other clients that omit it are treated as Android.
		if platform == "" {
			d.l.Warn("push token registered with empty platform, defaulting to android; client may be outdated", "device_id", deviceID)
			platform = "android"
		}
	}
	if err := d.store.SaveDevicePushToken(ctx, deviceID, token, platform); err != nil {
		return fmt.Errorf("save push token: %w", err)
	}
	return nil
}

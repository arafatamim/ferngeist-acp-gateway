// Package push delivers background notifications (turn-complete, agent-crash) to
// paired devices. It is split into a platform-neutral core and pluggable delivery
// providers so the gateway can back any Ferngeist client, not just Android:
//
//   - Notification is the semantic event the gateway emits, free of any wire
//     format. The session layer only ever speaks this.
//   - PushService.Notify resolves a device to its registered token+platform and
//     dispatches to the matching Provider. Dead-token eviction lives here, so it
//     is identical across platforms.
//   - Provider is the per-platform transport (FCM today; APNs/WebPush are additive
//     and require no change to the core or the session layer).
package push

import (
	"context"
	"errors"
)

// ErrTokenUnregistered is returned by a Provider when the destination token is
// permanently invalid (app uninstalled, token rotated). The dispatcher treats it
// as a signal to evict the token, not as a delivery failure to retry.
var ErrTokenUnregistered = errors.New("push token is unregistered")

// Notification categories. Providers may use these for platform-specific
// formatting (e.g. an iOS interruption level); the gateway core does not branch
// on them.
const (
	CategoryTurnComplete      = "turn_complete"
	CategoryPermissionRequest = "permission_request"
	CategoryError             = "agent_error"
	CategoryAgentCrash        = "agent_crash"
)

// Notification is a platform-neutral, renderable event destined for a device.
// Deep-link fields are optional: a client opens straight into a chat only when it
// receives both ServerID (the gateway-owned id the client paired against) and
// SessionID; otherwise a tap just opens the app.
type Notification struct {
	Title     string
	Body      string
	Category  string // one of the Category* constants
	ServerID  string // gateway-owned stable id, for client-side deep-link resolution
	SessionID string // target session/chat
	Cwd       string // working directory for the chat route, optional
}

// PushService is the gateway-facing entry point. Implementations resolve the
// device's token and platform and fan the notification out to the right provider.
type PushService interface {
	Notify(ctx context.Context, deviceID string, n Notification) error
}

// Provider delivers a single already-resolved notification to one token over a
// specific platform's transport. Implementations return ErrTokenUnregistered when
// the token is permanently dead so the dispatcher can evict it.
type Provider interface {
	Send(ctx context.Context, token string, n Notification) error
}

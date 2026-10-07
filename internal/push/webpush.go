package push

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// PlatformWebPush is the platform a Web Push subscription is registered under.
const PlatformWebPush = "webpush"

// vapidSubscriber is the contact sent in the VAPID JWT `sub` claim.
const vapidSubscriber = "https://github.com/arafatamim/ferngeist-acp-gateway"

// Subscription is a Web Push subscription as browsers serialize it
// (PushSubscription.toJSON()) and as UnifiedPush hands it to an Android app. It
// is stored verbatim as the device's push token.
type Subscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// WebPushProvider delivers notifications over Web Push (RFC 8030), encrypted
// end to end (RFC 8291) and signed with this gateway's own VAPID key (RFC 8292).
// It needs no third-party credentials: the endpoint the client registered says
// where the message goes — FCM (UnifiedPush's embedded distributor), a
// UnifiedPush distributor like ntfy, or a browser's push service — and only the
// client can decrypt it.
type WebPushProvider struct {
	publicKey, privateKey string
	httpClient            *http.Client
	l                     *slog.Logger
}

func NewWebPushProvider(publicKey, privateKey string, logger *slog.Logger) *WebPushProvider {
	if logger == nil {
		logger = slog.Default()
	}
	return &WebPushProvider{
		publicKey:  publicKey,
		privateKey: privateKey,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		l:          logger,
	}
}

// GenerateVAPIDKeys returns a new VAPID key pair, base64url encoded.
func GenerateVAPIDKeys() (privateKey, publicKey string, err error) {
	return webpush.GenerateVAPIDKeys()
}

// Payload is the decrypted JSON body a client receives. Empty fields are omitted.
type Payload struct {
	Title     string `json:"title,omitempty"`
	Body      string `json:"body,omitempty"`
	Category  string `json:"category,omitempty"`
	ServerID  string `json:"serverId,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
}

func (p *WebPushProvider) Send(ctx context.Context, token string, n Notification) error {
	var sub Subscription
	if err := json.Unmarshal([]byte(token), &sub); err != nil || sub.Endpoint == "" {
		// Not a subscription we can ever deliver to; evict rather than retry.
		return ErrTokenUnregistered
	}
	payload, err := json.Marshal(Payload{
		Title: n.Title, Body: n.Body, Category: n.Category,
		ServerID: n.ServerID, SessionID: n.SessionID, Cwd: n.Cwd,
	})
	if err != nil {
		return fmt.Errorf("marshal web push payload: %w", err)
	}

	opts := &webpush.Options{
		HTTPClient:      p.httpClient,
		Subscriber:      vapidSubscriber,
		VAPIDPublicKey:  p.publicKey,
		VAPIDPrivateKey: p.privateKey,
		TTL:             24 * 60 * 60,
		Urgency:         webpush.UrgencyHigh,
	}
	switch n.Category {
	case CategoryProgress:
		// Progress is stale within minutes, must not wake a dozing phone, and a
		// newer update replaces an undelivered older one for the same chat.
		opts.TTL = 5 * 60
		opts.Urgency = webpush.UrgencyNormal
		opts.Topic = progressTopic(n.SessionID)
	case CategoryUpdate:
		// Worth seeing whenever the phone next wakes, never worth waking it for.
		opts.Urgency = webpush.UrgencyNormal
	}

	resp, err := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys:     webpush.Keys{P256dh: sub.Keys.P256dh, Auth: sub.Keys.Auth},
	}, opts)
	if err != nil {
		return fmt.Errorf("send web push: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return ErrTokenUnregistered
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	return fmt.Errorf("web push rejected: %s: %s", resp.Status, body)
}

// progressTopic is the Topic header for a chat's progress pushes: at most 32
// URL-safe characters (RFC 8030 §5.4), so the session id is hashed.
func progressTopic(sessionID string) string {
	sum := sha256.Sum256([]byte("progress:" + sessionID))
	return hex.EncodeToString(sum[:16])
}

package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// fcmMessagingScope is the OAuth2 scope required to send messages via the FCM
// HTTP v1 API.
const fcmMessagingScope = "https://www.googleapis.com/auth/firebase.messaging"

// defaultFCMEndpoint is the FCM HTTP v1 base URL. Overridable on the provider for tests.
const defaultFCMEndpoint = "https://fcm.googleapis.com"

// FCMProvider delivers notifications via the Firebase Cloud Messaging HTTP v1 API.
// It authenticates with a service-account OAuth2 token source and is the Android
// transport today; because FCM also relays to APNs and Web Push, the same provider
// can later cover iOS/web via platform override blocks.
//
// It sends HYBRID notification+data messages. The `notification` block is what
// lets Android display the alert when the app is killed — the system renders it
// with no app process running, so a swiped-away or memory-reaped client still
// notifies the user. The `data` block carries the same title/body plus the
// deep-link keys (serverId, sessionId, category, cwd); the foreground client reads
// `data` to suppress duplicates and route a tap into the right chat. The android
// block sets high priority and a per-category channel so a system-displayed
// notification lands in the Alerts vs Updates channel.
//
// The provider is store-free: token resolution and dead-token eviction belong to
// the Dispatcher, so this type only knows how to put one message on the wire and
// reports ErrTokenUnregistered when FCM says the token is dead.
type FCMProvider struct {
	projectID   string
	tokenSource oauth2.TokenSource
	httpClient  *http.Client
	endpoint    string
	l           *slog.Logger
}

// NewFCMProvider builds an FCM provider from a Firebase service-account JSON
// credentials file. The Firebase project ID is read from the credentials, so it
// does not need to be configured separately. Returns an error if the file is
// missing, malformed, or omits a project ID.
func NewFCMProvider(ctx context.Context, credentialsFile string, logger *slog.Logger) (*FCMProvider, error) {
	data, err := os.ReadFile(credentialsFile)
	if err != nil {
		return nil, fmt.Errorf("read FCM credentials: %w", err)
	}
	creds, err := google.CredentialsFromJSONWithType(ctx, data, google.ServiceAccount, fcmMessagingScope)
	if err != nil {
		return nil, fmt.Errorf("parse FCM credentials: %w", err)
	}
	if creds.ProjectID == "" {
		return nil, fmt.Errorf("FCM credentials do not contain a project_id")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &FCMProvider{
		projectID:   creds.ProjectID,
		tokenSource: creds.TokenSource,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
		endpoint:    defaultFCMEndpoint,
		l:           logger,
	}, nil
}

// fcmMessage mirrors the FCM HTTP v1 request body. It is a hybrid notification+data
// message (see FCMProvider); data values must be strings.
type fcmMessage struct {
	Message struct {
		Token        string            `json:"token"`
		Notification *fcmNotification  `json:"notification,omitempty"`
		Data         map[string]string `json:"data,omitempty"`
		Android      *fcmAndroidConfig `json:"android,omitempty"`
	} `json:"message"`
}

// fcmNotification is the system-displayed notification block. Android renders this
// when the app is killed, with no app process needed.
type fcmNotification struct {
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
}

// fcmAndroidConfig carries Android-specific delivery overrides: a high priority to
// wake the device promptly and the channel a system-displayed notification routes to.
type fcmAndroidConfig struct {
	Priority     string                  `json:"priority,omitempty"`
	Notification *fcmAndroidNotification `json:"notification,omitempty"`
}

// fcmAndroidNotification selects the notification channel for a system-displayed push.
type fcmAndroidNotification struct {
	ChannelID string `json:"channel_id,omitempty"`
}

// Android notification channels. Alerts (heads-up) carry time-sensitive events;
// Updates is the quiet channel for routine turn-complete notifications.
const (
	alertChannelID   = "ferngeist_push"
	updatesChannelID = "ferngeist_push_updates"
)

// channelForCategory routes a notification to the Alerts channel for events that
// warrant a heads-up (permission requests, errors, crashes) and to the quiet
// Updates channel for everything else (turn-complete).
func channelForCategory(category string) string {
	switch category {
	case CategoryPermissionRequest, CategoryError, CategoryAgentCrash:
		return alertChannelID
	default:
		return updatesChannelID
	}
}

// dataPayload flattens a neutral Notification into the string map the Ferngeist
// client expects, omitting empty optional fields. A push deep-links into a chat
// only when it carries both serverId and sessionId.
func dataPayload(n Notification) map[string]string {
	data := map[string]string{}
	put := func(k, v string) {
		if v != "" {
			data[k] = v
		}
	}
	put("title", n.Title)
	put("body", n.Body)
	put("category", n.Category)
	put("serverId", n.ServerID)
	put("sessionId", n.SessionID)
	put("cwd", n.Cwd)
	return data
}

// Send delivers one notification to a single FCM registration token. It returns
// ErrTokenUnregistered when FCM reports the token as permanently dead (so the
// dispatcher evicts it), and a wrapped error for any other non-2xx response.
func (p *FCMProvider) Send(ctx context.Context, token string, n Notification) error {
	var msg fcmMessage
	msg.Message.Token = token
	// The notification block lets a killed app's system display the alert; the data
	// block duplicates title/body so the foreground client renders and routes it.
	msg.Message.Notification = &fcmNotification{Title: n.Title, Body: n.Body}
	msg.Message.Data = dataPayload(n)
	msg.Message.Android = &fcmAndroidConfig{
		Priority:     "high",
		Notification: &fcmAndroidNotification{ChannelID: channelForCategory(n.Category)},
	}

	payload, err := json.Marshal(&msg)
	if err != nil {
		return fmt.Errorf("marshal FCM message: %w", err)
	}

	accessToken, err := p.tokenSource.Token()
	if err != nil {
		return fmt.Errorf("obtain FCM access token: %w", err)
	}

	url := fmt.Sprintf("%s/v1/projects/%s/messages:send", p.endpoint, p.projectID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build FCM request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	accessToken.SetAuthHeader(req)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send FCM request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return nil
	}

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))

	// A 404 (or an UNREGISTERED error code at any status) means the registration
	// token is dead — the app was uninstalled or the token rotated.
	if resp.StatusCode == http.StatusNotFound || isUnregistered(respBody) {
		return ErrTokenUnregistered
	}

	return fmt.Errorf("FCM send failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
}

// isUnregistered reports whether an FCM error body carries the UNREGISTERED
// error code, which FCM uses for permanently invalid registration tokens.
func isUnregistered(body []byte) bool {
	var parsed struct {
		Error struct {
			Status  string `json:"status"`
			Details []struct {
				ErrorCode string `json:"errorCode"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	for _, d := range parsed.Error.Details {
		if d.ErrorCode == "UNREGISTERED" {
			return true
		}
	}
	return false
}

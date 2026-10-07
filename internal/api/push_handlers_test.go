package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

// registerPushToken POSTs to /v1/devices/push-token with the given bearer token
// and raw JSON body, returning the response recorder.
func registerPushToken(t *testing.T, server *Server, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/devices/push-token", bytes.NewReader([]byte(body)))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

func TestRegisterPushTokenStoresTokenAndPlatform(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "push.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	server := newTestServerWithStore(t.TempDir(), store)
	server.store = store

	creds := pairDeviceResponse(t, server)

	rec := registerPushToken(t, server, creds.Token, `{"token":"fcm-abc","platform":"android"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("register status = %d, want %d, body=%s", rec.Code, http.StatusNoContent, rec.Body.String())
	}

	// Identity must come from the credential, not the body — the token is stored
	// against the authenticated device.
	token, platform, err := store.GetDevicePushToken(context.Background(), creds.DeviceID)
	if err != nil {
		t.Fatalf("GetDevicePushToken() error = %v", err)
	}
	if token != "fcm-abc" || platform != "android" {
		t.Fatalf("stored = (%q, %q), want (fcm-abc, android)", token, platform)
	}

	// Re-registration with a rotated token replaces the prior one (idempotent upsert).
	rec = registerPushToken(t, server, creds.Token, `{"token":"fcm-rotated","platform":"android"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("re-register status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	token, _, err = store.GetDevicePushToken(context.Background(), creds.DeviceID)
	if err != nil {
		t.Fatalf("GetDevicePushToken(after rotate) error = %v", err)
	}
	if token != "fcm-rotated" {
		t.Fatalf("stored token after rotate = %q, want fcm-rotated", token)
	}
}

func TestRegisterPushTokenDefaultsPlatform(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "push.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	server := newTestServerWithStore(t.TempDir(), store)
	server.store = store

	creds := pairDeviceResponse(t, server)

	rec := registerPushToken(t, server, creds.Token, `{"token":"fcm-noplatform"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("register status = %d, want %d, body=%s", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	_, platform, err := store.GetDevicePushToken(context.Background(), creds.DeviceID)
	if err != nil {
		t.Fatalf("GetDevicePushToken() error = %v", err)
	}
	if platform != "android" {
		t.Fatalf("platform = %q, want android (default when omitted)", platform)
	}
}

func TestRegisterPushTokenRejectsMissingToken(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "push.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	server := newTestServerWithStore(t.TempDir(), store)
	server.store = store

	creds := pairDeviceResponse(t, server)

	rec := registerPushToken(t, server, creds.Token, `{"platform":"android"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d for missing token", rec.Code, http.StatusBadRequest)
	}
}

func TestRegisterPushTokenRequiresCredential(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "push.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	server := newTestServerWithStore(t.TempDir(), store)
	server.store = store

	rec := registerPushToken(t, server, "", `{"token":"fcm-abc","platform":"android"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d for missing credential", rec.Code, http.StatusUnauthorized)
	}
}

func TestPairCompleteReturnsGatewayID(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "push.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	server := newTestServerWithStore(t.TempDir(), store)
	server.store = store
	server.cfg.GatewayID = "gw-test-id"

	creds := pairDeviceResponse(t, server)
	if creds.GatewayID != "gw-test-id" {
		t.Fatalf("pair/complete gatewayId = %q, want %q", creds.GatewayID, "gw-test-id")
	}

	// Sanity: the JSON field is named "gatewayId".
	raw, _ := json.Marshal(pairCompleteResponse{GatewayID: "x"})
	if !bytes.Contains(raw, []byte(`"gatewayId":"x"`)) {
		t.Fatalf("pairCompleteResponse JSON missing gatewayId field: %s", raw)
	}
}

func TestRegisterWebPushSubscription(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "push.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	server := newTestServerWithStore(t.TempDir(), store)
	server.store = store
	creds := pairDeviceResponse(t, server)

	for _, bad := range []string{
		`{"platform":"webpush","subscription":{"endpoint":"http://push.example/x","keys":{"p256dh":"k","auth":"a"}}}`,
		`{"platform":"webpush","subscription":{"endpoint":"https://push.example/x","keys":{"p256dh":"k"}}}`,
	} {
		if rec := registerPushToken(t, server, creds.Token, bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 for %s", rec.Code, bad)
		}
	}

	rec := registerPushToken(t, server, creds.Token,
		`{"platform":"webpush","subscription":{"endpoint":"https://fcm.googleapis.com/fcm/send/t","keys":{"p256dh":"k","auth":"a"}}}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	token, platform, err := store.GetDevicePushToken(context.Background(), creds.DeviceID)
	if err != nil {
		t.Fatalf("GetDevicePushToken() error = %v", err)
	}
	if platform != "webpush" || token != `{"endpoint":"https://fcm.googleapis.com/fcm/send/t","keys":{"p256dh":"k","auth":"a"}}` {
		t.Fatalf("stored = (%q, %q)", token, platform)
	}
}

func TestPushConfigReturnsVAPIDPublicKey(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "push.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	server := newTestServerWithStore(t.TempDir(), store)
	server.store = store
	server.cfg.VAPIDPublicKey = "BPUB"
	creds := pairDeviceResponse(t, server)

	get := func(bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/devices/push-config", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		return rec
	}
	if rec := get(""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", rec.Code)
	}
	rec := get(creds.Token)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"vapidPublicKey":"BPUB"}` {
		t.Fatalf("push-config = %d %s", rec.Code, rec.Body.String())
	}
}

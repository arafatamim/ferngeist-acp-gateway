package push

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

type fakeTokenStore struct {
	token     string
	platform  string
	getErr    error
	deleted   bool
	deleteErr error
}

func (f *fakeTokenStore) GetDevicePushToken(ctx context.Context, deviceID string) (string, string, error) {
	return f.token, f.platform, f.getErr
}

func (f *fakeTokenStore) DeleteDevicePushToken(ctx context.Context, deviceID string) error {
	f.deleted = true
	return f.deleteErr
}

// recordingProvider captures the token it was asked to deliver to and returns a
// configurable error so dispatcher behavior can be asserted in isolation.
type recordingProvider struct {
	gotToken string
	called   bool
	err      error
}

func (r *recordingProvider) Send(ctx context.Context, token string, n Notification) error {
	r.called = true
	r.gotToken = token
	return r.err
}

func newTestDispatcher(store TokenStore, providers map[string]Provider) *Dispatcher {
	return NewDispatcher(store, providers, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestDispatcherRoutesToPlatformProvider(t *testing.T) {
	store := &fakeTokenStore{token: "android-token", platform: "android"}
	android := &recordingProvider{}
	ios := &recordingProvider{}

	d := newTestDispatcher(store, map[string]Provider{"android": android, "ios": ios})
	if err := d.Notify(context.Background(), "device-1", Notification{Title: "T"}); err != nil {
		t.Fatalf("Notify() error = %v", err)
	}
	if !android.called || android.gotToken != "android-token" {
		t.Errorf("android provider got token %q, called=%v", android.gotToken, android.called)
	}
	if ios.called {
		t.Error("ios provider should not be called for an android device")
	}
}

func TestDispatcherSkipsWhenNoToken(t *testing.T) {
	store := &fakeTokenStore{token: "", platform: ""}
	android := &recordingProvider{}

	d := newTestDispatcher(store, map[string]Provider{"android": android})
	if err := d.Notify(context.Background(), "device-1", Notification{Title: "T"}); err != nil {
		t.Fatalf("Notify() error = %v", err)
	}
	if android.called {
		t.Error("provider was called despite the device having no token")
	}
}

func TestDispatcherSkipsUnknownPlatform(t *testing.T) {
	store := &fakeTokenStore{token: "tok", platform: "ios"}
	android := &recordingProvider{}

	d := newTestDispatcher(store, map[string]Provider{"android": android})
	if err := d.Notify(context.Background(), "device-1", Notification{Title: "T"}); err != nil {
		t.Fatalf("Notify() error = %v, want nil (unconfigured platform is skipped)", err)
	}
	if android.called {
		t.Error("android provider should not handle an ios device")
	}
}

func TestDispatcherEvictsUnregisteredToken(t *testing.T) {
	store := &fakeTokenStore{token: "dead", platform: "android"}
	android := &recordingProvider{err: ErrTokenUnregistered}

	d := newTestDispatcher(store, map[string]Provider{"android": android})
	if err := d.Notify(context.Background(), "device-1", Notification{Title: "T"}); err != nil {
		t.Fatalf("Notify() error = %v, want nil (eviction is best-effort)", err)
	}
	if !store.deleted {
		t.Error("expected dead token to be evicted, but DeleteDevicePushToken was not called")
	}
}

func TestDispatcherReturnsErrorOnDeliveryFailure(t *testing.T) {
	store := &fakeTokenStore{token: "live", platform: "android"}
	android := &recordingProvider{err: errors.New("boom")}

	d := newTestDispatcher(store, map[string]Provider{"android": android})
	if err := d.Notify(context.Background(), "device-1", Notification{Title: "T"}); err == nil {
		t.Fatal("expected delivery error to propagate, got nil")
	}
	if store.deleted {
		t.Error("token should not be evicted on a transient delivery failure")
	}
}

func TestDispatcherReturnsErrorOnLookupFailure(t *testing.T) {
	store := &fakeTokenStore{getErr: context.DeadlineExceeded}
	android := &recordingProvider{}

	d := newTestDispatcher(store, map[string]Provider{"android": android})
	if err := d.Notify(context.Background(), "device-1", Notification{Title: "T"}); err == nil {
		t.Fatal("expected error when token lookup fails, got nil")
	}
	if android.called {
		t.Error("provider should not be called when token lookup fails")
	}
}

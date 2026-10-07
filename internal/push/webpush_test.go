package push

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSubscriber is a push endpoint plus the client keys only it can decrypt with.
type fakeSubscriber struct {
	priv   *ecdh.PrivateKey
	auth   []byte
	status int
	got    *http.Request
	body   []byte
	srv    *httptest.Server
}

func newFakeSubscriber(t *testing.T) *fakeSubscriber {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSubscriber{priv: priv, auth: make([]byte, 16), status: http.StatusCreated}
	_, _ = rand.Read(f.auth)
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.got = r
		f.body, _ = io.ReadAll(r.Body)
		w.WriteHeader(f.status)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSubscriber) token(t *testing.T) string {
	t.Helper()
	var sub Subscription
	sub.Endpoint = f.srv.URL + "/push/abc"
	sub.Keys.P256dh = base64.RawURLEncoding.EncodeToString(f.priv.PublicKey().Bytes())
	sub.Keys.Auth = base64.RawURLEncoding.EncodeToString(f.auth)
	raw, _ := json.Marshal(sub)
	return string(raw)
}

// decrypt reverses RFC 8291 (aes128gcm, RFC 8188) as a client would.
func (f *fakeSubscriber) decrypt(t *testing.T) Payload {
	t.Helper()
	b := f.body
	salt, idLen := b[:16], int(b[20])
	asPublic, ciphertext := b[21:21+idLen], b[21+idLen:]
	senderKey, err := ecdh.P256().NewPublicKey(asPublic)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := f.priv.ECDH(senderKey)
	if err != nil {
		t.Fatal(err)
	}
	keyInfo := "WebPush: info\x00" + string(f.priv.PublicKey().Bytes()) + string(asPublic)
	ikm, _ := hkdf.Key(sha256.New, secret, f.auth, keyInfo, 32)
	cek, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	plain = bytes.TrimRight(plain, "\x00")
	plain = plain[:len(plain)-1] // the 0x02 last-record delimiter
	var p Payload
	if err := json.Unmarshal(plain, &p); err != nil {
		t.Fatalf("payload %q: %v", plain, err)
	}
	return p
}

func newTestWebPushProvider(t *testing.T, f *fakeSubscriber) (*WebPushProvider, string) {
	t.Helper()
	priv, pub, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	p := NewWebPushProvider(pub, priv, nil)
	p.httpClient = f.srv.Client()
	return p, pub
}

func TestWebPushDeliversDecryptablePayloadSignedWithGatewayKey(t *testing.T) {
	f := newFakeSubscriber(t)
	p, pub := newTestWebPushProvider(t, f)

	n := Notification{Title: "Permission Required", Body: "approve?", Category: CategoryPermissionRequest, ServerID: "gw", SessionID: "ses"}
	if err := p.Send(context.Background(), f.token(t), n); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := f.decrypt(t); got != (Payload{Title: n.Title, Body: n.Body, Category: n.Category, ServerID: "gw", SessionID: "ses"}) {
		t.Fatalf("payload = %+v", got)
	}
	h := f.got.Header
	if !strings.Contains(h.Get("Authorization"), "k="+pub) || h.Get("Content-Encoding") != "aes128gcm" {
		t.Fatalf("auth/encoding headers = %q / %q", h.Get("Authorization"), h.Get("Content-Encoding"))
	}
	if h.Get("Urgency") != "high" || h.Get("Topic") != "" {
		t.Fatalf("alert urgency/topic = %q / %q", h.Get("Urgency"), h.Get("Topic"))
	}
}

func TestWebPushProgressIsQuietShortLivedAndCollapsed(t *testing.T) {
	f := newFakeSubscriber(t)
	p, _ := newTestWebPushProvider(t, f)

	if err := p.Send(context.Background(), f.token(t), Notification{Category: CategoryProgress, SessionID: "ses"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	h := f.got.Header
	if h.Get("Urgency") != "normal" || h.Get("TTL") != "300" || h.Get("Topic") != progressTopic("ses") || len(progressTopic("ses")) > 32 {
		t.Fatalf("progress headers urgency=%q ttl=%q topic=%q", h.Get("Urgency"), h.Get("TTL"), h.Get("Topic"))
	}
}

func TestWebPushStatusMapping(t *testing.T) {
	f := newFakeSubscriber(t)
	p, _ := newTestWebPushProvider(t, f)

	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		f.status = status
		if err := p.Send(context.Background(), f.token(t), Notification{}); !errors.Is(err, ErrTokenUnregistered) {
			t.Fatalf("status %d: err = %v, want ErrTokenUnregistered", status, err)
		}
	}
	f.status = http.StatusTooManyRequests
	if err := p.Send(context.Background(), f.token(t), Notification{}); err == nil || errors.Is(err, ErrTokenUnregistered) {
		t.Fatalf("429: err = %v, want a retryable error", err)
	}
	if err := p.Send(context.Background(), "fcm-legacy-token", Notification{}); !errors.Is(err, ErrTokenUnregistered) {
		t.Fatalf("non-subscription token: err = %v, want ErrTokenUnregistered", err)
	}
}

package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

func TestArtifactsBindProofToOriginalQueryForGetAndHead(t *testing.T) {
	s := newTestServer()
	c := pairDeviceWithProof(t, s)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, mutate := range []bool{false, true} {
			path := url.QueryEscape(filepath.Join(t.TempDir(), "x"))
			r := httptest.NewRequest(method, "/v1/runtimes/x/artifacts?path="+path+"&acpSessionId=a", nil)
			r.Header.Set("Authorization", "Bearer "+c.token)
			r.Header.Set("Range", "bytes=0-0")
			signProofRequest(t, r, c, nil, time.Now(), method+"-"+time.Now().String())
			want := http.StatusServiceUnavailable
			if mutate {
				r.URL.RawQuery = "path=" + path + "y&acpSessionId=a"
				want = http.StatusUnauthorized
			}
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != want {
				t.Fatalf("%s changed=%v: status = %d, want %d", method, mutate, w.Code, want)
			}
			assertArtifactHeaders(t, w.Header())
		}
	}
}

func TestArtifactsRequireReadScope(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(filepath.Join(dir, "pairings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SavePairing(t.Context(), storage.PairingRecord{
		DeviceID: "control-device", DeviceName: "control", Token: "control-token",
		ExpiresAt: time.Now().Add(time.Hour), Scopes: []string{pairing.ScopeControl},
	}); err != nil {
		t.Fatal(err)
	}
	s := newTestServerWithStore(dir, store)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		r := httptest.NewRequest(method, "/v1/runtimes/x/artifacts?path=/tmp/x&acpSessionId=a", nil)
		r.Header.Set("Authorization", "Bearer control-token")
		r.Header.Set("Range", "bytes=0-0")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s = %d, want 403", method, w.Code)
		}
		assertArtifactHeaders(t, w.Header())
	}
}

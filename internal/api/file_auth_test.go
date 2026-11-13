package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

func TestFilesBindProofToOriginalQueryForGetAndHead(t *testing.T) {
	s := newTestServer()
	c := pairDeviceWithProof(t, s)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, mutate := range []bool{false, true} {
			path := url.QueryEscape(filepath.Join(t.TempDir(), "x"))
			r := httptest.NewRequest(method, "/v1/files?path="+path, nil)
			r.Header.Set("Authorization", "Bearer "+c.token)
			r.Header.Set("Range", "bytes=0-0")
			signProofRequest(t, r, c, nil, time.Now(), method+"-"+time.Now().String())
			want := http.StatusNotFound // authorized; the file does not exist
			if mutate {
				r.URL.RawQuery = "path=" + path + "y"
				want = http.StatusUnauthorized
			}
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != want {
				t.Fatalf("%s changed=%v: status = %d, want %d", method, mutate, w.Code, want)
			}
			assertFileHeaders(t, w.Header())
		}
	}
}

func TestFilesRequireReadAndControlScopes(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(filepath.Join(dir, "pairings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, scope := range []string{pairing.ScopeRead, pairing.ScopeControl} {
		if err := store.SavePairing(t.Context(), storage.PairingRecord{
			DeviceID: scope, DeviceName: scope, Token: scope + "-token",
			ExpiresAt: time.Now().Add(time.Hour), Scopes: []string{scope},
		}); err != nil {
			t.Fatal(err)
		}
	}
	s := newTestServerWithStore(dir, store)
	for _, scope := range []string{pairing.ScopeRead, pairing.ScopeControl} {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
			r := httptest.NewRequest(method, "/v1/files?path=/tmp/x", nil)
			if method == http.MethodPost {
				r = httptest.NewRequest(method, "/v1/files/stat", strings.NewReader(`{"paths":["/tmp/x"]}`))
			}
			r.Header.Set("Authorization", "Bearer "+scope+"-token")
			r.Header.Set("Range", "bytes=0-0")
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("%s-only %s = %d, want 403", scope, method, w.Code)
			}
			assertFileHeaders(t, w.Header())
		}
	}
}

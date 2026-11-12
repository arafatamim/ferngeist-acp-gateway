package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func requestArtifactHeaders(t *testing.T, e *artifactEnv, method string, headers http.Header) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, e.target(), nil)
	r.Header.Set("Authorization", "Bearer "+e.h.token)
	for key, values := range headers {
		r.Header[key] = values
	}
	w := httptest.NewRecorder()
	e.h.server.Handler().ServeHTTP(w, r)
	assertArtifactHeaders(t, w.Header())
	return w
}

func TestArtifactSingleByteRanges(t *testing.T) {
	e := newArtifactEnv(t, "video.mp4")
	if err := os.WriteFile(e.path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ value, body, contentRange string }{
		{"bytes=2-5", "2345", "bytes 2-5/10"},
		{"bytes=6-", "6789", "bytes 6-9/10"},
		{"bytes=-3", "789", "bytes 7-9/10"},
		{"bytes=7-100", "789", "bytes 7-9/10"},
		{"bytes=-99", "0123456789", "bytes 0-9/10"},
		{"bytes=0-9", "0123456789", "bytes 0-9/10"},
		{"bytes= 2 - 5 ", "2345", "bytes 2-5/10"},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			w := requestArtifactHeaders(t, e, http.MethodGet, http.Header{"Range": {tc.value}})
			if w.Code != 206 || w.Body.String() != tc.body || w.Header().Get("Content-Range") != tc.contentRange {
				t.Fatalf("range = %d %v %q", w.Code, w.Header(), w.Body.String())
			}
			if w.Header().Get("Accept-Ranges") != "bytes" || w.Result().ContentLength != int64(len(tc.body)) || w.Header().Get("Content-Type") != "video/mp4" {
				t.Fatalf("metadata = %v", w.Header())
			}
		})
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		headers := http.Header{}
		if method == http.MethodHead {
			headers.Set("Range", "bytes=99-100")
		}
		w := requestArtifactHeaders(t, e, method, headers)
		if w.Code != 200 || w.Header().Get("Content-Length") != "10" || w.Header().Get("Content-Range") != "" || w.Header().Get("Accept-Ranges") != "bytes" {
			t.Fatalf("%s = %d %v", method, w.Code, w.Header())
		}
		if method == http.MethodGet && w.Body.String() != "0123456789" || method == http.MethodHead && w.Body.Len() != 0 {
			t.Fatalf("%s body = %q", method, w.Body.String())
		}
	}
}

func TestArtifactInvalidAndMultipleRangesFailWithSafetyHeaders(t *testing.T) {
	e := newArtifactEnv(t, "video.mp4")
	if err := os.WriteFile(e.path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, values := range [][]string{
		{""}, {"items=1-2"}, {"bytes=10-"}, {"bytes=4-2"}, {"bytes=-0"}, {"bytes=-"},
		{"bytes=+1-2"}, {"bytes=0--1"}, {"bytes=0-9223372036854775808"},
		{"bytes=9223372036854775808-"}, {"bytes=-9223372036854775808"},
		{"bytes=0-1,2-3"}, {"bytes=0-1,"}, {"bytes=0-1", "bytes=2-3"},
	} {
		t.Run(strings.Join(values, ";"), func(t *testing.T) {
			w := requestArtifactHeaders(t, e, http.MethodGet, http.Header{"Range": values})
			if w.Code != 416 || w.Header().Get("Content-Range") != "bytes */10" {
				t.Fatalf("invalid range = %d %v %s", w.Code, w.Header(), w.Body.String())
			}
		})
	}
	if err := os.Truncate(e.path, 0); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"bytes=0-0", "bytes=0-", "bytes=-1"} {
		w := requestArtifactHeaders(t, e, http.MethodGet, http.Header{"Range": {value}})
		if w.Code != 416 || w.Header().Get("Content-Range") != "bytes */0" {
			t.Fatalf("empty range = %d %v", w.Code, w.Header())
		}
	}
	// Failed preparation must release every admission slot.
	e.h.server.artifactMu.Lock()
	defer e.h.server.artifactMu.Unlock()
	if e.h.server.artifactCount != 0 || len(e.h.server.artifactDevices) != 0 {
		t.Fatal("range errors retained slots")
	}
}

func TestArtifactIfRangeAlwaysReturnsCurrentFullFile(t *testing.T) {
	e := newArtifactEnv(t, "video.mp4")
	stamp := time.Unix(1700000000, 0)
	if err := os.WriteFile(e.path, []byte("old bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(e.path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	before := requestArtifactHeaders(t, e, http.MethodGet, http.Header{"Range": {"bytes=0-2"}})
	if before.Code != 206 || before.Body.String() != "old" {
		t.Fatalf("before mutation = %d %q", before.Code, before.Body.String())
	}
	if err := os.WriteFile(e.path, []byte("new bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(e.path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ value, ranges string }{
		{"", "bytes=0-2"}, {stamp.UTC().Format(http.TimeFormat), "bytes=0-2"},
		{`"metadata-is-not-identity"`, "bytes=0-2"}, {"invalid", "bytes=0-2"},
		{stamp.UTC().Format(http.TimeFormat), "bytes=0-2,4-5"},
	} {
		w := requestArtifactHeaders(t, e, http.MethodGet, http.Header{"Range": {tc.ranges}, "If-Range": {tc.value}})
		if w.Code != 200 || w.Body.String() != "new bytes" || w.Header().Get("Content-Range") != "" {
			t.Fatalf("If-Range %q = %d %v %q", tc.value, w.Code, w.Header(), w.Body.String())
		}
	}
}

func TestArtifactRangesPreserveAdmissionChecks(t *testing.T) {
	e := newArtifactEnv(t, "video.mp4")
	if err := os.WriteFile(e.path, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.path+"-unadvertised", []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	foreign := pairDevice(t, e.h.server)
	for _, tc := range []struct {
		token, target string
		status        int
	}{
		{"", e.target(), 401}, {foreign, e.target(), 404},
		{e.h.token, e.target() + "-unadvertised", 403},
	} {
		r := httptest.NewRequest(http.MethodGet, tc.target, nil)
		r.Header.Set("Authorization", "Bearer "+tc.token)
		r.Header.Set("Range", "bytes=0-0")
		w := httptest.NewRecorder()
		e.h.server.Handler().ServeHTTP(w, r)
		assertArtifactHeaders(t, w.Header())
		if w.Code != tc.status {
			t.Fatalf("admission = %d, want %d", w.Code, tc.status)
		}
	}
	if err := os.Truncate(e.path, 512<<20+1); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		w := requestArtifactHeaders(t, e, method, http.Header{"Range": {"bytes=0-0"}})
		if w.Code != 413 {
			t.Fatalf("tiny oversized %s range = %d", method, w.Code)
		}
	}
}

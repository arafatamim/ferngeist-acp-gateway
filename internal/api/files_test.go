package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/workspace"
)

type fileEnv struct {
	h    *resilientTestHarness
	path string
}

func newFileEnv(t *testing.T, name string) *fileEnv {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("file retrieval requires Linux, macOS or Windows")
	}
	return &fileEnv{h: newResilientTestHarness(t), path: filepath.Join(t.TempDir(), name)}
}

func (e *fileEnv) target() string {
	return "/v1/files?path=" + url.QueryEscape(e.path)
}

func (e *fileEnv) request(t *testing.T, method, target, token string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Range", "bytes=1-2")
	r.Header.Set("If-Range", `"old-version"`)
	w := httptest.NewRecorder()
	e.h.server.Handler().ServeHTTP(w, r)
	assertFileHeaders(t, w.Header())
	return w
}

func TestFilesReturnCompleteBytesAndHeadMetadata(t *testing.T) {
	assertCompleteFile(t, newFileEnv(t, "live-full-history-test.mp4"))
}

func assertCompleteFile(t *testing.T, e *fileEnv) {
	t.Helper()
	data := bytes.Repeat([]byte("mp4\x00"), (9<<20)/4)
	if err := os.WriteFile(e.path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		w := e.request(t, method, e.target(), e.h.token)
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d: %s", method, w.Code, w.Body.String())
		}
		if w.Header().Get("Content-Type") != "video/mp4" || w.Header().Get("Content-Length") != "9437184" {
			t.Fatalf("metadata = %v", w.Header())
		}
		kind, params, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
		if err != nil || kind != "attachment" || params["filename"] != "live-full-history-test.mp4" {
			t.Fatalf("disposition = %v, %v", params, err)
		}
		if method == http.MethodGet && sha256.Sum256(w.Body.Bytes()) != sha256.Sum256(data) {
			t.Fatal("file bytes differ")
		}
		if method == http.MethodHead && w.Body.Len() != 0 {
			t.Fatal("HEAD returned a body")
		}
	}
}

func TestFilesReportAdmissionErrorsWithoutHostPaths(t *testing.T) {
	e := newFileEnv(t, "file.bin")
	if err := os.WriteFile(e.path, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(e.path); err != nil {
		t.Fatal(err)
	}
	w := e.request(t, http.MethodGet, e.target(), e.h.token)
	if w.Code != 404 || strings.Contains(w.Body.String(), e.path) {
		t.Fatalf("missing = %d: %s", w.Code, w.Body.String())
	}
	if err := os.WriteFile(e.path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(e.path, 512<<20+1); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		if w := e.request(t, method, e.target(), e.h.token); w.Code != 413 {
			t.Fatalf("oversize %s = %d", method, w.Code)
		}
	}
	admin := httptest.NewRecorder()
	e.h.server.AdminHandler().ServeHTTP(admin, httptest.NewRequest(http.MethodGet, e.target(), nil))
	if admin.Code != 404 {
		t.Fatalf("admin file route = %d", admin.Code)
	}
}

func TestFileAttachmentNamesAreSafe(t *testing.T) {
	for _, name := range []string{`quoted"name.mp4`, `back\slash.mp4`, "résumé 你好.mp4"} {
		t.Run(name, func(t *testing.T) {
			if runtime.GOOS == "windows" && strings.ContainsAny(name, `"\`) {
				t.Skip("invalid Windows filename; lexical rejection is covered at the workspace boundary")
			}
			e := newFileEnv(t, name)
			if err := os.WriteFile(e.path, []byte("video"), 0o600); err != nil {
				t.Fatal(err)
			}
			w := e.request(t, http.MethodHead, e.target(), e.h.token)
			kind, params, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
			if w.Code != 200 || err != nil || kind != "attachment" || params["filename"] != name {
				t.Fatalf("disposition = %q (%v), status %d", w.Header().Get("Content-Disposition"), err, w.Code)
			}
			if strings.Contains(w.Header().Get("Content-Disposition"), filepath.Dir(e.path)) {
				t.Fatal("disposition exposes parent")
			}
		})
	}
}

func TestFilesRequireAuthenticationBeforeSelectors(t *testing.T) {
	s := newTestServer()
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		r := httptest.NewRequest(method, "/v1/files?path=%ZZ", nil)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d, want 401", method, w.Code)
		}
		assertFileHeaders(t, w.Header())
	}
}

func TestFilesRejectInvalidSelectors(t *testing.T) {
	s := newTestServer()
	token := pairDevice(t, s)
	validPath := url.QueryEscape(filepath.Join(t.TempDir(), "video.mp4"))
	valid := "path=" + validPath
	queries := []string{"", "acpSessionId=a", valid + "&path=/tmp/y", valid + "&acpSessionId=b", valid + "&cwd=/tmp", valid + "&deviceId=b", valid + "&PATH=/tmp/x", valid + "&bad=%ZZ", valid + ";bad=x"}
	for _, path := range []string{"relative", "/tmp/../tmp/x", "/tmp/", "/tmp/\n", "/tmp/\x00", "/tmp/\u200b", "/" + strings.Repeat("x", 4096), "/tmp/\xff"} {
		queries = append(queries, "path="+url.QueryEscape(path))
	}
	for _, query := range queries {
		r := httptest.NewRequest(http.MethodGet, "/v1/files?"+query, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("query %q: status = %d, want 400: %s", query, w.Code, w.Body.String())
		}
		assertFileHeaders(t, w.Header())
	}
}

func assertFileHeaders(t *testing.T, h http.Header) {
	t.Helper()
	if h.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("missing nosniff: %v", h)
	}
	// Only a response carrying a validator may be stored, and that validator is weak.
	if etag := h.Get("ETag"); etag == "" && h.Get("Cache-Control") != "no-store" ||
		etag != "" && (h.Get("Cache-Control") != "private, no-cache" || !strings.HasPrefix(etag, `W/"`)) {
		t.Fatalf("cache headers = %v", h)
	}
}

func TestFilesContentTypeSniffsUnknownExtensions(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"main.unknownext", []byte("package main\n"), "text/plain; charset=utf-8"},
		{"blob.unknownext", []byte{'a', 0, 'b'}, "application/octet-stream"},
		{"data.json", []byte{0}, "application/json"},                    // a known text type wins
		{"app.tsx", []byte("export {}\n"), "text/plain; charset=utf-8"}, // host tables may say application/x-tiled-tsx
		{"clip.mp4", []byte("\xff\xfe\x01\xff\xfe\x01"), "video/mp4"},   // not UTF-8: keep the media type
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFileEnv(t, tc.name)
			if err := os.WriteFile(e.path, tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			w := e.request(t, http.MethodHead, e.target(), e.h.token)
			if w.Code != 200 || w.Header().Get("Content-Type") != tc.want {
				t.Fatalf("status %d, Content-Type %q, want %q", w.Code, w.Header().Get("Content-Type"), tc.want)
			}
		})
	}
}

func TestFileStatDescribesEachPathAsResourceLink(t *testing.T) {
	e := newFileEnv(t, "rec ording.mp4")
	if err := os.WriteFile(e.path, []byte("ab\x00d"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(filepath.Dir(e.path), "gone.pdf")
	stat := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/files/stat", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+e.h.token)
		w := httptest.NewRecorder()
		e.h.server.Handler().ServeHTTP(w, r)
		return w
	}
	paths, _ := json.Marshal([]string{e.path, missing, "relative.txt"})
	w := stat(`{"paths":` + string(paths) + `}`)
	if w.Code != http.StatusOK {
		t.Fatalf("stat = %d: %s", w.Code, w.Body.String())
	}
	var got struct{ Files []fileStat }
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 3 {
		t.Fatalf("files = %+v", got.Files)
	}
	f := got.Files[0]
	if f.Type != "resource_link" || f.Name != "rec ording.mp4" || f.MimeType != "video/mp4" ||
		f.Size == nil || *f.Size != 4 || f.Error != "" || !strings.HasSuffix(f.URI, "/rec%20ording.mp4") ||
		!strings.HasPrefix(f.URI, "file:///") {
		t.Fatalf("found file = %+v", f)
	}
	if get := e.request(t, http.MethodHead, e.target(), e.h.token); f.ETag == "" || f.ETag != get.Header().Get("ETag") {
		t.Fatalf("stat etag %q, GET etag %q", f.ETag, get.Header().Get("ETag"))
	}
	if got.Files[1].Error != "not_found" || got.Files[1].Size != nil {
		t.Fatalf("missing file = %+v", got.Files[1])
	}
	if got.Files[2].Error != "invalid_path" {
		t.Fatalf("relative path = %+v", got.Files[2])
	}

	tooMany, _ := json.Marshal(map[string][]string{"paths": make([]string, maxStatPaths+1)})
	if w := stat(string(tooMany)); w.Code != http.StatusBadRequest {
		t.Fatalf("%d paths = %d, want 400", maxStatPaths+1, w.Code)
	}
}

func TestFileChangedAfterAdmissionCannotPublishCurrentValidator(t *testing.T) {
	e := newFileEnv(t, "grow.txt")
	if err := os.WriteFile(e.path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, info, err := workspace.OpenHostFile(t.Context(), e.path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.WriteFile(e.path, []byte("new-longer"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	w.Header().Set("Cache-Control", "no-store")
	e.h.server.serveFile(w, httptest.NewRequest(http.MethodGet, e.target(), nil), f, info, filepath.Base(e.path))
	if w.Code != http.StatusConflict || w.Header().Get("ETag") != "" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("changed file = %d %v %q", w.Code, w.Header(), w.Body.String())
	}
	next := requestFileHeaders(t, e, http.MethodGet, nil)
	if next.Code != http.StatusOK || next.Body.String() != "new-longer" {
		t.Fatalf("retry = %d %q", next.Code, next.Body.String())
	}
}

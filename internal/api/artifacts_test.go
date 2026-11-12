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

	"github.com/arafatamim/ferngeist-acp-gateway/internal/turns"
	"github.com/coder/websocket"
)

type artifactEnv struct {
	h          *resilientTestHarness
	connection runtimeConnectResponse
	sid        string
	path       string
}

func newArtifactEnv(t *testing.T, name string) *artifactEnv {
	t.Helper()
	return newArtifactEnvWithAdvertisement(t, name, true)
}

func newArtifactEnvWithAdvertisement(t *testing.T, name string, structured bool) *artifactEnv {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("artifact retrieval requires Linux or macOS")
	}
	path := filepath.Join(t.TempDir(), name)
	uri := ""
	reply := "[Recorded test](" + path + ")"
	if structured {
		uri = (&url.URL{Scheme: "file", Path: path}).String()
		reply = "Here is your artifact."
	}
	t.Setenv("FERNGEIST_MOCK_RESOURCE_URI", uri)
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Reply through the actual mock-agent LLM seam, without gateway grant hooks.
		type message struct {
			Content string `json:"content"`
		}
		type choice struct {
			Message message `json:"message"`
		}
		if err := json.NewEncoder(w).Encode(struct {
			Choices []choice `json:"choices"`
		}{[]choice{{message{reply}}}}); err != nil {
			t.Errorf("encode mock LLM reply: %v", err)
		}
	}))
	t.Cleanup(llm.Close)
	t.Setenv("FERNGEIST_MOCK_LLM_ENDPOINT", llm.URL)
	h := newResilientTestHarness(t)
	connection, sid := advertiseArtifact(t, h, h.token)
	return &artifactEnv{h: h, connection: connection, sid: sid, path: path}
}

func advertiseArtifact(t *testing.T, h *resilientTestHarness, token string) (runtimeConnectResponse, string) {
	t.Helper()
	connection := connectAgent(t, h.server, token, "mock-acp")
	copy := *h
	copy.runtimeID = connection.RuntimeID
	ws := copy.dialSessionWS(connection.SessionID, connection.AttachToken)
	t.Cleanup(func() { _ = ws.Close(websocket.StatusNormalClosure, "test finished") })
	sendWSMessage(t, ws, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
	readWSMessage(t, ws)
	newSession, err := json.Marshal(struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
		Params struct {
			Cwd string `json:"cwd"`
		} `json:"params"`
	}{ID: 2, Method: "session/new", Params: struct {
		Cwd string `json:"cwd"`
	}{t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	sendWSMessage(t, ws, string(newSession))
	var result struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(readWSMessage(t, ws).Result, &result); err != nil || result.SessionID == "" {
		t.Fatalf("session/new = %v, %v", result, err)
	}
	readWSMessage(t, ws)
	sendWSMessage(t, ws, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"`+result.SessionID+`","prompt":[{"type":"text","text":"Create an artifact"}]}}`)
	for {
		if msg := readWSMessage(t, ws); len(msg.ID) > 0 {
			break
		}
	}
	return connection, result.SessionID
}

func (e *artifactEnv) target() string {
	return "/v1/runtimes/" + e.connection.RuntimeID + "/artifacts?acpSessionId=" + url.QueryEscape(e.sid) + "&path=" + url.QueryEscape(e.path)
}

func (e *artifactEnv) request(t *testing.T, method, target, token string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Range", "bytes=1-2")
	r.Header.Set("If-Range", `"old-version"`)
	w := httptest.NewRecorder()
	e.h.server.Handler().ServeHTTP(w, r)
	assertArtifactHeaders(t, w.Header())
	return w
}

func TestArtifactsReturnCompleteBytesAndHeadMetadata(t *testing.T) {
	for _, structured := range []bool{true, false} {
		name := "structured"
		if !structured {
			name = "Markdown"
		}
		t.Run(name, func(t *testing.T) {
			e := newArtifactEnvWithAdvertisement(t, "live-full-history-test.mp4", structured)
			assertCompleteArtifact(t, e)
		})
	}
}

func assertCompleteArtifact(t *testing.T, e *artifactEnv) {
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
			t.Fatal("artifact bytes differ")
		}
		if method == http.MethodHead && w.Body.Len() != 0 {
			t.Fatal("HEAD returned a body")
		}
	}
	// Existing turns remain visible through their own contract.
	awaitTurn(t, e.h.server, e.h.token, e.sid, func(st turns.Status) bool { return st.State == turns.StateSucceeded })
}

func TestArtifactsEnforceDeviceContext(t *testing.T) {
	e := newArtifactEnv(t, "artifact.bin")
	if err := os.WriteFile(e.path, []byte("artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := pairDeviceResponse(t, e.h.server)
	b, bSid := advertiseArtifact(t, e.h, second.Token)
	bTarget := strings.Replace(e.target(), e.connection.RuntimeID, b.RuntimeID, 1)
	bTarget = strings.Replace(bTarget, url.QueryEscape(e.sid), url.QueryEscape(bSid), 1)
	for _, tc := range []struct {
		name, target, token string
		status              int
	}{
		{"own", e.target(), e.h.token, 200},
		{"other owns", bTarget, second.Token, 200},
		{"foreign", bTarget, e.h.token, 404},
		{"unknown runtime", strings.Replace(bTarget, b.RuntimeID, "unknown", 1), e.h.token, 404},
		{"wrong ACP", strings.Replace(e.target(), url.QueryEscape(e.sid), "wrong", 1), e.h.token, 404},
		{"unadvertised existing", e.target() + "-different", e.h.token, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := e.request(t, http.MethodGet, tc.target, tc.token)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
		})
	}
}

func TestArtifactsReportAdmissionErrorsWithoutHostPaths(t *testing.T) {
	e := newArtifactEnv(t, "artifact.bin")
	if err := os.WriteFile(e.path, []byte("artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Evidence rejection precedes lookup for both existing and missing paths.
	other := e.path + "-different"
	if err := os.WriteFile(other, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := e.request(t, http.MethodGet, e.target()+"-different", e.h.token)
	if w.Code != 403 {
		t.Fatalf("existing unadvertised = %d", w.Code)
	}
	if err := os.Remove(e.path); err != nil {
		t.Fatal(err)
	}
	w = e.request(t, http.MethodGet, e.target(), e.h.token)
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
		t.Fatalf("admin artifact route = %d", admin.Code)
	}
	credential, err := e.h.server.pairing.ValidateCredential(e.h.token)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.h.server.sessionSvc.Close(t.Context(), e.connection.SessionID, credential.DeviceID); err != nil {
		t.Fatal(err)
	}
	if w := e.request(t, http.MethodGet, e.target(), e.h.token); w.Code != 404 {
		t.Fatalf("closed context = %d", w.Code)
	}
}

func TestArtifactAttachmentNamesAreSafe(t *testing.T) {
	for _, name := range []string{`quoted"name.mp4`, `back\slash.mp4`, "résumé 你好.mp4"} {
		t.Run(name, func(t *testing.T) {
			e := newArtifactEnv(t, name)
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

func TestArtifactsRequireAuthenticationBeforeSelectors(t *testing.T) {
	s := newTestServer()
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		r := httptest.NewRequest(method, "/v1/runtimes/unknown/artifacts?path=%ZZ", nil)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d, want 401", method, w.Code)
		}
		assertArtifactHeaders(t, w.Header())
	}
}

func TestArtifactsRejectInvalidSelectors(t *testing.T) {
	s := newTestServer()
	token := pairDevice(t, s)
	valid := "path=%2Ftmp%2Fvideo.mp4&acpSessionId=opaque"
	queries := []string{"", "path=/tmp/x", "acpSessionId=a", valid + "&path=/tmp/y", valid + "&acpSessionId=b", valid + "&cwd=/tmp", valid + "&deviceId=b", valid + "&deviceID=b", valid + "&PATH=/tmp/x", valid + "&bad=%ZZ", valid + ";bad=x"}
	for _, path := range []string{"relative", "/tmp/../tmp/x", "/tmp/", "/tmp/\n", "/tmp/\x00", "/tmp/\u200b", "/" + strings.Repeat("x", 4096), "/tmp/\xff"} {
		queries = append(queries, "acpSessionId=a&path="+url.QueryEscape(path))
	}
	for _, id := range []string{"", " padded", "padded ", "a\n", "a\u200b", "\xff", strings.Repeat("x", 1025)} {
		queries = append(queries, "path=/tmp/x&acpSessionId="+url.QueryEscape(id))
	}
	for _, query := range queries {
		r := httptest.NewRequest(http.MethodGet, "/v1/runtimes/unknown/artifacts?"+query, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("query %q: status = %d, want 400: %s", query, w.Code, w.Body.String())
		}
		assertArtifactHeaders(t, w.Header())
	}
}

func assertArtifactHeaders(t *testing.T, h http.Header) {
	t.Helper()
	if h.Get("Cache-Control") != "no-store" || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("missing artifact safety headers: %v", h)
	}
	if h.Get("ETag") != "" || h.Get("Last-Modified") != "" {
		t.Fatalf("unexpected artifact validator: %v", h)
	}
}

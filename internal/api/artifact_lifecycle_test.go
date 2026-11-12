package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type blockedArtifact struct {
	*httptest.ResponseRecorder
	entered, resume, expired, done chan struct{}
	deadlines                      chan time.Time
	once, expireOnce               sync.Once
	cancel                         context.CancelFunc
}

type networkArtifactGate struct {
	http.ResponseWriter
	entered, resume chan struct{}
	once            sync.Once
}

func (g *networkArtifactGate) Unwrap() http.ResponseWriter { return g.ResponseWriter }

func (g *networkArtifactGate) Write(p []byte) (int, error) {
	g.once.Do(func() { close(g.entered); <-g.resume })
	return g.ResponseWriter.Write(p)
}

func TestArtifactNetworkClientDetectsShortBody(t *testing.T) {
	e := newArtifactEnv(t, "video.mp4")
	if err := os.WriteFile(e.path, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	entered, resume := make(chan struct{}), make(chan struct{})
	var release sync.Once
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.h.server.Handler().ServeHTTP(&networkArtifactGate{ResponseWriter: w, entered: entered, resume: resume}, r)
	}))
	srv.Config.WriteTimeout = time.Nanosecond
	srv.Start()
	t.Cleanup(srv.Close)
	t.Cleanup(func() { release.Do(func() { close(resume) }) })
	result := make(chan error, 1)
	go func() {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+e.target(), nil)
		if err != nil {
			result <- err
			return
		}
		r.Header.Set("Authorization", "Bearer "+e.h.token)
		response, err := srv.Client().Do(r)
		if err != nil {
			result <- err
			return
		}
		defer response.Body.Close()
		_, err = io.Copy(io.Discard, response.Body)
		result <- err
	}()
	select {
	case <-entered:
	case err := <-result:
		t.Fatalf("transfer failed before body: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("transfer never started")
	}
	if err := os.Truncate(e.path, 0); err != nil {
		t.Fatal(err)
	}
	release.Do(func() { close(resume) })
	select {
	case err := <-result:
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("short body = %v, want unexpected EOF", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("short body never finished")
	}
}

func (b *blockedArtifact) Write(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.resume:
		return b.ResponseRecorder.Write(p)
	case <-b.expired:
		return 0, context.Canceled
	}
}

func (b *blockedArtifact) SetWriteDeadline(t time.Time) error {
	if t.Before(time.Now()) {
		b.expireOnce.Do(func() { close(b.expired) })
	}
	b.deadlines <- t
	return nil
}

func blockArtifact(t *testing.T, e *artifactEnv, target, token string) *blockedArtifact {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	b := &blockedArtifact{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), resume: make(chan struct{}), expired: make(chan struct{}), done: make(chan struct{}), deadlines: make(chan time.Time, 2), cancel: cancel}
	r := httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+token)
	go func() { defer close(b.done); e.h.server.Handler().ServeHTTP(b, r) }()
	t.Cleanup(func() { cancel(); awaitArtifactDone(t, b) })
	select {
	case <-b.entered:
	case <-b.done:
		t.Fatalf("transfer did not start: %d %s", b.Code, b.Body.String())
	case <-time.After(5 * time.Second):
		t.Fatal("transfer never started")
	}
	return b
}

func awaitArtifactDone(t *testing.T, b *blockedArtifact) {
	t.Helper()
	select {
	case <-b.done:
	case <-time.After(5 * time.Second):
		t.Fatal("transfer did not release")
	}
}

func TestArtifactCapacityCoversWholeTransfersAndLeavesTurnsAvailable(t *testing.T) {
	e := newArtifactEnv(t, "video.mp4")
	if err := os.WriteFile(e.path, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	other, third := pairDevice(t, e.h.server), pairDevice(t, e.h.server)
	a := blockArtifact(t, e, e.target(), e.h.token)
	b := blockArtifact(t, e, e.target(), e.h.token)
	if w := e.request(t, http.MethodHead, e.target(), e.h.token); w.Code != 429 || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("device capacity = %d, %v", w.Code, w.Header())
	}
	second, sid := advertiseArtifact(t, e.h, other)
	otherTarget := "/v1/runtimes/" + second.RuntimeID + "/artifacts?acpSessionId=" + sid + "&path=" + url.QueryEscape(e.path)
	c := blockArtifact(t, e, otherTarget, other)
	d := blockArtifact(t, e, otherTarget, other)
	if w := e.request(t, http.MethodGet, e.target(), third); w.Code != 429 {
		t.Fatalf("global capacity = %d", w.Code)
	}
	turns := dialTurnStream(t, e.h.httpSrv, e.h.token)
	readTurnStreamOpen(t, turns)
	_ = turns.Close(websocket.StatusNormalClosure, "test finished")
	a.cancel()
	awaitArtifactDone(t, a)
	// After cancellation a full response is admitted again for that device.
	if w := e.request(t, http.MethodGet, e.target(), e.h.token); w.Code != 200 {
		t.Fatalf("released capacity = %d", w.Code)
	}
	for _, stream := range []*blockedArtifact{b, c, d} {
		stream.cancel()
		awaitArtifactDone(t, stream)
	}
	e.h.server.artifactMu.Lock()
	defer e.h.server.artifactMu.Unlock()
	if len(e.h.server.artifactDevices) != 0 || e.h.server.artifactCount != 0 {
		t.Fatal("idle artifact counters retained")
	}
}

func TestArtifactRevocationAppliesOnlyToNewRequests(t *testing.T) {
	e := newArtifactEnv(t, "video.mp4")
	if err := os.WriteFile(e.path, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := blockArtifact(t, e, e.target(), e.h.token)
	deadline := <-b.deadlines
	if left := time.Until(deadline); left < 9*time.Minute || left > 10*time.Minute {
		t.Fatalf("transfer deadline = %s away", left)
	}
	credential, err := e.h.server.pairing.ValidateCredential(e.h.token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.server.pairing.RevokeDevice(credential.DeviceID); err != nil {
		t.Fatal(err)
	}
	if w := e.request(t, http.MethodGet, e.target(), e.h.token); w.Code != 401 {
		t.Fatalf("revoked request = %d", w.Code)
	}
	close(b.resume)
	awaitArtifactDone(t, b)
	if b.Body.String() != "video" {
		t.Fatalf("admitted body = %q", b.Body.String())
	}
}

func TestArtifactShutdownCancelsAdmittedTransfers(t *testing.T) {
	e := newArtifactEnv(t, "video.mp4")
	if err := os.WriteFile(e.path, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := blockArtifact(t, e, e.target(), e.h.token)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := e.h.server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	awaitArtifactDone(t, b)
	if w := e.request(t, http.MethodGet, e.target(), e.h.token); w.Code != 503 {
		t.Fatalf("post-shutdown = %d", w.Code)
	}
}

func TestArtifactTransferFreezesLengthAndDetectsShrink(t *testing.T) {
	for _, shrink := range []bool{false, true} {
		t.Run(map[bool]string{false: "growth", true: "shrink"}[shrink], func(t *testing.T) {
			e := newArtifactEnv(t, "video.mp4")
			if err := os.WriteFile(e.path, make([]byte, 1<<20), 0o600); err != nil {
				t.Fatal(err)
			}
			b := blockArtifact(t, e, e.target(), e.h.token)
			size := int64(2 << 20)
			if shrink {
				size = 0
			}
			if err := os.Truncate(e.path, size); err != nil {
				t.Fatal(err)
			}
			close(b.resume)
			awaitArtifactDone(t, b)
			if b.Header().Get("Content-Length") != "1048576" {
				t.Fatalf("length = %s", b.Header().Get("Content-Length"))
			}
			if (!shrink && b.Body.Len() != 1<<20) || (shrink && b.Body.Len() >= 1<<20) {
				t.Fatalf("body length = %d (shrink %v)", b.Body.Len(), shrink)
			}
		})
	}
}

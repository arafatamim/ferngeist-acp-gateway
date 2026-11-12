package api

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/session"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/workspace"
)

const artifactTransferTimeout = 10 * time.Minute

func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	credential, ok := s.requireGatewayScope(w, r, pairing.ScopeRead)
	if !ok {
		return
	}
	path, sid, err := artifactSelectors(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid artifact selectors")
		return
	}
	if s.sessionSvc == nil || s.artifactCtx.Err() != nil {
		writeError(w, http.StatusServiceUnavailable, "artifact service unavailable")
		return
	}
	release, ok := s.acquireArtifactSlot(credential.DeviceID)
	if !ok {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "artifact capacity reached")
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), artifactTransferTimeout)
	defer cancel()
	stop := context.AfterFunc(s.artifactCtx, cancel)
	defer stop()
	r = r.WithContext(ctx)
	if !s.authorizeArtifact(w, r, credential.DeviceID, sid, path) {
		return
	}
	f, info, err := workspace.OpenArtifact(ctx, path)
	if err != nil {
		writeWorkspaceError(w, err)
		return
	}
	defer f.Close()
	s.serveArtifact(w, r, f, info, filepath.Base(path))
}

func artifactSelectors(raw string) (string, string, error) {
	q, err := url.ParseQuery(raw)
	if err != nil {
		return "", "", err
	}
	// An explicit selector set rejects device/cwd overrides and case aliases
	// instead of letting a caller believe an ignored selector was enforced.
	if len(q) != 2 || len(q["path"]) != 1 || len(q["acpSessionId"]) != 1 {
		return "", "", errors.New("exactly one path and acpSessionId required")
	}
	path, sid := q["path"][0], q["acpSessionId"][0]
	if err := workspace.ValidateArtifactPath(path); err != nil {
		return "", "", err
	}
	if sid == "" || len(sid) > 1024 || !utf8.ValidString(sid) || strings.TrimSpace(sid) != sid ||
		strings.ContainsFunc(sid, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) {
		return "", "", errors.New("invalid ACP session selector")
	}
	return path, sid, nil
}

func (s *Server) authorizeArtifact(w http.ResponseWriter, r *http.Request, deviceID, sid, path string) bool {
	advertised, err := s.sessionSvc.ArtifactAdvertised(deviceID, r.PathValue("runtimeId"), sid, path)
	switch {
	case errors.Is(err, session.ErrDeviceMismatch), errors.Is(err, session.ErrSessionNotFound):
		writeError(w, http.StatusNotFound, "artifact context not found")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not authorize artifact")
	case !advertised:
		writeError(w, http.StatusForbidden, "artifact was not advertised")
	default:
		return true
	}
	return false
}

func (s *Server) acquireArtifactSlot(deviceID string) (func(), bool) {
	s.artifactMu.Lock()
	defer s.artifactMu.Unlock()
	if s.artifactCount >= 4 || s.artifactDevices[deviceID] >= 2 {
		return nil, false
	}
	s.artifactCount++
	s.artifactDevices[deviceID]++
	return func() {
		s.artifactMu.Lock()
		defer s.artifactMu.Unlock()
		s.artifactCount--
		s.artifactDevices[deviceID]--
		if s.artifactDevices[deviceID] == 0 {
			delete(s.artifactDevices, deviceID)
		}
	}, true
}

func (s *Server) serveArtifact(w http.ResponseWriter, r *http.Request, f *os.File, info os.FileInfo, name string) {
	controller := http.NewResponseController(w)
	deadline, _ := r.Context().Deadline() // handleArtifact always supplies the absolute bound.
	if err := controller.SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		writeError(w, http.StatusInternalServerError, "could not prepare artifact transfer")
		return
	}
	// Cancellation owns the descriptor and wakes a blocked network write.
	// Join a running callback before returning so it cannot touch a later request.
	done := make(chan struct{})
	stop := context.AfterFunc(r.Context(), func() {
		f.Close()
		// A disconnected peer or test recorder may no longer accept deadlines.
		_ = controller.SetWriteDeadline(time.Now())
		close(done)
	})
	defer func() {
		if !stop() {
			<-done
		}
	}()
	if r.Context().Err() != nil {
		return
	}
	contentType := mime.TypeByExtension(filepath.Ext(name))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", artifactDisposition(name))
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	n, err := io.CopyN(w, io.NewSectionReader(f, 0, info.Size()), info.Size())
	if err != nil {
		// Keep the promised Content-Length: clients detect a short response.
		// Filesystem error strings can contain the host path, so omit them.
		s.logger.Warn("artifact transfer incomplete", "bytes", n, "size", info.Size())
	}
}

func artifactDisposition(name string) string {
	fallback := strings.Map(func(r rune) rune {
		if r < 32 || r > 126 || r == '\\' {
			return '_'
		}
		return r
	}, name)
	params := map[string]string{"filename": fallback}
	if fallback != name {
		params["filename*"] = "UTF-8''" + strings.ReplaceAll(url.QueryEscape(name), "+", "%20")
	}
	return mime.FormatMediaType("attachment", params)
}

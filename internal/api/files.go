package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/workspace"
)

const fileTransferTimeout = 10 * time.Minute

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	credential, ok := s.requireHostAccess(w, r)
	if !ok {
		return
	}
	path, err := fileSelector(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid file selector")
		return
	}
	if s.fileCtx.Err() != nil {
		writeError(w, http.StatusServiceUnavailable, "file service unavailable")
		return
	}
	release, ok := s.acquireFileSlot(credential.DeviceID)
	if !ok {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "file capacity reached")
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), fileTransferTimeout)
	defer cancel()
	stop := context.AfterFunc(s.fileCtx, cancel)
	defer stop()
	r = r.WithContext(ctx)
	f, info, err := workspace.OpenHostFile(ctx, path)
	if err != nil {
		writeWorkspaceError(w, err)
		return
	}
	defer f.Close()
	s.serveFile(w, r, f, info, filepath.Base(path))
}

const maxStatPaths = 64

// fileStat is an ACP ResourceLink content block, plus a gateway-only error
// code that plain ACP readers ignore.
type fileStat struct {
	Type     string `json:"type"`
	URI      string `json:"uri"`
	Name     string `json:"name"`
	MimeType string `json:"mimeType,omitempty"`
	Size     *int64 `json:"size,omitempty"`
	ETag     string `json:"etag,omitempty"`
	Error    string `json:"error,omitempty"`
}

// handleFileStat describes files with the same checks and Content-Type a GET
// of /v1/files would use. It takes no transfer slot: each open is closed
// before the next, and the path cap bounds the work per request.
func (s *Server) handleFileStat(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if _, ok := s.requireHostAccess(w, r); !ok {
		return
	}
	var req struct {
		Paths []string `json:"paths"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxStatPaths*8192)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Paths) == 0 || len(req.Paths) > maxStatPaths {
		writeError(w, http.StatusBadRequest, "body must be {\"paths\": [...]} with 1 to 64 paths")
		return
	}
	if s.fileCtx.Err() != nil {
		writeError(w, http.StatusServiceUnavailable, "file service unavailable")
		return
	}
	files := make([]fileStat, len(req.Paths))
	for i, path := range req.Paths {
		files[i] = statFile(r.Context(), path)
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

func statFile(ctx context.Context, path string) fileStat {
	name := filepath.Base(path)
	stat := fileStat{Type: "resource_link", URI: fileURI(path), Name: name}
	f, info, err := workspace.OpenHostFile(ctx, path)
	if err != nil {
		stat.Error = statErrorCode(err)
		return stat
	}
	defer f.Close()
	etag, stable := fileETag(f, info)
	if !stable {
		stat.Error = "busy"
		return stat
	}
	size := info.Size()
	stat.Size = &size
	stat.MimeType = fileContentType(f, size, name)
	stat.ETag = etag
	return stat
}

func statErrorCode(err error) string {
	var workspaceError workspace.Error
	if !errors.As(err, &workspaceError) {
		return "unavailable"
	}
	switch workspaceError.StatusCode() {
	case http.StatusBadRequest:
		return "invalid_path" // also special, nonregular and reparse paths
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusConflict:
		return "busy"
	case http.StatusRequestEntityTooLarge:
		return "too_large"
	default:
		return "unavailable"
	}
}

// fileURI spells an absolute host path as a file URI: /a b -> file:///a%20b,
// C:\a -> file:///C:/a.
func fileURI(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

func fileSelector(raw string) (string, error) {
	q, err := url.ParseQuery(raw)
	if err != nil {
		return "", err
	}
	// An explicit selector set rejects device/cwd overrides and case aliases
	// instead of letting a caller believe an ignored selector was enforced.
	if len(q) != 1 || len(q["path"]) != 1 {
		return "", errors.New("exactly one path required")
	}
	path := q["path"][0]
	if err := workspace.ValidateHostFilePath(path); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Server) acquireFileSlot(deviceID string) (func(), bool) {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	if s.fileCount >= 4 || s.fileDevices[deviceID] >= 2 {
		return nil, false
	}
	s.fileCount++
	s.fileDevices[deviceID]++
	return func() {
		s.fileMu.Lock()
		defer s.fileMu.Unlock()
		s.fileCount--
		s.fileDevices[deviceID]--
		if s.fileDevices[deviceID] == 0 {
			delete(s.fileDevices, deviceID)
		}
	}, true
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, f *os.File, info os.FileInfo, name string) {
	controller := http.NewResponseController(w)
	deadline, _ := r.Context().Deadline() // handleFile always supplies the absolute bound.
	if err := controller.SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		writeError(w, http.StatusInternalServerError, "could not prepare file transfer")
		return
	}
	// Cancellation owns the descriptor and wakes a blocked network write.
	// handleFile's deferred Close then just returns an ignored error.
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
	if !prepareFileCache(w, r, f, info) {
		return
	}
	offset, length, status := fileRange(r, info.Size())
	w.Header().Set("Accept-Ranges", "bytes")
	if status == http.StatusRequestedRangeNotSatisfiable {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Del("ETag")
		w.Header().Del("Last-Modified")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", info.Size()))
		writeError(w, status, "invalid or unsatisfiable file range")
		return
	}
	if status == http.StatusPartialContent {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, info.Size()))
	}
	w.Header().Set("Content-Type", fileContentType(f, info.Size(), name))
	w.Header().Set("Content-Disposition", fileDisposition(name))
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	n, err := io.CopyN(w, io.NewSectionReader(f, offset, length), length)
	if err != nil {
		// Keep the promised Content-Length: clients detect a short response.
		// Filesystem error strings can contain the host path, so omit them.
		s.logger.Warn("file transfer incomplete", "bytes", n, "size", length)
	}
}

func prepareFileCache(w http.ResponseWriter, r *http.Request, f *os.File, info os.FileInfo) bool {
	etag, stable := fileETag(f, info)
	if !stable {
		writeError(w, http.StatusConflict, "file changed while preparing transfer")
		return false
	}
	// Without a validator the client could not revalidate a stored copy.
	if etag == "" {
		return true
	}
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("ETag", etag)
	w.Header().Set("Last-Modified", info.ModTime().UTC().Format(http.TimeFormat))
	if etagListMatches(r.Header.Values("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return false
	}
	return true
}

// fileETag is weak: equal file metadata does not prove equal bytes, so it
// may answer If-None-Match but never satisfies If-Range's strong comparison.
func fileETag(f *os.File, admitted os.FileInfo) (string, bool) {
	version := workspace.HostFileVersion(f)
	current, err := f.Stat()
	// Bracket the metadata read so the validator cannot describe a different
	// size or modification time from the admitted representation.
	if err != nil || current.Size() != admitted.Size() || !current.ModTime().Equal(admitted.ModTime()) ||
		workspace.HostFileVersion(f) != version {
		return "", false
	}
	if version == "" {
		return "", true
	}
	sum := sha256.Sum256([]byte(version))
	return `W/"` + base64.RawURLEncoding.EncodeToString(sum[:16]) + `"`, true
}

// etagListMatches applies If-None-Match's weak comparison (RFC 9110 13.1.2).
func etagListMatches(values []string, etag string) bool {
	opaque := strings.TrimPrefix(etag, "W/")
	for _, value := range values {
		for _, tag := range strings.Split(value, ",") {
			tag = strings.TrimSpace(tag)
			if tag == "*" || strings.TrimPrefix(tag, "W/") == opaque {
				return true
			}
		}
	}
	return false
}

func fileContentType(f *os.File, size int64, name string) string {
	byExt := mime.TypeByExtension(filepath.Ext(name))
	if textMediaType(byExt) {
		return byExt
	}
	// Sniff with git's NUL heuristic so viewers can tell text from binary:
	// unknown extensions are mostly source files, and host mime tables
	// mislabel some (.ts as video/mp2t, .tsx as application/x-tiled-tsx).
	// Overriding a known type also needs valid UTF-8; the last 3 bytes may
	// hold a rune cut by the sniff window.
	head := make([]byte, min(size, 8000))
	if n, _ := f.ReadAt(head, 0); n == len(head) && !workspace.IsBinary(head) &&
		(byExt == "" || utf8.Valid(head[:len(head)-min(len(head), 3)])) {
		return "text/plain; charset=utf-8"
	}
	if byExt != "" {
		return byExt
	}
	return "application/octet-stream"
}

// textMediaType matches the types docs/api.md tells clients to show as text.
func textMediaType(contentType string) bool {
	t, _, _ := strings.Cut(contentType, ";")
	return strings.HasPrefix(t, "text/") || t == "application/json" || t == "application/xml" ||
		strings.HasSuffix(t, "+json") || strings.HasSuffix(t, "+xml")
}

func fileDisposition(name string) string {
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

package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
)

func runGitCLI(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// TestWorkspaceEndpoints_EndToEnd drives a real resilient session, captures the
// runtime's project cwd from a session/new frame, and asserts the file, git
// status, and git diff endpoints return the expected data for a temp git repo.
func TestWorkspaceEndpoints_EndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	// Temp git repo: one committed file (committed.txt), one untracked (wip.txt),
	// and one modified after commit (edited.txt).
	proj := t.TempDir()
	runGitCLI(t, proj, "init", "-q")
	runGitCLI(t, proj, "config", "user.email", "t@t")
	runGitCLI(t, proj, "config", "user.name", "t")
	runGitCLI(t, proj, "config", "commit.gpgSign", "false")
	if err := os.WriteFile(filepath.Join(proj, "committed.txt"), []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "edited.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCLI(t, proj, "add", "committed.txt", "edited.txt")
	runGitCLI(t, proj, "commit", "-qm", "init")
	if err := os.WriteFile(filepath.Join(proj, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "edited.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A tracked file renamed in the working tree: status must report the
	// destination path, and the whole-tree diff must surface the destination
	// content instead of a phantom deletion of the source.
	if err := os.WriteFile(filepath.Join(proj, "renamed_src.txt"), []byte("renamed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCLI(t, proj, "add", "renamed_src.txt")
	runGitCLI(t, proj, "commit", "-qm", "add renamed_src")
	// A partially staged file: part of the edit is staged, the rest is not.
	// Status counts must cover the whole HEAD → worktree delta (what /git/diff
	// compares), not just the unstaged portion. Commit the baseline first so
	// the later staged + unstaged edits happen against a real HEAD.
	if err := os.WriteFile(filepath.Join(proj, "partial.txt"), []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCLI(t, proj, "add", "partial.txt")
	runGitCLI(t, proj, "commit", "-qm", "add partial")
	if err := os.WriteFile(filepath.Join(proj, "partial.txt"), []byte("a\nX\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCLI(t, proj, "add", "partial.txt")
	if err := os.WriteFile(filepath.Join(proj, "partial.txt"), []byte("a\nX\nY\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCLI(t, proj, "mv", "renamed_src.txt", "renamed_dst.txt")
	// A file added to the index (status "A") is invisible to `git diff --numstat`
	// (worktree vs index is clean); only `--cached` sees its additions.
	if err := os.WriteFile(filepath.Join(proj, "staged.txt"), []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCLI(t, proj, "add", "staged.txt")
	// A binary untracked file whose name contains non-ASCII (git would quote the
	// path without core.quotePath=false) must still be detected as binary.
	binaryName := "clip é 😎.bin"
	if err := os.WriteFile(filepath.Join(proj, binaryName), []byte{0x00, 0x01, 0x02, 0x03}, 0o644); err != nil {
		t.Fatal(err)
	}

	h := newResilientTestHarness(t)
	resp := h.connectResilient()
	ws := h.dialSessionWS(resp.SessionID, resp.AttachToken)
	defer ws.CloseNow()

	// Complete the ACP handshake, then send session/new with the project cwd so
	// the pump captures it. The frame is built with json.Marshal so the Windows
	// absolute path (backslashes) is escaped into valid JSON.
	sendWSMessage(t, ws, `{"jsonrpc":"2.0","id":"1","method":"initialize","params":{"protocolVersion":1,"capabilities":{},"clientInfo":{"name":"test-client","version":"1.0.0"}}}`)
	readWSMessage(t, ws) // initialize result
	sendWSMessage(t, ws, `{"jsonrpc":"2.0","id":"2","method":"authenticate","params":{}}`)
	readWSMessage(t, ws) // authenticate result
	newFrame, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      10,
		"method":  "session/new",
		"params":  map[string]any{"cwd": proj},
	})
	if err != nil {
		t.Fatal(err)
	}
	sendWSMessage(t, ws, string(newFrame))
	readWSMessage(t, ws) // session/new result
	readWSMessage(t, ws) // session_info_update notification

	do := func(t *testing.T, path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+h.token)
		rec := httptest.NewRecorder()
		h.server.Handler().ServeHTTP(rec, req)
		return rec
	}

	// 1. File reader returns committed file content as an ACP TextResourceContents.
	rec := do(t, "/v1/runtimes/"+h.runtimeID+"/files?path=committed.txt")
	if rec.Code != http.StatusOK {
		t.Fatalf("files status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var fr fileReadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &fr); err != nil {
		t.Fatal(err)
	}
	if fr.Text != "committed\n" || fr.Truncated {
		t.Fatalf("file response = %+v, want text %q untruncated", fr, "committed\n")
	}
	if fr.Uri == "" {
		t.Fatalf("file response Uri empty: %+v", fr)
	}
	if fr.Size != len("committed\n") {
		t.Fatalf("file response Size = %d, want %d", fr.Size, len("committed\n"))
	}
	if !strings.HasPrefix(fr.Uri, "file://") {
		t.Fatalf("Uri = %q, want file:// prefix", fr.Uri)
	}

	// 1b. Binary file returns a BlobResourceContents whose blob base64-decodes
	// to the original bytes.
	rec = do(t, "/v1/runtimes/"+h.runtimeID+"/files?path=clip%20%C3%A9%20%F0%9F%98%8E.bin")
	if rec.Code != http.StatusOK {
		t.Fatalf("binary files status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var br blobReadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &br); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(br.Blob)
	if err != nil {
		t.Fatalf("blob base64 decode: %v", err)
	}
	if !bytes.Equal(decoded, []byte{0x00, 0x01, 0x02, 0x03}) {
		t.Fatalf("blob decoded = %v, want original bytes", decoded)
	}
	if br.Size != 4 || br.Truncated {
		t.Fatalf("binary response = %+v, want size 4 untruncated", br)
	}

	// 2. File reader rejects path traversal.
	rec = do(t, "/v1/runtimes/"+h.runtimeID+"/files?path=../../outside.txt")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("traversal status = %d, want 400", rec.Code)
	}

	// 3. Git status lists the untracked and modified files.
	rec = do(t, "/v1/runtimes/"+h.runtimeID+"/git/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("git status code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var gs gitStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &gs); err != nil {
		t.Fatal(err)
	}
	if gs.Branch == "" {
		t.Fatalf("branch = %q, want non-empty", gs.Branch)
	}
	paths := map[string]bool{}
	byPath := map[string]gitChangedFile{}
	for _, f := range gs.Changed {
		paths[f.Path] = true
		byPath[f.Path] = f
	}
	if !paths["wip.txt"] || !paths["edited.txt"] {
		t.Fatalf("changed files = %+v, want wip.txt and edited.txt", gs.Changed)
	}
	// The binary untracked file with a non-ASCII name must be present, listed
	// unquoted, and flagged as binary.
	if f, ok := byPath["clip é 😎.bin"]; !ok || !f.Binary {
		t.Fatalf("binary file not detected as binary: %+v (present=%v)", f, ok)
	}

	// Per-file line counts: edited.txt had one line replaced (old\n -> new\n),
	// and wip.txt is a single-line untracked file.
	if f := byPath["edited.txt"]; f.Added != 1 || f.Removed != 1 || f.Binary {
		t.Fatalf("edited.txt counts = %+v, want {added:1 removed:1}", f)
	}
	// A rename must report the destination path (renamed_dst.txt), not the
	// source (renamed_src.txt).
	if f, ok := byPath["renamed_dst.txt"]; !ok || f.Status != "R" {
		t.Fatalf("rename entry = %+v (present=%v), want {renamed_dst.txt R}", f, ok)
	}
	if _, ok := byPath["renamed_src.txt"]; ok {
		t.Fatalf("status lists source path renamed_src.txt, want destination only: %+v", byPath["renamed_src.txt"])
	}
	if f := byPath["wip.txt"]; f.Added != 1 || f.Removed != 0 || f.Binary {
		t.Fatalf("wip.txt counts = %+v, want {added:1 removed:0}", f)
	}
	// staged.txt was added to the index (3 lines); its count must come from
	// the --cached diff, not be zero.
	if f := byPath["staged.txt"]; f.Added != 3 || f.Removed != 0 || f.Binary {
		t.Fatalf("staged.txt counts = %+v, want {added:3 removed:0}", f)
	}
	// partial.txt has staged (b->X) plus unstaged (add Y) edits; counts must
	// cover the whole HEAD → worktree delta (2 added, 1 removed), not just the
	// unstaged portion.
	if f := byPath["partial.txt"]; f.Added != 2 || f.Removed != 1 || f.Binary {
		t.Fatalf("partial.txt counts = %+v, want {added:2 removed:1}", f)
	}

	// 4. Git diff for the edited file returns a ToolCallContentDiff with the
	// new text and the committed old text.
	rec = do(t, "/v1/runtimes/"+h.runtimeID+"/git/diff?path=edited.txt")
	if rec.Code != http.StatusOK {
		t.Fatalf("git diff code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var gd acp.ToolCallContentDiff
	if err := json.Unmarshal(rec.Body.Bytes(), &gd); err != nil {
		t.Fatal(err)
	}
	if gd.Type != "diff" {
		t.Fatalf("diff Type = %q, want diff", gd.Type)
	}
	if gd.Path != "edited.txt" {
		t.Fatalf("diff Path = %q, want edited.txt", gd.Path)
	}
	if gd.NewText != "new\n" {
		t.Fatalf("diff NewText = %q, want %q", gd.NewText, "new\n")
	}
	if gd.OldText == nil || *gd.OldText != "old\n" {
		t.Fatalf("diff OldText = %v, want pointer to %q", gd.OldText, "old\n")
	}

	// 4b. Whole-tree diff returns an array with an entry per changed file.
	rec = do(t, "/v1/runtimes/"+h.runtimeID+"/git/diff")
	if rec.Code != http.StatusOK {
		t.Fatalf("git diff whole-tree code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var gds []acp.ToolCallContentDiff
	if err := json.Unmarshal(rec.Body.Bytes(), &gds); err != nil {
		t.Fatal(err)
	}
	diffPaths := map[string]bool{}
	for _, d := range gds {
		diffPaths[d.Path] = true
	}
	if !diffPaths["edited.txt"] || !diffPaths["wip.txt"] {
		t.Fatalf("whole-tree diff paths = %v, want edited.txt and wip.txt", diffPaths)
	}
	// A rename must surface the destination's content, not a phantom deletion
	// of the source.
	if !diffPaths["renamed_dst.txt"] {
		t.Fatalf("whole-tree diff paths = %v, want renamed_dst.txt", diffPaths)
	}
	if diffPaths["renamed_src.txt"] {
		t.Fatalf("whole-tree diff lists source renamed_src.txt, want destination only: %v", diffPaths)
	}
	// The renamed file's entry carries the working content as newText with no
	// oldText (renamed_dst.txt does not exist in HEAD).
	for _, d := range gds {
		if d.Path == "renamed_dst.txt" && (d.NewText != "renamed\n" || d.OldText != nil) {
			t.Fatalf("rename diff entry = %+v, want {newText: renamed\\n, no oldText}", d)
		}
	}
	for _, d := range gds {
		if d.Type != "diff" {
			t.Fatalf("whole-tree diff entry Type = %q, want diff", d.Type)
		}
	}
}

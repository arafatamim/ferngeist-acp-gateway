package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/workspace"
	"github.com/coder/acp-go-sdk"
)

func runGitCLI(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func runGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// workspaceEnv is a runtime wired up for workspace endpoint tests: the
// resilient harness, a request helper already authed with the pair token, and
// the ACP session id the handshake created.
type workspaceEnv struct {
	h            *resilientTestHarness
	do           func(t *testing.T, path string) *httptest.ResponseRecorder
	acpSessionID string
}

// newWorkspaceEnv boots a resilient harness, completes the ACP handshake, then
// sends session/new with the given project cwd so the pump captures it. The
// frame is built with json.Marshal so the Windows absolute path (backslashes)
// is escaped into valid JSON.
func newWorkspaceEnv(t *testing.T, proj string) *workspaceEnv {
	t.Helper()
	env := &workspaceEnv{h: newResilientTestHarness(t)}
	resp := env.h.connectResilient()
	ws := env.h.dialSessionWS(resp.SessionID, resp.AttachToken)
	t.Cleanup(func() { _ = ws.CloseNow() })

	// Complete the ACP handshake, then send session/new with the project cwd.
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
	var newResult struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(readWSMessage(t, ws).Result, &newResult); err != nil || newResult.SessionID == "" {
		t.Fatalf("session/new result: sessionId missing (err %v)", err)
	}
	readWSMessage(t, ws) // session_info_update notification
	env.acpSessionID = newResult.SessionID

	env.do = func(t *testing.T, path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+env.h.token)
		rec := httptest.NewRecorder()
		env.h.server.Handler().ServeHTTP(rec, req)
		return rec
	}
	return env
}

func TestWorkspaceGitStatusCleanTreeReturnsEmptyArray(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	proj := t.TempDir()
	runGitCLI(t, proj, "init", "-q", "-b", "main")
	runGitCLI(t, proj, "config", "user.email", "t@t")
	runGitCLI(t, proj, "config", "user.name", "t")
	runGitCLI(t, proj, "config", "commit.gpgSign", "false")
	runGitCLI(t, proj, "commit", "--allow-empty", "-qm", "init")
	env := newWorkspaceEnv(t, proj)
	for _, query := range []string{"", "?compare=main"} {
		t.Run(query, func(t *testing.T) {
			rec := env.do(t, "/v1/runtimes/"+env.h.runtimeID+"/git/status"+query)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
			}
			var body struct {
				Changed json.RawMessage `json:"changed"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if string(body.Changed) != "[]" {
				t.Fatalf("changed = %s, want []", body.Changed)
			}
		})
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
	runGitCLI(t, proj, "init", "-q", "-b", "main")
	runGitCLI(t, proj, "config", "user.email", "t@t")
	runGitCLI(t, proj, "config", "user.name", "t")
	runGitCLI(t, proj, "config", "commit.gpgSign", "false")
	if err := os.WriteFile(filepath.Join(proj, "committed.txt"), []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "edited.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// base.txt is the compare fixture: the cmp branch changes it, and the
	// working tree later takes cmp's content, so it is dirty against HEAD but
	// identical to cmp.
	if err := os.WriteFile(filepath.Join(proj, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCLI(t, proj, "add", "committed.txt", "edited.txt", "base.txt")
	runGitCLI(t, proj, "commit", "-qm", "init")

	// Compare fixture: the cmp branch changes base.txt and adds cmp-only.txt.
	// The working tree later takes cmp's base.txt content (dirty against HEAD
	// but identical to cmp) and leaves cmp-only.txt absent, so a compare against
	// cmp must show neither as a modification of base.txt.
	runGitCLI(t, proj, "checkout", "-q", "-b", "cmp")
	if err := os.WriteFile(filepath.Join(proj, "base.txt"), []byte("cmp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "cmp-only.txt"), []byte("only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCLI(t, proj, "add", "base.txt", "cmp-only.txt")
	runGitCLI(t, proj, "commit", "-qm", "cmp work")
	cmpSha := runGitOut(t, proj, "rev-parse", "HEAD")
	runGitCLI(t, proj, "checkout", "-q", "main")
	runGitCLI(t, proj, "tag", "v1")
	if err := os.WriteFile(filepath.Join(proj, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "edited.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The worktree holds cmp's base.txt content: dirty against HEAD, identical
	// to cmp, so a compare against cmp must not list it.
	if err := os.WriteFile(filepath.Join(proj, "base.txt"), []byte("cmp\n"), 0o644); err != nil {
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

	env := newWorkspaceEnv(t, proj)
	h := env.h
	do := env.do

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

	// Scoping to the ACP session the agent just created resolves the same
	// project; a session the gateway never saw opened is unknown.
	rec = do(t, "/v1/runtimes/"+h.runtimeID+"/git/status?acpSessionId="+env.acpSessionID)
	if rec.Code != http.StatusOK {
		t.Fatalf("scoped git status code = %d, body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, "/v1/runtimes/"+h.runtimeID+"/git/status?acpSessionId=never-opened")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown acpSessionId code = %d, want 404", rec.Code)
	}

	// 5. Ref listing for the compare picker.
	rec = do(t, "/v1/runtimes/"+h.runtimeID+"/git/refs")
	if rec.Code != http.StatusOK {
		t.Fatalf("git refs code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var refs workspace.RefsPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &refs); err != nil {
		t.Fatal(err)
	}
	byName := map[string]workspace.Ref{}
	for _, r := range refs.Refs {
		byName[r.Name] = r
	}
	if r := byName["main"]; r.Kind != "branch" || !r.IsHead {
		t.Fatalf("refs main = %+v, want branch with isHead", r)
	}
	if r := byName["cmp"]; r.Kind != "branch" || r.IsHead {
		t.Fatalf("refs cmp = %+v, want branch without isHead", r)
	}
	if r := byName["v1"]; r.Kind != "tag" {
		t.Fatalf("refs v1 = %+v, want tag", r)
	}
	if refs.Default != "main" {
		t.Fatalf("refs default = %q, want main", refs.Default)
	}
	if rec := do(t, "/v1/runtimes/"+h.runtimeID+"/git/refs?acpSessionId=never-opened"); rec.Code != http.StatusNotFound {
		t.Fatalf("refs unknown acpSessionId code = %d, want 404", rec.Code)
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
	byPath := map[string]workspace.ChangedFile{}
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

	// staged reflects whether the index holds a change, independent of the
	// combined counts above: true for index changes (git add / git mv), false
	// for worktree-only edits and untracked files.
	for path, want := range map[string]bool{
		"staged.txt":      true,  // added to the index
		"partial.txt":     true,  // the b->X edit is staged
		"renamed_dst.txt": true,  // git mv stages the rename
		"edited.txt":      false, // worktree-only edit
		"wip.txt":         false, // untracked
		"clip é 😎.bin":    false, // untracked
	} {
		if f := byPath[path]; f.Staged != want {
			t.Fatalf("%s staged = %v, want %v", path, f.Staged, want)
		}
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

	// 6. History: log, commit files, and a file diff from a commit.
	rec = do(t, "/v1/runtimes/"+h.runtimeID+"/git/log?limit=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("git log code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var lp workspace.LogPage
	if err := json.Unmarshal(rec.Body.Bytes(), &lp); err != nil {
		t.Fatal(err)
	}
	if len(lp.Commits) != 1 || !lp.HasMore || lp.Head != lp.Commits[0].SHA || lp.Commits[0].Subject != "add partial" {
		t.Fatalf("git log page = %+v", lp)
	}
	// The page reports the size of the whole walk, not of the page, so a
	// client can size its list: skip, limit and beyond leave it alone.
	if lp.Total != 3 {
		t.Fatalf("git log total = %d, want 3 (the whole walk)", lp.Total)
	}
	rec = do(t, "/v1/runtimes/"+h.runtimeID+"/git/log?head="+lp.Head+"&limit=1&skip=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("git log last page code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var lpLast workspace.LogPage
	if err := json.Unmarshal(rec.Body.Bytes(), &lpLast); err != nil {
		t.Fatal(err)
	}
	if len(lpLast.Commits) != 1 || lpLast.Total != 3 {
		t.Fatalf("git log last page = %+v, want one commit and total 3", lpLast)
	}
	rec = do(t, "/v1/runtimes/"+h.runtimeID+"/git/commits/"+lp.Head)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"path":"partial.txt"`) {
		t.Fatalf("git commit code = %d, body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, "/v1/runtimes/"+h.runtimeID+"/git/diff?path=partial.txt&commit="+lp.Head)
	var cd acp.ToolCallContentDiff
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &cd) != nil || cd.NewText != "a\nb\nc\n" || cd.OldText != nil {
		t.Fatalf("commit diff code = %d, body=%s", rec.Code, rec.Body.String())
	}

	// 7. Compare mode: the file list is the strict two-dot `git diff cmp` set.
	rec = do(t, "/v1/runtimes/"+h.runtimeID+"/git/status?compare=cmp")
	if rec.Code != http.StatusOK {
		t.Fatalf("compare status code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var cs gitStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &cs); err != nil {
		t.Fatal(err)
	}
	if cs.Compare != "cmp" || cs.CompareCommit != cmpSha {
		t.Fatalf("compare status = %+v, want compare cmp and commit %s", cs, cmpSha)
	}
	if cs.Branch != "main" {
		t.Fatalf("compare status branch = %q, want main", cs.Branch)
	}
	cmpPaths := map[string]workspace.ChangedFile{}
	for _, f := range cs.Changed {
		cmpPaths[f.Path] = f
	}
	if f, ok := cmpPaths["base.txt"]; ok {
		t.Fatalf("base.txt listed: dirty against HEAD but identical to cmp: %+v", f)
	}
	if f, ok := cmpPaths["cmp-only.txt"]; !ok || f.Status != "D" || f.Added != 0 || f.Removed != 1 {
		t.Fatalf("cmp-only.txt = %+v (present=%v), want D with 1 removed", f, ok)
	}
	if f, ok := cmpPaths["wip.txt"]; !ok || f.Status != "?" || f.Added != 1 {
		t.Fatalf("wip.txt = %+v, want untracked with 1 added line", f)
	}
	if f, ok := cmpPaths["edited.txt"]; !ok || f.Status != "M" || f.Added != 1 || f.Removed != 1 {
		t.Fatalf("edited.txt = %+v, want M with 1/1", f)
	}
	if f, ok := cmpPaths["renamed_dst.txt"]; !ok || f.Status != "A" || f.Added != 1 {
		// cmp was cut before the rename, so it holds neither path: the
		// destination is a plain addition against cmp, not an R.
		t.Fatalf("renamed_dst.txt = %+v (present=%v), want A with 1 added", f, ok)
	}
	if f, ok := cmpPaths["committed.txt"]; ok {
		t.Fatalf("committed.txt listed against cmp: %+v", f)
	}
	for _, f := range cs.Changed {
		if f.Staged {
			t.Fatalf("%s staged = true in compare mode, want false", f.Path)
		}
	}

	// Without an honoured compare= the field is absent, so a worktree's
	// merge-base sha can never masquerade as a comparison. An empty or
	// whitespace-only compare= is not an honoured comparison either.
	for _, q := range []string{"", "compare=", "compare=%20"} {
		rec = do(t, "/v1/runtimes/"+h.runtimeID+"/git/status?"+q)
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"compare"`) {
			t.Fatalf("git/status?%s code = %d, body=%s", q, rec.Code, rec.Body.String())
		}
	}
	// A malformed or unknown target is a 400: never working-tree data under a
	// branch label.
	for _, q := range []string{"compare=..%2Fetc", "compare=-x", "compare=nope"} {
		if rec := do(t, "/v1/runtimes/"+h.runtimeID+"/git/status?"+q); rec.Code != http.StatusBadRequest {
			t.Fatalf("git/status?%s code = %d, want 400, body=%s", q, rec.Code, rec.Body.String())
		}
	}

	// 7b. Compare mode: oldText comes from the compare ref, so a file that
	// exists only in the target reads as a deletion. Without compare it has no
	// oldText at all (cmp-only.txt is not in HEAD), so this distinguishes the
	// two comparisons.
	rec = do(t, "/v1/runtimes/"+h.runtimeID+"/git/diff?path=cmp-only.txt&compare=cmp")
	if rec.Code != http.StatusOK {
		t.Fatalf("compare diff code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var cdiff acp.ToolCallContentDiff
	if err := json.Unmarshal(rec.Body.Bytes(), &cdiff); err != nil {
		t.Fatal(err)
	}
	if cdiff.Path != "cmp-only.txt" || cdiff.OldText == nil || *cdiff.OldText != "only\n" || cdiff.NewText != "" {
		t.Fatalf("compare diff = %+v, want oldText %q and empty newText", cdiff, "only\n")
	}
	for _, q := range []string{"compare=nope", "compare=..%2Fetc"} {
		if rec := do(t, "/v1/runtimes/"+h.runtimeID+"/git/diff?path=base.txt&"+q); rec.Code != http.StatusBadRequest {
			t.Fatalf("git/diff?path=base.txt&%s code = %d, want 400, body=%s", q, rec.Code, rec.Body.String())
		}
	}

	// 7b-2. The whole-tree diff honours compare too: cmp-only.txt is not in HEAD
	// and not on disk, so without compare it is absent; with compare=cmp it
	// reads as a deletion with oldText from cmp. base.txt is dirty against HEAD
	// but identical to cmp, so compare must leave it out.
	rec = do(t, "/v1/runtimes/"+h.runtimeID+"/git/diff?compare=cmp")
	if rec.Code != http.StatusOK {
		t.Fatalf("compare whole-tree diff code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var cgds []acp.ToolCallContentDiff
	if err := json.Unmarshal(rec.Body.Bytes(), &cgds); err != nil {
		t.Fatal(err)
	}
	wholeByPath := map[string]acp.ToolCallContentDiff{}
	for _, d := range cgds {
		wholeByPath[d.Path] = d
	}
	if d, ok := wholeByPath["cmp-only.txt"]; !ok || d.OldText == nil || *d.OldText != "only\n" || d.NewText != "" {
		t.Fatalf("whole-tree compare entry = %+v (present=%v), want cmp-only.txt with oldText from cmp", d, ok)
	}
	if d, ok := wholeByPath["base.txt"]; ok {
		t.Fatalf("base.txt in whole-tree compare: dirty against HEAD but identical to cmp: %+v", d)
	}
	for _, q := range []string{"compare=nope", "compare=..%2Fetc"} {
		if rec := do(t, "/v1/runtimes/"+h.runtimeID+"/git/diff?"+q); rec.Code != http.StatusBadRequest {
			t.Fatalf("git/diff?%s code = %d, want 400, body=%s", q, rec.Code, rec.Body.String())
		}
	}

	// 7c. Compare mode: the history range is HEAD --not cmp, newest first.
	rec = do(t, "/v1/runtimes/"+h.runtimeID+"/git/log?compare=cmp&limit=50")
	if rec.Code != http.StatusOK {
		t.Fatalf("compare log code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var clp workspace.LogPage
	if err := json.Unmarshal(rec.Body.Bytes(), &clp); err != nil {
		t.Fatal(err)
	}
	var subjects []string
	for _, c := range clp.Commits {
		subjects = append(subjects, c.Subject)
	}
	if got := strings.Join(subjects, ","); got != "add partial,add renamed_src" {
		t.Fatalf("compare log subjects = %q, want %q", got, "add partial,add renamed_src")
	}
	if clp.Head == cmpSha {
		t.Fatalf("compare log head = %s, want HEAD's sha", clp.Head)
	}
	// base is shared, so the range is just this branch's two commits.
	if clp.Total != 2 {
		t.Fatalf("compare log total = %d, want 2 (HEAD --not cmp)", clp.Total)
	}
	// No target unless asked for; beyond=1 is the target's tip, beyond=2 one more.
	if len(clp.Target) != 0 {
		t.Fatalf("compare log target = %+v, want none without beyond", clp.Target)
	}
	rec = do(t, "/v1/runtimes/"+h.runtimeID+"/git/log?compare=cmp&limit=50&beyond=1")
	var clb workspace.LogPage
	if err := json.Unmarshal(rec.Body.Bytes(), &clb); err != nil || len(clb.Target) != 1 || clb.Target[0].SHA != cmpSha {
		t.Fatalf("compare log beyond=1 target = %+v, %v", clb.Target, err)
	}
	rec = do(t, "/v1/runtimes/"+h.runtimeID+"/git/log?compare=cmp&limit=50&beyond=2")
	var clb2 workspace.LogPage
	if err := json.Unmarshal(rec.Body.Bytes(), &clb2); err != nil || len(clb2.Target) != 2 || clb2.Target[0].SHA != cmpSha {
		t.Fatalf("compare log beyond=2 target = %+v, %v", clb2.Target, err)
	}
	// beyond adds target commits but does not change the walk being counted.
	if clb2.Total != 2 {
		t.Fatalf("compare log beyond=2 total = %d, want 2", clb2.Total)
	}
	// Paging by the pinned head keeps the same range.
	rec = do(t, "/v1/runtimes/"+h.runtimeID+"/git/log?compare=cmp&head="+clp.Head+"&limit=1&skip=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("compare log page 2 code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var clp2 workspace.LogPage
	if err := json.Unmarshal(rec.Body.Bytes(), &clp2); err != nil {
		t.Fatal(err)
	}
	if len(clp2.Commits) != 1 || clp2.Commits[0].Subject != "add renamed_src" || clp2.HasMore {
		t.Fatalf("compare log page 2 = %+v, want one commit and no more", clp2)
	}
	if clp2.Total != 2 {
		t.Fatalf("compare log page 2 total = %d, want 2: the range, not the page", clp2.Total)
	}
	// A branch compared with itself is an empty walk: no commits and total 0,
	// while hasMore stays the authority on whether another page exists.
	rec = do(t, "/v1/runtimes/"+h.runtimeID+"/git/log?compare=main")
	if rec.Code != http.StatusOK {
		t.Fatalf("self compare log code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var self workspace.LogPage
	if err := json.Unmarshal(rec.Body.Bytes(), &self); err != nil {
		t.Fatal(err)
	}
	if len(self.Commits) != 0 || self.Total != 0 || self.HasMore || !strings.Contains(rec.Body.String(), `"commits":[]`) {
		t.Fatalf("self compare log = %+v, body=%s, want no commits and total 0", self, rec.Body.String())
	}
	for _, q := range []string{"compare=nope", "compare=..%2Fetc"} {
		if rec := do(t, "/v1/runtimes/"+h.runtimeID+"/git/log?"+q); rec.Code != http.StatusBadRequest {
			t.Fatalf("git/log?%s code = %d, want 400, body=%s", q, rec.Code, rec.Body.String())
		}
	}
	for _, path := range []string{
		"/git/log?head=nothex",
		"/git/log?limit=x",
		"/git/log?compare=cmp&beyond=x",
		"/git/log?compare=cmp&beyond=-1",
		"/git/commits/zzzzzzz",
		"/git/commits/" + strings.Repeat("0", 40),
		"/git/diff?commit=" + lp.Head,
		"/git/diff?commit=" + lp.Head + "&path=../outside.txt",
	} {
		if rec := do(t, "/v1/runtimes/"+h.runtimeID+path); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s code = %d, want 400, body=%s", path, rec.Code, rec.Body.String())
		}
	}

	// An empty repository answers with head "", no commits and total 0. Its cwd
	// comes from a second agent session pointed at it.
	empty := t.TempDir()
	runGitCLI(t, empty, "init", "-q", "-b", "main")
	resp2 := h.connectResilient()
	ws2 := h.dialSessionWS(resp2.SessionID, resp2.AttachToken)
	defer func() { _ = ws2.CloseNow() }()
	sendWSMessage(t, ws2, `{"jsonrpc":"2.0","id":"1","method":"initialize","params":{"protocolVersion":1,"capabilities":{},"clientInfo":{"name":"test-client","version":"1.0.0"}}}`)
	readWSMessage(t, ws2) // initialize result
	sendWSMessage(t, ws2, `{"jsonrpc":"2.0","id":"2","method":"authenticate","params":{}}`)
	readWSMessage(t, ws2) // authenticate result
	emptyFrame, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      10,
		"method":  "session/new",
		"params":  map[string]any{"cwd": empty},
	})
	if err != nil {
		t.Fatal(err)
	}
	sendWSMessage(t, ws2, string(emptyFrame))
	readWSMessage(t, ws2) // session/new result
	readWSMessage(t, ws2) // session_info_update notification

	rec = do(t, "/v1/runtimes/"+resp2.RuntimeID+"/git/log")
	if rec.Code != http.StatusOK {
		t.Fatalf("empty repo log code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var emptyLog workspace.LogPage
	if err := json.Unmarshal(rec.Body.Bytes(), &emptyLog); err != nil {
		t.Fatal(err)
	}
	if emptyLog.Head != "" || len(emptyLog.Commits) != 0 || emptyLog.Total != 0 || emptyLog.HasMore ||
		!strings.Contains(rec.Body.String(), `"commits":[]`) {
		t.Fatalf("empty repo log = %+v, body=%s, want head \"\", total 0 and no commits", emptyLog, rec.Body.String())
	}
}

// TestWorkspaceFileList covers GET /v1/runtimes/{id}/files/list against a real
// temp repo: the listing rule, relative paths, -z, the gzip branch, and 422
// for a non-repo cwd. The cap cut itself is pinned at the ListFiles unit
// level; the handler always passes MaxFileListEntries.
func TestWorkspaceFileList(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	// Repo: tracked (README.md, .gitignore, sub/nested.txt), ignored
	// (build/out.txt behind .gitignore "build/"), untracked with a space
	// ("wip file.txt") and non-ASCII ("wörk.txt").
	proj := t.TempDir()
	runGitCLI(t, proj, "init", "-q", "-b", "main")
	runGitCLI(t, proj, "config", "user.email", "t@t")
	runGitCLI(t, proj, "config", "user.name", "t")
	runGitCLI(t, proj, "config", "commit.gpgSign", "false")
	if err := os.MkdirAll(filepath.Join(proj, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(proj, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, content string) {
		if err := os.WriteFile(filepath.Join(proj, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "readme\n")
	write(".gitignore", "build/\n")
	write("sub/nested.txt", "n\n")
	runGitCLI(t, proj, "add", "README.md", ".gitignore", "sub/nested.txt")
	runGitCLI(t, proj, "commit", "-qm", "init")
	write("wip file.txt", "w\n")
	write("wörk.txt", "ö\n")
	write("build/out.txt", "ignored\n")

	env := newWorkspaceEnv(t, proj)
	h := env.h

	var plain fileListResponse

	req := func(t *testing.T, path string, hdr map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+h.token)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.server.Handler().ServeHTTP(rec, r)
		return rec
	}

	url := "/v1/runtimes/" + h.runtimeID + "/files/list"

	// 1. Plain request: 200, nothing ignored, space and non-ASCII paths
	// unquoted, relative paths, plain JSON with an empty-safe "files" array.
	rec := req(t, url, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("file list status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if e := rec.Header().Get("Content-Encoding"); e != "" {
		t.Fatalf("plain request Content-Encoding = %q, want absent", e)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &plain); err != nil {
		t.Fatal(err)
	}
	want := []string{".gitignore", "README.md", "sub/nested.txt", "wip file.txt", "wörk.txt"}
	if !slices.Equal(plain.Files, want) {
		t.Fatalf("files = %q, want %q", plain.Files, want)
	}
	if plain.Truncated {
		t.Fatal("truncated = true, want false")
	}
	if strings.Contains(rec.Body.String(), "null") {
		t.Fatalf("body contains null: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "build/out.txt") {
		t.Fatalf("ignored build/out.txt leaked into the list: %s", rec.Body.String())
	}

	// 2. Gzip accepted: Content-Encoding: gzip, Vary echoed, and the body
	// decodes to the identical list.
	rec = req(t, url, map[string]string{"Accept-Encoding": "gzip"})
	if rec.Code != http.StatusOK {
		t.Fatalf("gzip status = %d", rec.Code)
	}
	if e := rec.Header().Get("Content-Encoding"); e != "gzip" {
		t.Fatalf("gzip Content-Encoding = %q, want gzip", e)
	}
	if vary := rec.Header().Get("Vary"); !strings.Contains(vary, "Accept-Encoding") {
		t.Fatalf("gzip Vary = %q, want Accept-Encoding", vary)
	}
	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	var viaGzip fileListResponse
	if err := json.NewDecoder(zr).Decode(&viaGzip); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(viaGzip.Files, plain.Files) || viaGzip.Truncated != plain.Truncated {
		t.Fatalf("gzip decode = %+v, want %+v", viaGzip, plain)
	}

	// 3. Accept-Encoding that does not offer gzip (identity): still plain
	// JSON, with Vary always set.
	rec = req(t, url, map[string]string{"Accept-Encoding": "identity"})
	if e := rec.Header().Get("Content-Encoding"); e != "" {
		t.Fatalf("identity Content-Encoding = %q, want absent", e)
	}
	if vary := rec.Header().Get("Vary"); !strings.Contains(vary, "Accept-Encoding") {
		t.Fatalf("identity Vary = %q, want Accept-Encoding", vary)
	}

	// 4. Cap lowered end to end ("so a test can lower it"): two entries and
	// truncated=true, cutting the sorted list.
	origCap := workspace.MaxFileListEntries
	workspace.MaxFileListEntries = 2
	t.Cleanup(func() { workspace.MaxFileListEntries = origCap })
	rec = req(t, url, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("capped status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var capped fileListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &capped); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(capped.Files, []string{".gitignore", "README.md"}) || !capped.Truncated {
		t.Fatalf("capped list = %+v, want [.gitignore README.md] truncated", capped)
	}
	workspace.MaxFileListEntries = origCap // restored here so later parts see the real cap; Cleanup is the safety net

	// 5. A session cwd that is a repo subdirectory: paths are relative to that
	// session's project directory, the same root /files reads from, so every
	// listed path can go straight into /files?path=.
	env2 := newWorkspaceEnv(t, filepath.Join(proj, "sub"))
	rec = env2.do(t, "/v1/runtimes/"+env2.h.runtimeID+"/files/list")
	if rec.Code != http.StatusOK {
		t.Fatalf("subdir status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var sub fileListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &sub); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sub.Files, []string{"nested.txt"}) {
		t.Fatalf("subdir files = %q, want [nested.txt]", sub.Files)
	}

	// 6. A session cwd that is not a repository: 422 with an error envelope.
	env3 := newWorkspaceEnv(t, t.TempDir())
	rec = env3.do(t, "/v1/runtimes/"+env3.h.runtimeID+"/files/list")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("non-repo status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var e422 struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e422); err != nil || strings.TrimSpace(e422.Error) == "" {
		t.Fatalf("non-repo body = %q, want {\"error\": ...} envelope (err %v)", rec.Body.String(), err)
	}
}

// TestWorkspaceFileListRequiresReadScope pins the gate: a credential that only
// holds gateway.control gets 403 from /files/list exactly like it does from
// /files. No session setup is needed — the scope check runs first.
func TestWorkspaceFileListRequiresReadScope(t *testing.T) {
	baseDir := t.TempDir()
	store, err := storage.Open(filepath.Join(baseDir, "pairings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	rawToken := "scopeless-device-token"
	if err := store.SavePairing(context.Background(), storage.PairingRecord{
		DeviceID:   "dev-scopeless",
		DeviceName: "scopeless",
		// Stored unhashed on purpose: pairing hashes the token when it loads
		// persisted credentials at construction (see pairing.loadPersistedCredentials).
		Token:     rawToken,
		ExpiresAt: time.Now().Add(time.Hour),
		Scopes:    []string{pairing.ScopeControl},
	}); err != nil {
		t.Fatal(err)
	}
	server := newTestServerWithStore(baseDir, store)
	for _, path := range []string{"/v1/runtimes/x/files/list", "/v1/runtimes/x/files"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+rawToken)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s scopeless status = %d, want 403 (body=%s)", path, rec.Code, rec.Body.String())
		}
	}
}

package api

import (
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

// TestWorktrees_EndToEnd creates a managed worktree, runs an ACP session in
// it, commits there, and checks the workspace endpoints diff against the
// worktree's base (so committed agent work stays visible), then lists and
// removes it.
func TestWorktrees_EndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	runGitCLI(t, repo, "init", "-q")
	runGitCLI(t, repo, "config", "user.email", "t@t")
	runGitCLI(t, repo, "config", "user.name", "t")
	runGitCLI(t, repo, "config", "commit.gpgSign", "false")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCLI(t, repo, "add", "a.txt")
	runGitCLI(t, repo, "commit", "-qm", "init")

	h := newResilientTestHarness(t)
	h.server.store = h.store
	do := func(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+h.token)
		rec := httptest.NewRecorder()
		h.server.Handler().ServeHTTP(rec, req)
		return rec
	}
	repoJSON, _ := json.Marshal(repo)

	if rec := do(t, http.MethodPost, "/v1/worktrees", `{"repo":`+string(repoJSON)+`,"branch":"bad..name"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid branch status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, http.MethodPost, "/v1/worktrees", `{"repo":`+string(repoJSON)+`,"base":"nope"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown base status = %d, body=%s", rec.Code, rec.Body.String())
	}

	rec := do(t, http.MethodPost, "/v1/worktrees", `{"repo":`+string(repoJSON)+`,"branch":"feat/x"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var wt worktreeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &wt); err != nil {
		t.Fatal(err)
	}
	if wt.Branch != "feat/x" || wt.BaseCommit == "" || filepath.Base(wt.Path) != "feat-x" ||
		filepath.Base(filepath.Dir(wt.Path)) != ".worktrees" {
		t.Fatalf("create response = %+v", wt)
	}
	// .worktrees/ is excluded locally, so the main checkout stays clean.
	if out, err := exec.Command("git", "-C", repo, "status", "--porcelain").CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("main checkout status = %q (err %v)", out, err)
	}
	if rec := do(t, http.MethodPost, "/v1/worktrees", `{"repo":`+string(repoJSON)+`,"branch":"feat/x"}`); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate branch status = %d, body=%s", rec.Code, rec.Body.String())
	}

	// The agent commits an edit and a new file, leaving the tree clean.
	if err := os.WriteFile(filepath.Join(wt.Path, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt.Path, "b.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCLI(t, wt.Path, "add", "a.txt", "b.txt")
	runGitCLI(t, wt.Path, "commit", "-qm", "agent work")

	conn := h.connectResilient()
	ws := h.dialSessionWS(conn.SessionID, conn.AttachToken)
	defer func() { _ = ws.CloseNow() }()
	sendWSMessage(t, ws, `{"jsonrpc":"2.0","id":"1","method":"initialize","params":{"protocolVersion":1,"capabilities":{},"clientInfo":{"name":"test-client","version":"1.0.0"}}}`)
	readWSMessage(t, ws)
	sendWSMessage(t, ws, `{"jsonrpc":"2.0","id":"2","method":"authenticate","params":{}}`)
	readWSMessage(t, ws)
	newFrame, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 10, "method": "session/new", "params": map[string]any{"cwd": wt.Path}})
	sendWSMessage(t, ws, string(newFrame))
	readWSMessage(t, ws) // session/new result
	readWSMessage(t, ws) // session_info_update

	rec = do(t, http.MethodGet, "/v1/runtimes/"+h.runtimeID+"/git/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("git status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var st gitStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	got := map[string]gitChangedFile{}
	for _, f := range st.Changed {
		got[f.Path] = f
	}
	if got["a.txt"].Status != "M" || got["a.txt"].Added != 1 || got["b.txt"].Status != "A" || len(got) != 2 {
		t.Fatalf("status vs base = %+v", st.Changed)
	}

	rec = do(t, http.MethodGet, "/v1/runtimes/"+h.runtimeID+"/git/diff?path=a.txt", "")
	var diff acp.ToolCallContentDiff
	if err := json.Unmarshal(rec.Body.Bytes(), &diff); err != nil {
		t.Fatalf("diff: %v body=%s", err, rec.Body.String())
	}
	if diff.OldText == nil || *diff.OldText != "base\n" || diff.NewText != "changed\n" {
		t.Fatalf("diff vs base = %+v", diff)
	}

	rec = do(t, http.MethodGet, "/v1/worktrees", "")
	var list []worktreeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Ahead == nil || *list[0].Ahead != 1 || list[0].Dirty == nil || *list[0].Dirty {
		t.Fatalf("list = %s", rec.Body.String())
	}

	// The worktree is in use as the session cwd; git still removes it. The
	// branch has an unmerged commit, so it is kept.
	rec = do(t, http.MethodDelete, "/v1/worktrees/"+wt.ID, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"branchDeleted":false`) {
		t.Fatalf("delete = %d, body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree dir still exists: %v", err)
	}
	if rec := do(t, http.MethodGet, "/v1/worktrees", ""); strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("list after delete = %s", rec.Body.String())
	}

	// Uncommitted changes need force.
	rec = do(t, http.MethodPost, "/v1/worktrees", `{"repo":`+string(repoJSON)+`,"branch":"feat/dirty"}`)
	if err := json.Unmarshal(rec.Body.Bytes(), &wt); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt.Path, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rec := do(t, http.MethodDelete, "/v1/worktrees/"+wt.ID, ""); rec.Code != http.StatusConflict {
		t.Fatalf("dirty delete = %d, body=%s", rec.Code, rec.Body.String())
	}

	// A worktree git already forgot (a removal that could not delete the folder)
	// still removes, with or without force.
	if err := os.Remove(filepath.Join(wt.Path, ".git")); err != nil {
		t.Fatal(err)
	}
	runGitCLI(t, repo, "worktree", "prune")
	if rec := do(t, http.MethodDelete, "/v1/worktrees/"+wt.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("leftover delete = %d, body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("leftover dir still exists: %v", err)
	}
}

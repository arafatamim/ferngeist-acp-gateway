package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/workspace"
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
	got := map[string]workspace.ChangedFile{}
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
	rec = do(t, http.MethodGet, "/v1/worktrees/"+wt.ID, "")
	var one worktreeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &one); err != nil || rec.Code != http.StatusOK || one.ID != wt.ID || one.Ahead == nil || *one.Ahead != 1 || len(one.Chats) != 1 {
		t.Fatalf("get = %d, body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, http.MethodGet, "/v1/worktrees/nope", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("get unknown = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(list[0].Chats) != 1 {
		t.Fatalf("chats while the session is open = %v, want one", list[0].Chats)
	}

	// A live chat uses the worktree as its cwd, so it cannot be deleted, even
	// with force.
	if rec := do(t, http.MethodDelete, "/v1/worktrees/"+wt.ID+"?force=true", ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "in use by chat") {
		t.Fatalf("delete while in use = %d, body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, http.MethodDelete, "/v1/sessions/"+conn.SessionID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("close session = %d, body=%s", rec.Code, rec.Body.String())
	}

	rec = do(t, http.MethodGet, "/v1/worktrees", "")
	if strings.Contains(rec.Body.String(), `"chats"`) {
		t.Fatalf("chats after the session closed = %s", rec.Body.String())
	}

	// Once the chat is closed the worktree goes. The branch has an unmerged
	// commit, so it is kept.
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

// TestWorktreeRebase_Endpoint rebases a worktree onto a base that moved,
// checks the response carries the list's ahead/dirty state, pushes it, then merges it.
func TestWorktreeRebase_Endpoint(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	runGitCLI(t, repo, "init", "-q", "-b", "main")
	runGitCLI(t, repo, "config", "user.email", "t@t")
	runGitCLI(t, repo, "config", "user.name", "t")
	runGitCLI(t, repo, "config", "commit.gpgSign", "false")
	commit := func(dir, name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGitCLI(t, dir, "add", name)
		runGitCLI(t, dir, "commit", "-qm", name)
	}
	commit(repo, "a.txt")

	h := newResilientTestHarness(t)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+h.token)
		rec := httptest.NewRecorder()
		h.server.Handler().ServeHTTP(rec, req)
		return rec
	}
	repoJSON, _ := json.Marshal(repo)
	var wt worktreeResponse
	rec := do(http.MethodPost, "/v1/worktrees", `{"repo":`+string(repoJSON)+`,"base":"main","branch":"feat/x"}`)
	if err := json.Unmarshal(rec.Body.Bytes(), &wt); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("create = %d, body=%s", rec.Code, rec.Body.String())
	}
	commit(wt.Path, "agent.txt")
	commit(repo, "upstream.txt")

	rec = do(http.MethodPost, "/v1/worktrees/"+wt.ID+"/rebase", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("rebase = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got worktreeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Ahead == nil || *got.Ahead != 1 || got.Dirty == nil || *got.Dirty {
		t.Fatalf("rebase response = %s", rec.Body.String())
	}
	if rec := do(http.MethodPost, "/v1/worktrees/nope/rebase", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown worktree = %d, body=%s", rec.Code, rec.Body.String())
	}

	bare := t.TempDir()
	runGitCLI(t, bare, "init", "-q", "--bare")
	runGitCLI(t, repo, "remote", "add", "origin", bare)
	rec = do(http.MethodPost, "/v1/worktrees/"+wt.ID+"/push", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"pushed":true`) || strings.Contains(rec.Body.String(), "prUrl") {
		t.Fatalf("push = %d, body=%s", rec.Code, rec.Body.String())
	}
	runGitCLI(t, bare, "rev-parse", "--verify", "refs/heads/feat/x")

	// The rebased branch fast-forwards into main; an empty body asks for that.
	rec = do(http.MethodPost, "/v1/worktrees/"+wt.ID+"/merge", `{"delete":true}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"worktreeDeleted":true`) || !strings.Contains(rec.Body.String(), `"branchDeleted":true`) {
		t.Fatalf("merge = %d, body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(repo, "agent.txt")); err != nil {
		t.Fatalf("merged file missing from the main checkout: %v", err)
	}
	if rec := do(http.MethodPost, "/v1/worktrees/"+wt.ID+"/merge", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("merge of a deleted worktree = %d, body=%s", rec.Code, rec.Body.String())
	}
}

// TestWorktreeRefs_Endpoint pins the repo-scoped refs route: the default falls
// back to a local main, follows origin/HEAD once there is one, and a relative
// or non-repo path is refused.
func TestWorktreeRefs_Endpoint(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	runGitCLI(t, repo, "init", "-q", "-b", "main")
	runGitCLI(t, repo, "config", "user.email", "t@t")
	runGitCLI(t, repo, "config", "user.name", "t")
	runGitCLI(t, repo, "config", "commit.gpgSign", "false")
	runGitCLI(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	runGitCLI(t, repo, "branch", "feat/x")

	h := newResilientTestHarness(t)
	refs := func(repoPath string) (int, workspace.RefsPayload) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/worktrees/refs?repo="+url.QueryEscape(repoPath), nil)
		req.Header.Set("Authorization", "Bearer "+h.token)
		rec := httptest.NewRecorder()
		h.server.Handler().ServeHTTP(rec, req)
		var payload workspace.RefsPayload
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
		}
		return rec.Code, payload
	}

	code, payload := refs(repo)
	if code != http.StatusOK || payload.Default != "main" || len(payload.Refs) != 2 {
		t.Fatalf("refs = %d %+v, want default main with 2 branches", code, payload)
	}

	origin := t.TempDir()
	runGitCLI(t, origin, "init", "-q", "--bare")
	runGitCLI(t, repo, "remote", "add", "origin", origin)
	runGitCLI(t, repo, "push", "-q", "origin", "feat/x")
	runGitCLI(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/feat/x")
	if code, payload = refs(repo); code != http.StatusOK || payload.Default != "feat/x" {
		t.Fatalf("refs = %d %+v, want default from origin/HEAD", code, payload)
	}

	if code, _ = refs(t.TempDir()); code != http.StatusUnprocessableEntity {
		t.Fatalf("non-repo = %d, want 422", code)
	}
	if code, _ = refs("relative/repo"); code != http.StatusBadRequest {
		t.Fatalf("relative repo = %d, want 400", code)
	}
}

func TestWorktree_UnmanagedAndAdopt(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	runGitCLI(t, repo, "init", "-q")
	runGitCLI(t, repo, "checkout", "-qb", "main")
	runGitCLI(t, repo, "config", "user.email", "t@t")
	runGitCLI(t, repo, "config", "user.name", "t")
	runGitCLI(t, repo, "config", "commit.gpgSign", "false")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCLI(t, repo, "add", "a.txt")
	runGitCLI(t, repo, "commit", "-qm", "init")

	extPath := filepath.Join(t.TempDir(), "external-wt")
	runGitCLI(t, repo, "worktree", "add", "-b", "feat/ext", extPath, "main")

	h := newResilientTestHarness(t)
	do := func(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+h.token)
		rec := httptest.NewRecorder()
		h.server.Handler().ServeHTTP(rec, req)
		return rec
	}

	// GET /v1/worktrees/unmanaged error cases
	if rec := do(t, http.MethodGet, "/v1/worktrees/unmanaged?repo=relative/path", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("unmanaged relative = %d, want 400", rec.Code)
	}
	if rec := do(t, http.MethodGet, "/v1/worktrees/unmanaged?repo="+url.QueryEscape(t.TempDir()), ""); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unmanaged non-repo = %d, want 422", rec.Code)
	}

	// GET /v1/worktrees/unmanaged lists extPath
	rec := do(t, http.MethodGet, "/v1/worktrees/unmanaged?repo="+url.QueryEscape(repo), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("unmanaged code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var unmanaged []unmanagedWorktreeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &unmanaged); err != nil {
		t.Fatal(err)
	}
	if len(unmanaged) != 1 || unmanaged[0].Path != extPath || unmanaged[0].Branch != "feat/ext" {
		t.Fatalf("unmanaged = %+v, want 1 worktree at %s with branch feat/ext", unmanaged, extPath)
	}

	// POST /v1/worktrees/adopt
	adoptBody, _ := json.Marshal(map[string]string{
		"repo": repo,
		"path": extPath,
	})
	rec = do(t, http.MethodPost, "/v1/worktrees/adopt", string(adoptBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("adopt code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var adopted worktreeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &adopted); err != nil {
		t.Fatal(err)
	}
	if adopted.Path != extPath || adopted.Branch != "feat/ext" || adopted.BaseCommit == "" {
		t.Fatalf("adopted response = %+v", adopted)
	}

	// GET /v1/worktrees now includes the adopted worktree
	rec = do(t, http.MethodGet, "/v1/worktrees", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list code = %d", rec.Code)
	}
	var list []worktreeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != adopted.ID {
		t.Fatalf("list = %+v, want adopted worktree %s", list, adopted.ID)
	}

	// GET /v1/worktrees/unmanaged is now empty
	rec = do(t, http.MethodGet, "/v1/worktrees/unmanaged?repo="+url.QueryEscape(repo), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("unmanaged code = %d", rec.Code)
	}
	var unmanagedAfter []unmanagedWorktreeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &unmanagedAfter); err != nil {
		t.Fatal(err)
	}
	if len(unmanagedAfter) != 0 {
		t.Fatalf("unmanagedAfter = %+v, want empty", unmanagedAfter)
	}

	// Adopting again returns 409
	rec = do(t, http.MethodPost, "/v1/worktrees/adopt", string(adoptBody))
	if rec.Code != http.StatusConflict {
		t.Fatalf("adopt again code = %d, want 409", rec.Code)
	}
}

func TestWorktree_CreateFromRemoteBranchAndPR(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	runGitCLI(t, repo, "init", "-q")
	runGitCLI(t, repo, "checkout", "-qb", "main")
	runGitCLI(t, repo, "config", "user.email", "t@t")
	runGitCLI(t, repo, "config", "user.name", "t")
	runGitCLI(t, repo, "config", "commit.gpgSign", "false")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCLI(t, repo, "add", "a.txt")
	runGitCLI(t, repo, "commit", "-qm", "init")

	bare := t.TempDir()
	runGitCLI(t, bare, "init", "-q", "--bare")
	runGitCLI(t, repo, "remote", "add", "origin", bare)
	runGitCLI(t, repo, "push", "-q", "origin", "main:refs/heads/feat-remote")
	runGitCLI(t, repo, "push", "-q", "origin", "main:refs/pull/123/head")

	h := newResilientTestHarness(t)
	do := func(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+h.token)
		rec := httptest.NewRecorder()
		h.server.Handler().ServeHTTP(rec, req)
		return rec
	}

	repoJSON, _ := json.Marshal(repo)

	// Invalid base syntax returns 400
	if rec := do(t, http.MethodPost, "/v1/worktrees", `{"repo":`+string(repoJSON)+`,"base":"--flag"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid base code = %d, want 400", rec.Code)
	}

	// Create from origin/feat-remote
	rec := do(t, http.MethodPost, "/v1/worktrees", `{"repo":`+string(repoJSON)+`,"base":"origin/feat-remote","branch":"review-remote"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create from origin/feat-remote code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var wtRemote worktreeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &wtRemote); err != nil {
		t.Fatal(err)
	}
	if wtRemote.Branch != "review-remote" || wtRemote.BaseCommit == "" {
		t.Fatalf("wtRemote = %+v", wtRemote)
	}

	// Create from pr/123
	rec = do(t, http.MethodPost, "/v1/worktrees", `{"repo":`+string(repoJSON)+`,"base":"pr/123","branch":"review-pr"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create from pr/123 code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var wtPR worktreeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &wtPR); err != nil {
		t.Fatal(err)
	}
	if wtPR.Branch != "review-pr" || wtPR.BaseCommit == "" {
		t.Fatalf("wtPR = %+v", wtPR)
	}

	// Both appear in GET /v1/worktrees
	rec = do(t, http.MethodGet, "/v1/worktrees", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list code = %d", rec.Code)
	}
	var list []worktreeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2: %+v", len(list), list)
	}
}

func TestPathsMissingReportsOnlyGonePaths(t *testing.T) {
	h := newResilientTestHarness(t)
	there, gone := t.TempDir(), filepath.Join(t.TempDir(), "removed")
	post := func(body string) (int, []string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/paths/missing", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+h.token)
		rec := httptest.NewRecorder()
		h.server.Handler().ServeHTTP(rec, req)
		var payload struct{ Missing []string }
		_ = json.Unmarshal(rec.Body.Bytes(), &payload)
		return rec.Code, payload.Missing
	}

	paths, _ := json.Marshal(map[string][]string{"paths": {there, gone}})
	if code, missing := post(string(paths)); code != http.StatusOK || len(missing) != 1 || missing[0] != gone {
		t.Fatalf("missing = %d %v, want only %s", code, missing, gone)
	}
	if code, _ := post(`{"paths":["relative/dir"]}`); code != http.StatusBadRequest {
		t.Fatalf("relative path = %d, want 400", code)
	}
}

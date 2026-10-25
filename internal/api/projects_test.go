package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/projects"
)

func TestProjectsStreamsOneNDJSONLinePerDistinctAgentThenCheckouts(t *testing.T) {
	server, token, _ := projectListingServer(t)

	unauthenticated := httptest.NewRecorder()
	server.Handler().ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/v1/projects?agents=mock-acp", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("status without credential = %d, want %d", unauthenticated.Code, http.StatusUnauthorized)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/projects?agents=mock-acp,missing,mock-acp", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("status = %d, content type = %q", rec.Code, rec.Header().Get("Content-Type"))
	}

	byAgent := map[string]map[string]json.RawMessage{}
	var last map[string]json.RawMessage
	scanner := bufio.NewScanner(rec.Body)
	for scanner.Scan() {
		var line map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("line %q: %v", scanner.Text(), err)
		}
		if last != nil {
			byAgent[agentIDOf(last)] = last
		}
		last = line
	}
	// The mock's chats sit outside git, so no repo has a checkout.
	if string(last["type"]) != `"checkouts"` || string(last["repos"]) != "[]" {
		t.Fatalf("last line = %v, want an empty checkouts line", last)
	}
	if len(byAgent) != 2 {
		t.Fatalf("got lines for %d agents, want mock-acp and missing once each", len(byAgent))
	}
	ok := byAgent["mock-acp"]
	if string(ok["type"]) != `"agent"` || string(ok["status"]) != `"ok"` || string(ok["error"]) != "null" {
		t.Errorf("mock-acp line = %v", ok)
	}
	var chats []map[string]json.RawMessage
	if err := json.Unmarshal(ok["chats"], &chats); err != nil || len(chats) != 2 {
		t.Fatalf("chats = %s, err %v", ok["chats"], err)
	}
	for _, key := range []string{"sessionId", "title", "cwd", "updatedAt", "repo", "worktree"} {
		if _, present := chats[0][key]; !present {
			t.Errorf("chat lacks %q: %v", key, chats[0])
		}
	}
	if missing := byAgent["missing"]; string(missing["status"]) != `"failed"` ||
		string(missing["chats"]) != "[]" || string(missing["launchAvailable"]) != "false" {
		t.Errorf("missing line = %v, want failed with confirmed launch unavailability", missing)
	}
	if _, reported := ok["launchAvailable"]; reported {
		t.Errorf("successful listing must not claim launch unavailability: %v", ok)
	}
}

func agentIDOf(line map[string]json.RawMessage) string {
	var agentID string
	_ = json.Unmarshal(line["agentId"], &agentID)
	return agentID
}

func projectListingServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	baseDir := t.TempDir()
	t.Setenv("FERNGEIST_MOCK_LIST_ROOT", baseDir)
	for _, name := range []string{"project", "bulk"} {
		if err := os.Mkdir(filepath.Join(baseDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	buildMockAgent(t, baseDir)
	server := newTestServerWithBaseDir(baseDir)
	server.projects = projects.New(server.catalog, server.runtime, server.workspaceSvc)
	t.Cleanup(server.projects.Close)
	token := pairDevice(t, server)
	return server, token, baseDir

}

func TestProjectsReportsRemovedRepoEvenWhenAgentAnswerIsCached(t *testing.T) {
	server, token, root := projectListingServer(t)
	fetch := func() []projects.AgentLine {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/projects?agents=mock-acp", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		scanner := bufio.NewScanner(rec.Body)
		var agents []projects.AgentLine
		for scanner.Scan() {
			var agent projects.AgentLine
			if err := json.Unmarshal(scanner.Bytes(), &agent); err != nil {
				t.Fatal(err)
			}
			if agent.Type == "agent" {
				agents = append(agents, agent)
				continue
			}
			var checkouts projects.CheckoutsLine
			if err := json.Unmarshal(scanner.Bytes(), &checkouts); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(root, "project")); os.IsNotExist(err) {
				if len(checkouts.MissingRepos) != 1 || checkouts.MissingRepos[0] != filepath.Join(root, "project") {
					t.Fatalf("final line = %s, want confirmed deleted repo", scanner.Text())
				}
			}
		}
		if err := scanner.Err(); err != nil {
			t.Fatal(err)
		}
		return agents
	}
	first := fetch()
	if len(first) != 1 || len(first[0].Chats) != 2 {
		t.Fatalf("initial chats: %+v", first)
	}
	if err := os.Remove(filepath.Join(root, "project")); err != nil {
		t.Fatal(err)
	}
	second := fetch()
	if len(second) != 1 || len(second[0].Chats) != 1 || second[0].Chats[0].SessionID == "mock_sess_1" {
		t.Fatalf("cached chats after deletion: %+v", second)
	}
}

func TestProjectsChecksClientSnapshotRootsAfterDaemonRestart(t *testing.T) {
	server, token, root := projectListingServer(t)
	repo := filepath.Join(root, "deleted,project")
	encoded, err := json.Marshal([]string{repo})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/projects?agents=missing&repos="+url.QueryEscape(string(encoded)), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	scanner := bufio.NewScanner(rec.Body)
	var last []byte
	for scanner.Scan() {
		last = append(last[:0], scanner.Bytes()...)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	var checkouts projects.CheckoutsLine
	if err := json.Unmarshal(last, &checkouts); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || len(checkouts.MissingRepos) != 1 || checkouts.MissingRepos[0] != repo {
		t.Fatalf("status %d, final line %s: want confirmed missing snapshot root", rec.Code, last)
	}
}

func TestProjectsRejectsInvalidSnapshotRoots(t *testing.T) {
	server, token, _ := projectListingServer(t)
	tooMany, err := json.Marshal(slices.Repeat([]string{"/repo"}, 257))
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`["relative"]`, `["/bad\u0000path"]`, `null`, `{}`, `[`, string(tooMany)} {
		req := httptest.NewRequest(http.MethodGet, "/v1/projects?repos="+url.QueryEscape(raw), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("repos=%s: status %d, want 400", raw, rec.Code)
		}
	}
}

func TestProjectsReportsFreshRunningTurnsFromAnotherDevice(t *testing.T) {
	for _, tc := range []struct {
		name, mode, status string
		chats              int
	}{
		{"listed", "", "ok", 2},
		{"unsupported", "none", "unsupported", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FERNGEIST_MOCK_LIST", tc.mode)
			checkProjectRunningTurns(t, tc.status, tc.chats)
		})
	}
}

func checkProjectRunningTurns(t *testing.T, status string, chats int) {
	t.Helper()
	release := make(chan struct{})
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(llm.Close)
	t.Setenv("FERNGEIST_MOCK_LLM_ENDPOINT", llm.URL)
	server, token, _ := projectListingServer(t)
	rs := attachTestSessionService(t, server)
	t.Cleanup(func() { close(release) })
	ctx := context.Background()
	agent, err := server.catalog.Get("mock-acp")
	if err != nil {
		t.Fatal(err)
	}
	sess, _, err := rs.Create(ctx, agent, "another-device", nil)
	if err != nil {
		t.Fatal(err)
	}
	pump, err := rs.GetPump(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Prime the listing cache before turns start, including a chat absent from the listing.
	assertProjectRunning(t, server, token, status, chats, nil)
	for _, frame := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"unlisted","prompt":[]}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"mock_sess_1","prompt":[]}}`,
	} {
		if err := pump.WriteToAgent([]byte(frame)); err != nil {
			t.Fatal(err)
		}
	}
	assertProjectRunning(t, server, token, status, chats, []string{"mock_sess_1", "unlisted"})
	if err := rs.Close(ctx, sess.ID, "another-device"); err != nil {
		t.Fatal(err)
	}
	assertProjectRunning(t, server, token, status, chats, nil)
}

func assertProjectRunning(t *testing.T, server *Server, token, status string, chats int, want []string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/projects?agents=mock-acp", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("projects status = %d: %s", rec.Code, rec.Body.String())
	}
	var line struct {
		Status  string          `json:"status"`
		Running json.RawMessage `json:"running"`
		Chats   []struct {
			Running json.RawMessage `json:"running"`
		} `json:"chats"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&line); err != nil {
		t.Fatal(err)
	}
	if line.Status != status || len(line.Chats) != chats {
		t.Fatalf("agent line = %+v", line)
	}
	var got []string
	if len(line.Running) > 0 {
		if err := json.Unmarshal(line.Running, &got); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("running = %s, want %v", line.Running, want)
	}
	if len(want) == 0 && line.Running != nil {
		t.Errorf("empty running must be omitted, got %s", line.Running)
	}
	for _, chat := range line.Chats {
		if chat.Running != nil {
			t.Errorf("chat must not carry running, got %s", chat.Running)
		}
	}
}

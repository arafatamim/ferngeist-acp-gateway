package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/catalog"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

// newCustomTestServer wires a test server whose catalog serves custom agents
// from an ephemeral SQLite store, mirroring the daemon's provider wiring.
func newCustomTestServer(t *testing.T) (*Server, *storage.SQLiteStore) {
	t.Helper()

	baseDir := newHarnessBaseDir(t)
	store, err := storage.Open(filepath.Join(baseDir, "test.db"))
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	server := newTestServerWithStore(baseDir, store)
	server.store = store
	server.catalog.SetCustomProvider(func() []catalog.CustomAgent {
		records, err := store.ListCustomAgents(context.Background())
		if err != nil {
			return nil
		}
		out := make([]catalog.CustomAgent, 0, len(records))
		for _, record := range records {
			out = append(out, catalog.CustomAgent{
				ID:          record.ID,
				DisplayName: record.DisplayName,
				Command:     record.Command,
				Args:        record.Args,
				Hint:        record.Hint,
			})
		}
		return out
	})
	return server, store
}

func doCustomAgentRequest(t *testing.T, server *Server, token, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

func TestCustomAgentCreateRoundTrip(t *testing.T) {
	server, store := newCustomTestServer(t)
	token := pairDevice(t, server)

	recorder := doCustomAgentRequest(t, server, token, http.MethodPost, "/v1/agents/custom",
		`{"displayName":"My Agent","command":"my-agent","args":["--acp"]}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", recorder.Code, recorder.Body.String())
	}

	var created struct {
		ID     string `json:"id"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("Unmarshal(create) error = %v", err)
	}
	if created.ID != "custom-my-agent" || created.Source != "custom" {
		t.Fatalf("created = %+v", created)
	}

	stored, err := store.GetCustomAgent(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetCustomAgent() error = %v", err)
	}
	if stored.DisplayName != "My Agent" || stored.Command != "my-agent" || len(stored.Args) != 1 || stored.Args[0] != "--acp" {
		t.Fatalf("stored record = %+v", stored)
	}

	// The list endpoint serves it as a custom agent.
	listRecorder := doCustomAgentRequest(t, server, token, http.MethodGet, "/v1/agents", "")
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("agents status = %d", listRecorder.Code)
	}
	var listed agentsResponse
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &listed); err != nil {
		t.Fatalf("Unmarshal(agents) error = %v", err)
	}
	if got := findAgentState(t, listed.Agents, created.ID); got.Source != "custom" {
		t.Fatalf("listed source = %q, want custom", got.Source)
	}
}

func TestCustomAgentDuplicateDisplayNameSuffixesID(t *testing.T) {
	server, store := newCustomTestServer(t)
	token := pairDevice(t, server)

	first := doCustomAgentRequest(t, server, token, http.MethodPost, "/v1/agents/custom",
		`{"displayName":"My Agent","command":"my-agent","hint":"first"}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, body %s", first.Code, first.Body.String())
	}
	if id := customAgentIDFromResponse(t, first); id != "custom-my-agent" {
		t.Fatalf("first id = %q, want custom-my-agent", id)
	}

	second := doCustomAgentRequest(t, server, token, http.MethodPost, "/v1/agents/custom",
		`{"displayName":"My Agent","command":"other-agent","hint":"second"}`)
	if second.Code != http.StatusCreated {
		t.Fatalf("second create status = %d, body %s", second.Code, second.Body.String())
	}
	if id := customAgentIDFromResponse(t, second); id != "custom-my-agent-2" {
		t.Fatalf("second id = %q, want custom-my-agent-2", id)
	}

	// A save that upserted on the base id would silently replace the first
	// agent's definition while still answering 201.
	original, err := store.GetCustomAgent(context.Background(), "custom-my-agent")
	if err != nil {
		t.Fatalf("GetCustomAgent(custom-my-agent) error = %v", err)
	}
	if original.Command != "my-agent" || original.Hint != "first" {
		t.Fatalf("first agent overwritten: %+v", original)
	}
}

func customAgentIDFromResponse(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()

	var agent struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &agent); err != nil {
		t.Fatalf("Unmarshal(agent) error = %v", err)
	}
	return agent.ID
}

func TestCustomAgentBoundsCountCharactersNotBytes(t *testing.T) {
	server, _ := newCustomTestServer(t)
	token := pairDevice(t, server)

	// 80 runes / 240 bytes: the documented limit is characters, so this must be
	// accepted (a byte-counted bound rejects it two-thirds of the way in).
	atLimit := strings.Repeat("日", maxCustomNameLen)
	accepted := doCustomAgentRequest(t, server, token, http.MethodPost, "/v1/agents/custom",
		fmt.Sprintf(`{"displayName":%q,"command":"my-agent"}`, atLimit))
	if accepted.Code != http.StatusCreated {
		t.Fatalf("create at limit status = %d, body %s", accepted.Code, accepted.Body.String())
	}

	overLimit := strings.Repeat("日", maxCustomNameLen+1)
	rejected := doCustomAgentRequest(t, server, token, http.MethodPost, "/v1/agents/custom",
		fmt.Sprintf(`{"displayName":%q,"command":"my-agent"}`, overLimit))
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("create over limit status = %d, body %s", rejected.Code, rejected.Body.String())
	}
}

func TestCustomAgentCreateRejectsRelativePath(t *testing.T) {
	server, store := newCustomTestServer(t)
	token := pairDevice(t, server)

	recorder := doCustomAgentRequest(t, server, token, http.MethodPost, "/v1/agents/custom",
		`{"displayName":"Relative","command":"tools/relative-agent"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("create status = %d, body %s", recorder.Code, recorder.Body.String())
	}

	// A rejected agent must not be left behind in storage.
	count, err := store.CountCustomAgents(context.Background())
	if err != nil {
		t.Fatalf("CountCustomAgents() error = %v", err)
	}
	if count != 0 {
		t.Fatalf("stored customs = %d, want 0", count)
	}
}

func TestCustomAgentUpdateAppliesAndRollsBackInvalidEdit(t *testing.T) {
	server, store := newCustomTestServer(t)
	token := pairDevice(t, server)

	recorder := doCustomAgentRequest(t, server, token, http.MethodPost, "/v1/agents/custom",
		`{"displayName":"My Agent","command":"my-agent"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", recorder.Code, recorder.Body.String())
	}

	updateRecorder := doCustomAgentRequest(t, server, token, http.MethodPut, "/v1/agents/custom/custom-my-agent",
		`{"hint":"needs a token","args":["--acp","--verbose"]}`)
	if updateRecorder.Code != http.StatusOK {
		t.Fatalf("update status = %d, body %s", updateRecorder.Code, updateRecorder.Body.String())
	}
	updated, err := store.GetCustomAgent(context.Background(), "custom-my-agent")
	if err != nil {
		t.Fatalf("GetCustomAgent() error = %v", err)
	}
	if updated.Hint != "needs a token" || len(updated.Args) != 2 {
		t.Fatalf("updated record = %+v", updated)
	}

	// A relative command fails catalog validation: the edit is rejected and the
	// previous record stays in place.
	badRecorder := doCustomAgentRequest(t, server, token, http.MethodPut, "/v1/agents/custom/custom-my-agent",
		`{"displayName":"Renamed","command":"tools/relative-agent"}`)
	if badRecorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid update status = %d, body %s", badRecorder.Code, badRecorder.Body.String())
	}
	rolledBack, err := store.GetCustomAgent(context.Background(), "custom-my-agent")
	if err != nil {
		t.Fatalf("GetCustomAgent() after rollback error = %v", err)
	}
	if rolledBack.DisplayName != "My Agent" || rolledBack.Command != "my-agent" || rolledBack.Hint != "needs a token" {
		t.Fatalf("rolled back record = %+v", rolledBack)
	}

	// Unknown ids are a 404.
	if missing := doCustomAgentRequest(t, server, token, http.MethodPut, "/v1/agents/custom/custom-nope", `{"hint":"x"}`); missing.Code != http.StatusNotFound {
		t.Fatalf("update unknown status = %d, want 404", missing.Code)
	}
}

func TestCustomAgentCapReached(t *testing.T) {
	server, store := newCustomTestServer(t)
	token := pairDevice(t, server)

	for i := 0; i < maxCustomAgents; i++ {
		record := storage.CustomAgentRecord{
			ID:          fmt.Sprintf("custom-fill-%d", i),
			DisplayName: fmt.Sprintf("Fill %d", i),
			Command:     "my-agent",
		}
		if err := store.SaveCustomAgent(context.Background(), record); err != nil {
			t.Fatalf("SaveCustomAgent() error = %v", err)
		}
	}

	recorder := doCustomAgentRequest(t, server, token, http.MethodPost, "/v1/agents/custom",
		`{"displayName":"One Too Many","command":"my-agent"}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("create status = %d, body %s", recorder.Code, recorder.Body.String())
	}
}

func TestCustomAgentDeleteBlockedWhileRunning(t *testing.T) {
	server, store := newCustomTestServer(t)
	token := pairDevice(t, server)

	binaryPath := filepath.Join(t.TempDir(), namedBinary("custom-mock-agent"))
	buildMockStdioAgent(t, binaryPath)

	createRecorder := doCustomAgentRequest(t, server, token, http.MethodPost, "/v1/agents/custom",
		fmt.Sprintf(`{"displayName":"Runnable","command":%q,"args":["--acp"]}`, binaryPath))
	if createRecorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", createRecorder.Code, createRecorder.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createRecorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("Unmarshal(create) error = %v", err)
	}

	startRecorder := doCustomAgentRequest(t, server, token, http.MethodPost, "/v1/agents/"+created.ID+"/start", "")
	if startRecorder.Code != http.StatusOK {
		t.Fatalf("start status = %d, body %s", startRecorder.Code, startRecorder.Body.String())
	}
	t.Cleanup(func() {
		doCustomAgentRequest(t, server, token, http.MethodPost, "/v1/agents/"+created.ID+"/stop", "")
	})

	blocked := doCustomAgentRequest(t, server, token, http.MethodDelete, "/v1/agents/custom/"+created.ID, "")
	if blocked.Code != http.StatusConflict {
		t.Fatalf("delete while running status = %d, body %s", blocked.Code, blocked.Body.String())
	}
	if _, err := store.GetCustomAgent(context.Background(), created.ID); err != nil {
		t.Fatalf("record removed by blocked delete: %v", err)
	}

	stopRecorder := doCustomAgentRequest(t, server, token, http.MethodPost, "/v1/agents/"+created.ID+"/stop", "")
	if stopRecorder.Code != http.StatusOK {
		t.Fatalf("stop status = %d, body %s", stopRecorder.Code, stopRecorder.Body.String())
	}

	deleted := doCustomAgentRequest(t, server, token, http.MethodDelete, "/v1/agents/custom/"+created.ID, "")
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body %s", deleted.Code, deleted.Body.String())
	}
	var deleteResponse customAgentDeleteResponse
	if err := json.Unmarshal(deleted.Body.Bytes(), &deleteResponse); err != nil {
		t.Fatalf("Unmarshal(delete) error = %v", err)
	}
	if deleteResponse.Deleted != created.ID {
		t.Fatalf("deleted = %q, want %q", deleteResponse.Deleted, created.ID)
	}
	if _, err := store.GetCustomAgent(context.Background(), created.ID); err == nil {
		t.Fatal("record still present after delete")
	}

	second := doCustomAgentRequest(t, server, token, http.MethodDelete, "/v1/agents/custom/"+created.ID, "")
	if second.Code != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want 404", second.Code)
	}
}

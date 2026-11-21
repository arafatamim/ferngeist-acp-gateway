package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/api"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/catalog"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/push"
	gatewayruntime "github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
	"github.com/coder/websocket"
)

func TestRestartUsesChildHarnessStore(t *testing.T) {
	t.Setenv("FERNGEIST_MOCK_LLM_ENDPOINT", "offline:")
	t.Setenv("FERNGEIST_MOCK_PERMISSION", "")
	t.Setenv("FERNGEIST_MOCK_LIST", "")
	bin := filepath.Join(t.TempDir(), "mock-stdio-agent")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if output, err := exec.Command("go", "build", "-o", bin, "../../cmd/mock-stdio-agent").CombinedOutput(); err != nil {
		t.Fatalf("build mock agent: %v\n%s", err, output)
	}
	t.Run("Claude relabels the child transcript", func(t *testing.T) {
		daemonDir, childDir := t.TempDir(), t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", daemonDir)
		daemonTranscript := writeClaudeTranscript(t, daemonDir)
		childTranscript := writeClaudeTranscript(t, childDir)

		ctx, conn := restartHarnessAgent(t, bin, "claude-acp", map[string]string{"CLAUDE_CONFIG_DIR": childDir})
		prompt := `{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"mock_sess_1","prompt":[]}}`
		harnessReply(t, ctx, conn, prompt)

		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			if transcriptEntrypoint(t, daemonTranscript) != "sdk-cli" {
				t.Fatal("Claude altered the daemon's transcript")
			}
			if transcriptEntrypoint(t, childTranscript) == "cli" {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("Claude did not relabel the child transcript")
			case <-ticker.C:
			}
		}
	})
	t.Run("Hermes lists the child store", func(t *testing.T) {
		daemonDir, childDir := t.TempDir(), t.TempDir()
		t.Setenv("HERMES_HOME", daemonDir)
		writeHermesStore(t, daemonDir, "daemon-chat")
		writeHermesStore(t, childDir, "child-chat")

		ctx, conn := restartHarnessAgent(t, bin, "hermes", map[string]string{"HERMES_HOME": childDir})
		reply := harnessReply(t, ctx, conn, `{"jsonrpc":"2.0","id":1,"method":"session/list","params":{}}`)
		var result struct {
			Sessions []struct {
				ID string `json:"sessionId"`
			} `json:"sessions"`
		}
		if err := json.Unmarshal(reply, &result); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, row := range result.Sessions {
			ids = append(ids, row.ID)
		}
		if !slices.Contains(ids, "child-chat") || slices.Contains(ids, "daemon-chat") {
			t.Fatalf("listed sessions = %v, want child-chat and no daemon-chat", ids)
		}
	})
}

func restartHarnessAgent(t *testing.T, bin, id string, env map[string]string) (context.Context, *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)

	store, logger, dir := openTestStore(t), testLogger(t), t.TempDir()
	rt := gatewayruntime.NewSupervisor(logger, dir, store, nil)
	t.Cleanup(rt.Close)
	server, sessions := composeServer(api.Deps{Logger: logger, Catalog: catalog.New(dir, nil), Runtime: rt, Push: push.NewDispatcher(store, nil, logger)}, store)
	t.Cleanup(sessions.Shutdown)
	t.Cleanup(func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	agent := catalog.Agent{ID: id, Detected: true, Security: catalog.SecurityConfig{AllowsRemoteStart: true}, Launch: catalog.LaunchConfig{Mode: "process", Command: bin}}
	old, _, err := sessions.Create(ctx, agent, "device", nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	fresh, token, err := sessions.Restart(ctx, old.RuntimeID, "device", agent, env)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	query := url.Values{"sessionId": {fresh.ID}, "attachToken": {token}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/v1/acp/" + fresh.RuntimeID + "?" + query.Encode()
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() }) // The test has already read its reply.
	return ctx, conn
}

func harnessReply(t *testing.T, ctx context.Context, conn *websocket.Conn, request string) json.RawMessage {
	t.Helper()
	if err := conn.Write(ctx, websocket.MessageText, []byte(request)); err != nil {
		t.Fatal(err)
	}
	for {
		_, frame, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var reply struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(frame, &reply); err != nil {
			t.Fatal(err)
		}
		if len(reply.Error) > 0 {
			t.Fatalf("ACP error: %s", reply.Error)
		}
		if reply.ID == 1 {
			return reply.Result
		}
	}
}

func writeClaudeTranscript(t *testing.T, dir string) string {
	t.Helper()
	project := filepath.Join(dir, "projects", "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "mock_sess_1.jsonl")
	if err := os.WriteFile(path, []byte(`{"entrypoint":"sdk-cli"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func transcriptEntrypoint(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var row struct {
		Entrypoint string `json:"entrypoint"`
	}
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatal(err)
	}
	return row.Entrypoint
}

func writeHermesStore(t *testing.T, dir, sessionID string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }() // Writes finish before the gateway opens the store.
	_, err = db.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY, source TEXT, created_source TEXT, session_key TEXT, model_config TEXT,
		started_at REAL, ended_at REAL, end_reason TEXT, message_count INTEGER, cwd TEXT,
		title TEXT, last_activity_at REAL, hidden INTEGER, archived INTEGER, parent_session_id TEXT
	)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO sessions (id, source, started_at, message_count, hidden, archived)
		VALUES (?, 'tui', 100, 1, 0, 0)`, sessionID)
	if err != nil {
		t.Fatal(err)
	}
}

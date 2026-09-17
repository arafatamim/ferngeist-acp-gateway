package adminclient

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/api"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/catalog"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/config"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/discovery"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/gateway"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/pairing"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

func TestDaemonUnreachableIsReportedWithHint(t *testing.T) {
	// A listener that is closed immediately leaves a port nothing answers on,
	// so the dial is refused the way it is when the daemon is stopped.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("listener.Close() error = %v", err)
	}

	client := New(config.Config{AdminListenAddr: addr})
	_, err = client.ListAgents(context.Background())
	if err == nil {
		t.Fatal("ListAgents() error = nil, want a daemon-unreachable error")
	}
	if !IsDaemonUnreachable(err) {
		t.Fatalf("IsDaemonUnreachable(%v) = false, want true", err)
	}
	if !strings.Contains(err.Error(), DaemonUnreachableHint) {
		t.Fatalf("error = %q, want the shared recovery hint", err.Error())
	}
}

func TestDaemonAnsweredErrorIsNotUnreachable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"unknown custom agent"}`))
	}))
	t.Cleanup(server.Close)

	client := New(config.Config{AdminListenAddr: strings.TrimPrefix(server.URL, "http://")})
	_, err := client.ListAgents(context.Background())
	if err == nil {
		t.Fatal("ListAgents() error = nil, want the gateway's error")
	}
	if IsDaemonUnreachable(err) {
		t.Fatalf("IsDaemonUnreachable(%v) = true, want false for an answered request", err)
	}
	if err.Error() != "unknown custom agent" {
		t.Fatalf("error = %q, want the gateway's own message", err.Error())
	}
}

// newTestCustomAgentClient serves the real admin API — routes, handlers, custom
// agent service and SQLite store — on loopback, so the agent methods are
// exercised end to end against the transport they ship with. It mirrors the
// daemon's catalog provider wiring (custom agents are read back from the store).
func newTestCustomAgentClient(t *testing.T) *Client {
	t.Helper()

	baseDir := t.TempDir()
	store, err := storage.Open(filepath.Join(baseDir, "custom_agents.db"))
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	catalogSvc := catalog.NewWithBaseDir(baseDir)
	catalogSvc.SetCustomProvider(func() []catalog.CustomAgent {
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

	server := api.NewServer(
		config.Config{ListenAddr: "127.0.0.1:0"},
		api.BuildInfo{},
		logger,
		catalogSvc,
		runtime.NewSupervisorWithBaseDir(logger, baseDir, store),
		pairing.NewService(logger, store),
		gateway.New(logger, store),
		discovery.New(logger),
		nil,
		nil,
		store,
		nil,
	)
	adminServer := httptest.NewServer(server.AdminHandler())
	t.Cleanup(adminServer.Close)

	return &Client{baseURL: adminServer.URL, httpClient: adminServer.Client()}
}

func TestAddAndRemoveCustomAgent(t *testing.T) {
	client := newTestCustomAgentClient(t)

	added, err := client.AddCustomAgent(context.Background(), AgentInput{DisplayName: "CLI Agent", Command: "cli-agent"})
	if err != nil {
		t.Fatalf("AddCustomAgent() error = %v", err)
	}
	if added.ID != "custom-cli-agent" {
		t.Fatalf("ID = %q, want custom-cli-agent", added.ID)
	}
	if added.DisplayName != "CLI Agent" || added.Source != "custom" {
		t.Fatalf("agent = %+v, want displayName=%q source=%q", added, "CLI Agent", "custom")
	}
	if added.Launch.Command != "cli-agent" {
		t.Fatalf("Launch.Command = %q, want cli-agent", added.Launch.Command)
	}

	if err := client.RemoveCustomAgent(context.Background(), added.ID); err != nil {
		t.Fatalf("RemoveCustomAgent() error = %v", err)
	}
	agents, err := client.ListAgents(context.Background())
	if err != nil {
		t.Fatalf("ListAgents() error = %v", err)
	}
	for _, agent := range agents {
		if agent.ID == added.ID {
			t.Fatalf("agent %q still listed after removal", added.ID)
		}
	}
}

func TestUpdateCustomAgentIsPartialAndListed(t *testing.T) {
	client := newTestCustomAgentClient(t)
	ctx := context.Background()

	added, err := client.AddCustomAgent(ctx, AgentInput{
		DisplayName: "CLI Agent",
		Command:     "cli-agent",
		Args:        []string{"--acp"},
		Hint:        "first",
	})
	if err != nil {
		t.Fatalf("AddCustomAgent() error = %v", err)
	}

	updated, err := client.UpdateCustomAgent(ctx, added.ID, AgentInput{Args: []string{"--acp", "--verbose"}, Hint: "second"})
	if err != nil {
		t.Fatalf("UpdateCustomAgent() error = %v", err)
	}
	if updated.ID != added.ID {
		t.Fatalf("ID = %q, want unchanged %q", updated.ID, added.ID)
	}
	// An omitted displayName/command is "unchanged"; args are replaced wholesale.
	if updated.DisplayName != "CLI Agent" || updated.Launch.Command != "cli-agent" {
		t.Fatalf("update cleared untouched fields: %+v", updated)
	}
	if len(updated.Launch.Args) != 2 || updated.Launch.Args[1] != "--verbose" {
		t.Fatalf("Launch.Args = %v, want [--acp --verbose]", updated.Launch.Args)
	}
	if updated.Hint != "second" {
		t.Fatalf("Hint = %q, want second", updated.Hint)
	}

	agents, err := client.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents() error = %v", err)
	}
	var listed *Agent
	for i, agent := range agents {
		if agent.ID == added.ID {
			listed = &agents[i]
		}
	}
	if listed == nil {
		t.Fatalf("agent %q missing from list of %d agents", added.ID, len(agents))
	}
	if listed.DisplayName != "CLI Agent" || len(listed.Launch.Args) != 2 {
		t.Fatalf("listed agent = %+v, want updated fields", *listed)
	}
}

// An empty args list means "clear", so it must survive JSON encoding as [] —
// with omitempty on AgentInput.Args the field would vanish and the API would
// read the update as "args unchanged", silently leaving the old args in place.
func TestUpdateCustomAgentClearArgsRoundTrips(t *testing.T) {
	client := newTestCustomAgentClient(t)
	ctx := context.Background()

	added, err := client.AddCustomAgent(ctx, AgentInput{
		DisplayName: "CLI Agent",
		Command:     "cli-agent",
		Args:        []string{"--acp"},
	})
	if err != nil {
		t.Fatalf("AddCustomAgent() error = %v", err)
	}
	if len(added.Launch.Args) != 1 {
		t.Fatalf("added Launch.Args = %v, want [--acp]", added.Launch.Args)
	}

	cleared, err := client.UpdateCustomAgent(ctx, added.ID, AgentInput{Args: []string{}})
	if err != nil {
		t.Fatalf("UpdateCustomAgent() error = %v", err)
	}
	if len(cleared.Launch.Args) != 0 {
		t.Fatalf("Launch.Args = %v, want cleared", cleared.Launch.Args)
	}

	agents, err := client.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents() error = %v", err)
	}
	for _, agent := range agents {
		if agent.ID == added.ID && len(agent.Launch.Args) != 0 {
			t.Fatalf("listed agent args = %v, want cleared", agent.Launch.Args)
		}
	}
}

func TestRemoveCustomAgentSurfacesNotFound(t *testing.T) {
	client := newTestCustomAgentClient(t)

	err := client.RemoveCustomAgent(context.Background(), "custom-missing")
	if err == nil || !strings.Contains(err.Error(), "unknown custom agent") {
		t.Fatalf("RemoveCustomAgent(unknown) error = %v, want not-found message", err)
	}
}

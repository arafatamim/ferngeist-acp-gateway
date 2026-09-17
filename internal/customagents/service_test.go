package customagents

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/catalog"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

// newTestService wires a service whose catalog serves custom agents from an
// ephemeral SQLite store, mirroring the daemon's provider wiring.
func newTestService(t *testing.T) *Service {
	t.Helper()

	store, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	catalogSvc := catalog.NewWithBaseDirAndRegistry(t.TempDir(), nil)
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(store, catalogSvc, func() []runtime.Runtime { return nil }, logger)
}

func TestCreateRejectsRelativePath(t *testing.T) {
	svc := newTestService(t)

	_, err := svc.Create(context.Background(), Create{DisplayName: "R", Command: "tools/r"})
	var opErr Error
	if !errors.As(err, &opErr) || opErr.StatusCode() != http.StatusBadRequest {
		t.Fatalf("Create(relative) = %v, want 400 Error", err)
	}
}

func TestCreateThenListServesCustomAgent(t *testing.T) {
	svc := newTestService(t)

	created, err := svc.Create(context.Background(), Create{
		DisplayName: "My Agent",
		Command:     "ferngeist-missing-binary",
		Args:        []string{"--acp"},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.ID != "custom-my-agent" || created.Source != "custom" {
		t.Fatalf("created = %+v", created)
	}
	if created.Detected {
		t.Fatalf("created.Detected = true for a missing binary")
	}

	var listed catalog.Agent
	found := false
	for _, agent := range svc.List() {
		if agent.ID == created.ID {
			listed, found = agent, true
			break
		}
	}
	if !found {
		t.Fatalf("List() does not serve %q", created.ID)
	}
	if listed.Source != "custom" {
		t.Fatalf("listed.Source = %q, want custom", listed.Source)
	}
	if listed.Detected {
		t.Fatalf("listed.Detected = true for a missing binary")
	}
}

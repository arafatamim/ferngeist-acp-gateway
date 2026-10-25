package projects

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/workspace"
)

func TestExpiredPayloadIsReclaimedWithoutForgettingItsRepos(t *testing.T) {
	lister := newMockLister(t)()
	lister.workspace = workspace.New(func() *storage.SQLiteStore { return nil }, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now, nil)
	var first AgentLine
	lister.List(context.Background(), []string{"mock-acp"}, func(line AgentLine) { first = line })
	lister.mu.Lock()
	fresh := len(lister.cached)
	lister.mu.Unlock()
	if fresh != 1 {
		t.Fatal("fresh listing was evicted")
	}
	lister.mu.Lock()
	cached := lister.cached["mock-acp"]
	cached.at = time.Now().Add(-cacheTTL)
	lister.cached["mock-acp"] = cached
	lister.mu.Unlock()
	waitForCacheEviction(t, lister)
	repo := first.Chats[0].Repo
	if err := os.Remove(repo); err != nil {
		t.Fatal(err)
	}
	checkouts := lister.Checkouts(context.Background(), nil)
	if len(checkouts.MissingRepos) != 1 || checkouts.MissingRepos[0] != repo {
		t.Fatalf("after eviction: %+v, want missing repo %s", checkouts, repo)
	}
	lister.List(context.Background(), []string{"mock-acp"}, func(line AgentLine) {
		if line.Status != StatusOK || len(line.Chats) != 1 {
			t.Fatalf("after expiry: %+v, want remaining chat", line)
		}
	})
}

func waitForCacheEviction(t *testing.T, lister *Lister) {
	t.Helper()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		lister.mu.Lock()
		count := len(lister.cached)
		lister.mu.Unlock()
		if count == 0 {
			return
		}
		select {
		case <-tick.C:
		case <-timeout.C:
			t.Fatal("expired listing payload is still retained")
		}
	}
}

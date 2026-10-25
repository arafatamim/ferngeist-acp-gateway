package projects

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

func TestPressureEvictsOldestIdleProcessAndAllowsACompleteRelisting(t *testing.T) {
	lister := newMockLister(t)()
	oldest := connectListing(t, lister)
	newer := connectListing(t, lister)
	borrowed := connectListing(t, lister)
	defer lister.discard(borrowed)
	lister.keepWarm("mock-acp", oldest)
	lister.keepWarm("newer", newer)
	lister.mu.Lock()
	oldest.lastUsed = time.Now().Add(-time.Minute)
	lister.mu.Unlock()
	before := listingChildrenRSS(os.Getpid())
	lister.maintain(time.Now(), true)
	select {
	case <-oldest.proc.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("oldest idle process was not stopped")
	}
	after := listingChildrenRSS(os.Getpid())
	assertProcessLists(t, newer)
	assertProcessLists(t, borrowed)
	start := time.Now()
	var line AgentLine
	lister.List(context.Background(), []string{"mock-acp"}, func(got AgentLine) { line = got })
	if line.Status != StatusOK || len(line.Chats) != 2 {
		t.Fatalf("after eviction: %+v, want complete listing", line)
	}
	t.Logf("simulated pressure: descendant RSS before=%d after=%d bytes; foreground relist=%s", before, after, time.Since(start))
}

func assertProcessLists(t *testing.T, agent *agentConn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sessions, err := listSessions(ctx, agent.conn)
	if err != nil || len(sessions) != 2 {
		t.Fatalf("retained process: %d sessions, %v", len(sessions), err)
	}
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

func listingProcessRSS(pid int) int64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		return -1
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return -1
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return -1
	}
	return pages * int64(os.Getpagesize())
}

func listingChildrenRSS(pid int) int64 {
	// A Go child may be spawned by any OS thread, not just the main thread.
	paths, err := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/children", pid))
	if err != nil || len(paths) == 0 {
		return -1
	}
	var rss int64
	for _, path := range paths {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			continue // A thread or child exiting during a sample is expected.
		}
		for _, raw := range strings.Fields(string(data)) {
			rss += listingChildRSS(raw)
		}
	}
	return rss
}

func listingChildRSS(raw string) int64 {
	child, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return max(0, listingProcessRSS(child)) + max(0, listingChildrenRSS(child))
}

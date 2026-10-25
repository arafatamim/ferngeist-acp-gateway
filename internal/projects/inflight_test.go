package projects

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

func TestStartedListingSurvivesItsLastRequesterAndServesLaterDevices(t *testing.T) {
	lister := newMockLister(t)()
	agent := connectListing(t, lister)
	requested, release := blockListingReply(t, agent)
	lister.keepWarm("mock-acp", agent)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	first := make(chan AgentLine, 1)
	go func() { first <- lister.agentLine(ctx, "mock-acp") }()
	select {
	case <-requested:
	case <-time.After(5 * time.Second):
		t.Fatal("listing never reached the agent")
	}
	cancel()
	if line := <-first; line.Status != StatusFailed {
		t.Fatalf("cancelled requester = %+v, want failure", line)
	}
	second := make(chan AgentLine, 1)
	go func() { second <- lister.agentLine(t.Context(), "mock-acp") }()
	waitForRequesters(t, lister, 1)
	close(release)
	select {
	case line := <-second:
		assertContinuedListing(t, line)
	case <-time.After(5 * time.Second):
		t.Fatal("later device did not receive the started listing")
	}
	assertContinuedListing(t, lister.agentLine(t.Context(), "mock-acp"))
	lister.mu.Lock()
	retained := lister.warm["mock-acp"] == agent
	lister.mu.Unlock()
	if !retained {
		t.Fatal("completed listing did not retain its original process")
	}
	select {
	case <-agent.proc.Done():
		t.Fatal("requester cancellation stopped the listing process")
	default:
	}
}

func assertContinuedListing(t *testing.T, line AgentLine) {
	t.Helper()
	if line.Status != StatusOK || len(line.Chats) != 1 || line.Chats[0].SessionID != "continued-chat" {
		t.Fatalf("continued listing = %+v, want the complete blocked reply", line)
	}
}

func blockListingReply(t *testing.T, agent *agentConn) (<-chan struct{}, chan struct{}) {
	t.Helper()
	requests, clientWrites := io.Pipe()
	replies, serverWrites := io.Pipe()
	requested, release, stop := make(chan struct{}, 1), make(chan struct{}), make(chan struct{})
	server := acp.NewConnection(func(ctx context.Context, method string, _ json.RawMessage) (any, *acp.RequestError) {
		if method != acp.AgentMethodSessionList {
			return nil, acp.NewMethodNotFound(method)
		}
		requested <- struct{}{}
		select {
		case <-release:
			return acp.ListSessionsResponse{Sessions: []acp.SessionInfo{{SessionId: "continued-chat", Cwd: "/fixture"}}}, nil
		case <-stop:
			return nil, acp.NewMethodNotFound(method)
		case <-ctx.Done():
			return nil, acp.NewMethodNotFound(method)
		}
	}, serverWrites, requests)
	agent.conn = acp.NewConnection(refuseClientRequests, clientWrites, replies)
	t.Cleanup(func() {
		close(stop)
		for _, pipe := range []io.Closer{requests, clientWrites, replies, serverWrites} {
			if err := pipe.Close(); err != nil {
				t.Error(err)
			}
		}
		<-server.Done()
		<-agent.conn.Done()
	})
	return requested, release
}

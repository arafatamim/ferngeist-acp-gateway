// Package projects lists the chats of bound agents for the client's
// project-first home. It starts each agent with the daemon's own environment,
// asks it for its sessions over ACP session/list, and groups every chat by the
// git repo and worktree its cwd belongs to. Listing runs outside the
// device-owned session lifecycle: its processes are never runtimes and never
// count against a device's session limit.
package projects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"golang.org/x/sync/singleflight"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/catalog"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/workspace"
)

// Listing limits from the client spec (§9.2).
const (
	maxConcurrentAgents = 4
	agentTimeout        = 10 * time.Second
	maxPages            = 5
	// warmTTL keeps a listed agent's process after its listing, so the next
	// fetch skips its startup.
	warmTTL = 3 * time.Minute
	// cacheTTL serves one agent's line to every device fetching within it.
	cacheTTL = 10 * time.Second
)

// Agent line statuses.
const (
	StatusOK           = "ok"
	StatusUnsupported  = "unsupported"
	StatusFailed       = "failed"
	StatusAuthRequired = "authRequired"
)

// authRequiredCode is the ACP error an agent answers with when it has no
// stored login.
const authRequiredCode = -32000

var errListUnsupported = errors.New("agent does not support session/list")

// AgentLine is one agent's result, written as one line of GET /v1/projects.
type AgentLine struct {
	Type            string   `json:"type"`
	AgentID         string   `json:"agentId"`
	Status          string   `json:"status"`
	Error           *string  `json:"error"`
	Chats           []Chat   `json:"chats"`
	Running         []string `json:"running,omitempty"`
	LaunchAvailable *bool    `json:"launchAvailable,omitempty"`
	// SessionCapabilities is the agent's own initialize answer, passed through
	// untouched so clients see unstable ones the SDK does not model; ok lines only.
	SessionCapabilities json.RawMessage `json:"sessionCapabilities,omitempty"`
}

// Chat is one agent session, placed in its repo and worktree.
type Chat struct {
	SessionID string  `json:"sessionId"`
	Title     *string `json:"title"`
	Cwd       string  `json:"cwd"`
	UpdatedAt *string `json:"updatedAt"`
	Repo      string  `json:"repo"`
	Worktree  *string `json:"worktree"`
}

// Lister lists chats across agents. Every request shares its concurrency
// limit, its per-agent cache and its warm processes.
type Lister struct {
	catalog   *catalog.Service
	runtime   *runtime.Supervisor
	workspace *workspace.Service
	slots     chan struct{}
	flights   singleflight.Group

	mu     sync.Mutex
	cached map[string]cachedLine
	warm   map[string]*agentConn
	closed bool
	// Known roots carry no resolution result: every request rechecks them, including
	// when an agent fails and the client must retain its previous snapshot.
	knownRepos map[string]struct{}
	// stopping tracks processes being discarded in the background, for Close.
	stopping sync.WaitGroup
}

type cachedLine struct {
	line AgentLine
	at   time.Time
}

// agentConn is an initialized agent that advertises session/list.
type agentConn struct {
	proc  *runtime.Process
	conn  *acp.Connection
	caps  json.RawMessage
	timer *time.Timer
}

// initReply is the part of initialize the lister reads, kept raw: the SDK's
// type drops capabilities it does not model yet.
type initReply struct {
	AgentCapabilities struct {
		SessionCapabilities json.RawMessage `json:"sessionCapabilities"`
	} `json:"agentCapabilities"`
}

// refuseClientRequests answers every request the agent makes of the client. A
// listing opens no session, so none is expected; notifications are dropped.
func refuseClientRequests(_ context.Context, method string, _ json.RawMessage) (any, *acp.RequestError) {
	return nil, acp.NewMethodNotFound(method)
}

// New returns a Lister that resolves agents through catalog, starts them
// through rt and reads checkouts through ws.
func New(catalog *catalog.Service, rt *runtime.Supervisor, ws *workspace.Service) *Lister {
	return &Lister{
		catalog:    catalog,
		runtime:    rt,
		workspace:  ws,
		slots:      make(chan struct{}, maxConcurrentAgents),
		cached:     map[string]cachedLine{},
		warm:       map[string]*agentConn{},
		knownRepos: map[string]struct{}{},
	}
}

// List lists each agent's chats and calls emit once per agent as soon as that
// agent finishes. emit is never called concurrently.
func (l *Lister) List(ctx context.Context, agentIDs []string, emit func(AgentLine)) {
	repos := &repoCache{byPath: map[string]repoInfo{}}
	var emitMu sync.Mutex
	var wg sync.WaitGroup
	for _, id := range agentIDs {
		wg.Go(func() {
			line := l.placeChats(ctx, repos, l.agentLine(ctx, id))
			emitMu.Lock()
			emit(line)
			emitMu.Unlock()
		})
	}
	wg.Wait()
}

// placeChats rechecks the filesystem even when the ACP answer came from cache.
// The raw cached chats stay intact so a later listing can resolve a moved repo.
func (l *Lister) placeChats(ctx context.Context, repos *repoCache, line AgentLine) AgentLine {
	if ctx.Err() != nil {
		return line.failed(StatusFailed, ctx.Err())
	}
	chats := make([]Chat, 0, len(line.Chats))
	for _, chat := range line.Chats {
		if missingFolder(ctx, chat.Cwd) {
			continue
		}
		info := repos.resolve(ctx, chat.Cwd)
		chat.Repo, chat.Worktree = info.repo, info.worktree
		l.mu.Lock()
		l.knownRepos[chat.Repo] = struct{}{}
		l.mu.Unlock()
		if !missingFolder(ctx, chat.Repo) {
			chats = append(chats, chat)
		}
	}
	if ctx.Err() != nil {
		return line.failed(StatusFailed, ctx.Err())
	}
	line.Chats = chats
	return line
}

// agentLine answers from the cache, else joins the agent's one in-flight
// listing, so concurrent devices start it once.
func (l *Lister) agentLine(ctx context.Context, id string) AgentLine {
	l.mu.Lock()
	cached, ok := l.cached[id]
	l.mu.Unlock()
	if ok && time.Since(cached.at) < cacheTTL {
		return cached.line
	}
	flight := l.flights.DoChan(id, func() (any, error) {
		// Other requests may be waiting on it, so this client leaving must not cancel it.
		line := l.listAgent(context.WithoutCancel(ctx), id)
		l.mu.Lock()
		l.cached[id] = cachedLine{line: line, at: time.Now()}
		l.mu.Unlock()
		return line, nil
	})
	select {
	case res := <-flight:
		return res.Val.(AgentLine)
	case <-ctx.Done():
		return AgentLine{Type: "agent", AgentID: id, Chats: []Chat{}}.failed(StatusFailed, ctx.Err())
	}
}

// listAgent lists one agent's chats on its warm process or a fresh one, and
// keeps the process warm only after a full listing.
func (l *Lister) listAgent(ctx context.Context, id string) AgentLine {
	l.slots <- struct{}{}
	defer func() { <-l.slots }()
	line := AgentLine{Type: "agent", AgentID: id, Status: StatusOK, Chats: []Chat{}}
	agentCtx, cancel := context.WithTimeout(ctx, agentTimeout)
	defer cancel()
	agent, err := l.connect(agentCtx, id)
	var sessions []acp.SessionInfo
	if err == nil {
		if sessions, err = listSessions(agentCtx, agent.conn); err != nil {
			l.discard(agent)
		}
	}
	var reqErr *acp.RequestError
	switch {
	case errors.Is(err, errListUnsupported):
		return line.failed(StatusUnsupported, err)
	case errors.As(err, &reqErr) && reqErr.Code == authRequiredCode:
		return line.failed(StatusAuthRequired, errors.New(reqErr.Message))
	case agentCtx.Err() != nil:
		return line.failed(StatusFailed, fmt.Errorf("timed out after %s", agentTimeout))
	case err != nil:
		return line.failed(StatusFailed, err)
	}
	l.keepWarm(id, agent)
	line.SessionCapabilities = agent.caps
	for _, s := range sessions {
		line.Chats = append(line.Chats, Chat{
			SessionID: string(s.SessionId),
			Title:     s.Title,
			Cwd:       s.Cwd,
			UpdatedAt: s.UpdatedAt,
			Repo:      s.Cwd,
		})
	}
	return line
}

func (line AgentLine) failed(status string, err error) AgentLine {
	msg := err.Error()
	line.Status = status
	line.Error = &msg
	if errors.Is(err, catalog.ErrAgentNotFound) || errors.Is(err, runtime.ErrAgentNotDetected) ||
		errors.Is(err, runtime.ErrExecutableNotFound) {
		available := false
		line.LaunchAvailable = &available
	}
	return line
}

// connect takes the agent's warm process if it is still running, else starts
// one and initializes it without terminal-auth capability. It never calls
// authenticate.
func (l *Lister) connect(ctx context.Context, id string) (*agentConn, error) {
	l.mu.Lock()
	warm := l.warm[id]
	delete(l.warm, id)
	l.mu.Unlock()
	if warm != nil {
		warm.timer.Stop()
		select {
		case <-warm.proc.Done():
			l.discard(warm)
		default:
			return warm, nil
		}
	}
	agent, err := l.catalog.Get(id)
	if err != nil {
		return nil, err
	}
	proc, err := l.runtime.Spawn(agent)
	if err != nil {
		return nil, err
	}
	fresh := &agentConn{proc: proc, conn: acp.NewConnection(refuseClientRequests, proc.Stdin, proc.Stdout)}
	init, err := acp.SendRequest[initReply](fresh.conn, ctx, acp.AgentMethodInitialize,
		acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	fresh.caps = init.AgentCapabilities.SessionCapabilities
	var caps struct {
		List *json.RawMessage `json:"list"`
	}
	switch {
	case err != nil:
		err = fmt.Errorf("initialize: %w", err)
	case json.Unmarshal(fresh.caps, &caps) != nil || caps.List == nil:
		err = errListUnsupported
	}
	if err != nil {
		l.discard(fresh)
		return nil, err
	}
	return fresh, nil
}

// keepWarm parks agent for warmTTL; the agent's one-flight-at-a-time listing
// means no other process of it is parked.
func (l *Lister) keepWarm(id string, agent *agentConn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		l.discardLocked(agent)
		return
	}
	agent.timer = time.AfterFunc(warmTTL, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.warm[id] == agent {
			delete(l.warm, id)
			l.discardLocked(agent)
		}
	})
	l.warm[id] = agent
}

// discard stops agent's process in the background, so no line waits on it.
func (l *Lister) discard(agent *agentConn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.discardLocked(agent)
}

func (l *Lister) discardLocked(agent *agentConn) {
	l.stopping.Go(func() { l.runtime.Discard(agent.proc) })
}

// Close stops every warm process and waits for each discard. It runs after the
// HTTP server drained, so no listing parks a process afterwards.
func (l *Lister) Close() {
	l.mu.Lock()
	l.closed = true
	for id, agent := range l.warm {
		agent.timer.Stop()
		delete(l.warm, id)
		l.discardLocked(agent)
	}
	l.mu.Unlock()
	l.stopping.Wait()
}

// listSessions pages session/list.
func listSessions(ctx context.Context, conn *acp.Connection) ([]acp.SessionInfo, error) {
	var sessions []acp.SessionInfo
	var cursor *string
	for range maxPages {
		page, err := acp.SendRequest[acp.ListSessionsResponse](conn, ctx, acp.AgentMethodSessionList,
			acp.ListSessionsRequest{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("session/list: %w", err)
		}
		sessions = append(sessions, page.Sessions...)
		if page.NextCursor == nil {
			break
		}
		cursor = page.NextCursor
	}
	return sessions, nil
}

package session

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
	"github.com/coder/websocket"
)

// acpWebSocketWriteTimeout is the write deadline per WebSocket frame — keep in
// sync with api/server.go:acpWebSocketWriteTimeout (same value, separate context
// ownership: API package uses its own context for handler frames; pump creates
// its own for live stdout writes).
const acpWebSocketWriteTimeout = 30 * time.Second

// maxAgentFrameBytes is the pathological-agent safety bound on a single agent
// stdout frame. Real ACP frames are far smaller; this only guards against a
// misbehaving agent emitting an unbounded line (which would otherwise grow the
// accumulation buffer without limit). It is deliberately much larger than the
// old scanner cap so legitimate large frames (chat history, tool output) pass.
const maxAgentFrameBytes = 64 << 20 // 64 MiB

// maxClientQueueBytes bounds the per-client outbound backlog. A session/load
// history replay arrives as one burst (one frame per streamed chunk, so a long
// transcript is tens of thousands of frames in milliseconds) while the phone
// drains one WebSocket write at a time. The queue therefore has no frame cap:
// any frame cap smaller than the largest replay drops frames, and a dropped
// frame either corrupts the transcript (turns glued together) or is the
// session/load response itself, which leaves the client waiting out its load
// deadline. Overflowing this byte cap disconnects the client instead of
// dropping (see queueLocked); the client reconnects and re-loads.
const maxClientQueueBytes = 128 << 20 // 128 MiB

// clientQueue is the per-client outbound frame queue: unbounded in frames,
// capped in queued bytes. Frames a writer has taken but not yet written are
// not counted, so worst-case memory is about twice the cap.
type clientQueue struct {
	mu     sync.Mutex
	frames []string
	bytes  int
	ready  chan struct{} // capacity 1: wakes the writer when frames are queued
}

func newClientQueue() *clientQueue {
	return &clientQueue{ready: make(chan struct{}, 1)}
}

// push queues frame. It reports false, queuing nothing, when the frame would
// take the backlog past maxClientQueueBytes. A lone frame larger than the cap
// is still accepted into an empty queue so it can never wedge the session.
func (q *clientQueue) push(frame string) bool {
	q.mu.Lock()
	if len(q.frames) > 0 && q.bytes+len(frame) > maxClientQueueBytes {
		q.mu.Unlock()
		return false
	}
	q.frames = append(q.frames, frame)
	q.bytes += len(frame)
	q.mu.Unlock()
	select {
	case q.ready <- struct{}{}:
	default:
	}
	return true
}

// take removes and returns every queued frame, oldest first.
func (q *clientQueue) take() []string {
	q.mu.Lock()
	frames := q.frames
	q.frames = nil
	q.bytes = 0
	q.mu.Unlock()
	return frames
}

// StdioPump moves bytes between one agent process and whichever WebSocket
// client is attached, and owns the agent session's memory of the ACP
// conversation while doing it. It runs independently of any WebSocket client —
// agent output is forwarded when a client is attached and discarded when none
// is — and it always fires push notifications on notable events, because the
// gateway cannot tell whether the app is foregrounded (only whether a socket is
// attached, a poor proxy). The client decides whether to display them.
//
// The pump itself parses nothing: it reads a frame, hands it to the ordered
// stage list in pipeline.go, and queues whatever comes back. Agent-side
// conversation state lives in acp (acpstate.go), push notification decisions in
// notify.go, client request ids in request_ids.go, and re-load replay in
// load_recovery.go.
type StdioPump struct {
	proc      *runtime.Process
	runtimeID string
	sessionID string
	agentID   string
	logger    *slog.Logger
	appendLog func(string, string, string)

	// frameLog optionally records every raw ACP frame (in + out) as
	// newline-delimited JSON in per-agent files. nil when disabled.
	frameLog *frameLogManager

	// acp is everything the gateway remembers about the ACP conversation on
	// this agent process (initialize cache, agent name, session ids and cwds,
	// turn state, running tool kinds). One struct, one mutex.
	acp acpState

	// onPushNotification receives notable events; nil disables pushes.
	onPushNotification func(PushEvent)

	// ProgressInterval is the minimum time between progress pushes.
	// 0 means push every changed summary with no throttle.
	ProgressInterval time.Duration

	// Throttle/dedupe state for live progress pushes, per ACP session so one
	// chat's progress never mutes another's on a shared agent. Guarded by
	// progressMu. ponytail: entries are never evicted; one per chat the pump hosts.
	progressMu sync.Mutex
	progress   map[string]*progressState

	// Translates client request ids so requests from successive connections
	// never share one. See requestIDs.
	reqIDs requestIDs

	// Resilient re-load support: buffers session/update history and recovers
	// "already loaded" rejections from agents that keep the session loaded
	// across a disconnect. See LoadRecovery. Nil when disabled.
	loadRecovery *LoadRecovery

	// turnResults keeps a prompt's outcome when it finishes while no client
	// is attached (its reply is then dropped): stop reason, usage and
	// duration per ACP session, flushed ahead of the next session/load or
	// session/resume reply. Guarded by turnMu; latest wins.
	turnMu      sync.Mutex
	turnResults map[string]turnResult

	clientMu sync.Mutex
	client   *websocket.Conn // current connected WebSocket, or nil
	connGen  int64           // bumped on every Attach; fences stale Bind/Detach from evicted conns

	// Per-client write queue. The drain loop enqueues outbound frames and
	// returns immediately — it must never block on a client write, or a slow
	// client would stall agent stdout draining and the session would look
	// dead. A dedicated writer goroutine (owned by the bound client) drains
	// the queue with per-frame timeouts. Guarded by clientMu; writerCtx is
	// cancelled on takeover/detach to stop the goroutine, and writerDone is
	// closed when the goroutine exits (after flushing queued frames).
	writerQ      *clientQueue
	writerCtx    context.Context
	writerCancel context.CancelFunc
	writerDone   chan struct{}
	closed       bool // set when the drain loop exits; no new Bind may attach

	// Cumulative count of outbound frames not delivered because the client's
	// backlog overflowed maxClientQueueBytes. Each overflow disconnects the
	// client (logged where it happens), and this counter lets tests and
	// diagnostics assert a session stayed lossless.
	droppedFrames atomic.Uint64

	// Unix nanos of the last frame successfully written to a client. The
	// keepalive reads it: a pong queued behind a long replay can arrive late,
	// but a peer still draining writes is not the half-open socket the
	// keepalive exists to catch.
	lastClientWrite atomic.Int64

	lastStdoutAt time.Time // updated on each agent stdout line; used by reaper to avoid killing active agents
	lastStdoutMu sync.Mutex
}

// StdoutDrainLoop continuously reads from agent stdout and forwards frames
// to an attached WebSocket client. When no client is connected, frames are
// discarded after their stage list runs and the log append. Push notifications
// fire on notable events regardless of whether a client is attached. The loop
// stops when the context is cancelled.
func (p *StdioPump) StdoutDrainLoop(ctx context.Context) {
	// ReadSlice, not ReadString. ReadString copies every byte until the newline
	// before returning, so a newline-less stream grows without bound inside
	// bufio — the old cap only set a flag after that copy. A Scanner is also
	// wrong here: its hard line cap (previously 1 MiB) returns ErrTooLong and
	// the drain loop exits, which closed the client WebSocket with no log
	// ("connection dropped while loading a session"). readStdoutFrame keeps
	// frames up to maxAgentFrameBytes and discards the rest of a longer line.
	reader := bufio.NewReader(p.proc.Stdout)
drain:
	for {
		select {
		case <-ctx.Done():
			// Session teardown must run the same exit path as EOF below
			// (stop writer, flush, close conn): returning here would orphan
			// a bound clientWriterLoop parked in select with a live ctx.
			break drain
		default:
		}
		frame, dropped, err := readStdoutFrame(reader, maxAgentFrameBytes)
		if dropped {
			p.logger.Warn("agent stdout frame exceeds safety cap; dropping",
				"runtime_id", p.runtimeID, "cap", maxAgentFrameBytes)
		} else if err == nil || frame != "" {
			p.handleStdoutLine(frame)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				p.logger.Warn("agent stdout read failed",
					"runtime_id", p.runtimeID, "error", err)
			}
			break
		}
	}

	// After the loop exits (ctx cancelled, agent stdout closed, or read error),
	// stop the writer and wait for it to flush its queue, then close the
	// attached client WebSocket. This unblocks proxyWebSocketToStdio's read
	// loop so handleSessionWebSocket can clean up the connection. Without this,
	// a dead agent — whose stdout pipe has closed — leaves the WebSocket open
	// and the client waiting forever.
	p.clientMu.Lock()
	conn := p.client
	done := p.writerDone
	p.closed = true
	p.stopWriterLocked()
	p.clientMu.Unlock()
	if done != nil {
		// Bound the wait: a stuck client must not hang shutdown, but normally
		// the flush completes in milliseconds.
		select {
		case <-done:
		case <-time.After(acpWebSocketWriteTimeout + time.Second):
			p.logger.Warn("timed out waiting for client write flush")
		}
	}
	if conn != nil {
		_ = conn.CloseNow()
	}
}

func (p *StdioPump) stopWriterLocked() {
	if p.writerCancel != nil {
		p.writerCancel()
		p.writerCancel = nil
	}
	p.writerQ = nil
	p.writerCtx = nil
	p.writerDone = nil
}

// readStdoutFrame reads one newline-delimited stdout frame.
//
// The payload (bytes before the delimiter) is kept only up to max. Exactly
// max is a valid frame; one byte more is dropped. Once past the cap, the
// rest of the line is discarded until the newline or EOF and is not retained,
// so a newline-less stream cannot grow the buffer. The delimiter itself does
// not count toward max and is not part of frame. An oversized line is never
// returned in pieces — a partial frame would be parsed as ACP.
//
// ReadString cannot implement this. It copies every byte of the line into one
// string before returning, so the cap would run only after the allocation.
func readStdoutFrame(r *bufio.Reader, max int) (frame string, dropped bool, err error) {
	var buf strings.Builder
	for {
		// ReadSlice's slice is invalid after the next read; copy kept bytes
		// before continuing.
		frag, readErr := r.ReadSlice('\n')
		if !dropped {
			payload := frag
			if n := len(frag); n > 0 && frag[n-1] == '\n' {
				payload = frag[:n-1]
			}
			room := max - buf.Len()
			if room < 0 {
				room = 0
			}
			if len(payload) > room {
				dropped = true
				buf.Reset()
			} else if len(payload) > 0 {
				buf.Write(payload)
			}
		}
		switch {
		case errors.Is(readErr, bufio.ErrBufferFull):
			continue
		case readErr != nil:
			if dropped {
				return "", true, readErr
			}
			return buf.String(), false, readErr
		default:
			if dropped {
				return "", true, nil
			}
			return buf.String(), false, nil
		}
	}
}

// queueLocked moves frames to the bound client's writer goroutine and returns
// immediately — the drain loop must never block on a client write, or a slow
// client would stall agent stdout draining (agent pipe fills, agent stalls,
// session looks dead) and takeover would be blocked. The writer goroutine
// applies the per-frame timeout and closes the conn on failure. Caller holds
// clientMu.
//
// A client whose backlog overflows is disconnected rather than sent a stream
// with holes in it: a slow client must not backpressure the agent, and a
// JSON-RPC stream missing frames is worse than a reconnect. The returned
// connection (if any) must be closed by the caller after releasing clientMu.
func (p *StdioPump) queueLocked(frames []string) *websocket.Conn {
	for i, frame := range frames {
		if p.writerQ.push(frame) {
			continue
		}
		dropped := p.droppedFrames.Add(uint64(len(frames) - i))
		p.logger.Warn("client backlog exceeds cap; disconnecting client",
			"runtime_id", p.runtimeID, "session_id", p.sessionID,
			"cap_bytes", maxClientQueueBytes, "dropped_total", dropped)
		evict := p.client
		p.client = nil
		p.stopWriterLocked()
		return evict
	}
	return nil
}

// WriteToAgent writes one client→agent frame to the agent's stdin. The frame is
// parsed once. A duplicate `initialize` from a reconnecting client is answered
// from the cached response instead: the agent is already initialized, and a
// strict agent may reject a second handshake by exiting. Anything else is
// rewritten for this connection and recorded (see prepareOutbound).
func (p *StdioPump) WriteToAgent(payload []byte) error {
	f := parseClientFrame(payload)
	if p.replayInitialize(f) {
		return nil
	}
	payload = p.prepareOutbound(f)
	if p.frameLog != nil {
		p.frameLog.append(p.agentID, p.runtimeID, p.sessionID, "in", payload)
	}
	return p.writeToAgent(payload)
}

// WriteSessionClose sends the agent a session/close request during session
// teardown. It is the only stdin write path that bypasses the inbound frame
// observation in WriteToAgent — the frame is gateway-originated (not a client
// frame, so it must never contaminate the ACP session-id/cwd state) and it is
// always the final frame before the process is killed. It still passes through
// the frame log so the teardown handshake is auditable.
func (p *StdioPump) WriteSessionClose(payload []byte) error {
	if p.frameLog != nil {
		p.frameLog.append(p.agentID, p.runtimeID, p.sessionID, "in", payload)
	}
	return p.writeToAgent(payload)
}

// writeToAgent writes one frame to the agent's stdin followed by a newline.
func (p *StdioPump) writeToAgent(payload []byte) error {
	_, err := p.proc.Stdin.Write(append(payload, '\n'))
	return err
}

// Attach claims the pump for a new client: bumps the connection generation and
// clears the current client so no frames are written during the WebSocket
// upgrade. It returns the generation the caller must pass to Bind and Detach.
// Any previously-bound connection is evicted (closed) after the lock is
// released — once the pointer is cleared no write path can reach it, so the
// close cannot race a live forward.
func (p *StdioPump) Attach() int64 {
	p.clientMu.Lock()
	evict := p.client
	p.client = nil
	p.stopWriterLocked()
	p.connGen++
	gen := p.connGen
	p.clientMu.Unlock()
	if evict != nil {
		_ = evict.CloseNow()
	}
	return gen
}

// Bind attaches conn to the pump iff gen is still the current generation and
// the pump has not closed. It returns false when a newer Attach has superseded
// this generation or the drain loop has exited (session closing), in which
// case the caller must discard conn. On success it spawns the per-client
// writer goroutine that drains outbound frames to conn.
func (p *StdioPump) Bind(conn *websocket.Conn, gen int64) bool {
	p.clientMu.Lock()
	defer p.clientMu.Unlock()
	if p.closed || p.connGen != gen {
		return false
	}
	p.client = conn
	p.startWriterLocked(conn)
	return true
}

// Detach clears the current client iff gen is still the current generation. A
// stale detach (from a superseded connection's handler) is a no-op and returns
// false, so an evicted connection cannot clobber the connection that replaced
// it. The pump keeps running regardless.
func (p *StdioPump) Detach(gen int64) bool {
	p.clientMu.Lock()
	defer p.clientMu.Unlock()
	if p.connGen != gen {
		return false
	}
	p.client = nil
	p.stopWriterLocked()
	return true
}

// DroppedFrames reports how many outbound frames were not delivered because a
// client's backlog overflowed. Used by tests; operators should watch the
// "client backlog exceeds cap" warning in the logs instead.
func (p *StdioPump) DroppedFrames() uint64 {
	return p.droppedFrames.Load()
}

// LastClientWriteAt reports when a frame was last written to a client, or the
// zero time if none has been.
func (p *StdioPump) LastClientWriteAt() time.Time {
	nanos := p.lastClientWrite.Load()
	if nanos == 0 {
		return time.Time{}
	}
	return time.Unix(0, nanos)
}

// startWriterLocked spawns the writer goroutine for conn. Caller holds clientMu.
func (p *StdioPump) startWriterLocked(conn *websocket.Conn) {
	ctx, cancel := context.WithCancel(context.Background())
	p.writerCtx = ctx
	p.writerCancel = cancel
	q := newClientQueue()
	p.writerQ = q
	done := make(chan struct{})
	p.writerDone = done
	go func() {
		defer close(done)
		p.clientWriterLoop(ctx, conn, q)
	}()
}

// clientWriterLoop drains the outbound queue to conn. It runs for the lifetime
// of one bound client: a write failure or a cancelled context stops it. The
// queue never blocks the drain loop; a client that falls too far behind is
// disconnected (see queueLocked).
//
// q is passed in rather than read from p.writerQ: a Detach can nil the field
// before this goroutine first runs.
func (p *StdioPump) clientWriterLoop(ctx context.Context, conn *websocket.Conn, q *clientQueue) {
	for {
		select {
		case <-ctx.Done():
			// Agent gone or client detached: flush whatever is still queued so
			// the client receives the tail before the conn closes.
			for _, frame := range q.take() {
				writeCtx, cancel := context.WithTimeout(context.Background(), acpWebSocketWriteTimeout)
				_ = conn.Write(writeCtx, websocket.MessageText, []byte(frame))
				cancel()
			}
			return
		case <-q.ready:
		}
		for _, frame := range q.take() {
			writeCtx, cancel := context.WithTimeout(context.Background(), acpWebSocketWriteTimeout)
			err := conn.Write(writeCtx, websocket.MessageText, []byte(frame))
			cancel()
			if err == nil {
				p.lastClientWrite.Store(time.Now().UnixNano())
				continue
			}
			p.logger.Warn("write to client failed", "error", err)
			// The client is dead/slow. Clear it (only if it's still the
			// same conn — a takeover may have replaced it) and close it so
			// the handler's read loop unblocks and runs its
			// (generation-fenced) DetachClient. Session state is owned by
			// the handler, not the pump, so there is a single detach path.
			p.clientMu.Lock()
			if p.client == conn {
				p.client = nil
				p.stopWriterLocked()
			}
			p.clientMu.Unlock()
			_ = conn.CloseNow()
			return
		}
	}
}

// AwaitingClient reports whether the agent is waiting on the client to answer
// one of its requests (a permission prompt, an elicitation, ...).
func (p *StdioPump) AwaitingClient() bool {
	return p.reqIDs.hasAgentRequests()
}

// LastStdoutAt returns the timestamp of the agent's most recent stdout line.
// Zero time means the pump has never received any output — the reaper falls
// back to DisconnectedAt in that case.
func (p *StdioPump) LastStdoutAt() time.Time {
	p.lastStdoutMu.Lock()
	defer p.lastStdoutMu.Unlock()
	return p.lastStdoutAt
}

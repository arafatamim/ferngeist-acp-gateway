// Package session manages durable, reconnectable agent sessions for the gateway.
// Each session wraps a runtime process with a long-lived stdio pump that runs
// independently of any WebSocket client. Clients can disconnect and later reattach
// via single-use attach tokens, using ACP session/load for context restoration.
//
// Key guarantees:
//   - One session per runtime: connecting launches the agent, and the session
//     owns that process directly.
//   - The pump runs regardless of client connectivity.
//   - Close always stops the backing runtime (no reference counting).
//   - Inbound client messages are logged asynchronously for audit; hot path never
//     blocks on SQLite I/O.
//   - Sessions orphaned by daemon restart are reconciled to "failed" on startup.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/push"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
	"github.com/arafatamim/ferngeist-acp-gateway/internal/storage"
)

const (
	StatusActive       = "active"
	StatusDisconnected = "disconnected"
	StatusClosing      = "closing"
	StatusFailed       = "failed"

	defaultMaxDisconnected  = 15 * time.Minute
	defaultReaperInterval   = 30 * time.Second
	defaultProgressInterval = 15 * time.Second

	// closeWriteTimeout bounds how long Close waits for the agent to accept the
	// session/close frame before stopping the runtime anyway.
	closeWriteTimeout = 2 * time.Second

	// shutdownTimeout bounds Shutdown's parallel stop of every agent process:
	// each Stop escalates to a kill after 2s, so this leaves room to record
	// the exits.
	shutdownTimeout = 5 * time.Second

	// awaitingClientGrace is how long a disconnected session survives while the
	// agent waits on a request the client has not answered. ponytail: a fixed
	// bound, move it to Config if it needs tuning.
	awaitingClientGrace = 24 * time.Hour
)

var (
	ErrSessionNotFound     = errors.New("session not found")
	ErrSessionNotActive    = errors.New("session is not in an active state")
	ErrAttachTokenInvalid  = errors.New("attach token is invalid or expired")
	ErrAttachTokenExpired  = errors.New("attach token has expired")
	ErrSessionLimitReached = errors.New("session limit for this device has been reached")
	ErrDeviceMismatch      = errors.New("session does not belong to this device")
	ErrCwdUnknown          = errors.New("agent working directory is not known yet")
)

type SessionSummary struct {
	SessionID string    `json:"sessionId"`
	RuntimeID string    `json:"runtimeId"`
	AgentID   string    `json:"agentId"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
}

// Config holds session-level tunables from the daemon configuration.
type Config struct {
	// MaxDisconnected is how long a disconnected session survives before the reaper closes it.
	MaxDisconnected time.Duration
	// MaxPerDevice limits the number of concurrent sessions a single device can hold.
	MaxPerDevice int
	// ReaperInterval is how often the reaper scans for expired disconnected sessions.
	ReaperInterval time.Duration
	// ProgressInterval is the minimum seconds between live progress push
	// notifications while the agent is mid-turn. 0 means use the default.
	ProgressInterval time.Duration
	// PushSvc is the push notification service for turn-complete notifications.
	// nil-able — when nil, push notifications are disabled.
	PushSvc push.PushService
	// BranchOf resolves the managed-worktree branch behind a chat's cwd for
	// push notifications, and "" for a chat outside any managed worktree or on
	// lookup failure. nil-able — when nil, pushes go out without a branch.
	// The daemon injects workspace.Service.BranchOf; the session layer keeps
	// only this narrow lookup, not the whole workspace service.
	BranchOf func(ctx context.Context, cwd string) string
	// GatewayID is this gateway's stable instance id, sent as the server identity
	// in push notifications so clients can deep-link into the right chat.
	GatewayID string
	// FrameLogEnabled toggles a raw ACP JSON-RPC frame log. When enabled, the
	// pump appends every client->agent and agent->client frame to
	// <FrameLogDir>/<agent>-agent.log as newline-delimited JSON.
	FrameLogEnabled bool
	// FrameLogDir is the directory for frame logs (normally the daemon log
	// directory). Ignored when FrameLogEnabled is false.
	FrameLogDir string
	// FrameLogMaxSize and FrameLogMaxBackups bound rotation of each agent's
	// frame log.
	FrameLogMaxSize    int64
	FrameLogMaxBackups int
}

// RuntimeSession is the central session orchestrator. It owns the in-memory
// session registry and the agent processes behind them, mints attach tokens via
// TokenService, and runs a background reaper to clean up expired disconnected
// sessions.
type RuntimeSession struct {
	logger     *slog.Logger
	store      *storage.SQLiteStore
	runtimeSvc *runtime.Supervisor // process launch + runtime diagnostics
	tokenSvc   *TokenService       // attach token mint/validate
	cfg        Config

	mu       sync.Mutex
	sessions map[string]*Session // in-memory registry, keyed by session ID
	closed   bool                // set by Shutdown; refuses new sessions
	// launching counts each device's Creates between the quota check and
	// registration, so in-flight launches hold their quota slot.
	launching map[string]int

	inbound *inboundWriter // async diagnostic logger for client->agent messages

	frameLog *frameLogManager // raw ACP frame logs; nil when disabled

	cancelReaper context.CancelFunc // shuts down the reaper goroutine on Close
}

// Session is a single resilient agent session. It owns its agent process
// directly and outlives any WebSocket connection — the pump continues draining
// agent stdout even when no client is attached.
type Session struct {
	ID             string
	RuntimeID      string
	DeviceID       string
	AgentID        string
	Status         string // active, disconnected, closing, failed
	CreatedAt      time.Time
	DisconnectedAt *time.Time // set when client detaches, nil when attached

	proc       *runtime.Process   // the agent process this session owns
	pump       *StdioPump         // long-lived stdout drain + stdin writer
	cancelPump context.CancelFunc // stops the StdoutDrainLoop on session close

	// exited is closed once the exit watcher has recorded the process exit on
	// the runtime and reclaimed the session. Close waits on it so a stopped
	// runtime is observed as stopped.
	exited chan struct{}

	// stopping is set before the process is intentionally terminated, so the
	// exit watcher records the exit as intentional rather than a crash.
	stopping atomic.Bool

	inboundSeq atomic.Int64 // monotonic counter for client->agent diagnostic frames

	mu sync.Mutex // protects Status, DisconnectedAt
}

// NewRuntimeSession creates a new session service and starts the reaper goroutine.
func NewRuntimeSession(logger *slog.Logger, store *storage.SQLiteStore, runtimeSvc *runtime.Supervisor, tokenSvc *TokenService, cfg Config) *RuntimeSession {
	frameLog, err := newFrameLogManager(cfg.FrameLogEnabled, cfg.FrameLogDir, cfg.FrameLogMaxSize, cfg.FrameLogMaxBackups)
	if err != nil {
		// A misconfigured frame log must not take down the session service;
		// log and continue without frame capture.
		logger.Warn("frame log disabled", "error", err)
		frameLog = nil
	}
	rs := &RuntimeSession{
		logger:     logger.With("component", "session"),
		store:      store,
		runtimeSvc: runtimeSvc,
		tokenSvc:   tokenSvc,
		cfg:        cfg,
		sessions:   make(map[string]*Session),
		launching:  make(map[string]int),
		frameLog:   frameLog,
	}
	if cfg.MaxDisconnected <= 0 {
		rs.cfg.MaxDisconnected = defaultMaxDisconnected
	}
	if cfg.ReaperInterval <= 0 {
		rs.cfg.ReaperInterval = defaultReaperInterval
	}
	if cfg.ProgressInterval <= 0 {
		rs.cfg.ProgressInterval = defaultProgressInterval
	}
	rs.inbound = newInboundWriter(store)
	ctx, cancel := context.WithCancel(context.Background())
	rs.cancelReaper = cancel
	go rs.reaperLoop(ctx)
	return rs
}

func generateID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

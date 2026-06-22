// Package session manages durable, reconnectable agent sessions for the gateway.
// Each session wraps a runtime process with a long-lived stdio pump that runs
// independently of any WebSocket client. Clients can disconnect and later reattach
// via single-use attach tokens, using ACP session/load for context restoration.
//
// Key guarantees:
//   - One session per runtime (enforced at create time via exclusive lease).
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
	"github.com/coder/websocket"
)

const (
	StatusActive       = "active"
	StatusDisconnected = "disconnected"
	StatusClosing      = "closing"
	StatusFailed       = "failed"

	defaultMaxDisconnected = 15 * time.Minute
	defaultReaperInterval  = 30 * time.Second
)

var (
	ErrSessionNotFound     = errors.New("session not found")
	ErrSessionNotActive    = errors.New("session is not in an active state")
	ErrAttachTokenInvalid  = errors.New("attach token is invalid or expired")
	ErrSessionLimitReached = errors.New("session limit for this device has been reached")
	ErrDeviceMismatch      = errors.New("session does not belong to this device")
	ErrRuntimeLeaseHeld    = errors.New("runtime is already leased by another session")
)

// ProcessManager is the interface RuntimeSession depends on for runtime
// lifecycle operations. Satisfied by *runtime.Supervisor. This interface
// exists to break the import cycle that would occur if the session package
// imported the runtime package directly. It exposes only the five methods
// RuntimeSession actually needs:
//
//   - AcquireLease: grants exclusive pipe access for a new session
//   - ReleaseLease: clears the lease on session close or failure
//   - OnProcessExit: registers a callback for agent death notification
//   - StopByRuntimeID: terminates the backing runtime process
//   - AppendLog: mirrors ACP traffic into the runtime log buffer
type ProcessManager interface {
	AcquireLease(runtimeID, leaseholder string) (runtime.Pipes, error)
	ReleaseLease(runtimeID, leaseholder string) error
	OnProcessExit(runtimeID string, callback func(string))
	StopByRuntimeID(runtimeID string) (runtime.Runtime, error)
	AppendLog(runtimeID, stream, message string)
}

// TokenService is the interface for attach token minting and validation.
// Satisfied by *token.Service. Single-use attach tokens prove the bearer owned
// the device credential at resume time, without storing secrets in the session.
type TokenService interface {
	// Mint creates a single-use, time-limited token bound to a session/device pair.
	Mint(sessionID, deviceID string, ttl time.Duration) (string, error)
	// Validate verifies and consumes the token, returning the session ID and
	// device ID from the claim. The caller is responsible for verifying the device ID
	// matches the session's device (so the session domain owns that check).
	Validate(token string) (string, string, error)
}

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
	// PushSvc is the push notification service for turn-complete notifications.
	// nil-able — when nil, push notifications are disabled.
	PushSvc push.PushService
	// GatewayID is this gateway's stable instance id, sent as the server identity
	// in push notifications so clients can deep-link into the right chat.
	GatewayID string
}

// RuntimeSession is the central session orchestrator. It owns the in-memory
// session registry, coordinates with ProcessManager for runtime lifecycle,
// mints attach tokens via TokenService, and runs a background reaper
// to clean up expired disconnected sessions.
type RuntimeSession struct {
	logger   *slog.Logger
	store    *storage.SQLiteStore
	pm       ProcessManager // runtime lifecycle (lease, stop, exit callback)
	tokenSvc TokenService   // attach token mint/validate
	cfg      Config

	mu       sync.Mutex
	sessions map[string]*Session // in-memory registry, keyed by session ID

	inbound *inboundWriter // async diagnostic logger for client->agent messages

	cancelReaper context.CancelFunc // shuts down the reaper goroutine on Close
}

// Session is a single resilient agent session. It outlives any WebSocket
// connection — the pump continues draining agent stdout even when no client
// is attached.
type Session struct {
	ID             string
	RuntimeID      string
	DeviceID       string
	AgentID        string
	Status         string // active, disconnected, closing, failed
	Leaseholder    string // always the session's own ID
	CreatedAt      time.Time
	DisconnectedAt *time.Time // set when client detaches, nil when attached

	pump        *StdioPump           // long-lived stdout drain + stdin writer
	leasedPipes runtime.Pipes        // exclusive stdio lease
	cancelPump  context.CancelFunc   // stops the StdoutDrainLoop on session close

	currentConn *websocket.Conn // the active client conn, or nil; used to evict on takeover
	connGen     int64           // bumped on every attach; fences stale detaches from evicted conns
	inboundSeq  atomic.Int64    // monotonic counter for client->agent diagnostic frames

	mu sync.Mutex // protects Status, DisconnectedAt, currentConn, connGen
}

// NewRuntimeSession creates a new session service and starts the reaper goroutine.
func NewRuntimeSession(logger *slog.Logger, store *storage.SQLiteStore, pm ProcessManager, tokenSvc TokenService, cfg Config) *RuntimeSession {
	rs := &RuntimeSession{
		logger:   logger.With("component", "session"),
		store:    store,
		pm:       pm,
		tokenSvc: tokenSvc,
		cfg:      cfg,
		sessions: make(map[string]*Session),
	}
	if cfg.MaxDisconnected <= 0 {
		rs.cfg.MaxDisconnected = defaultMaxDisconnected
	}
	if cfg.ReaperInterval <= 0 {
		rs.cfg.ReaperInterval = defaultReaperInterval
	}
	rs.inbound = newInboundWriter(store)
	ctx, cancel := context.WithCancel(context.Background())
	rs.cancelReaper = cancel
	go rs.reaperLoop(ctx)
	return rs
}

func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}


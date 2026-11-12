// Package agentext owns the agent-independent extension seam: named,
// immutable transformers that rewrite ACP bodies before the session applies
// its canonical handling, plus the bounds under which they run.
//
// The package imports no project package. Session owns request correlation,
// canonical state, generation fencing and delivery; harness profiles are the
// only supplier of registrations.
package agentext

import (
	"context"
	"encoding/json"
	"errors"
)

// Method is an exact ACP method name. Unknown extension methods are as valid as
// protocol ones; no method switch decides whether a transformer runs.
type Method string

// Direction is the direction of the original message.
type Direction uint8

const (
	// ToAgent marks a message the client sent to the agent.
	ToAgent Direction = iota
	// ToClient marks a message the agent sent to the client.
	ToClient
)

// Kind is the JSON-RPC message kind.
type Kind uint8

const (
	// Request carries a method and an id.
	Request Kind = iota
	// Notification carries a method and no id.
	Notification
	// Reply carries an id and no method.
	Reply
)

// Stage is where in the frame pipeline a transformer runs.
type Stage uint8

const (
	// BeforeState runs inline, before the session's canonical stages, and is
	// authoritative for the effective body.
	BeforeState Stage = iota
	// BeforeDelivery runs on a correlated reply after canonical processing and
	// cannot change canonical state.
	BeforeDelivery
)

// Mode is how a transformer runs.
type Mode uint8

const (
	// Inline runs on the goroutine that owns the frame.
	Inline Mode = iota
	// Deferred runs on one of the engine's bounded workers.
	Deferred
)

// Failure is what the engine does when a transformer chain fails.
type Failure uint8

const (
	// KeepOriginal continues with the body from before the whole chain.
	KeepOriginal Failure = iota
	// Reject answers a request, or closes a notification's or reply's bound
	// connection generation, with RejectRPCError.
	Reject
)

// RejectRPCErrorCode is the JSON-RPC error code for a rejected request.
const RejectRPCErrorCode = -32603

// RejectRPCErrorMessage is the JSON-RPC error message for a rejected request.
// It deliberately says nothing about the extension that failed.
const RejectRPCErrorMessage = "Gateway extension failed"

// Identity is the session an extension runs for. It carries no request or
// correlation id and no process or transport handle.
type Identity struct {
	AgentID   string
	RuntimeID string
	SessionID string
}

// Selector matches one exact message. For a correlated reply Method is the
// originating request's method; it is empty when the reply has no correlation.
type Selector struct {
	Direction Direction
	Kind      Kind
	Method    Method
}

// Payload is an opaque, owned JSON value. Its zero value means absent data,
// which is distinct from JSON null.
type Payload struct {
	body []byte
}

// NewPayload validates raw as one JSON value and copies it, so later changes to
// the caller's buffer cannot reach the payload.
func NewPayload(raw json.RawMessage) (Payload, error) {
	if len(raw) == 0 || !json.Valid(raw) {
		return Payload{}, ErrInvalidPayload
	}
	body := make([]byte, len(raw))
	copy(body, raw)
	return Payload{body: body}, nil
}

// Bytes returns a copy of the payload's JSON, or nil when the payload is
// absent. The copy is the caller's to mutate.
func (p Payload) Bytes() json.RawMessage {
	if p.body == nil {
		return nil
	}
	out := make([]byte, len(p.body))
	copy(out, p.body)
	return out
}

// IsZero reports whether the payload is absent.
func (p Payload) IsZero() bool {
	return p.body == nil
}

// Message is what a transformer sees: the session identity, the selector that
// matched it, and one owned body. It shares no ids, locks or handles with the
// session.
type Message struct {
	Identity Identity
	Selector Selector
	Body     Payload
}

// TransformInput is one transformer invocation.
type TransformInput struct {
	Message Message
	Stage   Stage
}

// TransformFunc runs one transformer. It runs inline or on a bounded worker, so
// it must not do I/O, wait, or leave work running after it returns. Returning a
// zero Payload means the body is unchanged.
type TransformFunc func(context.Context, TransformInput) (Payload, error)

// Transformer is one named registration. Registrations are immutable once a
// Registry is built.
type Transformer struct {
	Name             string
	Selector         Selector
	Stage            Stage
	Mode             Mode
	Failure          Failure
	IndependentReply bool
	Run              TransformFunc
}

// Set is a harness profile's registrations. New copies and validates it.
type Set struct {
	Transformers []Transformer
}

// Errors reported to the session. A callback failure is wrapped in ErrRejected
// only when its chain's failure policy is Reject; a KeepOriginal chain returns
// the original body and no error.
var (
	// ErrInvalidPayload reports a body that is not one JSON value.
	ErrInvalidPayload = errors.New("agentext: payload is not one JSON value")
	// ErrInvalidRegistration reports a Set that cannot be served.
	ErrInvalidRegistration = errors.New("agentext: invalid registration")
	// ErrExecutionMode reports an inline entry point used for a deferred chain,
	// or a submission for an inline chain. The callback never runs.
	ErrExecutionMode = errors.New("agentext: execution mode mismatch")
	// ErrCallbackPanic reports a recovered callback panic.
	ErrCallbackPanic = errors.New("agentext: callback panicked")
	// ErrRejected reports that a chain failed and its policy is Reject.
	ErrRejected = errors.New("agentext: extension rejected the message")
	// ErrSaturated reports admission refusal because a queue, engine or shared
	// budget is full.
	ErrSaturated = errors.New("agentext: extension capacity is exhausted")
	// ErrClosed reports a call after Close stopped admission.
	ErrClosed = errors.New("agentext: engine is closed")
)

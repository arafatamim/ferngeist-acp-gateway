// Package harness owns gateway-side knowledge about individual agent
// harnesses: the non-ACP behaviours that the ACP registry
// and protocol do not describe. A harness with no profile behaves like a plain
// ACP agent.
package harness

import (
	"context"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/agentext"
)

// Session is one chat a harness keeps outside the ACP surface it lists.
type Session struct {
	ID        string
	Cwd       string
	Title     string
	UpdatedAt string
}

// SessionSource lists a harness's native chats, which its ACP adapter omits
// from session/list.
type SessionSource interface {
	Sessions(ctx context.Context, cwd string) ([]Session, error)
}

// Profile is one harness's deviations from plain ACP.
type Profile struct {
	// TurnFinished runs after the agent answers a session/prompt, off the frame
	// pipeline, with the prompt's ACP session id. nil when the harness needs
	// nothing then.
	TurnFinished func(ctx context.Context, acpSessionID string) error
	// Sessions lists chats the harness's ACP adapter leaves out of session/list.
	// nil when the harness lists everything itself.
	Sessions SessionSource
	// Extensions are the agent-independent payload transformers this harness
	// registers. Empty when the harness needs none.
	Extensions agentext.Set
}

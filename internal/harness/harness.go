// Package harness owns gateway-side knowledge about individual agent
// harnesses: the non-ACP behaviours that the ACP registry
// and protocol do not describe. A harness with no profile behaves like a plain
// ACP agent.
package harness

import (
	"context"
	"slices"

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
	// ID is the catalog agent id, e.g. "claude-acp".
	ID string
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

// Registry resolves profiles by catalog agent id.
type Registry struct {
	byID map[string]Profile
}

// New returns a registry serving the given profiles.
func New(profiles ...Profile) *Registry {
	byID := make(map[string]Profile, len(profiles))
	for _, profile := range profiles {
		byID[profile.ID] = profile
	}
	return &Registry{byID: byID}
}

// TurnFinished returns agentID's after-turn hook, or nil when it has none.
func (r *Registry) TurnFinished(agentID string) func(ctx context.Context, acpSessionID string) error {
	if r == nil {
		return nil
	}
	return r.byID[agentID].TurnFinished
}

// Extensions returns the payload transformers agentID's profile registers. The
// returned slice is a copy; the registrations themselves are immutable.
func (r *Registry) Extensions(agentID string) agentext.Set {
	if r == nil {
		return agentext.Set{}
	}
	set := r.byID[agentID].Extensions
	return agentext.Set{Transformers: slices.Clone(set.Transformers)}
}

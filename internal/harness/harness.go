// Package harness owns gateway-side knowledge about individual agent
// harnesses: the launch overrides and non-ACP behaviours that the ACP registry
// and protocol do not describe. A harness with no profile behaves like a plain
// ACP agent.
package harness

import (
	"context"
	"maps"
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
	// LaunchEnv is set on the agent process. Caller overrides win.
	LaunchEnv map[string]string
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

// New returns a registry serving the given profiles. It keeps each profile's
// LaunchEnv map; callers must not mutate it after construction.
func New(profiles ...Profile) *Registry {
	byID := make(map[string]Profile, len(profiles))
	for _, profile := range profiles {
		byID[profile.ID] = profile
	}
	return &Registry{byID: byID}
}

// LaunchEnv returns a copy of the environment the harness needs at process
// start, or nil when it has none.
func (r *Registry) LaunchEnv(agentID string) map[string]string {
	if r == nil {
		return nil
	}
	env := r.byID[agentID].LaunchEnv
	if len(env) == 0 {
		return nil
	}
	return maps.Clone(env)
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

// DecorateList extends a final session/list reply with the chats agentID's ACP
// adapter omits. It returns nil when the profile has no session source or lists
// nothing new.
func (r *Registry) DecorateList(ctx context.Context, agentID, cwd string, reply []byte) ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	source := r.byID[agentID].Sessions
	if source == nil {
		return nil, nil
	}
	sessions, err := source.Sessions(ctx, cwd)
	if err != nil || len(sessions) == 0 {
		return nil, err
	}
	return mergeSessionList(reply, sessions), nil
}

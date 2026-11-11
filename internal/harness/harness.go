// Package harness owns gateway-side knowledge about individual agent
// harnesses: the launch overrides and non-ACP behaviours that the ACP registry
// and protocol do not describe. A harness with no profile behaves like a plain
// ACP agent.
package harness

import "maps"

// Profile is one harness's deviations from plain ACP.
type Profile struct {
	// ID is the catalog agent id, e.g. "claude-acp".
	ID string
	// LaunchEnv is set on the agent process. Caller overrides win.
	LaunchEnv map[string]string
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

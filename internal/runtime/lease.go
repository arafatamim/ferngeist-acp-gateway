package runtime

import (
	"io"
)

// Pipes is the interface for leased stdio pipes. LeasedPipes satisfies this.
type Pipes interface {
	WriteToAgent(payload []byte) error
	Release() error
}

// LeasedPipes wraps a runtime's stdio pipes with an explicit release mechanism.
// It does NOT expose Stdin directly for inbound writes — use WriteToAgent.
// On Release(), legacy leases ("legacy" leaseholder) close stdin (agent exits).
// Session leases (session ID leaseholder) only clear the leaseholder string —
// the pump keeps running.
type LeasedPipes struct {
	Stdin       io.WriteCloser
	Stdout      io.ReadCloser
	RuntimeID   string
	leaseholder string
	supervisor  *Supervisor
	released    bool
}

// WriteToAgent writes an inbound frame to the agent's stdin followed by a newline.
func (lp *LeasedPipes) WriteToAgent(payload []byte) error {
	if _, err := lp.Stdin.Write(append(payload, '\n')); err != nil {
		return err
	}
	return nil
}

// Release clears the lease. For legacy leaseholders, this also closes stdin
// which kills the agent process. For session leaseholders, only the leaseholder
// string is cleared — the pump continues to own the pipe.
//
// Release is nil-safe: if the supervisor backing store is unavailable (nil
// supervisor, e.g. from a test mock), the release is a no-op.
func (lp *LeasedPipes) Release() error {
	if lp.released {
		return nil
	}
	lp.released = true
	if lp.supervisor != nil {
		lp.supervisor.mu.Lock()
		if existing, exists := lp.supervisor.processes[lp.RuntimeID]; exists {
			existing.leaseholder = ""
		}
		lp.supervisor.mu.Unlock()
	}
	if lp.leaseholder == "legacy" {
		lp.Stdin.Close()
	}
	return nil
}

// AcquireLease grants exclusive use of a runtime's stdio pipes to the given
// leaseholder. The leaseholder is an opaque string (session ID for resilient
// sessions, "legacy" for traditional WebSocket connections). The returned
// LeasedPipes must be released explicitly via ReleaseLease or the Release()
// method when the bridge is torn down.
func (s *Supervisor) AcquireLease(runtimeID, leaseholder string) (Pipes, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneStoppedRuntimesLocked(s.now().UTC())

	r, ok := s.runtimes[runtimeID]
	if !ok {
		return nil, ErrRuntimeNotFound
	}
	if r.Status != StatusRunning {
		return nil, ErrRuntimeNotRunning
	}
	if r.Transport != "stdio" {
		return nil, ErrRuntimeNotConnectable
	}

	handle, ok := s.processes[runtimeID]
	if !ok || handle.stdin == nil || handle.stdout == nil {
		return nil, ErrRuntimeNotConnectable
	}
	if handle.leaseholder != "" {
		return nil, ErrRuntimeLeaseHeld
	}
	handle.leaseholder = leaseholder

	return &LeasedPipes{
		Stdin:       handle.stdin,
		Stdout:      handle.stdout,
		RuntimeID:   runtimeID,
		leaseholder: leaseholder,
		supervisor:  s,
	}, nil
}

// ReleaseLease releases the lease held by leaseholder on the given runtime.
// Returns ErrRuntimeNotFound if the runtime doesn't exist, or ErrRuntimeLeaseHeld
// if a different leaseholder owns the lease.
func (s *Supervisor) ReleaseLease(runtimeID, leaseholder string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	handle, ok := s.processes[runtimeID]
	if !ok {
		return ErrRuntimeNotFound
	}
	if handle.leaseholder != leaseholder {
		return ErrRuntimeLeaseHeld
	}
	handle.leaseholder = ""
	return nil
}

// OnProcessExit registers a callback to be invoked when the runtime process exits.
// This allows the session module to mark sessions as failed when their backing
// agent dies. The callback receives the runtime ID.
func (s *Supervisor) OnProcessExit(runtimeID string, callback func(string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onExitCallbacks[runtimeID] = callback
}


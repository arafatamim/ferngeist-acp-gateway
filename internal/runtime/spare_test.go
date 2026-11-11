package runtime

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func newSpareSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	baseDir := t.TempDir()
	buildMockAgent(t, baseDir)
	supervisor := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), baseDir, nil, nil)
	supervisor.EnableSpares()
	t.Cleanup(supervisor.Close)
	return supervisor
}

// waitSpare returns the agent's spare process once it has been spawned.
func waitSpare(t *testing.T, s *Supervisor, agentID string) *Process {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		var proc *Process
		if sp := s.spares[agentID]; sp != nil {
			proc = sp.proc
		}
		s.mu.Unlock()
		if proc != nil {
			return proc
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("spare never spawned")
	return nil
}

func TestLaunchClaimsPrebootedSpare(t *testing.T) {
	supervisor := newSpareSupervisor(t)
	first := launchMock(t, supervisor)
	spare := waitSpare(t, supervisor, "mock-acp")
	if got := len(supervisor.List()); got != 1 {
		t.Fatalf("len(List()) = %d with a spare waiting, want 1: spares stay unlisted", got)
	}

	second := launchMock(t, supervisor)
	if second != spare || second.Runtime.PID != spare.Runtime.PID {
		t.Fatalf("second Launch got pid %d, want the spare's pid %d", second.Runtime.PID, spare.Runtime.PID)
	}
	if second.Runtime.ID == first.Runtime.ID || second.Runtime.Status != StatusRunning {
		t.Fatalf("claimed runtime = %+v, want a distinct running runtime", second.Runtime)
	}
	if _, err := supervisor.Get(second.Runtime.ID); err != nil {
		t.Fatalf("claimed spare is not tracked: %v", err)
	}
	if next := waitSpare(t, supervisor, "mock-acp"); next == spare {
		t.Fatal("claiming a spare must start the next one")
	}
}

func TestLaunchSkipsSpareForChangedLaunch(t *testing.T) {
	supervisor := newSpareSupervisor(t)
	launchMock(t, supervisor)
	stale := waitSpare(t, supervisor, "mock-acp")

	agent := mockAgent()
	agent.Launch.Args = []string{"--new-flag"}
	proc, err := supervisor.Launch(agent, nil)
	if err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	t.Cleanup(func() { _ = proc.Stop(2 * time.Second) })
	if proc == stale {
		t.Fatal("a spare launched with other args must not be handed out")
	}
	select {
	case <-stale.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("stale spare was not stopped")
	}
}

func TestLaunchWithEnvOverridesNeverUsesSpare(t *testing.T) {
	supervisor := newSpareSupervisor(t)
	launchMock(t, supervisor)
	spare := waitSpare(t, supervisor, "mock-acp")

	proc, err := supervisor.Launch(mockAgent(), map[string]string{"TOKEN": "x"})
	if err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	t.Cleanup(func() { _ = proc.Stop(2 * time.Second) })
	if proc == spare {
		t.Fatal("a spare runs on the daemon and profile env; an env override must launch cold")
	}
}

func TestCloseStopsSpares(t *testing.T) {
	supervisor := newSpareSupervisor(t)
	launchMock(t, supervisor)
	spare := waitSpare(t, supervisor, "mock-acp")

	supervisor.Close()
	select {
	case <-spare.Done():
	default:
		t.Fatal("Close returned with the spare still running")
	}
	proc := launchMock(t, supervisor)
	if proc == spare {
		t.Fatal("Launch after Close handed out a stopped spare")
	}
}

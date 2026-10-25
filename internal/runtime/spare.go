package runtime

import (
	"strings"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/catalog"
)

// spareIdleTTL bounds how long a pre-booted agent waits to be claimed. A spare
// is an idle agent process (200-240MB measured for opencode and the npm
// agents), so it only lives while that agent is in active use: long enough for
// a list → open-chat hop, and the app's 30s list poll keeps renewing it.
const spareIdleTTL = 2 * time.Minute

// spare is a pre-booted process waiting for its agent's next Launch. Booting
// an agent (runtime start, module load) is 300-900ms of every connect; a spare
// has already paid it, so the client's initialize answers at once. proc is nil
// while the spare is still being spawned.
type spare struct {
	proc  *Process
	timer *time.Timer
}

// EnableSpares turns on pre-booting: after each launch without env overrides,
// the supervisor starts one idle process of that agent for the next launch to
// claim. Called by the daemon; Close stops the spares left.
func (s *Supervisor) EnableSpares() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spares == nil {
		s.spares = make(map[string]*spare)
	}
}

// spareKey identifies what a process was launched as, so a catalog change (a
// new registry version, edited custom agent args) never hands out a stale one.
func spareKey(commandPath, workingDir string, args []string) string {
	return strings.Join(append([]string{commandPath, workingDir}, args...), "\x00")
}

// claimSpare hands over agent's spare, registered as a fresh runtime, when it
// is still alive and matches the launch. It returns nil when there is none.
func (s *Supervisor) claimSpare(agent catalog.Agent) *Process {
	commandPath, workingDir, err := s.resolveLaunch(agent.Launch)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	sp := s.spares[agent.ID]
	if sp == nil || sp.proc == nil {
		s.mu.Unlock()
		return nil
	}
	delete(s.spares, agent.ID)
	s.sparesWG.Add(1) // under mu, so Close's Wait covers the discard below
	s.mu.Unlock()
	sp.timer.Stop()

	select {
	case <-sp.proc.done: // died while idle
	default:
		if sp.proc.launchKey == spareKey(commandPath, workingDir, agent.Launch.Args) {
			s.sparesWG.Done()
			sp.proc.Runtime.AgentName = agent.DisplayName
			s.register(sp.proc)
			return sp.proc
		}
	}
	go func() {
		defer s.sparesWG.Done()
		s.Discard(sp.proc)
	}()
	return nil
}

// refillSpare starts agent's next spare in the background unless spares are
// off, one already exists, or the supervisor is closed.
func (s *Supervisor) refillSpare(agent catalog.Agent) {
	s.mu.Lock()
	if s.spares == nil || s.closed || s.spares[agent.ID] != nil {
		s.mu.Unlock()
		return
	}
	sp := &spare{}
	s.spares[agent.ID] = sp
	s.sparesWG.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.sparesWG.Done()
		proc, err := s.spawn(agent, nil)
		s.mu.Lock()
		if err != nil || s.closed || s.spares[agent.ID] != sp {
			if s.spares[agent.ID] == sp {
				delete(s.spares, agent.ID)
			}
			s.mu.Unlock()
			if proc != nil {
				s.Discard(proc)
			}
			return
		}
		sp.proc = proc
		sp.timer = time.AfterFunc(spareIdleTTL, func() {
			s.mu.Lock()
			if s.spares[agent.ID] != sp {
				s.mu.Unlock()
				return
			}
			delete(s.spares, agent.ID)
			s.sparesWG.Add(1)
			s.mu.Unlock()
			defer s.sparesWG.Done()
			s.Discard(proc)
		})
		s.mu.Unlock()
	}()
}

// Discard stops a process that was never registered (an unclaimed spare, or
// one from Spawn) and drops its log ring once stderr has drained, so it leaves
// nothing behind.
func (s *Supervisor) Discard(proc *Process) {
	_ = proc.Stop(2 * time.Second)
	select {
	case <-proc.logsDone:
	case <-time.After(time.Second):
	}
	s.mu.Lock()
	delete(s.logs, proc.Runtime.ID)
	s.mu.Unlock()
}

// Close stops every spare and keeps new ones from starting. Processes already
// handed out belong to their sessions and are left alone.
func (s *Supervisor) Close() {
	s.mu.Lock()
	s.closed = true
	spares := s.spares
	s.spares = nil
	s.mu.Unlock()
	for _, sp := range spares {
		if sp.proc != nil {
			sp.timer.Stop()
			s.Discard(sp.proc)
		}
	}
	s.sparesWG.Wait()
}

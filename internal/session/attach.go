package session

import (
	"context"
	"fmt"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/catalog"
)

// AttachACP returns the gateway session a device reattaches its chat
// acpSessionID on agent through: the live session whose agent process holds
// that ACP session, with a fresh attach token, or else a newly launched one
// (launched reports which). An empty acpSessionID, a new chat, always
// launches. A held session marked active is reused too: its old connection is
// almost always a dead socket, and the attach that follows takes it over (see
// AttachClient).
func (rs *RuntimeSession) AttachACP(ctx context.Context, agent catalog.Agent, deviceID, acpSessionID string) (sess *Session, attachToken string, launched bool, err error) {
	if held := rs.holding(agent.ID, deviceID, acpSessionID); held != nil {
		token, err := rs.tokenSvc.Mint(held.ID, deviceID, 5*time.Minute)
		if err != nil {
			return nil, "", false, fmt.Errorf("mint attach token: %w", err)
		}
		return held, token, false, nil
	}
	sess, attachToken, err = rs.Create(ctx, agent, deviceID, nil)
	return sess, attachToken, true, err
}

// holding returns the device's newest live session for agentID whose agent
// process holds acpSessionID, or nil.
func (rs *RuntimeSession) holding(agentID, deviceID, acpSessionID string) *Session {
	if acpSessionID == "" {
		return nil
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	var newest *Session
	for _, sess := range rs.sessions {
		sess.mu.Lock()
		live := sess.AgentID == agentID && sess.DeviceID == deviceID &&
			(sess.Status == StatusActive || sess.Status == StatusDisconnected)
		sess.mu.Unlock()
		if live && sess.pump.AcpCwdFor(acpSessionID) != "" &&
			(newest == nil || sess.CreatedAt.After(newest.CreatedAt)) {
			newest = sess
		}
	}
	return newest
}

// Epoch names the pump's catch-up numbering: update seqs restart with every
// agent process, so a client's seq is only valid under the epoch it came with.
func (p *StdioPump) Epoch() string {
	if p.loadRecovery == nil {
		return ""
	}
	return p.loadRecovery.epoch
}

package session

import (
	"encoding/json"
	"slices"
	"sync"

	"github.com/coder/websocket"
)

// clientIDMetaKey carries a prompt's client-chosen id, in session/prompt's
// params._meta and in the user-message frames the gateway buffers for it.
const clientIDMetaKey = "ferngeist/clientId"

// promptDeliveredCode is the JSON-RPC error a repeated prompt gets. It means
// the prompt already reached the agent, so the client treats it as sent; never
// a synthetic end_turn, since the original turn may still be running.
const promptDeliveredCode = -32050

// maxDeliveredPrompts is how many recent clientIds each ACP session remembers.
// A client resends only its last few unconfirmed prompts.
const maxDeliveredPrompts = 32

// deliveredPrompts remembers, per ACP session, the clientIds of the prompts
// last delivered to the agent. Client→agent writes have no ordering across
// connections, so a half-open old socket can deliver a prompt after the new
// connection already resent it. The zero value is ready.
// ponytail: sessions are never evicted; one entry per chat the pump hosts.
type deliveredPrompts struct {
	mu  sync.Mutex
	ids map[string][]string // acpSessionId -> clientIds, oldest first
}

// claim records clientID for sessionID and reports whether it is new. An empty
// clientID (an older client) is always new and never recorded.
func (d *deliveredPrompts) claim(sessionID, clientID string) bool {
	if clientID == "" {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	ids := d.ids[sessionID]
	if slices.Contains(ids, clientID) {
		return false
	}
	if len(ids) >= maxDeliveredPrompts {
		ids = ids[1:]
	}
	if d.ids == nil {
		d.ids = make(map[string][]string)
	}
	d.ids[sessionID] = append(ids, clientID)
	return true
}

// release forgets clientID, a prompt that never reached the agent, so the
// client's resend is delivered.
func (d *deliveredPrompts) release(sessionID, clientID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ids, ok := d.ids[sessionID]; ok {
		d.ids[sessionID] = slices.DeleteFunc(ids, func(id string) bool { return id == clientID })
	}
}

// dropDuplicatePrompt answers a session/prompt whose clientId already reached
// the agent with promptDeliveredCode instead of forwarding it, and reports
// whether it did. Only connection gen is answered: a superseded connection's
// reply would land on its successor under an id the successor may also use.
func (p *StdioPump) dropDuplicatePrompt(gen int64, f *clientFrame) bool {
	if f.Method != "session/prompt" || f.ID == nil ||
		p.prompts.claim(f.Params.SessionID, f.Params.Meta.ClientID) {
		return false
	}
	p.logger.Info("dropping repeated prompt", "runtime_id", p.runtimeID,
		"acpSessionId", f.Params.SessionID, "clientId", f.Params.Meta.ClientID)
	p.clientMu.Lock()
	var evict *websocket.Conn
	if gen == p.connGen && p.client != nil && p.writerQ != nil {
		evict = p.queueLocked([]string{promptDeliveredFrame(f.ID, f.Params.Meta.ClientID)})
	}
	p.clientMu.Unlock()
	if evict != nil {
		_ = evict.CloseNow()
	}
	return true
}

func promptDeliveredFrame(id json.RawMessage, clientID string) string {
	out, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    promptDeliveredCode,
			"message": "prompt already delivered",
			"data":    map[string]string{"clientId": clientID},
		},
	})
	return string(out)
}

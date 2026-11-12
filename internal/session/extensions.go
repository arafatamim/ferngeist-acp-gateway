package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/agentext"
	"github.com/coder/websocket"
)

// extensionCloseTimeout bounds waiting for extension workers when agent stdout
// stops. A callback that ignores cancellation is abandoned.
const extensionCloseTimeout = 2 * time.Second

// bindExtensions returns the engine for one session, or nil when its profile
// registers no transformers.
func bindExtensions(ctx context.Context, cfg Config, registry *agentext.Registry, agentID, runtimeID, sessionID string) *agentext.Engine {
	if registry == nil {
		return nil
	}
	return registry.Bind(ctx, agentext.Identity{
		AgentID: agentID, RuntimeID: runtimeID, SessionID: sessionID,
	}, cfg.ExtensionBudget, nil)
}

// extensionRegistry builds the immutable registration snapshot for agentID. It
// runs before the agent process starts, so an invalid profile fails the connect
// instead of leaving the session without its transforms.
func extensionRegistry(cfg Config, agentID string) (*agentext.Registry, error) {
	if cfg.Extensions == nil {
		return nil, nil
	}
	set := cfg.Extensions(agentID)
	if len(set.Transformers) == 0 {
		return nil, nil
	}
	registry, err := agentext.New(set)
	if err != nil {
		return nil, fmt.Errorf("agent extensions for %s: %w", agentID, err)
	}
	return registry, nil
}

// outboundSelector describes an agent→client frame. A reply's method is the
// client request it correlates with, which the caller resolved with peek.
func outboundSelector(f *frame) (agentext.Selector, bool) {
	switch {
	case f.probe.Method != "" && f.probe.ID != nil:
		return agentext.Selector{Direction: agentext.ToClient, Kind: agentext.Request, Method: agentext.Method(f.probe.Method)}, true
	case f.probe.Method != "":
		return agentext.Selector{Direction: agentext.ToClient, Kind: agentext.Notification, Method: agentext.Method(f.probe.Method)}, true
	case f.probe.ID != nil && f.isReply:
		return agentext.Selector{Direction: agentext.ToClient, Kind: agentext.Reply, Method: agentext.Method(f.origin.method)}, true
	default:
		return agentext.Selector{}, false
	}
}

// inboundSelector describes a client→agent frame. A client reply carries no
// method, so its selector leaves Method empty.
func inboundSelector(f *clientFrame) (agentext.Selector, bool) {
	switch {
	case f.Method != "" && f.ID != nil:
		return agentext.Selector{Direction: agentext.ToAgent, Kind: agentext.Request, Method: agentext.Method(f.Method)}, true
	case f.Method != "":
		return agentext.Selector{Direction: agentext.ToAgent, Kind: agentext.Notification, Method: agentext.Method(f.Method)}, true
	case f.ID != nil:
		return agentext.Selector{Direction: agentext.ToAgent, Kind: agentext.Reply}, true
	default:
		return agentext.Selector{}, false
	}
}

// beforeState runs the matching BeforeState chain. It returns the body to use
// and whether the chain rejected the frame; on rejection or failure the body is
// the pre-chain one.
func (p *StdioPump) beforeState(ctx context.Context, selector agentext.Selector, body []byte) ([]byte, bool) {
	if p.ext == nil || !p.ext.Enabled(selector, agentext.BeforeState) {
		return body, false
	}
	payload, err := agentext.NewPayload(body)
	if err != nil {
		return body, false // A parsed frame is always valid JSON.
	}
	out, err := p.ext.TransformInline(ctx, agentext.TransformInput{
		Message: agentext.Message{Selector: selector, Body: payload},
		Stage:   agentext.BeforeState,
	})
	switch {
	case err != nil && errors.Is(err, agentext.ErrRejected):
		return body, true
	case err != nil, out.IsZero():
		// The engine logged a KeepOriginal failure; mode and close errors pass
		// through rather than rewriting a legitimate frame.
		return body, false
	}
	return out.Bytes(), false
}

// applyBeforeState normalizes one agent→client frame before canonical handling.
// A changed body is reparsed and re-correlated once.
func (p *StdioPump) applyBeforeState(f *frame) {
	selector, ok := outboundSelector(f)
	if !ok {
		return
	}
	effective, rejected := p.beforeState(p.drainContext(), selector, f.raw)
	if rejected {
		f.reject = true
		return
	}
	if bytes.Equal(effective, f.raw) {
		return
	}
	probe, ok := parseFrameProbe(effective)
	if !ok {
		// Payload construction guarantees valid JSON; never forward a body the
		// pipeline cannot classify.
		return
	}
	f.line, f.raw, f.out = string(effective), effective, []string{string(effective)}
	f.probe, f.ok = probe, true
	f.origin, f.isReply = p.reqIDs.peek(probe)
}

// rejectInbound applies a rejected client frame. A request gets a gateway error
// with its own id; a notification or reply closes only its bound generation,
// leaving an agent request pending so a successor can answer it.
func (p *StdioPump) rejectInbound(gen int64, f *clientFrame) {
	if f.Method != "" && f.ID != nil {
		p.queueClientReply(gen, func() []string { return []string{extensionErrorFrame(f.ID)} })
		return
	}
	p.closeGeneration(gen)
}

// answerRejectedAgentRequest answers an agent's request that BeforeState
// rejected, without ever delivering it to the client.
func (p *StdioPump) answerRejectedAgentRequest(f *frame) {
	if f.probe.ID == nil {
		return
	}
	frame := extensionErrorFrame(*f.probe.ID)
	if p.frameLog != nil {
		p.frameLog.append(p.agentID, p.runtimeID, p.sessionID, "in", []byte(frame))
	}
	if err := p.writeToAgent([]byte(frame)); err != nil {
		// The pipe is already failing; the process watch owns the teardown.
		p.logger.Warn("failed to answer a rejected agent request", "error", err)
	}
}

// rejectToClientLocked applies a rejected agent→client frame after canonical
// bookkeeping. A current reply or notification closes its generation; a stale
// reply still follows the normal correlation tail, so a successor sees a
// turn-ended notification and never the rejected body. Caller holds clientMu.
func (p *StdioPump) rejectToClientLocked(f *frame, out []string, bound bool) *websocket.Conn {
	if !bound {
		return nil
	}
	if f.isReply && f.origin.gen != 0 && f.origin.gen != p.connGen {
		return p.queueLocked(out)
	}
	return p.evictClientLocked()
}

// closeGeneration closes the bound connection only when gen is still current.
// A superseded generation can never close its successor.
func (p *StdioPump) closeGeneration(gen int64) {
	p.clientMu.Lock()
	if gen != p.connGen || p.client == nil {
		p.clientMu.Unlock()
		return
	}
	conn := p.evictClientLocked()
	p.clientMu.Unlock()
	if conn != nil {
		_ = conn.CloseNow()
	}
}

// extensionErrorFrame is the gateway's answer to a rejected request.
func extensionErrorFrame(id json.RawMessage) string {
	frame, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   rpcErrorBody    `json:"error"`
	}{
		JSONRPC: "2.0",
		ID:      id,
		Error:   rpcErrorBody{Code: agentext.RejectRPCErrorCode, Message: agentext.RejectRPCErrorMessage},
	})
	if err != nil {
		// A request id the probe accepted always marshals; keep the shape valid.
		return `{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"Gateway extension failed"}}`
	}
	return string(frame)
}

// rpcErrorBody is a JSON-RPC error object.
type rpcErrorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

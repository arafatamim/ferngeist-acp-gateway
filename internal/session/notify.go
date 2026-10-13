package session

import (
	"fmt"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/push"
	"github.com/coder/acp-go-sdk"
)

// This file turns agent frames into push notifications: turn complete,
// permission request, elicitation, prompt error, and throttled live progress.
// It never talks to a client — a notification is a platform-neutral PushEvent
// the session layer hands to the push dispatcher.

// PushEvent carries all context for a push notification fired by the pump.
// The client decides whether to display it based on its own foreground state.
type PushEvent struct {
	SessionID    string
	AcpSessionID string
	Category     string
	Title        string
	Body         string
}

// progressEvent is a tool call's status change from an agent's session/update
// notification. kind is empty when the frame omits it (usual for updates).
type progressEvent struct {
	toolCallID string
	kind       string
	status     string
	// The ACP session the push deep-links to; filled in by the notifier.
	acpSessionID string
}

// progressEventFromProbe reads a session/update tool_call or tool_call_update
// event from the shared parse, or nil for any other frame.
func progressEventFromProbe(probe frameProbe) *progressEvent {
	if probe.Method != "session/update" ||
		probe.Params == nil || probe.Params.Update == nil {
		return nil
	}
	u := probe.Params.Update
	switch u.Discriminator {
	case "tool_call", "tool_call_update":
	default:
		return nil
	}
	return &progressEvent{toolCallID: u.ToolCallID, kind: u.Kind, status: u.Status}
}

// verbForKind maps a ToolKind to the generic verb of one running call, worded
// like the app's tool group summary.
func verbForKind(kind string) string {
	switch kind {
	case "read":
		return "Reading a file"
	case "edit":
		return "Editing a file"
	case "delete":
		return "Deleting a file"
	case "move":
		return "Moving a file"
	case "search":
		return "Searching"
	case "execute":
		return "Running a command"
	case "think":
		return "Thinking"
	case "fetch":
		return "Fetching a URL"
	case "switch_mode":
		return "Switching mode"
	default:
		return "Using a tool"
	}
}

// notifyFrame is the notification stage: it fires a push when the agent emits a
// notable event (turn complete, permission request, elicitation, prompt error)
// or progress on a running tool call. It runs regardless of client attachment;
// the client decides whether to display. A frame the pipeline already replaced
// is skipped — that replacement raised its own, more specific, notification.
func (p *StdioPump) notifyFrame(f *frame) {
	if p.onPushNotification == nil || !f.ok || f.replaced {
		return
	}
	probe := f.probe
	acpID := p.pushAcpSessionID(probe, f.origin)
	switch {
	case probe.Result != nil && probe.Result.StopReason != "":
		p.endProgress(acpID)
		// The user cancelled this turn themselves; telling them it ended is noise.
		if probe.Result.StopReason != acp.StopReasonCancelled {
			p.onPushNotification(PushEvent{SessionID: p.sessionID, AcpSessionID: acpID, Category: push.CategoryTurnComplete, Title: "Turn Complete", Body: p.turnCompleteBody(f.origin)})
		}
	case probe.Method == "session/request_permission":
		p.onPushNotification(PushEvent{SessionID: p.sessionID, AcpSessionID: acpID, Category: push.CategoryPermissionRequest, Title: "Permission Required", Body: p.AgentName("Your agent") + " needs approval to run a tool."})
	case probe.Method == elicitationMethod:
		// ponytail: shares the permission category so clients route it without a new case.
		p.onPushNotification(PushEvent{SessionID: p.sessionID, AcpSessionID: acpID, Category: push.CategoryPermissionRequest, Title: "Input Required", Body: p.AgentName("Your agent") + " needs your input."})
	case probe.Error != nil:
		// Only a failed prompt is worth a push: errors for optional methods
		// (e.g. a redundant session/load, an unsupported set_mode) are not.
		if f.isReply && f.origin.method == "session/prompt" {
			p.endProgress(acpID)
			p.onPushNotification(PushEvent{SessionID: p.sessionID, AcpSessionID: acpID, Category: push.CategoryError, Title: "Agent Error", Body: p.AgentName("Your agent") + " encountered an unexpected error."})
		}
	default:
		// Only while a prompt runs: session/load replays finished tool calls as
		// the same updates, which would buzz the phone once per past tool call.
		if ev := progressEventFromProbe(probe); ev != nil && p.reqIDs.promptRunning(acpID) {
			ev.acpSessionID = acpID
			p.maybeNotifyProgress(ev)
		}
	}
}

// turnCompleteBody is the "Turn Complete" push text: the turn's length, e.g.
// "Claude finished in 4m", once it ran at least 10s. Shorter turns keep the
// plain body — "finished in 3s" is noise. origin carries the prompt's arrival
// time (zero for anything but a session/prompt reply).
func (p *StdioPump) turnCompleteBody(origin pendingRequest) string {
	if !origin.startedAt.IsZero() {
		if d := time.Since(origin.startedAt); d >= 10*time.Second {
			return p.AgentName("Your agent") + " finished in " + formatPushDuration(d) + "."
		}
	}
	return p.AgentName("Your agent") + " has finished processing."
}

// formatPushDuration renders d the way the app's chat footer does: 12s, 4m,
// 1h 5m.
func formatPushDuration(d time.Duration) string {
	s := int64(d.Seconds())
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	m := s / 60
	if m < 60 {
		return fmt.Sprintf("%dm", m)
	}
	if r := m % 60; r > 0 {
		return fmt.Sprintf("%dh %dm", m/60, r)
	}
	return fmt.Sprintf("%dh", m/60)
}

// notifySwallowedFailure pushes the one notification for a turn the agent
// reported as an empty success (see observeTurn).
func (p *StdioPump) notifySwallowedFailure(probe frameProbe, origin pendingRequest) {
	if p.onPushNotification == nil {
		return
	}
	p.onPushNotification(PushEvent{
		SessionID:    p.sessionID,
		AcpSessionID: p.pushAcpSessionID(probe, origin),
		Category:     push.CategoryError,
		Title:        "Agent Error",
		Body:         p.AgentName("The agent") + " finished without producing anything. This usually means the model call failed (rate limit or invalid setting). Check the gateway logs for details.",
	})
}

// pushAcpSessionID picks the ACP session a frame's push should deep-link to: the
// session the frame names, else the session of the prompt it answers, else the
// cached id. One agent process can host several chats, so the cached id alone
// would send every push to the first one.
func (p *StdioPump) pushAcpSessionID(probe frameProbe, origin pendingRequest) string {
	if probe.Params != nil && probe.Params.SessionID != "" {
		return probe.Params.SessionID
	}
	if origin.promptSession != "" {
		return origin.promptSession
	}
	return p.AcpSessionID()
}

type progressState struct {
	at      time.Time
	summary string
	// A throttled summary, sent when the window ends so the notification never
	// keeps naming a call the agent has moved past.
	pending string
	timer   *time.Timer
}

// workingTitle is the progress push title, e.g. "Claude Code working".
func (p *StdioPump) workingTitle() string {
	return p.AgentName("Agent") + " working"
}

// maybeNotifyProgress pushes the generic verb of a tool call that starts running,
// e.g. "Reading a file", the way the app's tool group summary names running calls.
// A finished call never pushes: its verb would claim work that is already done.
// Throttled by ProgressInterval (the latest throttled summary goes out when the
// window ends) and deduplicated against the previous summary.
func (p *StdioPump) maybeNotifyProgress(ev *progressEvent) {
	p.progressMu.Lock()
	defer p.progressMu.Unlock()
	if ev.acpSessionID == "" {
		ev.acpSessionID = p.AcpSessionID()
	}

	kind := p.acp.toolKindFor(ev.toolCallID, ev.kind)
	if ev.status == "completed" || ev.status == "failed" {
		p.acp.forgetTool(ev.toolCallID)
		return
	}
	if ev.status != "in_progress" {
		return
	}

	if p.progress == nil {
		p.progress = make(map[string]*progressState)
	}
	st := p.progress[ev.acpSessionID]
	if st == nil {
		st = &progressState{}
		p.progress[ev.acpSessionID] = st
	}
	summary := verbForKind(kind)
	wait := p.ProgressInterval - time.Since(st.at)
	if p.ProgressInterval <= 0 || wait <= 0 {
		p.sendProgressLocked(ev.acpSessionID, st, summary)
		return
	}
	st.pending = summary
	if st.timer == nil {
		acpID := ev.acpSessionID
		st.timer = time.AfterFunc(wait, func() {
			p.progressMu.Lock()
			defer p.progressMu.Unlock()
			if p.progress[acpID] != st || st.timer == nil {
				return // the turn ended first
			}
			st.timer = nil
			p.sendProgressLocked(acpID, st, st.pending)
		})
	}
}

// sendProgressLocked pushes summary unless it repeats the last push. Caller holds progressMu.
func (p *StdioPump) sendProgressLocked(acpID string, st *progressState, summary string) {
	st.pending = ""
	if summary == st.summary {
		return
	}
	st.at, st.summary = time.Now(), summary
	p.onPushNotification(PushEvent{
		SessionID:    p.sessionID,
		AcpSessionID: acpID,
		Category:     push.CategoryProgress,
		Title:        p.workingTitle(),
		Body:         summary,
	})
}

// endProgress drops a session's throttled progress at turn end: a push landing
// after "Turn Complete" would claim the agent is still working.
func (p *StdioPump) endProgress(acpID string) {
	p.progressMu.Lock()
	defer p.progressMu.Unlock()
	if st := p.progress[acpID]; st != nil && st.timer != nil {
		st.timer.Stop()
	}
	delete(p.progress, acpID)
}

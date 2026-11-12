package harness

import "encoding/json"

// mergeSessionList appends native chats to the final page of a session/list
// reply. The agent's rows, their order and every other member of the frame stay
// untouched; rows whose id the agent already listed are skipped. It returns nil
// when nothing was added.
//
// Callers pass a final page (no nextCursor): only that page is extended, so a
// paged listing never repeats an added chat.
func mergeSessionList(reply []byte, native []Session) []byte {
	var top map[string]json.RawMessage
	if json.Unmarshal(reply, &top) != nil || top == nil {
		return nil
	}
	rawResult, ok := top["result"]
	if !ok {
		return nil
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(rawResult, &result) != nil || result == nil {
		return nil
	}
	var sessions []json.RawMessage
	if raw, ok := result["sessions"]; ok && json.Unmarshal(raw, &sessions) != nil {
		return nil
	}
	sessions, added := appendNativeSessions(sessions, native)
	if !added {
		return nil
	}
	rawSessions, err := json.Marshal(sessions)
	if err != nil {
		return nil
	}
	result["sessions"] = rawSessions
	rawResult, err = json.Marshal(result)
	if err != nil {
		return nil
	}
	top["result"] = rawResult
	out, err := json.Marshal(top)
	if err != nil {
		return nil
	}
	return out
}

// appendNativeSessions keeps the agent's rows intact and appends unseen native ids.
func appendNativeSessions(sessions []json.RawMessage, native []Session) ([]json.RawMessage, bool) {
	seen := make(map[string]struct{}, len(sessions))
	for _, raw := range sessions {
		var head struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(raw, &head) == nil {
			seen[head.SessionID] = struct{}{}
		}
	}
	added := 0
	for _, session := range native {
		if session.ID == "" {
			continue
		}
		if _, duplicate := seen[session.ID]; duplicate {
			continue
		}
		raw, err := json.Marshal(nativeSessionInfo{
			SessionID: session.ID,
			Cwd:       session.Cwd,
			Title:     session.Title,
			UpdatedAt: session.UpdatedAt,
		})
		if err != nil {
			continue
		}
		sessions = append(sessions, raw)
		seen[session.ID] = struct{}{}
		added++
	}
	return sessions, added > 0
}

// nativeSessionInfo is the ACP SessionInfo shape for one native chat.
type nativeSessionInfo struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Title     string `json:"title,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

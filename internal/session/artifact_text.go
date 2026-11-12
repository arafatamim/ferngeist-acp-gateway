package session

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
)

// observeArtifactStreamLocked keeps no text or grants. A missing/oversized
// message identity cannot prove that adjoining chunks belong to one message.
func (r *LoadRecovery) observeArtifactStreamLocked(probe frameProbe, ok bool) (start, leading bool) {
	if !ok {
		r.artifactLost = true
	}
	if !ok || probe.Method != "session/update" || probe.Params == nil || probe.Params.Update == nil {
		r.endArtifactStreamLocked()
		return false, false
	}
	u := probe.Params.Update
	id, sid := u.MessageID, probe.Params.SessionID
	if u.Discriminator != "agent_message_chunk" || len(sid) > 1024 {
		r.endArtifactStreamLocked()
		return false, false
	}
	if id == "" || len(id) > 1024 {
		// An unidentified continuation may be the tail of a URL; a new chunk
		// alone never establishes a leading token boundary.
		leading = !r.artifactLost && r.artifactSession == ""
		r.endArtifactStreamLocked()
		r.artifactMessageID, r.artifactSession = "", sid
		r.artifactLost = false
		return false, leading
	}
	if id == r.artifactMessageID && sid == r.artifactSession {
		r.artifactLost = false
		return false, false
	}
	leading = !r.artifactLost && (r.artifactSession == "" || r.artifactSession == sid && r.artifactMessageID != "")
	r.completeArtifactMessageLocked(sid)
	r.endArtifactStreamLocked()
	r.artifactMessageID, r.artifactSession = id, sid
	r.artifactLost = false
	return leading, leading
}

func (r *LoadRecovery) endArtifactStream() {
	r.mu.Lock()
	r.endArtifactStreamLocked()
	r.mu.Unlock()
}

func (r *LoadRecovery) discardArtifactStream() {
	r.mu.Lock()
	// The discarded frame could have introduced another message identity.
	r.artifactLost = true
	r.endArtifactStreamLocked()
	r.mu.Unlock()
}

func (r *LoadRecovery) endArtifactStreamLocked() {
	// Keep the last identity: an interruption is not proof that the next
	// chunk with that identity starts a new message.
	r.artifactStream++
}

// completeArtifactMessage is called only for a correlated prompt reply. A
// generic interruption forbids joining; it does not prove a message ended.
func (r *LoadRecovery) completeArtifactMessage(sid string) {
	r.mu.Lock()
	r.completeArtifactMessageLocked(sid)
	r.mu.Unlock()
}

func (r *LoadRecovery) completeArtifactMessageLocked(sid string) {
	frames := r.history[sid]
	if len(frames) == 0 || r.artifactMessageID == "" || r.artifactSession != sid {
		return
	}
	last := &frames[len(frames)-1]
	if last.artifactStream == r.artifactStream {
		last.artifactEnd = true
	}
}

type artifactHistory []historyFrame

func (h artifactHistory) advertises(path string) bool {
	for i := 0; i < len(h); {
		f := h[i]
		if artifactInFrame(f.line, path) {
			return true
		}
		text, ok := artifactFrameText(f.line)
		if !ok {
			i++
			continue
		}
		text, end := h.textMessage(i, text)
		complete := f.artifactStart && h[end-1].artifactEnd
		if artifactInText(text, path, f.artifactLeading, complete) {
			return true
		}
		i = end
	}
	return false
}

func (h artifactHistory) textMessage(i int, first string) (string, int) {
	stream := h[i].artifactStream
	var b strings.Builder
	b.WriteString(first)
	end := i + 1
	for end < len(h) && stream != 0 && h[end].artifactStream == stream {
		text, ok := artifactFrameText(h[end].line)
		if !ok {
			break
		}
		b.WriteString(text)
		end++
	}
	return b.String(), end
}

func artifactFrameText(line string) (string, bool) {
	var f struct {
		Method string `json:"method"`
		Params struct {
			Update struct {
				Kind    string `json:"sessionUpdate"`
				Content struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		} `json:"params"`
	}
	if json.Unmarshal([]byte(line), &f) != nil || f.Method != "session/update" {
		return "", false
	}
	u := f.Params.Update
	return u.Content.Text, u.Kind == "agent_message_chunk" && u.Content.Type == "text"
}

// URL spans are removed before Markdown extraction, including nested-looking
// destinations, queries and fragments. NUL preserves a barrier, not whitespace
// that could turn a URL suffix into an independent local token.
var artifactHTTPSpan = regexp.MustCompile(`(?i)https?://[^\s<>]+`)
var artifactMarkdown = regexp.MustCompile(`\[[^\]\r\n]*\]\(([^()\s]+)\)`)

func artifactInText(text, path string, start, end bool) bool {
	text = artifactHTTPSpan.ReplaceAllString(text, "\x00")
	for _, m := range artifactMarkdown.FindAllStringSubmatchIndex(text, -1) {
		if m[0] == 0 && !start || m[0] > 0 && !unicode.IsSpace(rune(text[m[0]-1])) {
			continue
		}
		if artifactTextToken(text[m[2]:m[3]], path) {
			return true
		}
	}
	// Whitespace delimits plain tokens; paired markup may deliberately delimit
	// a destination without whitespace. Never strip sentence punctuation.
	offset := 0
	for _, token := range strings.Fields(text) {
		at := strings.Index(text[offset:], token) + offset
		through := at + len(token)
		offset = through
		if (at > 0 || start) && len(token) > 1 && pairedArtifactDelimiters(token[0], token[len(token)-1]) &&
			artifactTextToken(token[1:len(token)-1], path) {
			return true
		}
		if (at > 0 || start) && (through < len(text) || end) && artifactTextToken(token, path) {
			return true
		}
	}
	return false
}

func pairedArtifactDelimiters(first, last byte) bool {
	return first == last && strings.ContainsRune("`'\"", rune(first)) || first == '<' && last == '>' || first == '(' && last == ')'
}

func artifactTextToken(token, path string) bool {
	if strings.ContainsAny(token, "\x00`'\"<>()[ ],;!?") || strings.ContainsFunc(token, unicode.IsSpace) {
		return false
	}
	if strings.HasPrefix(token, "/") && strings.ContainsAny(token, "#:") {
		return false
	}
	return (artifactContent{Type: "resource_link", URI: token}).matches(path)
}

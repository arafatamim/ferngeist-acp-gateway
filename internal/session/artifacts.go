package session

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// ArtifactAdvertised checks retained agent advertisements in a device's
// accepted ACP context. This is an admission snapshot, not a durable file grant.
func (rs *RuntimeSession) ArtifactAdvertised(deviceID, runtimeID, acpSessionID, path string) (bool, error) {
	frames, err := rs.artifactFrames(deviceID, runtimeID, acpSessionID)
	if err != nil {
		return false, err
	}
	return frames.advertises(path), nil
}

func (rs *RuntimeSession) artifactFrames(deviceID, runtimeID, acpSessionID string) (artifactHistory, error) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.closed {
		return artifactHistory{}, ErrSessionNotFound
	}
	for _, s := range rs.sessions {
		if s.RuntimeID != runtimeID {
			continue
		}
		if s.DeviceID != deviceID {
			return artifactHistory{}, ErrDeviceMismatch
		}
		s.mu.Lock()
		active := s.Status == StatusActive || s.Status == StatusDisconnected
		s.mu.Unlock()
		if !active || !s.pump.acp.holds(acpSessionID) {
			return artifactHistory{}, ErrSessionNotFound
		}
		r := s.pump.loadRecovery
		r.mu.Lock()
		frames := artifactHistory(slices.Clone(r.history[acpSessionID]))
		r.mu.Unlock()
		return frames, nil
	}
	return artifactHistory{}, ErrSessionNotFound
}

type artifactContent struct {
	Type     string `json:"type"`
	URI      string `json:"uri"`
	Resource struct {
		URI string `json:"uri"`
	} `json:"resource"`
}

func artifactInFrame(line, path string) bool {
	var frame struct {
		Params struct {
			Update struct {
				Kind    string          `json:"sessionUpdate"`
				Content json.RawMessage `json:"content"`
			} `json:"update"`
		} `json:"params"`
	}
	if json.Unmarshal([]byte(line), &frame) != nil {
		return false
	}
	u := frame.Params.Update
	if u.Kind == "agent_message_chunk" {
		var block artifactContent
		return json.Unmarshal(u.Content, &block) == nil && block.matches(path)
	}
	if u.Kind != "tool_call" && u.Kind != "tool_call_update" {
		return false
	}
	var contents []struct {
		Type    string          `json:"type"`
		Content artifactContent `json:"content"`
	}
	if json.Unmarshal(u.Content, &contents) != nil {
		return false
	}
	for _, c := range contents {
		if c.Type == "content" && c.Content.matches(path) {
			return true
		}
	}
	return false
}

func (c artifactContent) matches(path string) bool {
	uri := c.URI
	switch c.Type {
	case "resource_link":
	case "resource":
		uri = c.Resource.URI
	default:
		return false
	}
	if uri == path {
		return true
	}
	// url.Parse drops an empty trailing fragment; the URI contract forbids it
	// just like a nonempty fragment. Escaped filename data remains valid.
	if strings.Contains(uri, "#") {
		return false
	}
	u, err := url.Parse(uri)
	return err == nil && u.Scheme == "file" && u.Host == "" && u.User == nil &&
		u.Opaque == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" &&
		(u.Path == path || runtime.GOOS == "windows" && u.Path == "/"+filepath.ToSlash(path))
}

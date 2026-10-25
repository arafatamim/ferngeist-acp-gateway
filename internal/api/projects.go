package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/projects"
)

const (
	// projectsLineWriteTimeout bounds each NDJSON line's write. A listing can
	// outlast the server's WriteTimeout, so the deadline moves per line.
	projectsLineWriteTimeout = 20 * time.Second
	// maxProjectsAgents caps ?agents=, since each ID can start a process.
	maxProjectsAgents = 64
	// maxProjectsRepos bounds filesystem checks requested by a client snapshot.
	maxProjectsRepos = 256
)

// handleProjects streams GET /v1/projects?agents=a,b as NDJSON: one line per
// named agent, flushed as that agent's listing finishes, then the checkouts
// line.
func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGatewayCredential(w, r); !ok {
		return
	}
	if s.projects == nil {
		writeError(w, http.StatusServiceUnavailable, "project listing not available")
		return
	}
	agentIDs := agentIDsParam(r.URL.Query().Get("agents"))
	if len(agentIDs) > maxProjectsAgents {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d agents", maxProjectsAgents))
		return
	}
	repos, err := knownReposParam(r.URL.Query().Get("repos"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rc := http.NewResponseController(w)
	enc := json.NewEncoder(w)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	// Write errors mean the client left, which cancels r.Context and ends
	// the listing.
	writeLine := func(line any) {
		_ = rc.SetWriteDeadline(time.Now().Add(projectsLineWriteTimeout))
		_ = enc.Encode(line)
		_ = rc.Flush()
	}
	s.projects.List(r.Context(), agentIDs, func(line projects.AgentLine) {
		for _, chat := range line.Chats {
			repos = append(repos, chat.Repo)
		}
		writeLine(line)
	})
	if r.Context().Err() == nil {
		checkouts := s.projects.Checkouts(r.Context(), slices.Compact(slices.Sorted(slices.Values(repos))))
		if r.Context().Err() == nil {
			writeLine(checkouts)
		}
	}
}

// agentIDsParam splits ?agents= into distinct IDs, so a repeated ID never
// starts its agent twice.
func agentIDsParam(raw string) []string {
	var ids []string
	seen := map[string]bool{}
	for id := range strings.SplitSeq(raw, ",") {
		if id = strings.TrimSpace(id); id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

// knownReposParam accepts snapshot roots without interpreting commas in paths.
func knownReposParam(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	var repos []string
	if err := json.Unmarshal([]byte(raw), &repos); err != nil || repos == nil {
		return nil, fmt.Errorf("repos must be a JSON array of absolute paths")
	}
	if len(repos) > maxProjectsRepos {
		return nil, fmt.Errorf("at most %d repos", maxProjectsRepos)
	}
	for _, repo := range repos {
		if !filepath.IsAbs(repo) || strings.ContainsRune(repo, 0) {
			return nil, fmt.Errorf("repos must contain valid absolute paths")
		}
	}
	return slices.Compact(slices.Sorted(slices.Values(repos))), nil
}

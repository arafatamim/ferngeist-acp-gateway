package api

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/projects"
)

type httpProjectMeasurement struct {
	Scenario    string `json:"scenario"`
	Device      int    `json:"device"`
	FirstMicros int64  `json:"first_us"`
	EndMicros   int64  `json:"complete_us"`
	Bytes       int    `json:"response_bytes"`
}

// TestProjectListingHTTPMeasurements measures the full loopback NDJSON endpoint.
// The deliberate cache-aging interval is fixture setup, outside measured calls.
func TestProjectListingHTTPMeasurements(t *testing.T) {
	if os.Getenv("FERNGEIST_MEASURE_PROJECTS") != "1" {
		t.Skip("set FERNGEIST_MEASURE_PROJECTS=1 to measure mock HTTP listings")
	}
	t.Setenv("FERNGEIST_MOCK_LIST", "")
	t.Setenv("FERNGEIST_MOCK_DELETE", "")
	server, token, root := projectListingServer(t)
	// Fixture setup pairs eight devices from one test IP before timing starts.
	cfg := server.cfg
	cfg.PairingBurstPerIP = 8
	server.rateLimiter = newPairingRateLimiter(cfg)
	tokens := []string{token}
	for range 7 {
		tokens = append(tokens, pairDevice(t, server))
	}
	for _, name := range []string{"project", "bulk"} {
		cmd := exec.Command("git", "init", "-q", "-b", "main", filepath.Join(root, name))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git init: %v: %s", err, out)
		}
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	for range 3 {
		server.projects.Close()
		server.projects = projects.New(server.catalog, server.runtime, server.workspaceSvc)
		t.Cleanup(server.projects.Close)
		measureProjectHTTP(t, httpServer.URL, tokens[:1], "cold")
		measureProjectHTTP(t, httpServer.URL, tokens[:1], "cached")
		timer := time.NewTimer(11 * time.Second)
		select {
		case <-timer.C:
		case <-t.Context().Done():
			timer.Stop()
			t.Fatal(t.Context().Err())
		}
		logIdleProjectAgents(t, "before-aged-cache")
		measureProjectHTTP(t, httpServer.URL, tokens[:1], "aged-cache")
		server.projects.Close()
		server.projects = projects.New(server.catalog, server.runtime, server.workspaceSvc)
		t.Cleanup(server.projects.Close)
		measureProjectHTTP(t, httpServer.URL, tokens, "concurrent-cold")
	}
}

func measureProjectHTTP(t *testing.T, baseURL string, tokens []string, scenario string) {
	t.Helper()
	var wg sync.WaitGroup
	start := make(chan struct{})
	for device, token := range tokens {
		wg.Go(func() {
			<-start
			result := fetchProjectMeasurement(t, baseURL, token)
			result.Scenario, result.Device = scenario, device+1
			out, err := json.Marshal(result)
			if err != nil {
				t.Error(err)
				return
			}
			t.Log(string(out))
		})
	}
	close(start)
	wg.Wait()
	logIdleProjectAgents(t, scenario)
}

func logIdleProjectAgents(t *testing.T, scenario string) {
	t.Helper()
	paths, err := filepath.Glob("/proc/self/task/*/children")
	if err != nil || len(paths) == 0 {
		t.Logf("idle %s: mock child RSS unavailable (requires Linux procfs)", scenario)
		return
	}
	var pids []string
	for _, path := range paths {
		children, readErr := os.ReadFile(path)
		if readErr != nil {
			continue // Threads may exit during the idle snapshot.
		}
		pids = append(pids, strings.Fields(string(children))...)
	}
	var count, rss int64
	for _, pid := range pids {
		bytes := idleProjectAgentRSS(pid)
		if bytes > 0 {
			count++
			rss += bytes
		}
	}
	t.Logf("idle %s: mock_children=%d rss_bytes=%d", scenario, count, rss)
}

func idleProjectAgentRSS(pid string) int64 {
	executable, err := os.Readlink("/proc/" + pid + "/exe")
	if err != nil || !strings.HasPrefix(filepath.Base(executable), "mock-stdio-agent") {
		return 0
	}
	data, err := os.ReadFile("/proc/" + pid + "/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * int64(os.Getpagesize())
}

func fetchProjectMeasurement(t *testing.T, baseURL, token string) httpProjectMeasurement {
	t.Helper()
	var result httpProjectMeasurement
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/v1/projects?agents=mock-acp", nil)
	if err != nil {
		t.Error(err)
		return result
	}
	req.Header.Set("Authorization", "Bearer "+token)
	start := time.Now()
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Error(err)
		return result
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	first, err := reader.ReadBytes('\n')
	result.FirstMicros = time.Since(start).Microseconds()
	if err != nil {
		t.Error(err)
		return result
	}
	rest, err := io.ReadAll(reader)
	result.EndMicros = time.Since(start).Microseconds()
	result.Bytes = len(first) + len(rest)
	if err != nil {
		t.Error(err)
		return result
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Errorf("measurement HTTP status %d, content type %s", response.StatusCode, response.Header.Get("Content-Type"))
	}
	var agent projects.AgentLine
	var checkouts projects.CheckoutsLine
	if err := json.Unmarshal(first, &agent); err != nil || agent.Status != projects.StatusOK || len(agent.Chats) != 2 {
		t.Errorf("measurement agent status %s, chats %d, decode %v", agent.Status, len(agent.Chats), err)
	}
	if err := json.Unmarshal(rest, &checkouts); err != nil || checkouts.Type != "checkouts" || len(checkouts.Repos) != 2 {
		t.Errorf("measurement checkouts type %s, repos %d, decode %v", checkouts.Type, len(checkouts.Repos), err)
	}
	return result
}

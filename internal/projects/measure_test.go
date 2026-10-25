package projects

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type listingMeasurement struct {
	Scenario       string `json:"scenario"`
	Devices        int    `json:"devices"`
	FirstMicros    int64  `json:"first_us"`
	CompleteMicros int64  `json:"complete_us"`
	ResponseBytes  int    `json:"agent_line_bytes"`
	GatewayRSS     int64  `json:"test_process_peak_rss_bytes"`
	ChildRSS       int64  `json:"children_peak_rss_bytes"`
	IdleChildRSS   int64  `json:"idle_children_rss_bytes"`
	WarmProcesses  int    `json:"warm_processes"`
}

// TestProjectListingMeasurements is an opt-in measurement, not a latency gate.
// Keep setup outside the measured interval and use identical inputs before/after.
func TestProjectListingMeasurements(t *testing.T) {
	if os.Getenv("FERNGEIST_MEASURE_PROJECTS") != "1" {
		t.Skip("set FERNGEIST_MEASURE_PROJECTS=1 to measure mock listings")
	}
	t.Setenv("FERNGEIST_MOCK_LIST", "")
	t.Setenv("FERNGEIST_MOCK_DELETE", "")
	newLister := newMockLister(t)
	for _, name := range []string{"project", "bulk"} {
		cmd := exec.Command("git", "init", "-q", "-b", "main", filepath.Join(os.Getenv("FERNGEIST_MOCK_LIST_ROOT"), name))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git init: %v: %s", err, out)
		}
	}
	for range 5 {
		lister := newLister()
		measureListing(t, lister, "cold", 1)
		// Expire only the response cache; avoid a wall-clock wait changing the fixture.
		lister.mu.Lock()
		clear(lister.cached)
		lister.mu.Unlock()
		measureListing(t, lister, "warm", 1)
		measureListing(t, lister, "cached", 1)
		lister.Close()
		concurrent := newLister()
		measureListing(t, concurrent, "concurrent-cold", 8)
		concurrent.Close()
	}
}

func measureListing(t *testing.T, lister *Lister, scenario string, devices int) {
	t.Helper()
	stop, sampled := make(chan struct{}), make(chan [2]int64, 1)
	go sampleListingRSS(stop, sampled)
	start := time.Now()
	results := make(chan listingMeasurement, devices)
	var wg sync.WaitGroup
	for range devices {
		wg.Go(func() {
			var result listingMeasurement
			var body bytes.Buffer
			lister.List(context.Background(), []string{"mock-acp"}, func(line AgentLine) {
				if line.Status != StatusOK || len(line.Chats) != 2 {
					t.Errorf("measurement fixture: status %s, %d chats, error %v", line.Status, len(line.Chats), line.Error)
				}
				if err := json.NewEncoder(&body).Encode(line); err != nil {
					t.Errorf("encode agent line: %v", err)
				}
				result.FirstMicros = time.Since(start).Microseconds()
			})
			result.ResponseBytes = body.Len()
			results <- result
		})
	}
	wg.Wait()
	result := listingMeasurement{Scenario: scenario, Devices: devices, CompleteMicros: time.Since(start).Microseconds()}
	close(stop)
	peak := <-sampled
	result.GatewayRSS, result.ChildRSS = peak[0], peak[1]
	result.IdleChildRSS = listingChildrenRSS(os.Getpid())
	close(results)
	for device := range results {
		if result.FirstMicros == 0 || device.FirstMicros < result.FirstMicros {
			result.FirstMicros = device.FirstMicros
		}
		result.ResponseBytes += device.ResponseBytes
	}
	lister.mu.Lock()
	result.WarmProcesses = len(lister.warm)
	lister.mu.Unlock()
	out, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(out))
}

// RSS sampling uses Linux procfs and sums descendants separately from the Go
// test process. Short-lived children between samples can be missed.
func sampleListingRSS(stop <-chan struct{}, result chan<- [2]int64) {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	peak := [2]int64{-1, -1}
	for {
		gateway := listingProcessRSS(os.Getpid())
		children := listingChildrenRSS(os.Getpid())
		peak[0], peak[1] = max(peak[0], gateway), max(peak[1], children)
		select {
		case <-stop:
			result <- peak
			return
		case <-ticker.C:
		}
	}
}

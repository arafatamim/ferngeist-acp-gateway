package projects

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMemoryPressureUsesNewStallsAndRebaselinesAfterUnavailableSignals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.pressure")
	sources := memoryPressure{{path: path}}
	cases := []struct {
		name string
		data string
		want bool
	}{
		{"baseline", "some avg10=0.00 total=12\nfull total=4\n", false},
		{"new stalls", "some avg10=0.00 total=23\nfull total=5\n", true},
		{"old stalls", "some avg10=0.00 total=23\nfull total=5\n", false},
		{"counter reset", "some total=3\n", false},
		{"new stalls after reset", "some total=4\n", true},
		{"malformed", "some total=bad\n", false},
		{"signal restored", "some total=100\n", false},
		{"new stalls after recovery", "some total=101\n", true},
		{"missing some", "full total=200\n", false},
		{"missing total", "some avg10=1.00\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := sources.increased(); got != tc.want {
				t.Errorf("pressure = %v, want %v", got, tc.want)
			}
		})
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if sources.increased() {
		t.Fatal("an unavailable signal must not cause eviction")
	}
}

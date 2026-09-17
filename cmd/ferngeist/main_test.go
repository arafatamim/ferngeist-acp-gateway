package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/adminclient"
)

// The exit codes are a documented contract (docs/usage.md): 2 means the daemon
// did not answer, 1 means the command itself failed. The same down daemon shows
// up wrapped in a command's own context, so the mapping must unwrap.
func TestExitCodeFor(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"no error", nil, 0},
		{"daemon down", adminclient.ErrDaemonUnreachable, exitDaemonUnreachable},
		{"daemon down, wrapped by the command", fmt.Errorf("list agents: %w", adminclient.ErrDaemonUnreachable), exitDaemonUnreachable},
		{"command failure", errors.New("remove agent: unknown custom agent"), 1},
		{"already reported", &alreadyReported{code: exitDaemonUnreachable}, exitDaemonUnreachable},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := exitCodeFor(test.err); got != test.want {
				t.Fatalf("exitCodeFor(%v) = %d, want %d", test.err, got, test.want)
			}
		})
	}
}

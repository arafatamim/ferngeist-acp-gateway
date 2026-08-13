// Package remote provisions outbound-only remote access for the gateway over
// Tailscale: it either drives an installed tailscale CLI or embeds a tsnet
// node, and reports the stable public HTTPS URL back to the daemon.
package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Runner executes external commands. Inject a fake in tests.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecRunner runs real binaries.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// CLI drives an installed tailscale binary.
type CLI struct {
	runner Runner
}

func NewCLI(runner Runner) *CLI { return &CLI{runner: runner} }

// Detect reports whether a tailscale binary exists and is logged in. A missing
// binary is not an error: callers use it to choose the tsnet fallback.
func (c *CLI) Detect(ctx context.Context) (found, loggedIn bool, err error) {
	if _, err := c.runner.Run(ctx, "tailscale", "version"); err != nil {
		return false, false, nil // not installed
	}
	if _, err := c.runner.Run(ctx, "tailscale", "status"); err != nil {
		return true, false, nil // installed, not logged in
	}
	return true, true, nil
}

type statusOutput struct {
	Self struct {
		DNSName string `json:"DNSName"`
	} `json:"Self"`
}

// FunnelURL returns the public HTTPS URL for this node from
// `tailscale status --json` (Self.DNSName).
func (c *CLI) FunnelURL(ctx context.Context) (string, error) {
	out, err := c.runner.Run(ctx, "tailscale", "status", "--json")
	if err != nil {
		return "", fmt.Errorf("tailscale status: %w", err)
	}
	var st statusOutput
	if err := json.Unmarshal(out, &st); err != nil {
		return "", fmt.Errorf("parse tailscale status: %w", err)
	}
	if st.Self.DNSName == "" {
		return "", errors.New("tailscale status: empty Self.DNSName (is the node up?)")
	}
	return "https://" + strings.TrimSuffix(st.Self.DNSName, "."), nil
}

// EnableFunnel starts a background funnel for the given port. First-run
// consent (HTTPS + funnel node attribute) requires an interactive run, so on
// failure the error tells the operator how to do it once.
func (c *CLI) EnableFunnel(ctx context.Context, port int) error {
	if _, err := c.runner.Run(ctx, "tailscale", "funnel", "--bg", fmt.Sprintf("%d", port)); err != nil {
		return fmt.Errorf("tailscale funnel --bg %d failed (run `tailscale funnel %d` once in a terminal to consent): %w", port, port, err)
	}
	return nil
}

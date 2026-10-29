// Package remote provisions outbound-only remote access for the gateway over
// Tailscale: it either drives an installed tailscale CLI or embeds a tsnet
// node, and reports the stable public HTTPS URL back to the daemon. It also
// owns the provisioning lifecycle (Access): the background retry loop, the
// persisted public URL, paired-device notifications, and the auth-pending
// snapshot the status API reads.
package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strconv"

	"strings"
	"tailscale.com/ipn"
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

// EnableRemote starts background Funnel or Serve for the given port. First-run
// consent (HTTPS + funnel node attribute) requires an interactive run, so on
// failure the error tells the operator how to do it once.
func (c *CLI) EnableRemote(ctx context.Context, port int, private bool) error {
	lock, err := openCLILock()
	if err != nil {
		return fmt.Errorf("open Tailscale provisioning lock: %w", err)
	}
	defer func() { _ = lock.Close() }() // Closing releases the OS lock even after command failure.
	if err := lockCLIFile(lock); err != nil {
		return fmt.Errorf("another gateway may be provisioning the host Tailscale endpoint; retry: %w", err)
	}

	out, err := c.runner.Run(ctx, "tailscale", "serve", "status", "--json")
	if err != nil {
		return fmt.Errorf("read tailscale serve config: %w", err)
	}
	var cfg ipn.ServeConfig
	if err := json.Unmarshal(out, &cfg); err != nil {
		return fmt.Errorf("parse tailscale serve config: %w", err)
	}
	if conflictingEndpoint(&cfg, port) {
		return errors.New("tailscale HTTPS endpoint is already in use; use mode=tsnet with a separate gateway state database")
	}
	command := "funnel"
	if private {
		command = "serve"
	}
	if _, err := c.runner.Run(ctx, "tailscale", command, "--bg", strconv.Itoa(port)); err != nil {
		return fmt.Errorf("tailscale %s --bg %d failed (run tailscale %s %d once in a terminal to consent): %w", command, port, command, port, err)
	}
	return nil
}

// conflictingEndpoint protects other services on the host's shared HTTPS port.
// A matching loopback root proxy is safe to reconfigure on gateway restart.
func conflictingEndpoint(cfg *ipn.ServeConfig, port int) bool {
	matched := false
	for host, web := range cfg.Web {
		if !strings.HasSuffix(string(host), ":443") {
			continue
		}
		if web == nil || len(web.Handlers) != 1 {
			return true
		}
		handler := web.Handlers["/"]
		if handler == nil {
			return true
		}
		target, err := url.Parse(handler.Proxy)
		if err != nil || target.Scheme != "http" || target.Port() != strconv.Itoa(port) {
			return true
		}
		host := target.Hostname()
		if host != "127.0.0.1" && host != "localhost" && host != "::1" {
			return true
		}
		if (target.Path != "" && target.Path != "/") || target.RawQuery != "" || target.User != nil || target.Fragment != "" {
			return true
		}
		matched = true
	}
	tcp := cfg.TCP[443]
	if tcp != nil && (!matched || !tcp.HTTPS || tcp.TCPForward != "") {
		return true
	}
	for _, foreground := range cfg.Foreground {
		if foreground != nil && (foreground.TCP[443] != nil || conflictingEndpoint(foreground, port)) {
			return true
		}
	}
	return false
}

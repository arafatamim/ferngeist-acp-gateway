package remote

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/config"

	"tailscale.com/ipn/ipnstate"
)

// errNilTSNet signals that the tsnet factory returned a nil node; exported
// behavior is the wrapped error message, this sentinel is for tests.
var errNilTSNet = errors.New("tsnet factory returned nil node")

// FunnelSetupHint inspects a provisioning error and returns plain-English
// guidance for the tailnet setting the operator still needs to change, or ""
// when the error is not a configuration issue. The error text comes from
// Tailscale itself (tsnet/ipn), so it is matched on stable substrings.
func FunnelSetupHint(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "HTTPS must be enabled"):
		return "Go to https://login.tailscale.com/admin/dns and turn on HTTPS Certificates, then the gateway will pick up automatically."
	case strings.Contains(msg, `"funnel" node attribute`), strings.Contains(msg, "node attribute not set"):
		return `Go to https://login.tailscale.com/admin/acls and add the "funnel" node attribute, then the gateway will pick up automatically.`
	case strings.Contains(msg, "not allowed for funnel"):
		return "Port 443 is not enabled for Funnel on this tailnet; allow port 443 in the funnel node attribute, then the gateway will pick up automatically."
	default:
		return ""
	}
}

// Result describes an active remote-access path.
type Result struct {
	Mode     string // "cli" or "tsnet"
	URL      string
	Listener net.Listener // nil in cli mode
	Close    func() error
}

// RemoteSetupSnapshot is the daemon's live remote-access provisioning state
// as seen by the status API: whether setup is blocked on an interactive
// Tailscale login (and the link), plus plain-English guidance for whatever is
// blocking setup. A nil snapshot means provisioning has no pending issue.
type RemoteSetupSnapshot struct {
	AuthRequired bool   `json:"authRequired,omitempty"`
	AuthURL      string `json:"authUrl,omitempty"`
	Hint         string `json:"hint,omitempty"`
	LastErr      string `json:"lastErr,omitempty"`
}

// TSNetFactory builds embedded nodes; a seam for tests.
type TSNetFactory func(hostname, authKey, stateDir string, private bool) *TSNet

type Provisioner struct {
	cli       *CLI
	factory   TSNetFactory
	logf      func(format string, args ...any)
	userLogf  func(format string, args ...any)
	loginHook func(authURL string)
}

func NewProvisioner(cli *CLI, factory TSNetFactory) *Provisioner {
	return &Provisioner{cli: cli, factory: factory}
}

// SetLogf routes the embedded node's internal subsystem lines to the given
// sink (nil keeps the default, which drops them).
func (p *Provisioner) SetLogf(logf func(format string, args ...any)) {
	p.logf = logf
}

// SetUserLogf routes tsnet's user-facing lines (state path, login link, auth
// loop) to the given sink (nil keeps the default, which drops them).
func (p *Provisioner) SetUserLogf(logf func(format string, args ...any)) {
	p.userLogf = logf
}

// SetLoginHook registers a callback fired once per pending interactive login
// with the link the operator must open (nil clears it). The daemon uses it to
// surface the link through its status API while provisioning waits.
func (p *Provisioner) SetLoginHook(hook func(authURL string)) {
	p.loginHook = hook
}

// waitableNode is the subset of TSNet the interactive-login wait needs; a
// seam for tests.
type waitableNode interface {
	WaitReady(ctx context.Context) (*ipnstate.Status, error)
}

// loginPollInterval is how often the login wait re-checks node readiness.
var loginPollInterval = 2 * time.Second

// waitForLogin keeps polling the node until it is Running, tolerating a
// pending interactive login. The node MUST stay alive for its login URL to
// remain valid — killing it (or burning the URL every retry tick) makes the
// login impossible for a human to finish. report fires once per distinct auth
// URL so the operator can act on it.
func waitForLogin(ctx context.Context, node waitableNode, report func(authURL string)) (*ipnstate.Status, error) {
	reported := ""
	for {
		st, err := node.WaitReady(ctx)
		if err == nil {
			return st, nil
		}
		var authErr *ErrAuthRequired
		if !errors.As(err, &authErr) {
			return nil, err
		}
		if report != nil && authErr.AuthURL != reported {
			report(authErr.AuthURL)
			reported = authErr.AuthURL
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(loginPollInterval):
		}
	}
}

// Provision establishes remote access per cfg.TailscaleMode. Returns
// (nil, nil) when disabled. localAddr is the origin the tsnet proxy targets
// (e.g. "127.0.0.1:5788"); port is the port funneled by the CLI path.
func (p *Provisioner) Provision(ctx context.Context, cfg config.Config, localAddr string, port int) (*Result, error) {
	mode := cfg.TailscaleMode
	if mode == "off" {
		return nil, nil
	}

	if mode == "auto" || mode == "cli" {
		found, loggedIn, err := p.cli.Detect(ctx)
		if err != nil {
			return nil, fmt.Errorf("detect tailscale: %w", err)
		}
		if found && loggedIn {
			// An existing, signed-in app is the smoothest path: drive it, and
			// read the address it already knows. (Embedding here would add a
			// second node to the user's tailnet — avoid that.)
			if err := p.cli.EnableFunnel(ctx, port); err != nil {
				return nil, err
			}
			url, err := p.waitForURL(ctx)
			if err != nil {
				return nil, err
			}
			return &Result{Mode: "cli", URL: url, Close: func() error { return nil }}, nil
		}
		if mode == "cli" {
			// Explicit cli mode: never silently switch providers.
			if !found {
				return nil, errors.New("tailscale CLI not found; install it or use mode=tsnet")
			}
			return nil, errors.New("tailscale is installed but not logged in; run `tailscale up` or set FERNGEIST_GATEWAY_TAILSCALE_AUTH_KEY")
		}
		// auto: the app is absent, or present but signed out. A signed-out app
		// means no tailnet is configured on this machine, so there is nothing
		// to duplicate — fall through to the embedded node, which does the
		// first-time login itself.
	}

	// tsnet (explicit or auto fallback)
	node := p.factory(cfg.TailscaleHostname, cfg.TailscaleAuthKey, filepath.Join(filepath.Dir(cfg.StateDBPath), "tsnet"), cfg.TailscalePrivate)
	if node == nil {
		return nil, errNilTSNet
	}
	node.SetLogf(p.logf)
	node.SetUserLogf(p.userLogf)
	if err := node.Start(); err != nil {
		_ = node.Close()
		return nil, err
	}
	// A pending interactive login is not a failure: the login URL belongs to
	// the node that issued it, so the node stays alive (and the URL valid)
	// until the human finishes logging in. Killing the node every retry tick
	// burned the link and made the login impossible to complete.
	st, err := waitForLogin(ctx, node, p.loginHook)
	if err != nil {
		_ = node.Close()
		return nil, err
	}
	ln, pubURL, err := node.openListener(st)
	if err != nil {
		// The node may already be started (and holding sockets/goroutines);
		// close it so a retry with a fresh node doesn't leak.
		_ = node.Close()
		return nil, err
	}
	target, err := url.Parse("http://" + localAddr)
	if err != nil {
		return nil, fmt.Errorf("parse local origin: %w", err)
	}
	srv := &http.Server{Handler: httputil.NewSingleHostReverseProxy(target)}
	go func() {
		_ = srv.Serve(ln) //nolint:errcheck // shutdown via Close
	}()
	return &Result{
		Mode:     "tsnet",
		URL:      pubURL,
		Listener: ln,
		Close: func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx) //nolint:errcheck
			return node.Close()
		},
	}, nil
}

// waitForURL polls the CLI for the node's public URL until it appears or the
// 30s budget expires.
func (p *Provisioner) waitForURL(ctx context.Context) (string, error) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for {
		url, err := p.cli.FunnelURL(ctx)
		if err == nil && url != "" {
			return url, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return "", fmt.Errorf("waiting for funnel URL: %w", lastErr)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}

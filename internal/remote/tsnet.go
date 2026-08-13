package remote

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

// TSNet embeds a Tailscale node in-process. Reachability: Funnel (public
// internet + tailnet) by default, ListenTLS (tailnet only) when private.
type TSNet struct {
	hostname string
	authKey  string
	stateDir string
	private  bool
	srv      *tsnet.Server
	// logf receives the embedded node's internal subsystem lines (tsnet's
	// Logf field). SetLogf routes them into an external logger (the daemon
	// sends them to slog as Debug "tsnet" entries).
	logf func(format string, args ...any)
	// userLogf receives tsnet's user-facing lines (state path, login link,
	// auth loop — tsnet's UserLogf field). SetUserLogf routes them into an
	// external logger; the daemon sends them to slog as Info "tsnet" entries.
	// Without it, tsnet prints them through the standard library logger.
	userLogf func(format string, args ...any)
}

func NewTSNet(hostname, authKey, stateDir string, private bool) *TSNet {
	noop := func(string, ...any) {}
	return &TSNet{hostname: hostname, authKey: authKey, stateDir: stateDir, private: private, logf: noop, userLogf: noop}
}

// SetLogf replaces the sink for the embedded node's internal subsystem lines.
func (t *TSNet) SetLogf(logf func(format string, args ...any)) {
	if logf != nil {
		t.logf = logf
	}
}

// SetUserLogf replaces the sink for tsnet's user-facing lines (state path,
// login link, auth loop).
func (t *TSNet) SetUserLogf(logf func(format string, args ...any)) {
	if logf != nil {
		t.userLogf = logf
	}
}

// ErrAuthRequired is returned when the embedded node needs a one-time
// interactive login before it can serve. AuthURL is the link the operator
// must open; the node also prints it to the user-facing log.
type ErrAuthRequired struct{ AuthURL string }

func (e *ErrAuthRequired) Error() string {
	return "tailscale login required; open " + e.AuthURL
}

// Start creates the state directory, constructs the embedded node, and kicks
// off its backend (login included). The node must stay alive for a pending
// interactive login's URL to remain valid, so callers should not close it
// while login is in flight.
func (t *TSNet) Start() error {
	if err := os.MkdirAll(t.stateDir, 0o700); err != nil {
		return fmt.Errorf("create tsnet state dir: %w", err)
	}
	t.srv = &tsnet.Server{
		Hostname: t.hostname,
		AuthKey:  t.authKey,
		Dir:      t.stateDir,
		Logf:     t.logf,
		UserLogf: t.userLogf,
	}
	if err := t.srv.Start(); err != nil {
		return fmt.Errorf("tsnet start: %w", err)
	}
	return nil
}

// WaitReady blocks until the node is Running. It returns ErrAuthRequired
// (with the interactive login link) as soon as the backend reports NeedsLogin
// instead of blocking until the human finishes logging in — the node is left
// running so the link stays valid while an operator acts on it. Terminal
// non-login states are errors.
func (t *TSNet) WaitReady(ctx context.Context) (*ipnstate.Status, error) {
	lc, err := t.srv.LocalClient()
	if err != nil {
		return nil, fmt.Errorf("tsnet local client: %w", err)
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		st, err := lc.Status(ctx)
		if err != nil {
			return nil, fmt.Errorf("tsnet status: %w", err)
		}
		switch st.BackendState {
		case "Running":
			return st, nil
		case "NeedsLogin", "NoState", "Starting":
			// The backend briefly reports these states before Running; only a
			// pending interactive login carries an auth URL.
			if st.AuthURL != "" {
				return nil, &ErrAuthRequired{AuthURL: st.AuthURL}
			}
		default:
			return nil, fmt.Errorf("tsnet backend in state %v", st.BackendState)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// openListener opens the public (Funnel) or tailnet-only (TLS) listener once
// the node is Running, and derives the public HTTPS URL.
func (t *TSNet) openListener(st *ipnstate.Status) (net.Listener, string, error) {
	fqdn := st.Self.DNSName // e.g. "gw.tail1234.ts.net"
	if fqdn == "" {
		return nil, "", errors.New("tsnet up: empty DNSName")
	}
	var err error
	var ln net.Listener
	if t.private {
		ln, err = t.srv.ListenTLS("tcp", ":443")
		if err != nil {
			return nil, "", fmt.Errorf("tsnet listen tls: %w", err)
		}
	} else {
		ln, err = t.srv.ListenFunnel("tcp", ":443", tsnet.FunnelOnly())
		if err != nil {
			return nil, "", fmt.Errorf("tsnet funnel: %w (enable Funnel in the tailnet admin console: https://login.tailscale.com/admin/acls — add the funnel node attribute, and enable HTTPS in DNS settings)", err)
		}
	}
	return ln, funnelURL(fqdn), nil
}

func (t *TSNet) Close() error {
	if t.srv == nil {
		return nil
	}
	return t.srv.Close()
}

// funnelURL builds the public HTTPS URL from a tailnet FQDN, tolerating a
// trailing dot or surrounding whitespace.
func funnelURL(fqdn string) string {
	return "https://" + strings.TrimSuffix(strings.TrimSpace(fqdn), ".")
}

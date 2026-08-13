package remote

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/config"

	"tailscale.com/ipn/ipnstate"
)

// fakeWaitableNode simulates a node that needs an interactive login, then
// reaches Running. It records whether Close was ever called during the wait —
// the node must stay alive for its login URL to remain valid.
type fakeWaitableNode struct {
	authPolls int    // how many polls report a pending login before Running
	closed    bool   // whether Close was called
	waitReady func() // optional hook for ordering
}

func (n *fakeWaitableNode) WaitReady(context.Context) (*ipnstate.Status, error) {
	if n.waitReady != nil {
		n.waitReady()
	}
	if n.authPolls > 0 {
		n.authPolls--
		return nil, &ErrAuthRequired{AuthURL: "https://login.tailscale.com/a/waitforlogin"}
	}
	return &ipnstate.Status{BackendState: "Running", Self: &ipnstate.PeerStatus{DNSName: "gw.tail1234.ts.net"}}, nil
}

func (n *fakeWaitableNode) Close() error { n.closed = true; return nil }

func TestWaitForLogin_keeps_polling_and_reports_once(t *testing.T) {
	loginPollInterval = time.Millisecond
	node := &fakeWaitableNode{authPolls: 4}
	reports := 0
	reportedURL := ""
	start := make(chan struct{})

	done := make(chan *ipnstate.Status, 1)
	errCh := make(chan error, 1)
	go func() {
		st, err := waitForLogin(context.Background(), node, func(url string) {
			reports++
			reportedURL = url
			close(start)
		})
		if err != nil {
			errCh <- err
			return
		}
		done <- st
	}()

	<-start
	if node.closed {
		t.Fatal("node was closed while a login was pending; the login URL dies with it")
	}
	if reports != 1 {
		t.Fatalf("login reported %d times after first detection, want 1", reports)
	}
	if reportedURL != "https://login.tailscale.com/a/waitforlogin" {
		t.Fatalf("reported URL = %q", reportedURL)
	}

	select {
	case st := <-done:
		if st.BackendState != "Running" {
			t.Fatalf("BackendState = %q, want Running", st.BackendState)
		}
	case err := <-errCh:
		t.Fatalf("waitForLogin returned error: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("waitForLogin never returned after login completed")
	}
	if node.closed {
		t.Fatal("node was closed after login completed but before handoff")
	}
}

func TestWaitForLogin_aborts_on_cancel_and_closes_node(t *testing.T) {
	loginPollInterval = time.Hour // must not matter: ctx wins
	node := &fakeWaitableNode{authPolls: 1 << 30}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := waitForLogin(ctx, node, nil)
		errCh <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitForLogin ignored context cancellation")
	}
}

func TestWaitForLogin_surfaces_nonlogin_errors(t *testing.T) {
	hardErr := errors.New("tsnet funnel: Funnel not available")
	_, err := waitForLogin(context.Background(), &hardFailingNode{err: hardErr}, nil)
	if !errors.Is(err, hardErr) {
		t.Fatalf("error = %v, want the non-login error to pass through", err)
	}
}

type hardFailingNode struct {
	err error
}

func (n *hardFailingNode) WaitReady(context.Context) (*ipnstate.Status, error) { return nil, n.err }

func TestProvisionDisabled(t *testing.T) {
	p := NewProvisioner(nil, nil)
	res, err := p.Provision(context.Background(), config.Config{TailscaleMode: "off"}, "", 0)
	if err != nil || res != nil {
		t.Fatalf("Provision(off) = (%v, %v), want (nil, nil)", res, err)
	}
}

func TestProvisionCLIMode(t *testing.T) {
	r := &fakeRunner{responses: map[string]string{"version": "v1.80.0", "status": statusJSON}}
	p := NewProvisioner(NewCLI(r), nil)
	res, err := p.Provision(context.Background(), config.Config{TailscaleMode: "cli"}, "", 5788)
	if err != nil {
		t.Fatalf("Provision(cli): %v", err)
	}
	if res.Mode != "cli" || res.URL != "https://mymachine.tail1234.ts.net" || res.Listener != nil {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.Close == nil {
		t.Fatal("Close must be non-nil")
	}
}

func TestProvisionAutoPrefersCLI(t *testing.T) {
	r := &fakeRunner{responses: map[string]string{"version": "v1.80.0", "status": statusJSON}}
	p := NewProvisioner(NewCLI(r), nil)
	res, err := p.Provision(context.Background(), config.Config{TailscaleMode: "auto"}, "", 5788)
	if err != nil || res.Mode != "cli" {
		t.Fatalf("Provision(auto) = (%+v, %v), want cli mode", res, err)
	}
}

func TestProvisionAutoFallsBackToTSNet(t *testing.T) {
	// No tailscale binary -> the tsnet branch must be reached (factory
	// returns nil, which surfaces as an error before any real node starts).
	r := &fakeRunner{errs: map[string]error{"version": errors.New("not found")}}
	p := NewProvisioner(NewCLI(r), func(hostname, authKey, stateDir string, private bool) *TSNet {
		return nil
	})
	_, err := p.Provision(context.Background(), config.Config{TailscaleMode: "auto"}, "", 5788)
	if err == nil {
		t.Fatal("Provision(auto) should reach the tsnet branch")
	}
	if !errors.Is(err, errNilTSNet) {
		t.Fatalf("error = %v, want errNilTSNet", err)
	}
}

func TestProvisionTSNetFactoryArgs(t *testing.T) {
	// Verify the tsnet branch passes cfg fields through to the factory.
	r := &fakeRunner{errs: map[string]error{"version": errors.New("not found")}}
	got := make(chan struct{})
	factory := func(hostname, authKey, stateDir string, private bool) *TSNet {
		defer close(got)
		if hostname != "my-gw" || authKey != "tskey-x" || private {
			t.Errorf("factory args = (%q, %q, %q, %v)", hostname, authKey, stateDir, private)
		}
		return nil
	}
	p := NewProvisioner(NewCLI(r), factory)
	stateDir := filepath.Join(t.TempDir(), "tsnet")
	_, _ = p.Provision(context.Background(), config.Config{
		TailscaleMode:     "tsnet",
		TailscaleHostname: "my-gw",
		TailscaleAuthKey:  "tskey-x",
		StateDBPath:       filepath.Join(filepath.Dir(stateDir), "state.db"),
	}, "127.0.0.1:5788", 5788)
	select {
	case <-got:
	default:
		t.Fatal("factory was not called in tsnet mode")
	}
}

func TestProvisionCLIModeNotInstalled(t *testing.T) {
	r := &fakeRunner{errs: map[string]error{"version": errors.New("not found")}}
	p := NewProvisioner(NewCLI(r), nil)
	_, err := p.Provision(context.Background(), config.Config{TailscaleMode: "cli"}, "", 5788)
	if err == nil {
		t.Fatal("Provision(cli) with no binary: want error")
	}
}

func TestProvisionAutoFallsBackToTSNetWhenCLISignedOut(t *testing.T) {
	// CLI installed but signed out = no tailnet configured yet; auto must
	// fall through to the embedded node rather than stopping.
	r := &fakeRunner{errs: map[string]error{"status": errors.New("Logged out")}}
	p := NewProvisioner(NewCLI(r), func(hostname, authKey, stateDir string, private bool) *TSNet {
		return nil
	})
	_, err := p.Provision(context.Background(), config.Config{TailscaleMode: "auto"}, "", 5788)
	if !errors.Is(err, errNilTSNet) {
		t.Fatalf("Provision(auto) error = %v, want errNilTSNet (tsnet branch reached)", err)
	}
}

func TestProvisionCLIModeNotSignedIn(t *testing.T) {
	// Explicit cli mode must not silently switch providers: report the
	// missing login instead.
	r := &fakeRunner{errs: map[string]error{"status": errors.New("Logged out")}}
	p := NewProvisioner(NewCLI(r), nil)
	_, err := p.Provision(context.Background(), config.Config{TailscaleMode: "cli"}, "", 5788)
	if err == nil {
		t.Fatal("Provision(cli) with signed-out CLI: want error")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("error = %v, want mention of login", err)
	}
}

func TestFunnelSetupHintHTTPS(t *testing.T) {
	err := errors.New("tsnet funnel: Funnel not available; HTTPS must be enabled. See https://tailscale.com/s/https.")
	hint := FunnelSetupHint(err)
	if !strings.Contains(hint, "admin/dns") {
		t.Fatalf("hint = %q, want the admin/dns step", hint)
	}
	if !strings.Contains(hint, "automatically") {
		t.Fatalf("hint = %q, want reassurance of automatic pickup", hint)
	}
}

func TestFunnelSetupHintNodeAttr(t *testing.T) {
	err := errors.New(`tsnet funnel: Funnel not available; "funnel" node attribute not set. See https://tailscale.com/s/no-funnel.`)
	hint := FunnelSetupHint(err)
	if !strings.Contains(hint, "admin/acls") {
		t.Fatalf("hint = %q, want the admin/acls step", hint)
	}
}

func TestFunnelSetupHintPort(t *testing.T) {
	err := errors.New("port 443 is not allowed for funnel")
	hint := FunnelSetupHint(err)
	if !strings.Contains(hint, "443") {
		t.Fatalf("hint = %q, want mention of port 443", hint)
	}
}

func TestFunnelSetupHintUnrelated(t *testing.T) {
	if hint := FunnelSetupHint(errors.New("tailscale CLI not found")); hint != "" {
		t.Fatalf("hint = %q, want empty for unrelated errors", hint)
	}
}

func TestFunnelSetupHintNil(t *testing.T) {
	if hint := FunnelSetupHint(nil); hint != "" {
		t.Fatalf("hint = %q, want empty for nil", hint)
	}
}

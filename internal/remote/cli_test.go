package remote

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/config"
)

type fakeRunner struct {
	responses map[string]string // key: first arg; value: output
	errs      map[string]error
}

func (f *fakeRunner) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	key := ""
	if len(args) > 0 {
		key = args[0]
	}
	if err := f.errs[key]; err != nil {
		return nil, err
	}
	return []byte(f.responses[key]), nil
}

const statusJSON = `{"Self":{"DNSName":"mymachine.tail1234.ts.net"}}`

func TestDetectFoundAndLoggedIn(t *testing.T) {
	r := &fakeRunner{responses: map[string]string{"version": "v1.80.0", "status": "Logged out."}}
	c := NewCLI(r)
	found, loggedIn, err := c.Detect(context.Background())
	if err != nil || !found || !loggedIn {
		t.Fatalf("Detect = (%v, %v, %v), want (true, true, nil)", found, loggedIn, err)
	}
}

func TestDetectNotFound(t *testing.T) {
	r := &fakeRunner{errs: map[string]error{"version": errors.New("executable file not found")}}
	c := NewCLI(r)
	found, loggedIn, err := c.Detect(context.Background())
	if err != nil || found || loggedIn {
		t.Fatalf("Detect = (%v, %v, %v), want (false, false, nil)", found, loggedIn, err)
	}
}

func TestDetectNotLoggedIn(t *testing.T) {
	r := &fakeRunner{errs: map[string]error{"status": errors.New("Logged out")}}
	c := NewCLI(r)
	found, loggedIn, err := c.Detect(context.Background())
	if err != nil || !found || loggedIn {
		t.Fatalf("Detect = (%v, %v, %v), want (true, false, nil)", found, loggedIn, err)
	}
}

func TestFunnelURL(t *testing.T) {
	r := &fakeRunner{responses: map[string]string{"status": statusJSON}}
	c := NewCLI(r)
	url, err := c.FunnelURL(context.Background())
	if err != nil || url != "https://mymachine.tail1234.ts.net" {
		t.Fatalf("FunnelURL = (%q, %v)", url, err)
	}
}

func TestFunnelURLEmptyDNSName(t *testing.T) {
	r := &fakeRunner{responses: map[string]string{"status": `{"Self":{}}`}}
	c := NewCLI(r)
	if _, err := c.FunnelURL(context.Background()); err == nil {
		t.Fatal("FunnelURL: want error for empty DNSName")
	}
}

func TestEnableRemote(t *testing.T) {
	r := &fakeRunner{responses: map[string]string{"serve": "{}"}}
	c := NewCLI(r)
	if err := c.EnableRemote(context.Background(), 5788, false); err != nil {
		t.Fatalf("EnableRemote: %v", err)
	}
}

func TestEnableRemoteErrorHintsInteractiveRun(t *testing.T) {
	r := &fakeRunner{responses: map[string]string{"serve": "{}"}, errs: map[string]error{"funnel": errors.New("consent required")}}
	c := NewCLI(r)
	err := c.EnableRemote(context.Background(), 5788, false)
	if err == nil {
		t.Fatal("EnableRemote: want error")
	}
	if got := err.Error(); !strings.Contains(got, "tailscale funnel 5788") {
		t.Fatalf("error %q should hint the interactive command", got)
	}
}

type tunnelRunner struct {
	config  string
	command string
}

func (r *tunnelRunner) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	if strings.Join(args, " ") == "serve status --json" {
		return []byte(r.config), nil
	}
	if args[0] == "version" {
		return []byte("1.102.2"), nil
	}
	if args[0] == "status" {
		return []byte(statusJSON), nil
	}
	r.command = strings.Join(args, " ")
	return nil, nil
}

func TestPrivateProvisionUsesServe(t *testing.T) {
	r := &tunnelRunner{config: "{}"}
	p := NewProvisioner(NewCLI(r), nil)
	_, err := p.Provision(context.Background(), config.Config{TailscaleMode: "cli", TailscalePrivate: true}, "", 5788)
	if err != nil {
		t.Fatal(err)
	}
	if r.command != "serve --bg 5788" {
		t.Fatalf("command=%q, want tailnet-only Serve", r.command)
	}
}

func TestProvisionRefusesAnotherOriginOnHostEndpoint(t *testing.T) {
	for _, proxy := range []string{"http://127.0.0.1:6000", "http://localhost:6000"} {
		t.Run(proxy, func(t *testing.T) {
			r := &tunnelRunner{config: fmt.Sprintf(`{"TCP":{"443":{"HTTPS":true}},"Web":{"mymachine.tail1234.ts.net:443":{"Handlers":{"/":{"Proxy":%q}}}}}`, proxy)}
			p := NewProvisioner(NewCLI(r), nil)
			_, err := p.Provision(context.Background(), config.Config{TailscaleMode: "cli"}, "", 5788)
			if err == nil {
				t.Fatal("provision succeeded over another origin")
			}
			if r.command != "" {
				t.Fatalf("conflict mutated host config: %s", r.command)
			}
		})
	}
}

type pendingTunnelRunner struct {
	tunnelRunner
	entered chan struct{}
	release chan struct{}
}

func (r *pendingTunnelRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if args[0] == "funnel" {
		close(r.entered)
		<-r.release
	}
	return r.tunnelRunner.Run(ctx, name, args...)
}

func TestConcurrentProvisionCannotOverwriteHostEndpoint(t *testing.T) {
	r := &pendingTunnelRunner{tunnelRunner: tunnelRunner{config: "{}"}, entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- NewCLI(r).EnableRemote(context.Background(), 5788, false) }()
	<-r.entered
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, t.TempDir())
	}
	second := &tunnelRunner{config: "{}"}
	err := NewCLI(second).EnableRemote(context.Background(), 6000, false)
	close(r.release)
	firstErr := <-done
	if firstErr != nil {
		t.Fatal(firstErr)
	}
	if err == nil {
		t.Fatal("concurrent gateway changed host endpoint during provisioning")
	}
	if second.command != "" {
		t.Fatal("concurrent gateway mutated host config")
	}
}

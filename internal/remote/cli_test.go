package remote

import (
	"context"
	"errors"
	"strings"
	"testing"
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

func TestEnableFunnel(t *testing.T) {
	r := &fakeRunner{responses: map[string]string{}}
	c := NewCLI(r)
	if err := c.EnableFunnel(context.Background(), 5788); err != nil {
		t.Fatalf("EnableFunnel: %v", err)
	}
}

func TestEnableFunnelErrorHintsInteractiveRun(t *testing.T) {
	r := &fakeRunner{errs: map[string]error{"funnel": errors.New("consent required")}}
	c := NewCLI(r)
	err := c.EnableFunnel(context.Background(), 5788)
	if err == nil {
		t.Fatal("EnableFunnel: want error")
	}
	if got := err.Error(); !strings.Contains(got, "tailscale funnel 5788") {
		t.Fatalf("error %q should hint the interactive command", got)
	}
}

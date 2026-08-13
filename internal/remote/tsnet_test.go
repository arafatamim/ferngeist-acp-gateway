package remote

import (
	"testing"
)

func TestFunnelURLFromFQDN(t *testing.T) {
	got := funnelURL("gw.tail1234.ts.net")
	if got != "https://gw.tail1234.ts.net" {
		t.Fatalf("funnelURL = %q", got)
	}
}

func TestFunnelURLStripsTrailingDot(t *testing.T) {
	got := funnelURL("gw.tail1234.ts.net.")
	if got != "https://gw.tail1234.ts.net" {
		t.Fatalf("funnelURL = %q", got)
	}
}

func TestFunnelURLStripsWhitespace(t *testing.T) {
	got := funnelURL("  gw.tail1234.ts.net  ")
	if got != "https://gw.tail1234.ts.net" {
		t.Fatalf("funnelURL = %q", got)
	}
}

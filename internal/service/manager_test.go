package service

import (
	"strings"
	"testing"
)

func TestNormalizeInstallOptionsTailscaleMode(t *testing.T) {
	got := NormalizeInstallOptions(InstallOptions{TailscaleMode: "auto"})
	if got.TailscaleMode != "auto" {
		t.Fatalf("TailscaleMode = %q, want %q", got.TailscaleMode, "auto")
	}
}

func TestNormalizeInstallOptionsTrimsTailscaleMode(t *testing.T) {
	got := NormalizeInstallOptions(InstallOptions{TailscaleMode: "  tsnet  "})
	if got.TailscaleMode != "tsnet" {
		t.Fatalf("TailscaleMode = %q, want %q", got.TailscaleMode, "tsnet")
	}
}

func TestValidateInstallOptionsAcceptsTailscaleModes(t *testing.T) {
	for _, mode := range []string{"", "off", "auto", "cli", "tsnet"} {
		err := ValidateInstallOptions(InstallOptions{Host: "127.0.0.1", TailscaleMode: mode})
		if err != nil {
			t.Fatalf("Validate(%q) = %v, want nil", mode, err)
		}
	}
}

func TestValidateInstallOptionsRejectsBadTailscaleMode(t *testing.T) {
	err := ValidateInstallOptions(InstallOptions{Host: "127.0.0.1", TailscaleMode: "bogus"})
	if err == nil {
		t.Fatal("want error for invalid TailscaleMode")
	}
	if !strings.Contains(err.Error(), "tailscale mode") {
		t.Fatalf("error = %v, want mention of tailscale mode", err)
	}
}

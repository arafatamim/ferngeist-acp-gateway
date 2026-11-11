package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReinstallKeepsCapturedPathWhileUpdatingSettings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", "/user tools/bin:/line\nbreak:/quote\"/bin")
	if err := saveInstallOptions(dir, InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "/usr/bin:/bin")
	if err := saveInstallOptions(dir, InstallOptions{Port: 9000}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dir, "install-options.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Path string
		Port int
	}
	if err := json.Unmarshal(content, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Path != "/user tools/bin:/line\nbreak:/quote\"/bin" || saved.Port != 9000 {
		t.Fatalf("reinstalled settings = %+v", saved)
	}
}

func TestReinstallDoesNotOverwriteMalformedSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "install-options.json")
	const broken = "{"
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveInstallOptions(dir, InstallOptions{}); err == nil {
		t.Fatal("reinstall overwrote undecodable settings")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != broken {
		t.Fatalf("saved settings changed after failure: %s", content)
	}
}

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

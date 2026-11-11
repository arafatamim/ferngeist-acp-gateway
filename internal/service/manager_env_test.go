//go:build linux && !android

package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxServiceEnvironmentCapturesInstallerPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{"special characters", `/custom tools/bin:/quote"/bin:/back\slash/bin:/$HOME/bin:/100%/bin`, `PATH="/custom tools/bin:/quote\"/bin:/back\\slash/bin:/$HOME/bin:/100%/bin"`},
		{"empty keeps manager default", "", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("PATH", test.path)
			paths := linuxPaths{envPath: filepath.Join(t.TempDir(), "daemon.env")}
			if err := writeLinuxEnvFile(paths, InstallOptions{}); err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(paths.envPath)
			if err != nil {
				t.Fatal(err)
			}
			var got string
			if _, value, found := strings.Cut(string(content), "\nPATH="); found {
				got = "PATH=" + strings.SplitN(value, "\n", 2)[0]
			}
			if got != test.want {
				t.Fatalf("persisted PATH = %q, want %q", got, test.want)
			}
		})
	}
}

func TestWriteLinuxEnvFileDropsPublicURLForLanOnly(t *testing.T) {
	paths := linuxPaths{envPath: filepath.Join(t.TempDir(), "daemon.env")}

	err := writeLinuxEnvFile(paths, InstallOptions{
		Host:      "0.0.0.0",
		Port:      5788,
		PublicURL: "https://stale.tail1234.ts.net",
	})
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(paths.envPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "PUBLIC_BASE_URL") {
		t.Fatalf("env file must not contain a stale public URL in LAN-only mode:\n%s", content)
	}
}

func TestWriteLinuxEnvFileKeepsPublicURLWithRemote(t *testing.T) {
	// --remote (TailscaleMode=auto) must keep the public URL even on a LAN
	// host: the operator explicitly asked for remote access.
	paths := linuxPaths{envPath: filepath.Join(t.TempDir(), "daemon.env")}

	err := writeLinuxEnvFile(paths, InstallOptions{
		Host:          "0.0.0.0",
		Port:          5788,
		PublicURL:     "https://gw.tail1234.ts.net",
		TailscaleMode: "auto",
	})
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(paths.envPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "FERNGEIST_GATEWAY_PUBLIC_BASE_URL=https://gw.tail1234.ts.net") {
		t.Fatalf("env file must keep the public URL with remote mode:\n%s", content)
	}
}

func TestWriteLinuxEnvFileKeepsExplicitPublicURLOnLocalhost(t *testing.T) {
	paths := linuxPaths{envPath: filepath.Join(t.TempDir(), "daemon.env")}

	err := writeLinuxEnvFile(paths, InstallOptions{
		Host:      "127.0.0.1",
		Port:      5788,
		PublicURL: "https://gw.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(paths.envPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "FERNGEIST_GATEWAY_PUBLIC_BASE_URL=https://gw.example.com") {
		t.Fatalf("env file must keep an explicit public URL on a loopback host:\n%s", content)
	}
}

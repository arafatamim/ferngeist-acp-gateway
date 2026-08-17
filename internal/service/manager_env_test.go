//go:build linux && !android

package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteLinuxEnvFileDropsPublicURLForLanOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix env file only")
	}
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
	if runtime.GOOS == "windows" {
		t.Skip("posix env file only")
	}
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
	if runtime.GOOS == "windows" {
		t.Skip("posix env file only")
	}
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

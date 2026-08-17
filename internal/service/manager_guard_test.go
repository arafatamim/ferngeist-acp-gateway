package service

import "testing"

// includePublicURL is platform-neutral and shared by all three service env
// writers (linux/darwin/windows). These tests run on every OS, so a guard
// regression can never hide behind a per-OS build tag again.
func TestIncludePublicURL(t *testing.T) {
	cases := []struct {
		name string
		opts InstallOptions
		want bool
	}{
		// LAN-only (non-loopback host, no remote): stale persisted URL dropped.
		{"lan host, no mode", InstallOptions{Host: "0.0.0.0", PublicURL: "https://stale.tail.ts.net"}, false},
		{"lan host, off mode", InstallOptions{Host: "0.0.0.0", TailscaleMode: "off", PublicURL: "https://stale.tail.ts.net"}, false},
		// Loopback install (reverse-proxy setup): explicit URL kept.
		{"localhost, no mode", InstallOptions{Host: "127.0.0.1", PublicURL: "https://gw.example.com"}, true},
		{"loopback, off mode", InstallOptions{Host: "127.0.0.1", TailscaleMode: "off", PublicURL: "https://gw.example.com"}, true},
		// Any valid remote mode: URL kept, even on a LAN host.
		{"lan host, auto", InstallOptions{Host: "0.0.0.0", TailscaleMode: "auto", PublicURL: "https://gw.tail.ts.net"}, true},
		{"lan host, cli", InstallOptions{Host: "0.0.0.0", TailscaleMode: "cli", PublicURL: "https://gw.tail.ts.net"}, true},
		{"lan host, tsnet", InstallOptions{Host: "0.0.0.0", TailscaleMode: "tsnet", PublicURL: "https://gw.tail.ts.net"}, true},
		{"localhost, auto", InstallOptions{Host: "127.0.0.1", TailscaleMode: "auto", PublicURL: "https://gw.tail.ts.net"}, true},
		// No URL at all.
		{"no url", InstallOptions{Host: "0.0.0.0"}, false},
		{"no url, remote", InstallOptions{Host: "0.0.0.0", TailscaleMode: "auto"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := includePublicURL(tc.opts)
			if got != tc.want {
				t.Fatalf("includePublicURL(%+v) = %v, want %v", tc.opts, got, tc.want)
			}
		})
	}
}

func TestRemoteModeRequested(t *testing.T) {
	for mode, want := range map[string]bool{
		"":      false,
		"off":   false,
		"auto":  true,
		"cli":   true,
		"tsnet": true,
	} {
		if got := remoteModeRequested(mode); got != want {
			t.Fatalf("remoteModeRequested(%q) = %v, want %v", mode, got, want)
		}
	}
}

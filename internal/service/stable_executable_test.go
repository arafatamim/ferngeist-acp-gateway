//go:build !windows

package service

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStableExecutable(t *testing.T) {
	prefix := t.TempDir()
	keg := filepath.Join(prefix, "Cellar", "ferngeist-gateway", "1.2.3", "bin", "ferngeist-gateway")
	otherKeg := filepath.Join(prefix, "Cellar", "ferngeist-gateway", "1.2.2", "bin", "ferngeist-gateway")
	for _, p := range []string{keg, otherKeg} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("bin"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	opt := filepath.Join(prefix, "opt", "ferngeist-gateway")
	if err := os.MkdirAll(filepath.Dir(opt), 0o755); err != nil {
		t.Fatal(err)
	}
	// Homebrew links opt/<name> to the current keg.
	if err := os.Symlink(filepath.Join(prefix, "Cellar", "ferngeist-gateway", "1.2.3"), opt); err != nil {
		t.Fatal(err)
	}
	optBin := filepath.Join(opt, "bin", "ferngeist-gateway")

	cases := []struct {
		name, resolved, want string
	}{
		{"distro package", "/usr/bin/ferngeist-gateway", "/usr/bin/ferngeist-gateway"},
		{"current brew keg", keg, optBin},
		{"keg opt no longer points at", otherKeg, ""},
		{"binary channel copy", "/home/u/.local/share/ferngeist-gateway/bin/ferngeist-gateway", ""},
		{"other name in /usr/bin", "/usr/bin/ferngeist", ""},
		{"temp build", filepath.Join(t.TempDir(), "ferngeist-gateway"), ""},
	}
	for _, c := range cases {
		got, ok := stableExecutable(c.resolved)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("%s: stableExecutable(%q) = %q, %v; want %q", c.name, c.resolved, got, ok, c.want)
		}
	}
}

package acquire

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/catalog"
)

const fakeSpec = "@acme/agent@1.2.3"

// fakeNpmAgent is the launch the catalog plans for a registry npm agent.
func fakeNpmAgent(spec string) catalog.Agent {
	return catalog.Agent{
		ID:       "acme",
		Launch:   catalog.LaunchConfig{Mode: "external", Command: "npm", Args: []string{"exec", "--yes", spec, "--", "--acp"}},
		Registry: catalog.RegistryInfo{NpxPackage: spec, NpxArgs: []string{"--acp"}},
	}
}

// fakePath puts stub executables on PATH. The npm stub answers
// `npm install --prefix DIR ... SPEC` by laying out @acme/agent with one bin.
func fakePath(t *testing.T, names ...string) string {
	t.Helper()
	if goruntime.GOOS == "windows" {
		t.Skip("shell stubs")
	}
	bin := t.TempDir()
	stubs := map[string]string{
		"npm": `#!/bin/sh
pkg="$3/node_modules/@acme/agent"
command -p mkdir -p "$pkg/dist" && printf '{"bin":{"agent":"dist/cli.js"}}' > "$pkg/package.json" && : > "$pkg/dist/cli.js"
`,
		"bun":  "#!/bin/sh\n",
		"node": "#!/bin/sh\n",
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(stubs[name]), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	return bin
}

func newNpmInstaller(t *testing.T, mode string) *Installer {
	i := New(slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir(), nil)
	i.SetJSRuntime(mode)
	return i
}

// waitNpmInstalls blocks until no background install is running.
func waitNpmInstalls(t *testing.T, i *Installer) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		i.mu.Lock()
		n := len(i.npmInstalling)
		i.mu.Unlock()
		if n == 0 {
			return
		}
	}
	t.Fatal("npm install did not finish")
}

func TestEnsureNpmInstallsThenLaunchesDirectly(t *testing.T) {
	cases := []struct {
		mode, want string
		path       []string
	}{
		{JSRuntimeAuto, "bun", []string{"npm", "bun", "node"}},
		{JSRuntimeAuto, "node", []string{"npm", "node"}},
		{JSRuntimeNode, "node", []string{"npm", "bun", "node"}},
		{JSRuntimeBun, "bun", []string{"npm", "bun", "node"}},
	}
	for _, tc := range cases {
		t.Run(tc.mode+"-"+tc.want, func(t *testing.T) {
			bin := fakePath(t, tc.path...)
			i := newNpmInstaller(t, tc.mode)
			agent := fakeNpmAgent(fakeSpec)

			// First launch: still npm exec while the install runs.
			if got := i.EnsureNpm(agent); got.Launch.Command != "npm" {
				t.Fatalf("first launch = %q, want npm exec until installed", got.Launch.Command)
			}
			waitNpmInstalls(t, i)

			got := i.EnsureNpm(agent)
			script := filepath.Join(i.managedDir, "npm", "@acme+agent@1.2.3", "node_modules", "@acme", "agent", "dist", "cli.js")
			if got.Launch.Command != filepath.Join(bin, tc.want) || !slices.Equal(got.Launch.Args, []string{script, "--acp"}) {
				t.Fatalf("launch = %q %q, want %s %q", got.Launch.Command, got.Launch.Args, tc.want, []string{script, "--acp"})
			}
		})
	}
}

func TestEnsureNpmKeepsNpmExec(t *testing.T) {
	cases := map[string]struct {
		mode, spec string
		path       []string
	}{
		"npm mode":       {JSRuntimeNpm, fakeSpec, []string{"npm", "bun", "node"}},
		"unpinned spec":  {JSRuntimeAuto, "@acme/agent", []string{"npm", "bun", "node"}},
		"tag spec":       {JSRuntimeAuto, "@acme/agent@latest", []string{"npm", "bun", "node"}},
		"runtime absent": {JSRuntimeBun, fakeSpec, []string{"npm", "node"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fakePath(t, tc.path...)
			i := newNpmInstaller(t, tc.mode)
			agent := fakeNpmAgent(tc.spec)
			if got := i.EnsureNpm(agent); got.Launch.Command != "npm" {
				t.Fatalf("launch = %q, want npm exec", got.Launch.Command)
			}
			waitNpmInstalls(t, i)
			if _, err := os.Stat(filepath.Join(i.managedDir, "npm")); err == nil {
				t.Fatal("installed a package it will not launch")
			}
		})
	}
}

func TestPruneNpmKeepsNewestCompleteInstall(t *testing.T) {
	i := newNpmInstaller(t, JSRuntimeAuto)
	root := filepath.Join(i.managedDir, "npm")
	install := func(name string, complete bool, at time.Time) string {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if complete {
			marker := filepath.Join(dir, npmCompleteMarker)
			if err := os.WriteFile(marker, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(marker, at, at); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	now := time.Now()
	old := install("@acme+agent@1.0.0", true, now.Add(-time.Hour))
	current := install("@acme+agent@1.1.0", true, now)
	partial := install("@acme+agent@1.2.0", false, now)
	other := install("pi-acp@0.0.34", true, now.Add(-time.Hour))

	i.PruneNpm()

	for dir, want := range map[string]bool{old: false, current: true, partial: false, other: true} {
		if _, err := os.Stat(dir); (err == nil) != want {
			t.Errorf("%s kept = %v, want %v", filepath.Base(dir), err == nil, want)
		}
	}
}

func TestEnsureNpmLaunchesNativeBinsDirectly(t *testing.T) {
	bin := fakePath(t, "npm", "node")
	native, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name               string
		symlink, noRuntime bool
	}{
		{name: "direct"},
		{name: "symlink", symlink: true},
		{name: "runtime unavailable", noRuntime: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.noRuntime {
				fakePath(t, "npm")
			}
			i := newNpmInstaller(t, JSRuntimeNode)
			dir := filepath.Join(i.managedDir, "npm", "@acme+agent@1.2.3")
			pkg := filepath.Join(dir, "node_modules", "@acme", "agent")
			if err := os.MkdirAll(pkg, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"bin":"agent"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, npmCompleteMarker), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(pkg, "agent")
			target := path
			if tc.symlink {
				target = filepath.Join(pkg, "agent-native")
			}
			if err := os.WriteFile(target, native, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.symlink {
				if err := os.Symlink("agent-native", path); err != nil {
					t.Fatal(err)
				}
			}
			agent := fakeNpmAgent(fakeSpec)
			agent.Launch.Args[len(agent.Launch.Args)-1] = "-test.run=^$"
			got := i.EnsureNpm(agent)
			if got.Launch.Command != path || !slices.Equal(got.Launch.Args, []string{"-test.run=^$"}) {
				t.Fatalf("launch = %q %q, want native %q (runtime %q)", got.Launch.Command, got.Launch.Args, path, bin)
			}
			out, err := exec.Command(got.Launch.Command, got.Launch.Args...).CombinedOutput()
			if err != nil || !bytes.Contains(out, []byte("PASS")) {
				t.Fatalf("native launch: output %q, error %v", out, err)
			}
		})
	}
}

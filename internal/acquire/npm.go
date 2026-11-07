package acquire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/catalog"
)

// JS runtimes for npm-distributed agents (FERNGEIST_GATEWAY_JS_RUNTIME).
const (
	JSRuntimeAuto = "auto" // bun when it is on PATH, else node
	JSRuntimeBun  = "bun"
	JSRuntimeNode = "node"
	JSRuntimeNpm  = "npm" // keep launching through npm exec
)

// npmInstallTimeout bounds one background `npm install` of an agent package.
const npmInstallTimeout = 10 * time.Minute

// npmCompleteMarker is written into an install dir once `npm install` has
// succeeded, so a dir left behind by an interrupted install is never launched.
const npmCompleteMarker = ".ferngeist-complete"

// SetJSRuntime picks the runtime EnsureNpm launches npm agents with: one of the
// JSRuntime constants. Anything else means JSRuntimeAuto.
func (i *Installer) SetJSRuntime(mode string) {
	i.mu.Lock()
	i.jsRuntime = mode
	i.mu.Unlock()
}

// EnsureNpm rewrites an `npm exec` launch to run the agent's bin from a
// gateway-managed install, using bun or node for JavaScript. The
// npm exec wrapper is a second node process that idles for the agent's whole
// life (~108MB) and adds 0.3-1.2s to every launch.
//
// Until the install exists, EnsureNpm starts it in the background and returns
// agent unchanged, so the first launch still goes through npm exec. Only
// version-pinned packages (as the registry publishes them) are installed: an
// unpinned spec would freeze on whatever version was current at install time.
func (i *Installer) EnsureNpm(agent catalog.Agent) catalog.Agent {
	spec := agent.Registry.NpxPackage
	args := agent.Launch.Args
	sep := slices.Index(args, "--")
	if agent.Launch.Mode != "external" || agent.Launch.Command != "npm" || spec == "" ||
		len(args) < 3 || args[0] != "exec" || sep < 0 || !slices.Contains(args[:sep], spec) {
		return agent
	}
	name, version, ok := splitPinnedNpmSpec(spec)
	if !ok {
		return agent
	}
	i.mu.Lock()
	mode := i.jsRuntime
	i.mu.Unlock()
	if mode == JSRuntimeNpm {
		return agent
	}
	runtimePath := lookupJSRuntime(mode)
	dir := filepath.Join(i.managedDir, "npm", npmDirName(name, version))
	script, err := npmBinScript(dir, name)
	if err != nil {
		if runtimePath != "" {
			i.installNpmInBackground(agent.ID, spec, name, dir)
		}
		return agent
	}
	javascript, err := npmBinIsJavaScript(script)
	if err != nil {
		return agent
	}
	if !javascript {
		agent.Launch.Command = script
		agent.Launch.Args = args[sep+1:]
		return agent
	}
	if runtimePath == "" {
		return agent
	}
	agent.Launch.Command = runtimePath
	agent.Launch.Args = append([]string{script}, args[sep+1:]...)
	return agent
}

// npmBinIsJavaScript follows npm bin symlinks and recognizes executable binaries
// and scripts with non-JavaScript interpreters before choosing a JS runtime.
func npmBinIsJavaScript(name string) (bool, error) {
	file, err := os.Open(name)
	if err != nil {
		return false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("npm bin %q is not a regular file", name)
	}
	buf := make([]byte, 512)
	n, err := io.ReadFull(file, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return false, err
	}
	buf = buf[:n]
	if bytes.Contains(buf, []byte{0}) {
		return false, nil
	}
	if !bytes.HasPrefix(buf, []byte("#!")) {
		return true, nil
	}
	line, _, _ := bytes.Cut(buf, []byte("\n"))
	fields := strings.Fields(string(line[2:]))
	if len(fields) == 0 {
		return false, nil
	}
	if filepath.Base(fields[0]) == "env" {
		fields = fields[1:]
		for len(fields) > 0 && strings.HasPrefix(fields[0], "-") {
			fields = fields[1:]
		}
	}
	if len(fields) == 0 {
		return false, nil
	}
	switch filepath.Base(fields[0]) {
	case "node", "nodejs", "bun":
		return true, nil
	default:
		return false, nil
	}
}

// lookupJSRuntime resolves mode to an executable path, or "" to keep npm exec.
func lookupJSRuntime(mode string) string {
	var candidates []string
	switch mode {
	case JSRuntimeNpm:
		return ""
	case JSRuntimeBun, JSRuntimeNode:
		candidates = []string{mode}
	default:
		candidates = []string{JSRuntimeBun, JSRuntimeNode}
	}
	for _, name := range candidates {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// installNpmInBackground runs one `npm install` of spec into dir unless one is
// already running for it.
func (i *Installer) installNpmInBackground(agentID, spec, name, dir string) {
	i.mu.Lock()
	if i.npmInstalling[dir] {
		i.mu.Unlock()
		return
	}
	if i.npmInstalling == nil {
		i.npmInstalling = make(map[string]bool)
	}
	i.npmInstalling[dir] = true
	i.mu.Unlock()

	go func() {
		defer func() {
			i.mu.Lock()
			delete(i.npmInstalling, dir)
			i.mu.Unlock()
		}()
		if err := installNpmPackage(spec, name, dir); err != nil {
			i.logger.Warn("npm agent install failed; launching through npm exec",
				"agent_id", agentID, "package", spec, "error", err)
			return
		}
		i.logger.Info("installed npm agent", "agent_id", agentID, "package", spec, "dir", dir)
	}()
}

// installNpmPackage installs spec into dir and marks it complete. Lifecycle
// scripts run, as they do under npm exec.
func installNpmPackage(spec, name, dir string) error {
	// A dir without the marker is a crashed earlier attempt; start clean.
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), npmInstallTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "npm", "install", "--prefix", dir,
		"--no-save", "--no-audit", "--no-fund", "--loglevel=error", spec)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("npm install: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if _, err := binScriptIn(dir, name); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, npmCompleteMarker), nil, 0o644)
}

// npmBinScript returns the bin script of a completed install of name in dir.
func npmBinScript(dir, name string) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, npmCompleteMarker)); err != nil {
		return "", err
	}
	return binScriptIn(dir, name)
}

// binScriptIn picks the script npm exec would run for name: its only bin, or
// the bin named after the package.
func binScriptIn(dir, name string) (string, error) {
	pkgDir := filepath.Join(dir, "node_modules", filepath.FromSlash(name))
	raw, err := os.ReadFile(filepath.Join(pkgDir, "package.json"))
	if err != nil {
		return "", err
	}
	var manifest struct {
		Bin json.RawMessage `json:"bin"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return "", err
	}
	var rel string
	var bins map[string]string
	if json.Unmarshal(manifest.Bin, &rel) != nil {
		if err := json.Unmarshal(manifest.Bin, &bins); err != nil {
			return "", fmt.Errorf("%s has no bin", name)
		}
		if len(bins) == 1 {
			for _, v := range bins {
				rel = v
			}
		} else {
			rel = bins[path.Base(name)]
		}
	}
	if rel == "" {
		return "", errors.New("no bin matches " + name)
	}
	script := filepath.Join(pkgDir, filepath.FromSlash(rel))
	if _, err := os.Stat(script); err != nil {
		return "", err
	}
	return script, nil
}

// splitPinnedNpmSpec splits "@scope/pkg@1.2.3" into name and exact version. A
// spec without a version, or with a tag or range, is not pinned.
func splitPinnedNpmSpec(spec string) (name, version string, ok bool) {
	at := strings.LastIndex(spec, "@")
	if at <= 0 {
		return "", "", false
	}
	name, version = spec[:at], spec[at+1:]
	if version == "" || version[0] < '0' || version[0] > '9' || strings.ContainsAny(version, "^~<>=*| xX") {
		return "", "", false
	}
	return name, version, true
}

// npmDirName is the install dir for one package version: "@scope+pkg@1.2.3".
func npmDirName(name, version string) string {
	return strings.ReplaceAll(name, "/", "+") + "@" + version
}

// PruneNpm removes installs of npm agent versions the registry has moved past,
// keeping the most recently completed install of each package. Run it before
// any agent launches: a running agent may still load files from its dir.
func (i *Installer) PruneNpm() {
	root := filepath.Join(i.managedDir, "npm")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	type install struct {
		dir  string
		done time.Time
	}
	newest := map[string]install{}
	var stale []string
	for _, e := range entries {
		at := strings.LastIndex(e.Name(), "@")
		if !e.IsDir() || at <= 0 {
			continue
		}
		dir := filepath.Join(root, e.Name())
		info, err := os.Stat(filepath.Join(dir, npmCompleteMarker))
		if err != nil {
			stale = append(stale, dir) // interrupted install
			continue
		}
		pkg := e.Name()[:at]
		if prev, ok := newest[pkg]; !ok || info.ModTime().After(prev.done) {
			if ok {
				stale = append(stale, prev.dir)
			}
			newest[pkg] = install{dir, info.ModTime()}
		} else {
			stale = append(stale, dir)
		}
	}
	for _, dir := range stale {
		if err := os.RemoveAll(dir); err != nil {
			i.logger.Warn("prune npm agent install failed", "dir", dir, "error", err)
		}
	}
}

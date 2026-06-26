package catalog

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"time"
	"unicode"

	acpregistry "github.com/arafatamim/ferngeist-acp-gateway/internal/registry"
)

var ErrAgentNotFound = errors.New("agent not found")

const mockAgentExecutablePlaceholder = "__MOCK_AGENT_EXECUTABLE__"

//go:embed manifests/*.json
var embeddedManifestFS embed.FS

// Agent is one gateway-visible ACP runtime entry. Entries may come from bundled
// gateway-owned manifests (such as the mock runtime) or be synthesized directly
// from the ACP registry when the registry provides a supported launch method.
type Agent struct {
	ID              string            `json:"id"`
	DisplayName     string            `json:"displayName"`
	Protocol        string            `json:"protocol"`
	PlatformSupport []string          `json:"platformSupport,omitempty"`
	Detection       DetectionConfig   `json:"detection"`
	Launch          LaunchConfig      `json:"launch"`
	HealthCheck     HealthCheckConfig `json:"healthCheck"`
	Capabilities    []string          `json:"capabilities,omitempty"`
	Security        SecurityConfig    `json:"security"`
	Registry        RegistryInfo      `json:"registry"`
	Detected        bool              `json:"detected"`
	ManifestValid   bool              `json:"manifestValid"`
	ValidationError string            `json:"validationError,omitempty"`
	Hint            string            `json:"hint,omitempty"`
}

type DetectionConfig struct {
	Mode    string `json:"mode"`
	Command string `json:"command,omitempty"`
	Path    string `json:"path,omitempty"`
}

type LaunchConfig struct {
	Mode      string          `json:"mode"`
	Command   string          `json:"command"`
	Args      []string        `json:"args"`
	Transport string          `json:"transport,omitempty"`
	Readiness ReadinessConfig `json:"readiness"`
	Restart   RestartConfig   `json:"restart"`
}

type ReadinessConfig struct {
	Mode           string `json:"mode"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
}

type RestartConfig struct {
	Mode           string `json:"mode"`
	MaxRetries     int    `json:"maxRetries,omitempty"`
	BackoffSeconds int    `json:"backoffSeconds,omitempty"`
}

type HealthCheckConfig struct {
	Mode           string `json:"mode"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
}

type SecurityConfig struct {
	CuratedLaunch          bool     `json:"curatedLaunch"`
	AllowsRemoteStart      bool     `json:"allowsRemoteStart"`
	UserConfigurableFields []string `json:"userConfigurableFields,omitempty"`
}

type RegistryInfo struct {
	Required                bool     `json:"required"`
	ValidationStatus        string   `json:"validationStatus"`
	Name                    string   `json:"name,omitempty"`
	Version                 string   `json:"version,omitempty"`
	Repository              string   `json:"repository,omitempty"`
	Website                 string   `json:"website,omitempty"`
	DistributionKinds       []string `json:"distributionKinds,omitempty"`
	CurrentBinaryPath       string   `json:"currentBinaryPath,omitempty"`
	CurrentBinaryArchiveURL string   `json:"currentBinaryArchiveUrl,omitempty"`
	CurrentBinaryCommand    string   `json:"currentBinaryCommand,omitempty"`
	CurrentBinaryArgs       []string `json:"currentBinaryArgs,omitempty"`
	NpxPackage              string   `json:"npxPackage,omitempty"`
	NpxArgs                 []string `json:"npxArgs,omitempty"`
	UvxPackage              string   `json:"uvxPackage,omitempty"`
	UvxArgs                 []string `json:"uvxArgs,omitempty"`
}

// registryLaunchPlan is the catalog's single source of truth for how an ACP
// registry entry should run on the current host. The gateway synthesizes this
// once from the raw registry record and then carries the chosen launch through
// the rest of the catalog pipeline via Agent.
type registryLaunchPlan struct {
	Launch    LaunchConfig
	Detection DetectionConfig
}

type RegistrySource interface {
	Snapshot(ctx context.Context) (acpregistry.Snapshot, error)
}

type Service struct {
	agents             []Agent
	adapters           []Agent
	baseDir            string
	registry           RegistrySource
	resolveNpmBinaries func(string) []string
}

// New returns the default gateway catalog rooted at the current working
// directory.
func New() *Service {
	return NewWithBaseDirAndRegistry(".", nil)
}

func NewWithBaseDir(baseDir string) *Service {
	return NewWithBaseDirAndRegistry(baseDir, nil)
}

func NewWithBaseDirAndRegistry(baseDir string, registrySource RegistrySource) *Service {
	agents, err := loadEmbeddedAgents()
	if err != nil {
		panic(fmt.Errorf("load embedded catalog manifests: %w", err))
	}
	service := &Service{
		adapters: agents,
		baseDir:  baseDir,
		registry: registrySource,
	}
	service.Refresh()
	return service
}

// SetNpmResolver sets the function used to resolve binary names from npm
// package metadata. Called by the daemon after construction; tests that
// construct Service directly or through daemon.Run() skip npm lookups.
func (s *Service) SetNpmResolver(resolve func(string) []string) {
	s.resolveNpmBinaries = resolve
}

func loadEmbeddedAgents() ([]Agent, error) {
	entries, err := fs.ReadDir(embeddedManifestFS, "manifests")
	if err != nil {
		return nil, err
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	agents := make([]Agent, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		path := "manifests/" + entry.Name()
		data, err := embeddedManifestFS.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}

		var agent Agent
		if err := json.Unmarshal(data, &agent); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}

		normalizeManifestPlaceholders(&agent)
		agents = append(agents, agent)
	}
	return agents, nil
}

func normalizeManifestPlaceholders(agent *Agent) {
	if agent == nil {
		return
	}
	mockName := mockAgentExecutableName()
	agent.Detection.Path = strings.ReplaceAll(agent.Detection.Path, mockAgentExecutablePlaceholder, mockName)
	agent.Launch.Command = strings.ReplaceAll(agent.Launch.Command, mockAgentExecutablePlaceholder, mockName)
}

func (s *Service) List() []Agent {
	s.Refresh()

	out := make([]Agent, len(s.agents))
	copy(out, s.agents)
	return out
}

func (s *Service) Get(id string) (Agent, error) {
	s.Refresh()

	for _, agent := range s.agents {
		if agent.ID == id {
			// If the agent would be launched via npm exec and we have a
			// metadata resolver, try to find a locally-installed binary
			// whose name differs from the registry ID (e.g., @github/copilot
			// installs binary "copilot"). This avoids an unnecessary npm
			// install when the binary is already on PATH.
			if s.resolveNpmBinaries != nil && agent.Launch.Command == "npm" && agent.Registry.NpxPackage != "" {
				for _, name := range s.resolveNpmBinaries(agent.Registry.NpxPackage) {
					if _, err := exec.LookPath(name); err == nil {
						agent.Launch.Command = name
						agent.Launch.Args = append([]string(nil), agent.Registry.NpxArgs...)
						agent.Detection = DetectionConfig{
							Mode:    "path_lookup",
							Command: name,
						}
						break
					}
				}
			}
			return agent, nil
		}
	}
	return Agent{}, ErrAgentNotFound
}

// Refresh revalidates built-in manifests, merges ACP registry metadata, and
// then reruns host detection. The gateway does this eagerly because the catalog
// is small and the API should reflect current host state.
func (s *Service) Refresh() {
	registrySnapshot, registryErr, registryEnabled := s.loadRegistry()
	adapterByID := make(map[string]Agent, len(s.adapters))
	for _, adapter := range s.adapters {
		adapterByID[adapter.ID] = adapter
	}

	capacity := len(s.adapters)
	if len(registrySnapshot.Agents) > capacity {
		capacity = len(registrySnapshot.Agents)
	}
	visible := make([]Agent, 0, capacity)
	seen := make(map[string]struct{})

	if registryEnabled && registryErr == nil {
		ids := sortedRegistryIDs(registrySnapshot.Agents)
		for _, id := range ids {
			entry := registrySnapshot.Agents[id]
			registryAgent := registryEntryToAgent(entry, nil)
			agent := registryAgent
			if adapter, ok := adapterByID[id]; ok {
				validationError := validateAgent(adapter)
				agent.ManifestValid = validationError == nil
				agent.ValidationError = validationErrorString(validationError)
				if validationError == nil {
					agent = mergeAdapter(registryAgent, adapter)
					agent, agent.Registry = applyRegistryInfo(agent, agent.Registry, registrySnapshot, nil, true, agent.ID, nil)
				}
			} else {
				agent.ManifestValid = true
			}
			agent.Detected = s.detect(agent)
			visible = append(visible, agent)
			seen[id] = struct{}{}
		}
	}

	for _, adapter := range s.adapters {
		if _, ok := seen[adapter.ID]; ok {
			continue
		}

		validationError := validateAgent(adapter)
		agent := adapter
		agent, agent.Registry = applyRegistryInfo(agent, agent.Registry, registrySnapshot, registryErr, registryEnabled, agent.ID, nil)
		if validationError == nil && agent.Registry.Required && agent.Registry.ValidationStatus == "missing" {
			validationError = errors.New("agent is not present in ACP registry")
		}
		agent.ManifestValid = validationError == nil
		agent.ValidationError = validationErrorString(validationError)
		agent.Detected = validationError == nil && s.detect(agent)
		visible = append(visible, agent)
	}

	sort.Slice(visible, func(i, j int) bool {
		return visible[i].ID < visible[j].ID
	})
	s.agents = visible
}

func (s *Service) detect(agent Agent) bool {
	if !isPlatformSupported(agent.PlatformSupport, goruntime.GOOS) {
		return false
	}

	switch agent.Detection.Mode {
	case "local_file":
		path := agent.Detection.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(s.baseDir, path)
		}
		info, err := os.Stat(path)
		return err == nil && !info.IsDir()
	case "path_lookup":
		_, err := exec.LookPath(agent.Detection.Command)
		return err == nil
	default:
		return false
	}
}

func detectNpmPackage(pkg string) bool {
	if strings.TrimSpace(pkg) == "" {
		return false
	}
	_, err := exec.LookPath("npm")
	return err == nil
}

func detectUvxPackage(pkg string) bool {
	if strings.TrimSpace(pkg) == "" {
		return false
	}
	_, err := exec.LookPath("uvx")
	return err == nil
}

// validateAgent enforces the "curated launch only" boundary. This is where the
// gateway rejects manifests that would drift into arbitrary command execution or
// unsupported transport combinations.
func validateAgent(agent Agent) error {
	if strings.TrimSpace(agent.ID) == "" {
		return errors.New("agent id is required")
	}
	if strings.TrimSpace(agent.DisplayName) == "" {
		return errors.New("display name is required")
	}
	if strings.TrimSpace(agent.Protocol) == "" {
		return errors.New("protocol is required")
	}
	if agent.Protocol != "acp" {
		return fmt.Errorf("unsupported protocol %q", agent.Protocol)
	}
	if len(agent.PlatformSupport) == 0 {
		return errors.New("platform support is required")
	}

	for _, capability := range agent.Capabilities {
		if strings.TrimSpace(capability) == "" {
			return errors.New("capabilities cannot contain empty values")
		}
	}

	if !agent.Security.CuratedLaunch {
		return errors.New("launch definitions must be curated")
	}

	if err := validateDetection(agent.Detection); err != nil {
		return err
	}
	if err := validateLaunch(agent.Launch); err != nil {
		return err
	}
	if err := validateHealthCheck(agent.HealthCheck, agent.Launch.Transport); err != nil {
		return err
	}
	if err := validateDetectionLaunchPair(agent.Detection, agent.Launch); err != nil {
		return err
	}

	return nil
}

func validateDetection(detection DetectionConfig) error {
	switch detection.Mode {
	case "local_file":
		if strings.TrimSpace(detection.Path) == "" {
			return errors.New("local_file detection requires path")
		}
	case "path_lookup":
		if err := validateExecutableName(detection.Command); err != nil {
			return fmt.Errorf("path_lookup detection requires safe command: %w", err)
		}
	default:
		return fmt.Errorf("unsupported detection mode %q", detection.Mode)
	}
	return nil
}

// validateLaunch keeps launch templates intentionally narrow. The gateway should
// not accept shell fragments or transport-specific fields that do not match the
// declared launch mode.
func validateLaunch(launch LaunchConfig) error {
	switch launch.Mode {
	case "process":
		if strings.TrimSpace(launch.Command) == "" {
			return errors.New("process launch requires command")
		}
		if launch.Transport != "stdio" {
			return fmt.Errorf("unsupported process transport %q", launch.Transport)
		}
		if launch.Readiness.Mode == "" {
			return errors.New("process launch requires readiness mode")
		}
	case "external":
		if err := validateExecutableName(launch.Command); err != nil {
			return fmt.Errorf("external launch requires safe command: %w", err)
		}
		if launch.Transport != "stdio" {
			return fmt.Errorf("unsupported external transport %q", launch.Transport)
		}
		if launch.Readiness.Mode == "" {
			return errors.New("external launch requires readiness mode")
		}
	default:
		return fmt.Errorf("unsupported launch mode %q", launch.Mode)
	}

	for _, arg := range launch.Args {
		if strings.TrimSpace(arg) == "" {
			return errors.New("launch args cannot contain empty values")
		}
		if containsControlChars(arg) {
			return errors.New("launch args cannot contain control characters")
		}
	}

	if containsControlChars(launch.Command) {
		return errors.New("launch command cannot contain control characters")
	}
	if err := validateReadiness(launch.Readiness, launch.Transport); err != nil {
		return err
	}
	if err := validateRestart(launch.Restart, launch.Transport); err != nil {
		return err
	}
	return nil
}

func validateDetectionLaunchPair(detection DetectionConfig, launch LaunchConfig) error {
	switch {
	case detection.Mode == "local_file" && launch.Mode != "process":
		return errors.New("local_file detection must pair with process launch")
	case detection.Mode == "path_lookup" && launch.Mode != "external":
		return errors.New("path_lookup detection must pair with external launch")
	}

	if detection.Mode == "local_file" && filepath.Clean(detection.Path) != filepath.Clean(launch.Command) {
		return errors.New("local_file detection path must match process launch command")
	}
	if detection.Mode == "path_lookup" && detection.Command != launch.Command {
		return errors.New("path_lookup detection command must match external launch command")
	}
	return nil
}

func validateExecutableName(command string) error {
	command = strings.TrimSpace(command)
	if command == "" {
		return errors.New("command is required")
	}
	if strings.Contains(command, "/") || strings.Contains(command, `\`) {
		return errors.New("command must not contain path separators")
	}
	if containsControlChars(command) || strings.ContainsAny(command, " \t") {
		return errors.New("command must be a single executable name")
	}
	return nil
}

func validateReadiness(readiness ReadinessConfig, transport string) error {
	switch transport {
	case "stdio":
		if readiness.Mode != "immediate" {
			return fmt.Errorf("stdio transport requires readiness mode %q", "immediate")
		}
	default:
		return fmt.Errorf("unsupported readiness transport %q", transport)
	}
	if readiness.TimeoutSeconds < 0 {
		return errors.New("readiness timeout must not be negative")
	}
	return nil
}

func validateRestart(restart RestartConfig, transport string) error {
	mode := restart.Mode
	if mode == "" {
		mode = "never"
	}

	switch mode {
	case "never":
	case "on_failure":
		if transport != "stdio" {
			return errors.New("automatic restart is only supported for stdio runtimes")
		}
	default:
		return fmt.Errorf("unsupported restart mode %q", restart.Mode)
	}

	if restart.MaxRetries < 0 {
		return errors.New("restart maxRetries must not be negative")
	}
	if restart.BackoffSeconds < 0 {
		return errors.New("restart backoffSeconds must not be negative")
	}
	return nil
}

func validateHealthCheck(healthCheck HealthCheckConfig, transport string) error {
	mode := healthCheck.Mode
	if mode == "" {
		return errors.New("healthCheck mode is required")
	}

	switch transport {
	case "stdio":
		if mode != "none" {
			return fmt.Errorf("stdio transport requires healthCheck mode %q", "none")
		}
	default:
		return fmt.Errorf("unsupported healthCheck transport %q", transport)
	}

	if healthCheck.TimeoutSeconds < 0 {
		return errors.New("healthCheck timeout must not be negative")
	}
	return nil
}

func containsControlChars(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func isPlatformSupported(supported []string, target string) bool {
	for _, platform := range supported {
		if strings.EqualFold(strings.TrimSpace(platform), target) {
			return true
		}
	}
	return false
}

func validationErrorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func mergeAdapter(base Agent, adapter Agent) Agent {
	base.DisplayName = adapter.DisplayName
	base.Protocol = adapter.Protocol
	base.PlatformSupport = append([]string(nil), adapter.PlatformSupport...)
	base.Detection = adapter.Detection
	base.Launch = adapter.Launch
	base.HealthCheck = adapter.HealthCheck
	base.Capabilities = append([]string(nil), adapter.Capabilities...)
	base.Security = adapter.Security
	base.Hint = adapter.Hint
	if adapter.Registry.Required || adapter.Registry.ValidationStatus != "" {
		base.Registry.Required = adapter.Registry.Required
		if adapter.Registry.ValidationStatus != "" {
			base.Registry.ValidationStatus = adapter.Registry.ValidationStatus
		}
	}
	return base
}

func (s *Service) loadRegistry() (acpregistry.Snapshot, error, bool) {
	if s.registry == nil {
		return acpregistry.Snapshot{}, nil, false
	}
	snapshot, err := s.registry.Snapshot(context.Background())
	return snapshot, err, true
}

func sortedRegistryIDs(entries map[string]acpregistry.AgentEntry) []string {
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func registryEntryToAgent(entry acpregistry.AgentEntry, resolveBinaries func(string) []string) Agent {
	displayName := strings.TrimSpace(entry.Name)
	if displayName == "" {
		displayName = entry.ID
	}
	launchPlan := buildRegistryLaunchPlan(entry, resolveBinaries)

	agent := Agent{
		ID:              entry.ID,
		DisplayName:     displayName,
		Protocol:        "acp",
		PlatformSupport: []string{"windows", "darwin", "linux"},
		HealthCheck: HealthCheckConfig{
			Mode: "none",
		},
		Capabilities: []string{"acp"},
		Security: SecurityConfig{
			CuratedLaunch:          true,
			AllowsRemoteStart:      false,
			UserConfigurableFields: []string{},
		},
		Registry: registryInfoFromEntry(entry),
		Hint:     "Detected from the ACP registry.",
	}
	if launchPlan.Launch.Mode != "" {
		agent.Detection = launchPlan.Detection
		agent.Launch = launchPlan.Launch
		agent.Security.AllowsRemoteStart = true
	}
	if agent.Security.AllowsRemoteStart {
		agent.Hint = "Detected from the ACP registry and launchable with the gateway's generic ACP runtime policy."
	} else {
		agent.Hint = "Detected from the ACP registry, but no supported launch distribution is available yet."
	}
	return agent
}

func registryInfoFromEntry(entry acpregistry.AgentEntry) RegistryInfo {
	info := RegistryInfo{
		Required:          false,
		ValidationStatus:  "matched",
		Name:              entry.Name,
		Version:           entry.Version,
		Repository:        entry.Repository,
		Website:           entry.Website,
		DistributionKinds: append([]string(nil), entry.DistributionKinds...),
		NpxPackage:        entry.NpxPackage,
		NpxArgs:           append([]string(nil), entry.NpxArgs...),
		UvxPackage:        entry.UvxPackage,
		UvxArgs:           append([]string(nil), entry.UvxArgs...),
	}
	if entry.CurrentBinary != nil {
		info.CurrentBinaryCommand = entry.CurrentBinary.CommandName
		info.CurrentBinaryPath = entry.CurrentBinary.Command
		info.CurrentBinaryArchiveURL = entry.CurrentBinary.ArchiveURL
		info.CurrentBinaryArgs = append([]string(nil), entry.CurrentBinary.Args...)
	}
	return info
}

// applyRegistryInfo enriches an agent with ACP registry metadata. When a local
// gateway-owned manifest exists, the registry augments it. Otherwise, registry
// entries stand on their own as long as they provide a supported launch method.
func applyRegistryInfo(agent Agent, base RegistryInfo, snapshot acpregistry.Snapshot, registryErr error, registryEnabled bool, agentID string, resolveBinaries func(string) []string) (Agent, RegistryInfo) {
	if !base.Required {
		base.ValidationStatus = "not_required"
		return agent, base
	}
	if !registryEnabled {
		base.ValidationStatus = "unavailable"
		return agent, base
	}
	if registryErr != nil {
		base.ValidationStatus = "unavailable"
		return agent, base
	}
	entry, ok := snapshot.Agents[agentID]
	if !ok {
		base.ValidationStatus = "missing"
		return agent, base
	}

	registryInfo := registryInfoFromEntry(entry)
	base.ValidationStatus = registryInfo.ValidationStatus
	base.Name = registryInfo.Name
	base.Version = registryInfo.Version
	base.Repository = registryInfo.Repository
	base.Website = registryInfo.Website
	base.DistributionKinds = registryInfo.DistributionKinds
	base.CurrentBinaryPath = registryInfo.CurrentBinaryPath
	base.CurrentBinaryArchiveURL = registryInfo.CurrentBinaryArchiveURL
	base.CurrentBinaryCommand = registryInfo.CurrentBinaryCommand
	base.CurrentBinaryArgs = registryInfo.CurrentBinaryArgs
	base.NpxPackage = registryInfo.NpxPackage
	base.NpxArgs = registryInfo.NpxArgs
	base.UvxPackage = registryInfo.UvxPackage
	base.UvxArgs = registryInfo.UvxArgs
	agent = synthesizeRegistryLaunch(agent, buildRegistryLaunchPlan(entry, resolveBinaries))
	return agent, base
}

// synthesizeRegistryLaunch applies an already-chosen registry launch onto a
// curated adapter. Package runners always win, while binary launches only
// replace a local adapter when both point at the same executable family.
func synthesizeRegistryLaunch(agent Agent, plan registryLaunchPlan) Agent {
	if agent.Launch.Mode != "external" {
		return agent
	}
	if plan.Launch.Mode == "" {
		return agent
	}
	if normalizeExecutableName(plan.Launch.Command) == "npm" || normalizeExecutableName(plan.Launch.Command) == "uvx" {
		agent.Detection = plan.Detection
		agent.Launch = plan.Launch
		return agent
	}
	if sameExecutableName(agent.Launch.Command, plan.Launch.Command) {
		agent.Detection = plan.Detection
		agent.Launch = plan.Launch
	}
	return agent
}

// buildRegistryLaunchPlan picks the gateway launch strategy for a registry entry
// in host-aware order: registry binary, PATH binary, npm metadata, npm exec,
// uvx, then downloadable binary.
func buildRegistryLaunchPlan(entry acpregistry.AgentEntry, resolveBinaries func(string) []string) registryLaunchPlan {
	// Prefer a locally available binary from the registry if one is declared.
	if binary := entry.CurrentBinary; binary != nil && strings.TrimSpace(binary.CommandName) != "" {
		if _, err := exec.LookPath(binary.CommandName); err == nil {
			return registryExternalLaunchPlan(binary.CommandName, append([]string(nil), binary.Args...))
		}
	}

	// If no registry-declared binary was found, check if the agent's canonical
	// registry ID is available as a PATH binary.
	if _, err := exec.LookPath(entry.ID); err == nil {
		return registryExternalLaunchPlan(entry.ID, nil)
	}

	// If the registry provides an npm package and a resolver is set, query
	// npm metadata for the actual binary names it installs.
	if resolveBinaries != nil {
		if pkg := entry.NpxPackage; pkg != "" {
			for _, name := range resolveBinaries(pkg) {
				if _, err := exec.LookPath(name); err == nil {
					return registryExternalLaunchPlan(name, append([]string(nil), entry.NpxArgs...))
				}
			}
		}
	}

	// If no local binary was found, try the package-based launcher that ships the
	// agent on demand through npm exec.
	if detectNpmPackage(entry.NpxPackage) {
		return registryExternalLaunchPlan("npm", append([]string{"exec", "--yes", entry.NpxPackage, "--"}, entry.NpxArgs...))
	}

	// uvx is the next fallback when the agent is published as a Python package.
	if detectUvxPackage(entry.UvxPackage) {
		return registryExternalLaunchPlan("uvx", append([]string{entry.UvxPackage}, entry.UvxArgs...))
	}

	// If a binary was declared but not found in PATH, still use it as the
	// launch target — it has an ArchiveURL and can be downloaded.
	if binary := entry.CurrentBinary; binary != nil && strings.TrimSpace(binary.CommandName) != "" {
		return registryExternalLaunchPlan(binary.CommandName, append([]string(nil), binary.Args...))
	}

	// Nothing usable was advertised by the registry entry.
	return registryLaunchPlan{}
}

func registryExternalLaunchPlan(command string, args []string) registryLaunchPlan {
	launch := LaunchConfig{
		Mode:      "external",
		Command:   command,
		Args:      args,
		Transport: "stdio",
		Readiness: ReadinessConfig{Mode: "immediate"},
		Restart:   RestartConfig{Mode: "never"},
	}
	return registryLaunchPlan{
		Launch: launch,
		Detection: DetectionConfig{
			Mode:    "path_lookup",
			Command: command,
		},
	}
}

func sameExecutableName(left, right string) bool {
	return normalizeExecutableName(left) == normalizeExecutableName(right) && normalizeExecutableName(left) != ""
}

func normalizeExecutableName(command string) string {
	command = strings.TrimSpace(command)
	command = strings.TrimSuffix(command, ".exe")
	command = strings.TrimSuffix(command, ".EXE")
	return command
}

func mockAgentExecutableName() string {
	if goruntime.GOOS == "windows" {
		return "mock-stdio-agent.exe"
	}
	return "mock-stdio-agent"
}

// ResolveNpmBinaryNames queries the npm registry for a package's bin entries.
// Uses a 10-second timeout since this is a network call via `npm view`.
// Returns the binary names the package installs (e.g., @github/copilot → ["copilot"]),
// or nil if the query fails (npm metadata query fails, network error, or no bin field).
// Failures are silent — the caller falls through to npm exec as a fallback.
func ResolveNpmBinaryNames(pkg string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "npm", "view", pkg, "name", "bin", "--json")
	output, err := cmd.Output()
	if err != nil {
		return nil
	}
	var raw json.RawMessage
	if err := json.Unmarshal(output, &raw); err != nil {
		return nil
	}
	// npm view can return a single object or an array (one per version match).
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
		raw = arr[0]
	}
	var info struct {
		Name string          `json:"name"`
		Bin  json.RawMessage `json:"bin"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil
	}
	// Case 1: bin is an object {"command": "path"} — extract command names.
	var binMap map[string]string
	if err := json.Unmarshal(info.Bin, &binMap); err == nil {
		names := make([]string, 0, len(binMap))
		for name := range binMap {
			names = append(names, name)
		}
		sort.Strings(names)
		return names
	}
	// No bin field (or explicit null) — nothing to resolve.
	if len(info.Bin) == 0 || string(info.Bin) == "null" {
		return nil
	}
	// Case 2: bin is a string path — the command name is the package name.
	// For scoped packages (@scope/pkg), strip the scope → "pkg".
	if info.Name != "" {
		if idx := strings.LastIndex(info.Name, "/"); idx >= 0 {
			return []string{info.Name[idx+1:]}
		}
		return []string{info.Name}
	}
	return nil
}

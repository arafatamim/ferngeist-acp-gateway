package config

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"
)

const (
	defaultListenAddr             = "127.0.0.1:5788"
	defaultAdminAddr              = "127.0.0.1:5789"
	defaultLogLevel               = "info"
	defaultLogDir                 = "logs"
	defaultLogMaxSize             = int64(1024 * 1024)
	defaultLogBackups             = 3
	defaultPairingArmTTL          = 2 * time.Minute
	defaultPairingLockoutWindow   = 2 * time.Minute
	defaultPairingStartRefill     = 5 * time.Second
	defaultPairingCompleteRefill  = 2 * time.Second
	defaultPairingMaxAttempts     = 5
	defaultPairingBurstPerIP      = 5
	defaultPairingBurstGlobal     = 30
	defaultCredentialTTL          = 7 * 24 * time.Hour
	defaultCredentialGracePeriod  = 90 * 24 * time.Hour
	defaultSessionMaxDisconnected = 15 * time.Minute
	defaultMaxSessionsPerDevice   = 5
	defaultProgressInterval       = 15 * time.Second
	defaultUpdateCheckInterval    = 24 * time.Hour
)

// BuildInfo is injected from the build so status and diagnostics can describe
// the exact gateway binary that produced a failure report.
type BuildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	BuiltAt   string `json:"builtAt,omitempty"`
	GoVersion string `json:"goVersion,omitempty"`
	// UpdateChannel is the install channel baked in at build time ("", "self",
	// "apt", "pacman", "brew"). Not exposed over the API.
	UpdateChannel string `json:"-"`
}

// Config is the daemon's effective runtime configuration after environment
// variables and persisted settings have been merged.
type Config struct {
	ListenAddr            string
	AdminListenAddr       string
	LogLevel              string
	LogDir                string
	LogMaxSize            int64
	LogMaxBackups         int
	RegistryURL           string
	PublicBaseURL         string
	EnableLAN             bool
	StateDBPath           string
	GatewayName           string
	ManagedBinDir         string
	PairingArmTTL         time.Duration
	PairingMaxAttempts    int
	PairingLockoutWindow  time.Duration
	PairingStartRefill    time.Duration
	PairingCompleteRefill time.Duration
	PairingBurstPerIP     int
	PairingBurstGlobal    int
	CredentialTTL         time.Duration
	// CredentialGracePeriod is how long an expired credential stays recoverable
	// via refresh. A value of 0 disables grace and restores hard-delete-on-expiry.
	CredentialGracePeriod        time.Duration
	AllowDiagnosticsExport       bool
	AllowRuntimeRestartEnv       bool
	RequireProofOfPossession     bool
	AllowLegacyBearerCredentials bool
	// SessionMaxDisconnected is the TTL for disconnected sessions before the reaper closes them.
	SessionMaxDisconnected time.Duration
	// MaxSessionsPerDevice limits how many concurrent sessions a single device can hold.
	MaxSessionsPerDevice int
	// ProgressInterval is the minimum seconds between live progress push
	// notifications while an agent is mid-turn. 0 means use the default.
	ProgressInterval time.Duration
	// FrameLogEnabled toggles a raw ACP JSON-RPC frame log. When enabled, the
	// gateway appends every client->agent and agent->client frame to
	// <LogDir>/<agent>-agent.log as newline-delimited JSON, one line per frame,
	// rotated with LogMaxSize/LogMaxBackups. This is a debugging aid: frames
	// can contain project code and tool output, so treat the files as
	// sensitive.
	FrameLogEnabled bool
	// GatewayID is this gateway's stable instance identifier. It is not read from
	// the environment — the daemon populates it at boot from persisted storage —
	// and is handed to clients at pairing so they can address this gateway and
	// resolve its pushes for deep-linking.
	GatewayID string
	// VAPIDPublicKey is the public half of this gateway's Web Push signing key,
	// populated at boot from persisted storage. Clients create their push
	// subscription against it.
	VAPIDPublicKey string
	// UpdateCheckEnabled toggles the startup update-available check. Defaults on;
	// package-manager-installed builds are expected to disable it via env in their
	// service environment.
	UpdateCheckEnabled bool
	// UpdateCheckInterval is how often the daemon re-checks for a newer stable
	// release. 0 means use the default (24h).
	UpdateCheckInterval time.Duration
	// KeepAwake holds an OS sleep inhibitor while any agent turn is running.
	// Defaults on.
	KeepAwake bool
	// TailscaleMode controls automatic remote access provisioning:
	// "off" disables it, "auto" uses the tailscale CLI when available and
	// falls back to an embedded tsnet node, "cli" forces the CLI path,
	// "tsnet" forces the embedded node. Empty/invalid values mean "off".
	TailscaleMode string
	// TailscaleAuthKey optionally pre-authenticates the embedded tsnet node.
	TailscaleAuthKey string
	// TailscaleHostname is the tailnet device name for the embedded node.
	TailscaleHostname string
	// TailscalePrivate restricts exposure to the tailnet only (tsnet
	// ListenTLS / `tailscale serve`) instead of public Funnel.
	TailscalePrivate bool
	// JSRuntime runs npm-distributed agents: "auto" (bun when installed, else
	// node), "bun", "node", or "npm" (keep launching through npm exec).
	// Empty/invalid values mean "auto".
	JSRuntime string
}

type PersistedSettings struct {
	RegistryURL   string
	PublicBaseURL string
	EnableLAN     *bool
	GatewayName   string
}

// Load reads environment-driven configuration only. Persisted settings are
// applied later so explicit env vars always remain the strongest override.
func Load() Config {
	gatewayName := envOrDefault("FERNGEIST_GATEWAY_NAME", hostnameOrDefault("ferngeist-gateway"))
	cfg := Config{
		ListenAddr:             envOrDefault("FERNGEIST_GATEWAY_LISTEN_ADDR", defaultListenAddr),
		AdminListenAddr:        envOrDefault("FERNGEIST_GATEWAY_ADMIN_ADDR", defaultAdminAddr),
		LogLevel:               envOrDefault("FERNGEIST_GATEWAY_LOG_LEVEL", defaultLogLevel),
		LogDir:                 envOrDefault("FERNGEIST_GATEWAY_LOG_DIR", defaultLogDir),
		LogMaxSize:             envInt64OrDefault("FERNGEIST_GATEWAY_LOG_MAX_BYTES", defaultLogMaxSize),
		LogMaxBackups:          envIntOrDefault("FERNGEIST_GATEWAY_LOG_MAX_BACKUPS", defaultLogBackups),
		RegistryURL:            envOrDefault("FERNGEIST_GATEWAY_REGISTRY_URL", "https://cdn.agentclientprotocol.com/registry/v1/latest/registry.json"),
		PublicBaseURL:          strings.TrimSpace(os.Getenv("FERNGEIST_GATEWAY_PUBLIC_BASE_URL")),
		EnableLAN:              os.Getenv("FERNGEIST_GATEWAY_ENABLE_LAN") == "1",
		StateDBPath:            envOrDefault("FERNGEIST_GATEWAY_STATE_DB", "ferngeist-gateway.db"),
		GatewayName:            gatewayName,
		ManagedBinDir:          envOrDefault("FERNGEIST_GATEWAY_MANAGED_BIN_DIR", defaultManagedBinDir()),
		PairingArmTTL:          envDurationSecondsOrDefault("FERNGEIST_GATEWAY_PAIRING_ARM_TTL_SECONDS", defaultPairingArmTTL),
		PairingMaxAttempts:     envIntOrDefault("FERNGEIST_GATEWAY_PAIRING_MAX_ATTEMPTS", defaultPairingMaxAttempts),
		PairingLockoutWindow:   envDurationSecondsOrDefault("FERNGEIST_GATEWAY_PAIRING_LOCKOUT_SECONDS", defaultPairingLockoutWindow),
		PairingStartRefill:     envDurationSecondsOrDefault("FERNGEIST_GATEWAY_PAIRING_START_REFILL_SECONDS", defaultPairingStartRefill),
		PairingCompleteRefill:  envDurationSecondsOrDefault("FERNGEIST_GATEWAY_PAIRING_COMPLETE_REFILL_SECONDS", defaultPairingCompleteRefill),
		PairingBurstPerIP:      envIntOrDefault("FERNGEIST_GATEWAY_PAIRING_BURST_PER_IP", defaultPairingBurstPerIP),
		PairingBurstGlobal:     envIntOrDefault("FERNGEIST_GATEWAY_PAIRING_BURST_GLOBAL", defaultPairingBurstGlobal),
		CredentialTTL:          envDurationSecondsOrDefault("FERNGEIST_GATEWAY_CREDENTIAL_TTL_SECONDS", defaultCredentialTTL),
		CredentialGracePeriod:  credentialGracePeriodOrDefault(),
		AllowDiagnosticsExport: envBool("FERNGEIST_GATEWAY_ALLOW_REMOTE_DIAGNOSTICS_EXPORT"),
		AllowRuntimeRestartEnv: envBool("FERNGEIST_GATEWAY_ALLOW_REMOTE_RUNTIME_RESTART_ENV"),
		SessionMaxDisconnected: envDurationSecondsOrDefault("FERNGEIST_GATEWAY_SESSION_MAX_DISCONNECTED_SECONDS", defaultSessionMaxDisconnected),
		MaxSessionsPerDevice:   envIntOrDefault("FERNGEIST_GATEWAY_MAX_SESSIONS_PER_DEVICE", defaultMaxSessionsPerDevice),
		ProgressInterval:       envDurationSecondsOrDefault("FERNGEIST_GATEWAY_PROGRESS_INTERVAL_SECONDS", defaultProgressInterval),
		FrameLogEnabled:        envBool("FERNGEIST_GATEWAY_FRAME_LOG"),
		UpdateCheckInterval:    envDurationSecondsOrDefault("FERNGEIST_GATEWAY_UPDATE_CHECK_INTERVAL_SECONDS", defaultUpdateCheckInterval),
		TailscaleMode:          normalizeTailscaleMode(envOrDefault("FERNGEIST_GATEWAY_TAILSCALE_MODE", "off")),
		TailscaleAuthKey:       strings.TrimSpace(os.Getenv("FERNGEIST_GATEWAY_TAILSCALE_AUTH_KEY")),
		TailscaleHostname:      envOrDefault("FERNGEIST_GATEWAY_TAILSCALE_HOSTNAME", "ferngeist-gateway"),
		TailscalePrivate:       envBool("FERNGEIST_GATEWAY_TAILSCALE_PRIVATE"),
		JSRuntime:              normalizeJSRuntime(os.Getenv("FERNGEIST_GATEWAY_JS_RUNTIME")),
	}
	cfg.UpdateCheckEnabled = envBoolDefaultTrue("FERNGEIST_GATEWAY_UPDATE_CHECK_ENABLED")
	cfg.KeepAwake = envBoolDefaultTrue("FERNGEIST_GATEWAY_KEEP_AWAKE")
	return cfg.applySecurityDefaults()
}

// ApplyPersistedSettings fills only fields that were not set explicitly in the
// process environment.
func (c Config) ApplyPersistedSettings(settings PersistedSettings) Config {
	if !hasEnv("FERNGEIST_GATEWAY_REGISTRY_URL") && strings.TrimSpace(settings.RegistryURL) != "" {
		c.RegistryURL = strings.TrimSpace(settings.RegistryURL)
	}
	// A persisted remote URL only makes sense when this run actually
	// requests remote access (Tailscale mode on). It is written by a
	// --remote provision, so applying it on a plain LAN or localhost boot
	// would keep advertising the old tailnet URL. An explicit env pin below
	// still wins.
	if !hasEnv("FERNGEIST_GATEWAY_PUBLIC_BASE_URL") && strings.TrimSpace(settings.PublicBaseURL) != "" &&
		c.TailscaleMode != "off" {
		c.PublicBaseURL = strings.TrimSpace(settings.PublicBaseURL)
	}
	if !hasEnv("FERNGEIST_GATEWAY_ENABLE_LAN") && settings.EnableLAN != nil {
		c.EnableLAN = *settings.EnableLAN
	}
	if !hasEnv("FERNGEIST_GATEWAY_NAME") && strings.TrimSpace(settings.GatewayName) != "" {
		c.GatewayName = strings.TrimSpace(settings.GatewayName)
	}
	return c.applySecurityDefaults()
}

func (c Config) applySecurityDefaults() Config {
	publicMode := strings.TrimSpace(c.PublicBaseURL) != ""
	// Remote access is provisioned asynchronously when no auth key is set, so
	// the public URL may not be known at boot. Treat any enabled Tailscale mode
	// as intended public exposure: strict defaults apply from the first boot,
	// not only once the URL is persisted. Env overrides below still win.
	remoteExposed := c.TailscaleMode != "" && c.TailscaleMode != "off"
	if !hasEnv("FERNGEIST_GATEWAY_REQUIRE_PROOF_OF_POSSESSION") {
		c.RequireProofOfPossession = publicMode || remoteExposed
	} else {
		c.RequireProofOfPossession = envBool("FERNGEIST_GATEWAY_REQUIRE_PROOF_OF_POSSESSION")
	}
	if !hasEnv("FERNGEIST_GATEWAY_ALLOW_LEGACY_BEARER_CREDENTIALS") {
		c.AllowLegacyBearerCredentials = !publicMode && !remoteExposed
	} else {
		c.AllowLegacyBearerCredentials = envBool("FERNGEIST_GATEWAY_ALLOW_LEGACY_BEARER_CREDENTIALS")
	}
	return c
}

func envOrDefault(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}

// normalizeTailscaleMode coerces a FERNGEIST_GATEWAY_TAILSCALE_MODE value into
// one of the valid modes, defaulting to "off" for empty or unknown values.
func normalizeTailscaleMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "auto", "cli", "tsnet":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "off"
	}
}

func normalizeJSRuntime(value string) string {
	switch v := strings.ToLower(strings.TrimSpace(value)); v {
	case "bun", "node", "npm":
		return v
	default:
		return "auto"
	}
}

func hasEnv(key string) bool {
	_, ok := os.LookupEnv(key)
	return ok
}

func hostnameOrDefault(fallback string) string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return fallback
	}
	return name
}

func envInt64OrDefault(key string, fallback int64) int64 {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}

	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func envIntOrDefault(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}

	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func envDurationSecondsOrDefault(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return time.Duration(parsed) * time.Second
}

// credentialGracePeriodOrDefault reads FERNGEIST_GATEWAY_CREDENTIAL_GRACE_SECONDS.
// Unlike envDurationSecondsOrDefault, a value of 0 is meaningful: it disables
// the grace window, restoring hard-delete-on-expiry. Only unset, unparsable,
// or negative values fall back to the 90-day default.
func credentialGracePeriodOrDefault() time.Duration {
	value := strings.TrimSpace(os.Getenv("FERNGEIST_GATEWAY_CREDENTIAL_GRACE_SECONDS"))
	if value == "" {
		return defaultCredentialGracePeriod
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return defaultCredentialGracePeriod
	}
	return time.Duration(parsed) * time.Second
}

func envBool(key string) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	return value == "1" || value == "true" || value == "yes"
}

// envBoolDefaultTrue is true unless key is set to empty, 0 or false.
func envBoolDefaultTrue(key string) bool {
	v, ok := os.LookupEnv(key)
	return !ok || (strings.TrimSpace(v) != "" && v != "0" && !strings.EqualFold(v, "false"))
}

func defaultManagedBinDir() string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "managed-bin"
	}

	switch goruntime.GOOS {
	case "windows":
		if localAppData := strings.TrimSpace(os.Getenv("LocalAppData")); localAppData != "" {
			return filepath.Join(localAppData, "FerngeistGateway", "bin")
		}
		return filepath.Join(home, "AppData", "Local", "FerngeistGateway", "bin")
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Ferngeist Gateway", "bin")
	default:
		return filepath.Join(home, ".local", "share", "ferngeist-gateway", "bin")
	}
}

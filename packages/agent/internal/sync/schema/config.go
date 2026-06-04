// Package config handles agent configuration: YAML loading, Gateway pull, and merge.
package schema

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"gopkg.in/yaml.v3"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/paths"
)

// LogConfig controls logging behaviour.
type LogConfig struct {
	Level        string `yaml:"level"`        // trace, debug, info, warn, error (default: info)
	Format       string `yaml:"format"`       // json, text (default: json)
	File         string `yaml:"file"`         // optional: tee logs to this file (see also env LOG_FILE)
	StackOnError bool   `yaml:"stackOnError"` // attach goroutine stack on error-level logs (env LOG_STACK_ON_ERROR)
}

// AgentConfig is the typed agent configuration.
type AgentConfig struct {
	Log        LogConfig `yaml:"log"`
	DeviceID   string    `yaml:"deviceID"`
	CertFile   string    `yaml:"certFile"`
	KeyFile    string    `yaml:"keyFile"`
	CACertFile string    `yaml:"caCertFile"`
	// Platform bridge listen address (Linux/Windows transparent-proxy
	// port). Ignored on Darwin (uses Unix socket). Default 127.0.0.1:19080.
	PlatformBridgeAddress string `yaml:"platformBridgeAddress"`
	AuditDBPath           string `yaml:"auditDBPath"`
	HeartbeatIntervalSec  int    `yaml:"heartbeatIntervalSec"`
	AuditDrainIntervalSec int    `yaml:"auditDrainIntervalSec"`
	AuditBatchSize        int    `yaml:"auditBatchSize"`
	AuditRetentionDays    int    `yaml:"auditRetentionDays"`
	DefaultAction         string `yaml:"defaultAction"`
	UpdaterEnabled        bool   `yaml:"updaterEnabled"`
	UpdaterCheckSec       int    `yaml:"updaterCheckSec"`
	QuitAllowed           *bool  `yaml:"quitAllowed"`
	// TrafficUploadLevel controls which FlowResult events are uploaded
	// to Hub in audit batches. Enum (closed set, validated in MergeConfig):
	//   - "all"       — every flow including silent passthrough TCP
	//   - "processed" — flows where the agent ran HTTP inspection
	//                   (TLS bumped OR provider adapter recognised) — default
	//   - "blocked"   — only deny / block_soft / reject_hard / error flows
	// Local SQLite queue persists every flow regardless; this knob only
	// gates the Hub upload path. Default empty / unknown maps to "processed".
	TrafficUploadLevel string `yaml:"trafficUploadLevel"`
	// LocalBodyCapture controls whether the agent captures + stores request
	// and response bodies locally (small inline in SQLite, oversize in the
	// local spill store), INDEPENDENT of the Hub-pushed payload_capture
	// config. Default true: the agent is a local, low-volume device and users
	// expect to see what their own AI traffic contained in the agent UI. The
	// Hub payload_capture config governs only whether those captured bodies are
	// UPLOADED to Hub/S3 — not whether they are captured locally. Size params
	// (inline cutoff / read caps) still follow the Hub config when pushed, else
	// the local default. *bool distinguishes "unset in YAML" (nil → default
	// true) from "explicitly false".
	LocalBodyCapture *bool `yaml:"localBodyCapture"`
	// ThemeID names the theme pack the agent Dashboard should render
	// with — admin sets fleet-wide via CP UI, propagated via the
	// agent_settings shadow. Empty means "fall through to the agent
	// Dashboard's local default". Validated as a non-empty printable
	// string; the Dashboard handles unknown IDs by falling back to
	// the bundled `default` theme so a typo never breaks the UI.
	ThemeID string `yaml:"themeId"`
	// ForceQUICFallbackBundles is the bundle-ID allowlist whose UDP
	// flows the macOS NE proxy must close, forcing the calling app
	// to fall back from HTTP/3 (over QUIC/UDP) to HTTP/2 (over TCP)
	// — which our TLS-bump path actually inspects. Without this list,
	// Chrome/Edge/Safari prefer h3 to ChatGPT/Cloudflare-fronted AI
	// services and our TCP path never sees the request. Only browsers
	// + Electron AI desktop apps belong here; system processes (mDNS,
	// DHCP, NTP) MUST NOT be added — closing their UDP breaks DNS and
	// takes the host's network down (fail-open safety rule).
	// Empty/missing = no UDP gets killed (safe-default fail-open).
	ForceQUICFallbackBundles []string `yaml:"forceQUICFallbackBundles"`

	// MitmBridgeAddr is the loopback bind address for the macOS NE →
	// Go MITM bridge listener. When non-empty, the daemon listens for
	// `BRIDGE host:port flowId\n` framed connections and hands them to
	// the proxy.BumpFlow pipeline (shared/tlsbump) so the macOS NE inspect
	// path gains TLS termination + HTTP parse + hook execution + body
	// capture (parity with Linux/Windows). Default empty = listener
	// disabled.
	MitmBridgeAddr string `yaml:"mitmBridgeAddr"`
	// Cert-pin auto-exemption
	ExemptionEnabled          bool     `yaml:"exemptionEnabled"`
	ExemptionFailureThreshold int      `yaml:"exemptionFailureThreshold"`
	ExemptionWindowSec        int      `yaml:"exemptionWindowSec"`
	ExemptionDurationSec      int      `yaml:"exemptionDurationSec"`
	ExemptionAllowlist        []string `yaml:"exemptionAllowlist"`
	ExemptionDenylist         []string `yaml:"exemptionDenylist"`
	// Hub connection (replaces gateway for config sync + audit + heartbeat)
	HubURL     string `yaml:"hubURL"`     // WebSocket URL: wss://hub.example.com/ws
	HubHTTPURL string `yaml:"hubHTTPURL"` // HTTP URL: https://hub.example.com
	// Bootstrap CA for Hub enrollment TLS pinning. Used before a device cert
	// exists. Optional: falls back to CACertFile when empty.
	HubCACertFile string `yaml:"hubCACertFile"`
	// CpURL is an optional operator override for the Control Plane base
	// URL used by SSO self-enrollment. The default discovery path is
	// Hub's GET /api/public/agent-bootstrap, so the YAML field exists
	// only for sites that want to pin the URL out-of-band (air-gapped
	// installs, ops drills, etc.). Empty is the normal case.
	CpURL string `yaml:"cpURL"`
	// OpenTelemetry
	OtelEnabled      bool    `yaml:"otelEnabled"`
	OtelEndpoint     string  `yaml:"otelEndpoint"`
	OtelServiceName  string  `yaml:"otelServiceName"`
	OtelSamplingRate float64 `yaml:"otelSamplingRate"`
}

// EffectiveHubCA returns the CA path used to pin TLS for Hub HTTPS calls.
// HubCACertFile wins when set; otherwise CACertFile is reused on the
// assumption that Hub and Agent device CAs share a trust root.
func (c *AgentConfig) EffectiveHubCA() string {
	if c.HubCACertFile != "" {
		return c.HubCACertFile
	}
	return c.CACertFile
}

// LoadFromFile reads and validates a YAML config file.
func LoadFromFile(path string) (*AgentConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg AgentConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if cfg.HubHTTPURL == "" {
		return nil, fmt.Errorf("hubHTTPURL is required")
	}

	applyDefaults(&cfg)
	return &cfg, nil
}

func applyDefaults(cfg *AgentConfig) {
	if cfg.Log.Level == "" {
		cfg.Log.Level = "info"
	}
	if cfg.Log.Format == "" {
		cfg.Log.Format = "json"
	}
	// Environment variable overrides for log level.
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		cfg.Log.Level = v
	}

	// Filesystem path defaults come from platform.DefaultPaths so each OS
	// puts state in the right place (macOS /Library/Application Support,
	// Linux /var/lib, Windows %ProgramData%). YAML overrides win when set;
	// blank fields below fall through to the OS-idiomatic default.
	paths := paths.DefaultPaths()
	// Device cert + key are ISSUED at enroll time — they don't exist on
	// a fresh, never-enrolled install. We MUST leave the cfg fields
	// empty in that state so hub.NewClient's
	//   if cfg.CertFile != "" && cfg.KeyFile != "" { LoadX509KeyPair... }
	// branch skips mTLS instead of failing on a missing file. The
	// daemon's `if !enrollMgr.IsEnrolled() { runPendingEnrollment(...) }`
	// fork then takes over and serves the status IPC so the Dashboard
	// can drive SSO sign-in.
	//
	// Behaviour: only DEFAULT the path when a file is already present
	// at the canonical location. Enrollment writes the cert + key,
	// next daemon start re-reads applyDefaults, the files now exist,
	// and the paths get filled in → full mTLS-Hub client comes up.
	// This pattern matches what we already do for CACertFile below.
	if cfg.CertFile == "" {
		candidate := filepath.Join(paths.StateDir, "device.pem")
		if _, err := os.Stat(candidate); err == nil {
			cfg.CertFile = candidate
		}
	}
	if cfg.KeyFile == "" {
		candidate := filepath.Join(paths.StateDir, "device-key.pem")
		if _, err := os.Stat(candidate); err == nil {
			cfg.KeyFile = candidate
		}
	}
	// CACertFile is OPTIONAL: a Hub-TLS pinning override for air-gapped
	// installs / self-signed dev Hubs. Production Hub uses an AWS-signed
	// public cert chain that's already trusted by the OS cert store, so
	// the canonical prod path is "no CA pin, use system trust" → empty.
	//
	// Only default the path when a file actually exists at that location —
	// operators drop a `gateway-ca.pem` into StateDir to pin, and we
	// auto-discover it. When it isn't there, we leave the field empty
	// rather than setting a path that hub.NewClient would then
	// fatally error on. Earlier behaviour (always default the path)
	// crashed the daemon at startup on every fresh install where the
	// optional pinning file wasn't pre-staged.
	if cfg.CACertFile == "" {
		candidate := filepath.Join(paths.StateDir, "gateway-ca.pem")
		if _, err := os.Stat(candidate); err == nil {
			cfg.CACertFile = candidate
		}
	}
	// HubCACertFile is intentionally left blank-by-default — EffectiveHubCA()
	// falls back to CACertFile, which gives the right behaviour without
	// duplicating the same path in two fields.
	if cfg.AuditDBPath == "" {
		cfg.AuditDBPath = filepath.Join(paths.StateDir, "audit.db")
	}
	if cfg.Log.File == "" {
		cfg.Log.File = filepath.Join(paths.LogDir, "agent.log")
	}

	if cfg.HeartbeatIntervalSec == 0 {
		cfg.HeartbeatIntervalSec = 60
	}
	if cfg.AuditDrainIntervalSec == 0 {
		cfg.AuditDrainIntervalSec = 30
	}
	if cfg.AuditBatchSize == 0 {
		cfg.AuditBatchSize = 200
	}
	if cfg.AuditRetentionDays == 0 {
		cfg.AuditRetentionDays = 30
	}
	if cfg.DefaultAction == "" {
		cfg.DefaultAction = "passthrough"
	}
	// TrafficUploadLevel defaults to "processed" — only flows where the
	// agent did actual HTTP inspection (TLS bumped, adapter matched, hook
	// evaluated) reach Hub. Silent TCP passthroughs of WhatsApp / WeChat /
	// SSH / git etc. stay local-only. Admins can flip to "all" via the CP
	// Device Defaults page for audit / debugging windows, then back.
	switch cfg.TrafficUploadLevel {
	case "all", "processed", "blocked":
		// valid — keep as-is
	default:
		cfg.TrafficUploadLevel = "processed"
	}
	if cfg.UpdaterCheckSec == 0 {
		cfg.UpdaterCheckSec = 3600
	}
	// macOS NE → Go MITM bridge default. Empty in YAML means
	// "use the canonical loopback port" — operators rarely want to
	// move it, and an unset field should activate the feature.
	if cfg.MitmBridgeAddr == "" {
		cfg.MitmBridgeAddr = "127.0.0.1:9443"
	}
	// QuitAllowed defaults to true. Using *bool allows distinguishing
	// "not set in YAML" (nil → default true) from "explicitly set false".
	if cfg.QuitAllowed == nil {
		t := true
		cfg.QuitAllowed = &t
	}
	// LocalBodyCapture defaults to true — the agent always captures bodies
	// locally so users can inspect their own AI traffic; Hub config only gates
	// upload. *bool distinguishes unset (nil → true) from explicit false.
	if cfg.LocalBodyCapture == nil {
		t := true
		cfg.LocalBodyCapture = &t
	}
	// Cert-pin exemption defaults
	if cfg.ExemptionFailureThreshold == 0 {
		cfg.ExemptionFailureThreshold = 3
	}
	if cfg.ExemptionWindowSec == 0 {
		cfg.ExemptionWindowSec = 60
	}
	if cfg.ExemptionDurationSec == 0 {
		cfg.ExemptionDurationSec = 86400
	}
	if cfg.PlatformBridgeAddress == "" {
		cfg.PlatformBridgeAddress = "127.0.0.1:19080"
	}
	// OTEL defaults
	if cfg.OtelEndpoint == "" {
		cfg.OtelEndpoint = "http://localhost:4318"
	}
	if cfg.OtelServiceName == "" {
		cfg.OtelServiceName = "nexus-agent"
	}
	if cfg.OtelSamplingRate == 0 {
		cfg.OtelSamplingRate = 1.0
	}
}

// MergeConfig merges a Gateway-pulled config onto a local config.
// Local wins for filesystem paths; Gateway wins for intervals.
func MergeConfig(local *AgentConfig, remote map[string]any) *AgentConfig {
	merged := *local

	if v, ok := remote["heartbeatIntervalSec"].(float64); ok {
		merged.HeartbeatIntervalSec = int(v)
	}
	if v, ok := remote["auditDrainIntervalSec"].(float64); ok {
		merged.AuditDrainIntervalSec = int(v)
	}
	if v, ok := remote["defaultAction"].(string); ok {
		merged.DefaultAction = v
	}
	if v, ok := remote["quitAllowed"].(bool); ok {
		merged.QuitAllowed = &v
	}
	if v, ok := remote["trafficUploadLevel"].(string); ok {
		switch v {
		case "all", "processed", "blocked":
			merged.TrafficUploadLevel = v
		}
		// Silently ignore unknown values — never panic on a misconfigured
		// admin push. The OnFlowComplete consumer falls back to "processed"
		// when the field is empty, so the agent stays in a sane state.
	}
	if v, ok := remote["themeId"].(string); ok {
		// Open enum — we accept whatever ID admin sets. The Dashboard
		// ThemeProvider verifies the theme exists in /themes/ at load
		// time; an unknown ID falls back to the bundled `default` theme
		// rather than rendering broken. Validating here would force a
		// daemon redeploy every time someone adds a new theme pack.
		merged.ThemeID = v
	}
	if list, ok := remote["forceQUICFallbackBundles"].([]any); ok {
		merged.ForceQUICFallbackBundles = stringSliceFromAny(list)
	}
	// OTEL fields (nested object "otel")
	if otelObj, ok := remote["otel"].(map[string]any); ok {
		if v, ok := otelObj["enabled"].(bool); ok {
			merged.OtelEnabled = v
		}
		if v, ok := otelObj["endpoint"].(string); ok && v != "" {
			merged.OtelEndpoint = v
		}
		if v, ok := otelObj["serviceName"].(string); ok && v != "" {
			merged.OtelServiceName = v
		}
		if v, ok := otelObj["samplingRate"].(float64); ok {
			merged.OtelSamplingRate = v
		}
	}
	// Exemption fields (nested object "exemptions")
	if exObj, ok := remote["exemptions"].(map[string]any); ok {
		if v, ok := exObj["enabled"].(bool); ok {
			merged.ExemptionEnabled = v
		}
		if v, ok := exObj["failureThreshold"].(float64); ok {
			merged.ExemptionFailureThreshold = int(v)
		}
		if v, ok := exObj["windowSec"].(float64); ok {
			merged.ExemptionWindowSec = int(v)
		}
		if v, ok := exObj["durationSec"].(float64); ok {
			merged.ExemptionDurationSec = int(v)
		}
		if list, ok := exObj["allowlist"].([]any); ok {
			merged.ExemptionAllowlist = stringSliceFromAny(list)
		}
		if list, ok := exObj["denylist"].([]any); ok {
			merged.ExemptionDenylist = stringSliceFromAny(list)
		}
	}
	return &merged
}

func stringSliceFromAny(in []any) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// ConfigDiff describes what changed in a config swap.
type ConfigDiff struct {
	Old *AgentConfig
	New *AgentConfig
}

// Manager holds the live config and notifies subscribers on change.
type Manager struct {
	live        atomic.Value // *AgentConfig
	subscribers []chan ConfigDiff
	mu          sync.Mutex
}

// NewManager creates a config manager with an initial config.
func NewManager(initial *AgentConfig) *Manager {
	m := &Manager{}
	m.live.Store(initial)
	return m
}

// Get returns the current live config.
func (m *Manager) Get() *AgentConfig {
	return m.live.Load().(*AgentConfig)
}

// Swap atomically replaces the live config and notifies subscribers.
func (m *Manager) Swap(next *AgentConfig) {
	old := m.Get()
	m.live.Store(next)
	m.mu.Lock()
	defer m.mu.Unlock()
	diff := ConfigDiff{Old: old, New: next}
	for _, ch := range m.subscribers {
		select {
		case ch <- diff:
		default:
			slog.Warn("config subscriber channel full, dropping notification")
		}
	}
}

// Subscribe returns a channel that receives config change notifications.
// The channel is closed when Close() is called.
func (m *Manager) Subscribe() chan ConfigDiff {
	ch := make(chan ConfigDiff, 4)
	m.mu.Lock()
	m.subscribers = append(m.subscribers, ch)
	m.mu.Unlock()
	return ch
}

// Close closes all subscriber channels, unblocking goroutines that range
// over them. Must be called during shutdown to prevent goroutine leaks.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ch := range m.subscribers {
		close(ch)
	}
	m.subscribers = nil
}

// Package config handles agent configuration: YAML loading, Gateway pull, and merge.
package schema

import (
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"

	"gopkg.in/yaml.v3"
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
	// AuditLossMode is the audit-overflow policy (shared/audit/lossmode):
	// "spillblock" | "block" | "spill" | "drop". Empty or unrecognised resolves to
	// the shared no-loss default, so a typo can never make the audit trail lossy.
	//
	// The agent SHIPS "spill" rather than the no-loss default, and the reason is
	// CLAUDE.md's host rule: the macOS network extension sits in the machine's
	// outbound packet path, so a no-loss mode — which must block until the record is
	// durable, and a SQLite write under WAL contention can wait out the 5 s
	// busy_timeout — would turn audit bookkeeping into a frozen host network. Set
	// "spillblock" or "block" only where those stalls are acceptable.
	AuditLossMode   string `yaml:"auditLossMode"`
	DefaultAction   string `yaml:"defaultAction"`
	UpdaterEnabled  bool   `yaml:"updaterEnabled"`
	UpdaterCheckSec int    `yaml:"updaterCheckSec"`
	QuitAllowed     *bool  `yaml:"quitAllowed"`
	// UpstreamProxy, when set, routes the agent's MITM upstream forward through
	// an egress proxy (e.g. "socks5://127.0.0.1:10808" or "http://127.0.0.1:8080").
	// The agent still intercepts + inspects the flow; only the final hop to the
	// AI provider goes via the proxy. Empty = direct egress (default). Use in
	// environments where direct provider access is blocked and a local proxy
	// (corporate egress / circumvention client) is the only working path.
	UpstreamProxy string `yaml:"upstreamProxy"`
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

	// BypassBundles is the source-app exemption list: outbound flows whose
	// originating bundle (sourceAppSigningIdentifier) matches an entry are
	// passed through to native routing by the macOS NE proxy — no TLS bump,
	// no daemon bridge. Matching is by SOURCE bundle, never by host, so a
	// trusted developer tool (e.g. the local claude-code CLI whose pinned
	// TLS to api.anthropic.com breaks under bump) can be kept off the
	// inspection path WITHOUT blinding the agent to the same host reached
	// from other apps. Empty/missing = exempt nothing (inspect everything),
	// the safe default; this list never ships populated.
	BypassBundles []string `yaml:"bypassBundles"`

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

// MergeConfig overlays the Hub-pushed agent_settings fields onto a local
// config. Only the three runtime-overridable fields its sole caller
// (cmd/agent configappliers) actually publishes are merged: quitAllowed,
// trafficUploadLevel, forceQUICFallbackBundles. Everything else in
// AgentConfig is local-yaml-only (URLs, paths, intervals, otel) — the
// shadow never overrides those, so this function must not either.
func MergeConfig(local *AgentConfig, remote map[string]any) *AgentConfig {
	merged := *local

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
	if list, ok := remote["forceQUICFallbackBundles"].([]any); ok {
		merged.ForceQUICFallbackBundles = stringSliceFromAny(list)
	}
	if list, ok := remote["bypassBundles"].([]any); ok {
		merged.BypassBundles = stringSliceFromAny(list)
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

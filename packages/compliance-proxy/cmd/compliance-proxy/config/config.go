// Package config loads and validates the compliance-proxy YAML configuration.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/redisfactory"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore/spillfactory"
)

// OnboardingConfig controls the proxy's onboarding intercept mode.
type OnboardingConfig struct {
	// Enabled causes the proxy to respond to monitored-domain CONNECTs with
	// 407 + an HTML guide instead of 200. Toggle off once all endpoints have
	// the CA cert installed.
	Enabled bool `yaml:"enabled"`
	// CPUIBaseURL is the base URL of the Control Plane UI, included in the
	// 407 HTML body as a link to the setup guide (e.g. https://cp.company.com).
	CPUIBaseURL string `yaml:"cpUIBaseURL"`
}

// Config represents the top-level compliance-proxy configuration.
type Config struct {
	// ID is an optional operator-supplied stable identifier for this
	// service instance. When non-empty it is sent to the Hub as
	// `physicalId` at register time and persisted into
	// `thing.physical_id`, giving ops a stable handle independent of the
	// auto-derived thing_id. Leave blank when the auto-derived id is good
	// enough.
	ID string `yaml:"id,omitempty"`
	// PublicURL is the externally-reachable base URL clients of this
	// service use (scheme + host[:port], no trailing slash). The
	// compliance-proxy is a TLS CONNECT intercept point reached
	// directly by org-managed devices, not via public nginx, so prod
	// uses a hostname clients can resolve (e.g.
	// "https://compliance.example.com:3040"). Dev uses
	// "http://localhost:3040". Reported to the Thing Registry as part
	// of staticInfo so the CP admin API can render the real endpoint
	// in proxy-setup UI without hardcoded hostnames.
	PublicURL     string              `yaml:"publicURL"`
	Listener      ListenerConfig      `yaml:"listener"`
	CA            CAConfig            `yaml:"ca"`
	Database      DatabaseConfig      `yaml:"database"`
	Redis         redisfactory.Config `yaml:"redis"`
	AccessControl AccessControlConfig `yaml:"accessControl"`
	Connections   ConnectionsConfig   `yaml:"connections"`
	Upstream      UpstreamConfig      `yaml:"upstream"`
	Limits        LimitsConfig        `yaml:"limits"`
	Log           LogConfig           `yaml:"log"`
	Metrics       MetricsConfig       `yaml:"metrics"`
	Audit         AuditConfig         `yaml:"audit"`
	Compliance    ComplianceConfig    `yaml:"compliance"`
	RuntimeAPI    RuntimeAPIConfig    `yaml:"runtimeApi"`
	Alerting      AlertingConfig      `yaml:"alerting"`
	Registry      RegistryConfig      `yaml:"registry"`
	Auth          AuthConfig          `yaml:"auth"`
	MQ            MQConfig            `yaml:"mq"`
	Onboarding    OnboardingConfig    `yaml:"onboarding"`
	// DataDir is a writable directory the proxy uses for durable state that
	// must survive restart but is not a database row: break-glass JSONL
	// event log, pending-report buffer. When empty the break-glass PUT
	// surface (/runtime/config/{key} with elevated auth) returns 503.
	DataDir string `yaml:"dataDir"`
	// Spill configures out-of-band body storage for audit captures: bodies
	// at/above the inline threshold are written to the configured backend
	// instead of inline'd onto traffic_event_payload. Disabled by default
	// (every body stays inline). All services in the deployment must point
	// at the same backend so CP's read path can resolve refs produced
	// here. See shared/spillstore/spillfactory.FactoryConfig.
	Spill spillfactory.FactoryConfig `yaml:"spill"`
}

// MQConfig holds message queue connection configuration.
type MQConfig struct {
	Driver string `yaml:"driver"`
	NATS   struct {
		URL string `yaml:"url"`
	} `yaml:"nats"`
}

// RegistryConfig controls service registration with Nexus Hub via thingclient.
type RegistryConfig struct {
	NexusHubURL string `yaml:"nexusHubUrl"`
}

// AuthConfig holds shared inter-service authentication settings.
//
// InternalServiceToken is env-only per the "Secrets are env-only" binding
// (CLAUDE.md). yaml:"-" prevents a stale yaml field from silently
// overriding the env value.
type AuthConfig struct {
	InternalServiceToken string `yaml:"-"` // env INTERNAL_SERVICE_TOKEN — Bearer on Hub WS/HTTP + X-RS-Token on ai-gateway /v1/ai-guard/classify; must match Hub's value
}

// RuntimeAPIConfig controls the Go runtime API HTTP server address.
type RuntimeAPIConfig struct {
	ListenAddress string `yaml:"listenAddress"` // default "127.0.0.1:3002"
}

// AlertingConfig controls the built-in alerting evaluator. Channel, threshold
// and custom-check state are authoritative in Hub shadow (runtimeapi-slimming);
// no channel or webhook fields live in YAML any more.
type AlertingConfig struct {
	Enabled         bool                   `yaml:"enabled"`
	EvalIntervalSec int                    `yaml:"evalIntervalSec"` // default 30
	Cooldown        AlertingCooldownConfig `yaml:"cooldown"`
}

// AlertingCooldownConfig controls minimum intervals between repeated notifications.
type AlertingCooldownConfig struct {
	FireMinutes    int `yaml:"fireMinutes"`    // min interval between fire notifications (default 5)
	ResolveMinutes int `yaml:"resolveMinutes"` // min interval between resolve notifications (default 5)
}

// ComplianceConfig controls the compliance hook pipeline.
type ComplianceConfig struct {
	Enabled          bool `yaml:"enabled"`
	PerHookTimeoutMs int  `yaml:"perHookTimeoutMs"` // default 5000
	TotalTimeoutMs   int  `yaml:"totalTimeoutMs"`   // default 15000
	ParallelHooks    bool `yaml:"parallelHooks"`    // default false (aligned with ai-gateway and agent)
	// StreamingMode YAML field deleted in #115 — admin policy
	// (system_metadata['streaming_compliance.config'].default_mode)
	// is now the single source of truth across all three services
	// (agent / compliance-proxy / ai-gateway). Operators changing
	// streaming mode go through CP admin UI / API, not yaml.
	CheckpointChars int                  `yaml:"checkpointChars"` // default 500
	Hooks           []HookConfigEntry    `yaml:"hooks"`           // static hook configs (ignored; DB is source of truth)
	RejectResponse  RejectResponseConfig `yaml:"rejectResponse"`
	// AttestationEnabled is a per-cluster feature flag. When true the
	// compliance-proxy peeks the X-Nexus-Attestation header on every CONNECT
	// and, on a verified signature, transparently tunnels the connection
	// (skipping MITM + the hook pipeline). Per the fail-open contract, an
	// attestation failure never rejects the request — CP falls through to
	// the existing MITM path.
	AttestationEnabled bool `yaml:"attestationEnabled"`
}

// RejectResponseConfig controls the verbosity and content of reject responses.
type RejectResponseConfig struct {
	DefaultLevel int    `yaml:"defaultLevel"` // 0, 1, or 2
	ContactInfo  string `yaml:"contactInfo"`
}

// HookConfigEntry is a YAML-level hook configuration entry.
type HookConfigEntry struct {
	ImplementationID  string                 `yaml:"implementationId"`
	Name              string                 `yaml:"name"`
	Priority          int                    `yaml:"priority"`
	Enabled           bool                   `yaml:"enabled"`
	Stage             string                 `yaml:"stage"`
	FailBehavior      string                 `yaml:"failBehavior"`
	TimeoutMs         int                    `yaml:"timeoutMs"`
	ApplicableIngress string                 `yaml:"applicableIngress"`
	Config            map[string]interface{} `yaml:"config"`
}

// AuditConfig controls audit event persistence and pinning behavior.
type AuditConfig struct {
	Enabled bool             `yaml:"enabled"`
	Batch   AuditBatchConfig `yaml:"batch"`
	NDJSON  NDJSONConfig     `yaml:"ndjson"`
	Pinning PinningCfg       `yaml:"pinning"`
}

// DatabaseConfig holds the top-level PostgreSQL connection. Symmetric
// with nexus-hub and control-plane. Env override: DATABASE_URL.
type DatabaseConfig struct {
	URL string `yaml:"url"`
}

// AuditBatchConfig controls async batch write behavior for the MQ writer.
type AuditBatchConfig struct {
	Size              int `yaml:"size"`
	FlushIntervalMs   int `yaml:"flushIntervalMs"`
	ChannelBufferSize int `yaml:"channelBufferSize"`
}

// NDJSONConfig controls the NDJSON fallback writer.
type NDJSONConfig struct {
	Enabled        bool   `yaml:"enabled"`
	Dir            string `yaml:"dir"`
	MaxFileSizeMB  int    `yaml:"maxFileSizeMB"`
	MaxTotalSizeMB int    `yaml:"maxTotalSizeMB"`
}

// PinningCfg controls certificate pinning detection and exemptions.
type PinningCfg struct {
	Exemptions []PinningExemption `yaml:"exemptions"`
	AutoExempt AutoExemptCfg      `yaml:"autoExempt"`
}

// PinningExemption represents an admin-configured bump exemption.
type PinningExemption struct {
	Host   string `yaml:"host"`
	Reason string `yaml:"reason"`
}

// AutoExemptCfg controls automatic exemption after repeated pinning failures.
type AutoExemptCfg struct {
	Enabled                  bool `yaml:"enabled"`
	FailureThreshold         int  `yaml:"failureThreshold"`
	WindowSeconds            int  `yaml:"windowSeconds"`
	ExemptionDurationSeconds int  `yaml:"exemptionDurationSeconds"`
}

// ListenerConfig defines the address for incoming CONNECT requests.
type ListenerConfig struct {
	Address string `yaml:"address"`
}

// CAConfig specifies paths to the enterprise sub-CA certificate and key.
// The KMS sub-block lets the operator wrap the on-disk key with an external
// key-management service so the raw private key never lives on disk in cleartext.
type CAConfig struct {
	CertPath string      `yaml:"certPath"`
	KeyPath  string      `yaml:"keyPath"`
	KMS      CAKMSConfig `yaml:"kms"`
}

// CAKMSConfig configures the optional KMS unwrap step. Provider can be left
// empty (or "noop") to use the raw-PEM behaviour.
//
// "command" provider runs an external process whose stdout is the
// plaintext PEM. The args list may contain the "{file}" placeholder which
// will be replaced with the on-disk ciphertext path before exec.
type CAKMSConfig struct {
	Provider   string   `yaml:"provider"`
	Command    []string `yaml:"command"`
	TimeoutSec int      `yaml:"timeoutSec"`
	// SigningMode: "local" (default) loads the key into memory via KMS decrypt.
	// "remote" uses a CommandSigner — the key never leaves KMS.
	SigningMode string `yaml:"signingMode"`
	// SignCommand is the argv for the remote signer. {file} in any arg is
	// replaced with the digest temp file path.

	SignCommand []string `yaml:"signCommand"`
}

// AccessControlConfig defines source-IP and domain allowlists.
type AccessControlConfig struct {
	SourceIPAllowlist         []string `yaml:"sourceIpAllowlist"`
	DomainAllowlist           []string `yaml:"domainAllowlist"`
	InternalNetworkExceptions []string `yaml:"internalNetworkExceptions"`
	// AllowUnlistedPassthrough downgrades the proxy from a strict compliance
	// gate to a transparent forward proxy for unlisted destinations. When
	// true, CONNECTs whose target is not in the domain allowlist are
	// tunneled as raw TCP (no MITM, no audit) instead of returning 403.
	// Other rejection reasons (IP allowlist, private IP, SNI mismatch) are
	// unaffected. Default false. See docs/developers/specs/e31/e31-s5-cp-unlisted-passthrough.md.
	AllowUnlistedPassthrough bool `yaml:"allowUnlistedPassthrough"`
}

// ConnectionsConfig controls concurrency and timeouts for client tunnels.
type ConnectionsConfig struct {
	MaxConcurrentTunnels    int    `yaml:"maxConcurrentTunnels"`
	MaxStreamsPerConnection int    `yaml:"maxStreamsPerConnection"`
	IdleTimeout             string `yaml:"idleTimeout"`
	ShutdownGracePeriod     string `yaml:"shutdownGracePeriod"`
}

// UpstreamConfig controls connections to upstream (destination) hosts.
type UpstreamConfig struct {
	MaxConnsPerHost int    `yaml:"maxConnsPerHost"`
	IdleConnTimeout string `yaml:"idleConnTimeout"`
	DialTimeout     string `yaml:"dialTimeout"`
}

// LimitsConfig sets body-size caps for requests and responses.
type LimitsConfig struct {
	RequestBodyLimit  string `yaml:"requestBodyLimit"`
	ResponseBodyLimit string `yaml:"responseBodyLimit"`
	SSEBufferLimit    string `yaml:"sseBufferLimit"`
}

// LogConfig controls the structured logging level. Renamed from the
// historical `LoggingConfig` (yaml `logging`) so the top-level key matches
// `log:` across all four services.
type LogConfig struct {
	Level        string `yaml:"level"`
	Format       string `yaml:"format"`       // json, text (default json when empty)
	File         string `yaml:"file"`         // optional: tee logs to this file (see also env LOG_FILE)
	StackOnError bool   `yaml:"stackOnError"` // attach goroutine stack on error-level logs (env LOG_STACK_ON_ERROR)
}

// MetricsConfig defines the address for the health/metrics HTTP server
// and the host Hub uses to reach it.
type MetricsConfig struct {
	// Address is the bind address (e.g. ":9090" or "0.0.0.0:9090") the
	// health/metrics HTTP server listens on.
	Address string `yaml:"address"`
	// AdvertiseHost is the host portion of the URL Hub uses to reach
	// /metrics + /debug/runtime (registered via thingclient as
	// `metricsUrl`). Empty defaults to 127.0.0.1, which is only correct
	// when Hub and Compliance Proxy run on the same host. Set
	// explicitly in non-localhost deployments.
	AdvertiseHost string `yaml:"advertiseHost"`
}

// Load reads + parses the YAML at path, applies env-var overrides, and
// validates business-required fields. Mirrors the Hub canonical loader
// (defaults → yaml → applyEnvOverrides → validate) so all four services
// share one shape — see docs/developers/architecture/cross-cutting/
// foundation/service-bootstrap-config-architecture.md §3. A missing file
// is tolerated and falls back to defaults + env, matching Hub/CP/AIG.
func Load(path string) (*Config, error) {
	cfg := defaults()

	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("config: read file %s: %w", path, err)
	}
	if err == nil {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("config: unmarshal YAML: %w", err)
		}
	}

	applyEnvOverrides(cfg)

	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("config: validation: %w", err)
	}
	return cfg, nil
}

// defaults seeds the non-secret tunables that every CP-proxy deployment
// inherits. Listener.Address / CA paths / DB URL deliberately have no
// safe default — operators must supply them via yaml or env.
func defaults() *Config {
	return &Config{
		Log: LogConfig{
			Level:  "info",
			Format: "json",
		},
	}
}

func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		cfg.Database.URL = v
	}
	if v := os.Getenv("COMPLIANCE_PROXY_PUBLIC_URL"); v != "" {
		cfg.PublicURL = v
	}
	if v := os.Getenv("NEXUS_HUB_URL"); v != "" {
		cfg.Registry.NexusHubURL = v
	}
	if v := os.Getenv("INTERNAL_SERVICE_TOKEN"); v != "" {
		cfg.Auth.InternalServiceToken = v
	}
	if v := os.Getenv("MQ_DRIVER"); v != "" {
		cfg.MQ.Driver = v
	}
	if v := os.Getenv("NATS_URL"); v != "" {
		cfg.MQ.NATS.URL = v
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		cfg.Log.Level = v
	}
	if v := os.Getenv("LOG_FORMAT"); v != "" {
		cfg.Log.Format = v
	}
}

// validate enforces the business-required configuration set for the
// Compliance Proxy. Required-set mirrors the cross-service contract in
// docs/developers/architecture/cross-cutting/foundation/
// service-bootstrap-config-architecture.md §5:
//
//   - PublicURL: reported as Thing staticInfo (CP-proxy is a Thing too).
//   - Database.URL: traffic_event writes, audit pipeline.
//   - Auth.InternalServiceToken: Bearer on Hub WS/HTTP + X-RS-Token on
//     ai-gateway /v1/ai-guard/classify; mismatch → all calls 403.
//   - Redis.Addrs: per-domain rate limit, idempotency, cache.
//   - MQ.Driver (+ MQ.NATS.URL when nats): traffic_event publish to Hub.
//   - Registry.NexusHubURL: CP-proxy registers as a Thing on boot.
//   - Listener.Address, CA.CertPath, CA.KeyPath: required to terminate
//     MITM-TLS on the inbound CONNECT path.
//
// Redis.Addrs accepts either yaml OR env (REDIS_ADDRS) — env-merge
// happens inside redisfactory.New at wiring time, not config.Load, so
// validate checks both.
func validate(cfg *Config) error {
	if cfg.PublicURL == "" {
		return fmt.Errorf("publicURL is required (reported to Thing Registry as staticInfo; admin UI uses it to render Compliance Proxy URLs)")
	}
	if cfg.Database.URL == "" {
		return fmt.Errorf("database.url is required")
	}
	if cfg.Auth.InternalServiceToken == "" {
		return fmt.Errorf("auth.internalServiceToken is required (env INTERNAL_SERVICE_TOKEN; must match Hub)")
	}
	if len(cfg.Redis.Addrs) == 0 && os.Getenv("REDIS_ADDRS") == "" {
		return fmt.Errorf("redis.addrs is required (set in yaml or via REDIS_ADDRS env)")
	}
	if cfg.MQ.Driver == "" {
		return fmt.Errorf("mq.driver is required (e.g. \"nats\")")
	}
	if cfg.MQ.Driver == "nats" && cfg.MQ.NATS.URL == "" {
		return fmt.Errorf("mq.nats.url is required when mq.driver=\"nats\"")
	}
	if cfg.Registry.NexusHubURL == "" {
		return fmt.Errorf("registry.nexusHubUrl is required (Compliance Proxy registers as a Thing on boot)")
	}

	// Listener — terminating MITM-TLS CONNECT.
	if cfg.Listener.Address == "" {
		return fmt.Errorf("listener.address must be set")
	}

	// CA paths — sub-CA cert + key for on-the-fly leaf issuance.
	if cfg.CA.CertPath == "" {
		return fmt.Errorf("ca.certPath must be non-empty")
	}
	if cfg.CA.KeyPath == "" {
		return fmt.Errorf("ca.keyPath must be non-empty")
	}

	// Connection timeouts
	for label, raw := range map[string]string{
		"connections.idleTimeout":         cfg.Connections.IdleTimeout,
		"connections.shutdownGracePeriod": cfg.Connections.ShutdownGracePeriod,
		"upstream.idleConnTimeout":        cfg.Upstream.IdleConnTimeout,
		"upstream.dialTimeout":            cfg.Upstream.DialTimeout,
	} {
		if raw == "" {
			continue
		}
		d, err := ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		if d <= 0 {
			return fmt.Errorf("%s must be > 0", label)
		}
	}

	// Body-size limits
	for label, raw := range map[string]string{
		"limits.requestBodyLimit":  cfg.Limits.RequestBodyLimit,
		"limits.responseBodyLimit": cfg.Limits.ResponseBodyLimit,
		"limits.sseBufferLimit":    cfg.Limits.SSEBufferLimit,
	} {
		if raw == "" {
			continue
		}
		sz, err := ParseByteSize(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		if sz <= 0 {
			return fmt.Errorf("%s must be > 0", label)
		}
	}

	// Log level
	switch strings.ToLower(cfg.Log.Level) {
	case "trace", "debug", "info", "warn", "error", "":
		// valid
	default:
		return fmt.Errorf("log.level must be one of trace, debug, info, warn, error; got %q", cfg.Log.Level)
	}

	if f := strings.TrimSpace(cfg.Log.Format); f != "" {
		switch strings.ToLower(f) {
		case "json", "text":
		default:
			return fmt.Errorf("log.format must be json or text; got %q", cfg.Log.Format)
		}
	}

	// streamingMode validation deleted in #115 — admin policy is the
	// single source of truth; validation lives in
	// streampolicy.DecodeGlobalPolicy.

	return nil
}

// byteSizeRe matches a number followed by an optional unit suffix.
var byteSizeRe = regexp.MustCompile(`(?i)^\s*(\d+)\s*(B|KB|MB|GB|TB)?\s*$`)

// ParseByteSize converts a human-readable byte-size string (e.g. "10MB")
// into the equivalent number of bytes. Supported suffixes: B, KB, MB, GB, TB.
// If no suffix is given the value is treated as bytes.
func ParseByteSize(s string) (int64, error) {
	m := byteSizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("invalid byte size %q", s)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid byte size number %q: %w", m[1], err)
	}

	unit := strings.ToUpper(m[2])
	switch unit {
	case "", "B":
		// already bytes
	case "KB":
		n *= 1024
	case "MB":
		n *= 1024 * 1024
	case "GB":
		n *= 1024 * 1024 * 1024
	case "TB":
		n *= 1024 * 1024 * 1024 * 1024
	}
	return n, nil
}

// ParseDuration is a thin wrapper around time.ParseDuration that produces
// a friendlier error message for the config context.
func ParseDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	return d, nil
}

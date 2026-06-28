// Package config loads and holds the ai-gateway configuration.
package config

import (
	"context"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/forwardheader"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/kms"
	cfgpolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/redisfactory"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore/spillfactory"
)

// Config is the top-level ai-gateway configuration.
type Config struct {
	// ID is an optional operator-supplied stable identifier for this
	// service instance. When non-empty it is sent to the Hub as
	// `physicalId` at register time and persisted into
	// `thing.physical_id`, giving ops a stable handle independent of the
	// auto-derived `gw-<hostname>-<port>` thing_id. Leave blank in dev or
	// when the auto-derived id is good enough.
	ID string `yaml:"id,omitempty"`
	// PublicURL is the externally-reachable base URL clients use to
	// reach this service (scheme + host[:port], no trailing slash).
	// Prod: "https://api.example.com"; dev: "http://localhost:3050".
	// Reported to the Thing Registry as part of staticInfo so the CP
	// admin API can surface the real endpoint to UI pages.
	PublicURL string              `yaml:"publicURL"`
	Server    ServerConfig        `yaml:"server"`
	Database  DatabaseConfig      `yaml:"database"`
	Redis     redisfactory.Config `yaml:"redis"`
	Auth      AuthConfig          `yaml:"auth"`
	Log       LogConfig           `yaml:"log"`
	Registry  RegistryConfig      `yaml:"registry"`
	MQ        MQConfig            `yaml:"mq"`
	CORS      CORSConfig          `yaml:"cors"`
	Cache     CacheConfig         `yaml:"cache"`
	Otel      OtelConfig          `yaml:"otel"`
	// Audit configures the traffic_event audit pipeline's durable spill: when
	// the in-memory buffer is full after backpressure, overflow records are
	// written here as NDJSON instead of dropped silently.
	Audit AuditConfig `yaml:"audit"`
	// Spill configures out-of-band body storage for audit captures: bodies
	// at/above the inline threshold are written to the configured backend
	// instead of inline'd onto traffic_event_payload. Disabled by default
	// (every body stays inline regardless of size). See
	// shared/spillstore/spillfactory.FactoryConfig for field semantics.
	Spill spillfactory.FactoryConfig `yaml:"spill"`
	// SecretCustody is the server-side envelope-custody config. provider "noop"
	// (default) reads the crown-jewel secrets raw from env;
	// provider "command" reads them as base64 wrapped blobs and unwraps each once
	// at boot via the configured KMS/sops/age/vault command. yaml/argv only — no
	// secret here. Covers ADMIN_KEY_HMAC_SECRET / CREDENTIAL_ENCRYPTION_KEY /
	// CREDENTIAL_KEY_MAP. [MUST MATCH] the Control Plane on the unwrapped plaintext.
	SecretCustody kms.CustodyConfig `yaml:"secretCustody"`
	// Upstream tunes the HTTP client every provider adapter uses to call
	// the LLM upstream (OpenAI, Anthropic, Gemini, Bedrock, ZhipuAI, ...).
	// When the upstream call exceeds Upstream.TimeoutSec, ai-gateway returns
	// 504 Gateway Timeout. Defaults are seeded in Load() so omitting the
	// block keeps the previous hardcoded behavior.
	Upstream UpstreamConfig `yaml:"upstream"`
	// HTTPClients tunes the named HTTP clients ai-gateway constructs at
	// startup that are not the per-provider upstream client (which lives
	// under [Upstream]). All values fall back to historical defaults when
	// omitted.
	HTTPClients HTTPClientsConfig `yaml:"httpClients"`
	// Routing holds platform-wide routing defaults. Per-rule overrides
	// (e.g. RoutingRule.RetryPolicy) field-merge on top of these defaults
	// at evaluation time.
	Routing Routing `yaml:"routing"`
	// ForwardHeaders configures the request- and response-side
	// HTTP header allowlist applied when proxying to upstream
	// providers and when surfacing upstream responses to clients.
	// When nil (operator's YAML has no `forwardHeaders:` block) the
	// gateway falls back to the embedded defaults that reproduce the
	// historical hard-coded behavior; see
	// docs/developers/specs/e36/e36-s1-forward-header-yaml-request.md.
	ForwardHeaders *forwardheader.Config `yaml:"forwardHeaders,omitempty"`
	// Observability holds operator-side instrumentation toggles that
	// don't merit the full shadow-config dance (yaml-only, redeploy to
	// flip). Per configuration-architecture.md: yaml is for service-
	// shape and non-secret tunings.
	Observability ObservabilityConfig `yaml:"observability"`
}

// ObservabilityConfig groups ai-gateway instrumentation toggles.
//
// LatencyDetail controls whether the per-request `latency_breakdown`
// JSONB on traffic_event captures sub-millisecond phases (floored to
// 1ms) in addition to phases that took >= 1ms. Default false: compact
// rows for normal ops. Flip to true during a perf-investigation window
// so phases that round to 0ms (e.g. PhaseAuth, PhaseQuota) still show
// up as evidence of "yes this phase ran, just very fast".
type ObservabilityConfig struct {
	LatencyDetail bool `yaml:"latencyDetail"`
}

// Routing groups platform-wide routing defaults. Each field is overridable
// per RoutingRule via the same field-merge semantics
// (cfgpolicy.RetryPolicy.MergedWith).
type Routing struct {
	// DefaultRetryPolicy is the platform default applied when a routing rule
	// does not set its own RetryPolicy (or only sets a subset of fields).
	// Load() field-merges this against cfgpolicy.DefaultRetryPolicy() so
	// admins can specify only the knobs they want to change.
	DefaultRetryPolicy cfgpolicy.RetryPolicy `yaml:"defaultRetryPolicy"`
}

// HTTPClientsConfig groups ai-gateway's named HTTP clients.
type HTTPClientsConfig struct {
	// Webhook is the shared client used by the webhook-forward hook to
	// post compliance events to an external endpoint. Per-hook timeout
	// overrides may apply on top via HookConfig.TimeoutMs.
	Webhook HTTPClientPoolConfig `yaml:"webhook"`
	// External is the client used by /v1/ai-guard/classify when the
	// configured backend mode is "external_url" — i.e. an operator-provided
	// classifier endpoint instead of a regular LLM provider.
	External HTTPClientConfig `yaml:"external"`
}

// HTTPClientConfig is the minimal shape: a single timeout knob.
type HTTPClientConfig struct {
	TimeoutSec int `yaml:"timeoutSec"`
}

// HTTPClientPoolConfig is HTTPClientConfig + connection pool tunables for
// clients that are reused across many requests.
type HTTPClientPoolConfig struct {
	TimeoutSec          int `yaml:"timeoutSec"`
	MaxIdleConns        int `yaml:"maxIdleConns"`
	MaxIdleConnsPerHost int `yaml:"maxIdleConnsPerHost"`
	IdleConnTimeoutSec  int `yaml:"idleConnTimeoutSec"`
}

// UpstreamConfig tunes the HTTP transport used by every provider adapter
// when calling LLM upstreams. All values are in seconds; zero means "use
// the default seeded in Load()".
type UpstreamConfig struct {
	// TimeoutSec is the full request budget for one upstream call. The
	// http.Client.Timeout is set to TimeoutSec + 5s so a request that
	// blows the per-Do context deadline still returns a clean error
	// rather than racing the client-level timer.
	TimeoutSec int `yaml:"timeoutSec"`
	// StreamIdleTimeoutSec is the inter-chunk silence budget for a streaming
	// response. The downstream write deadline resets on every chunk, so an
	// actively-producing stream runs unbounded and only a stalled upstream is
	// cut after this much quiet. Zero falls back to the code default.
	StreamIdleTimeoutSec int `yaml:"streamIdleTimeoutSec"`
	// DialTimeoutSec is the TCP connect budget per Dial.
	DialTimeoutSec int `yaml:"dialTimeoutSec"`
	// TLSHandshakeTimeoutSec is the TLS handshake budget per Dial.
	TLSHandshakeTimeoutSec int `yaml:"tlsHandshakeTimeoutSec"`
	// KeepAliveSec is the TCP keep-alive interval on dialed connections.
	KeepAliveSec int `yaml:"keepAliveSec"`
	// IdleConnTimeoutSec is how long an idle pooled connection survives
	// before being closed.
	IdleConnTimeoutSec int `yaml:"idleConnTimeoutSec"`
	// MaxIdleConns is the global pool cap.
	MaxIdleConns int `yaml:"maxIdleConns"`
	// MaxIdleConnsPerHost is the per-host idle pool cap.
	MaxIdleConnsPerHost int `yaml:"maxIdleConnsPerHost"`
	// MaxConnsPerHost caps TOTAL connections per upstream host; 0 inherits the
	// shared-client default (100) which throttles upstream concurrency — the
	// default below pins it high (mirrors Bifrost's 5000). Env:
	// AI_GATEWAY_UPSTREAM_MAX_CONNS_PER_HOST.
	MaxConnsPerHost int `yaml:"maxConnsPerHost"`
}

// CORSConfig holds Cross-Origin Resource Sharing settings.
type CORSConfig struct {
	Enabled        bool     `yaml:"enabled"`
	AllowedOrigins []string `yaml:"allowedOrigins"`
	AllowedMethods []string `yaml:"allowedMethods"`
	AllowedHeaders []string `yaml:"allowedHeaders"`
	MaxAgeSec      int      `yaml:"maxAgeSec"`
}

// CacheConfig controls the response cache.
type CacheConfig struct {
	Enabled bool          `yaml:"enabled"`
	TTL     time.Duration `yaml:"ttl"`
	Prefix  string        `yaml:"prefix"`
	// Broker controls in-flight dedupe of same-cache-key MISS calls.
	// When false (default), every MISS opens its own upstream session
	// even if a same-key call is already running — optimised for low p99
	// under bursty parallel load. Trade-off: N concurrent same-key MISS
	// callers fire N upstream calls instead of sharing one, and the MISS
	// response is NOT written to the response cache (fills via the broker
	// pump only). Flip to true to restore 1-leader-N-joiners semantics +
	// cache fill at the cost of serialising same-key concurrency over a
	// single leaderFn (~upstream TTFB per request).
	Broker bool `yaml:"broker"`
}

// OtelConfig holds file-level OpenTelemetry settings. Runtime
// toggles (enabled, samplingRate) are stored in the system_metadata
// `observability.config` row and override these defaults.
type OtelConfig struct {
	Endpoint    string `yaml:"endpoint"`
	ServiceName string `yaml:"serviceName"`
}

// MQConfig holds message queue connection configuration.
type MQConfig struct {
	Driver string `yaml:"driver"`
	NATS   struct {
		URL string `yaml:"url"`
	} `yaml:"nats"`
}

// LogConfig controls logging behaviour.
type LogConfig struct {
	Level        string `yaml:"level"`        // trace, debug, info, warn, error (default: info)
	Format       string `yaml:"format"`       // json, text (default: json)
	File         string `yaml:"file"`         // optional: tee logs to this file (see also env LOG_FILE)
	StackOnError bool   `yaml:"stackOnError"` // attach goroutine stack on error-level logs (env LOG_STACK_ON_ERROR)
}

// ServerConfig is the HTTP server configuration.
type ServerConfig struct {
	Port         int           `yaml:"port"`
	ReadTimeout  time.Duration `yaml:"readTimeout"`
	WriteTimeout time.Duration `yaml:"writeTimeout"`
	// Host is the bind interface for the HTTP server. Empty (default) binds
	// all interfaces (":port") — what container / Kubernetes / direct
	// deployments need. Set to "127.0.0.1" to bind loopback-only (the
	// single-host appliance, where nginx fronts /v1 and the unauthenticated
	// /internal/* debug surface must not be externally reachable).
	Host string `yaml:"host"`
	// AdvertiseHost is the host portion of the URL Hub uses to reach this
	// service's /metrics + /debug/runtime endpoints (registered via
	// thingclient as `metricsUrl`). Empty defaults to 127.0.0.1, which is
	// only correct when Hub and AI Gateway run on the same host. Set
	// explicitly in non-localhost deployments.
	AdvertiseHost string `yaml:"advertiseHost"`

	// RequestReadBufKB pre-grows the pooled request-body read scratch to this
	// many KiB so reading a typical body completes with no bytes.Buffer
	// doubling+re-copy on the hot path. Tune to the deployment's typical body
	// size: 64 (default — conservative) suits a mixed/small-prompt fleet; raise
	// it (e.g. 128) when requests routinely carry ~128K-token contexts. FIXED
	// pre-grow, independent of the client Content-Length (the read stays bounded
	// by the maxBytes limit, never by a caller-supplied length, so a hostile
	// Content-Length cannot drive a large up-front allocation). 0 falls back to
	// the 64 default. Env: AI_GATEWAY_REQUEST_READ_BUF_KB.
	RequestReadBufKB int `yaml:"requestReadBufKb"`
}

// BindAddr returns the host:port the HTTP server listens on. Empty Host yields
// ":port" (all interfaces), preserving the historical default.
func (s ServerConfig) BindAddr() string {
	return fmt.Sprintf("%s:%d", s.Host, s.Port)
}

// DatabaseConfig and AuditConfig (the high-concurrency pool / spill tuning
// types) live in config_pools.go.

// RegistryConfig holds Hub connection settings for thingclient registration.
type RegistryConfig struct {
	NexusHubURL string `yaml:"nexusHubUrl"`
}

// AuthConfig holds authentication settings.
//
// All fields are env-only per the "Secrets are env-only" binding in
// CLAUDE.md. The yaml:"-" tags exist so a stale yaml field can't silently
// override the env value; values are populated by Load() from environment
// variables. See `.env.example` at repo root for the full contract.
type AuthConfig struct {
	HMACSecret string `yaml:"-"` // env ADMIN_KEY_HMAC_SECRET — single-version HMAC secret
	// HMACKeyMap is the versioned HMAC keyring (env ADMIN_KEY_HMAC_KEY_MAP,
	// "[*]vN:secret,…"). When set it supersedes HMACSecret:
	// the VK admission path tries every version (current-first) so a rotated VK
	// keeps admitting until re-issue/expiry. Either this OR HMACSecret is
	// required. [MUST MATCH] the Control Plane (same version's secret → same VK
	// hash).
	HMACKeyMap           string `yaml:"-"`
	CredentialMasterKey  string `yaml:"-"` // env CREDENTIAL_ENCRYPTION_KEY
	CredentialKeyMap     string `yaml:"-"` // env CREDENTIAL_KEY_MAP — multi-key "v1:hex64,v2:hex64"; takes precedence over single key
	InternalServiceToken string `yaml:"-"` // env INTERNAL_SERVICE_TOKEN — Bearer for Hub WS/HTTP + X-RS-Token on /v1/ai-guard/classify; must match Hub
}

// Load reads + parses the YAML at path, applies env-var overrides, and
// validates business-required fields. Mirrors the Hub canonical loader
// (defaults → yaml → applyEnvOverrides → validate) so all four services
// share one shape — see docs/developers/architecture/cross-cutting/
// foundation/service-bootstrap-config-architecture.md §3.
func Load(path string) (*Config, error) {
	cfg := defaults()

	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err == nil {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}

	applyEnvOverrides(cfg)

	// Field-merge routing.defaultRetryPolicy against the platform default so
	// partial YAML blocks (or no block at all) still produce a usable policy.
	// Clamp MaxAttemptsPerTarget to the supported [1,5] range. Runs after
	// env overrides so any env-future override would also flow through here.
	cfg.Routing.DefaultRetryPolicy = cfgpolicy.DefaultRetryPolicy().MergedWith(&cfg.Routing.DefaultRetryPolicy)
	cfg.Routing.DefaultRetryPolicy.MaxAttemptsPerTarget = cfgpolicy.ClampMaxAttempts(cfg.Routing.DefaultRetryPolicy.MaxAttemptsPerTarget)

	if err := resolveCustodySecrets(cfg); err != nil {
		return nil, fmt.Errorf("resolve custody secrets: %w", err)
	}
	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	return cfg, nil
}

func defaults() *Config {
	return &Config{
		Server: ServerConfig{
			Port:             8080,
			ReadTimeout:      30 * time.Second,
			WriteTimeout:     60 * time.Second,
			RequestReadBufKB: 64,
		},
		Database: DatabaseConfig{
			MaxConns:           25,
			MinConns:           5,
			MaxConnLifetimeSec: 300,
		},
		Audit: AuditConfig{
			SpoolDir:       "/var/lib/nexus/audit-spool",
			SpoolMaxFileMB: 256,
			// 50 GiB. The durable spool IS the no-loss buffer for spill-defer: at
			// peak the request path appends here instead of back-pressuring, and the
			// recovery sweeper drains it to Postgres. Disk is cheap relative to
			// dropping audit (the product promise), so the default is sized to absorb
			// a large burst, not a token 512 MB that overflows in seconds at 8k RPS.
			// Tune to the data disk via AI_GATEWAY_AUDIT_SPOOL_MAX_TOTAL_MB.
			SpoolMaxTotalMB:  51200,
			MaxQueuedRecords: 10000,
			// Default spillblock: the request path never back-pressures on audit
			// while the durable on-disk spool has room — overflow goes to the spool
			// and the spill-recovery sweeper replays it to PG. Unlike plain spill,
			// when the spool channel ITSELF saturates spillblock back-pressures the
			// request goroutine instead of taking spill's last-resort bounded drop,
			// so it is genuinely zero-loss (durable audit is a product + compliance
			// promise — never silently drop a record). In the normal regime it is
			// identical to spill (same RPS, off the request path); it differs only
			// at the extreme where spill would drop. block (hard synchronous
			// back-pressure) and spill/drop (lossy) remain selectable via
			// AI_GATEWAY_AUDIT_LOSS_MODE.
			LossMode:         "spillblock",
			Compress:         true,
			CompressMinBytes: 0, // 0 → shared/audit DefaultInlineCompressionMinBytes (1024)
			CompressLevel:    0, // 0 → library default (SpeedDefault)
			// 0 → defaults (2000 ms sweep / 50 ms pace) applied at wiring; negative
			// interval disables recovery.
			SpillRecoveryIntervalMs: 0,
			SpillRecoveryPaceMs:     0,
		},
		Upstream: UpstreamConfig{
			TimeoutSec:             600,
			StreamIdleTimeoutSec:   120,
			DialTimeoutSec:         15,
			TLSHandshakeTimeoutSec: 10,
			KeepAliveSec:           30,
			IdleConnTimeoutSec:     300,
			MaxIdleConns:           2000,
			MaxIdleConnsPerHost:    500,
			MaxConnsPerHost:        5000,
		},
		HTTPClients: HTTPClientsConfig{
			Webhook: HTTPClientPoolConfig{
				TimeoutSec:          10,
				MaxIdleConns:        20,
				MaxIdleConnsPerHost: 5,
				IdleConnTimeoutSec:  90,
			},
			External: HTTPClientConfig{
				TimeoutSec: 30,
			},
		},
	}
}

// resolveCustodySecrets unwraps the crown-jewel root secrets through the
// SecretCustody provider. provider "noop" returns each
// secret's raw env value (byte-identical to os.Getenv, preserving today's
// behavior); provider "command" unwraps a base64 wrapped blob once at boot. The
// unwrapped plaintext MUST match the Control Plane's (the [MUST MATCH] contract
// is on the plaintext, not the wrapped blob — each service wraps under its own
// grant). Empty stays empty (validate() enforces the required ones); a non-empty
// secret that fails to unwrap aborts the boot.
func resolveCustodySecrets(cfg *Config) error {
	custody, err := kms.NewCustody(cfg.SecretCustody)
	if err != nil {
		return err
	}
	ctx := context.Background()
	for _, s := range []struct {
		env string
		dst *string
	}{
		{"ADMIN_KEY_HMAC_SECRET", &cfg.Auth.HMACSecret},
		{"ADMIN_KEY_HMAC_KEY_MAP", &cfg.Auth.HMACKeyMap},
		{"CREDENTIAL_ENCRYPTION_KEY", &cfg.Auth.CredentialMasterKey},
		{"CREDENTIAL_KEY_MAP", &cfg.Auth.CredentialKeyMap},
	} {
		v, err := custody.Unwrap(ctx, s.env)
		if err != nil {
			return err
		}
		if v != "" {
			*s.dst = v
		}
	}
	return nil
}

// validate enforces the business-required configuration set for the
// AI Gateway. Required-set mirrors the cross-service contract in
// docs/developers/architecture/cross-cutting/foundation/
// service-bootstrap-config-architecture.md §5:
//
//   - PublicURL: reported as Thing staticInfo so admin UI can render the
//     gateway URL (integration help cards, smoke-test instructions).
//   - Database.URL: traffic_event writes, VK lookups, rate-limit counters.
//   - Auth.InternalServiceToken: Bearer for Hub WS/HTTP + X-RS-Token on
//     /v1/ai-guard/classify; mismatch with Hub → all calls 403.
//   - Auth.HMACSecret: hashes VK + Admin API keys before DB lookup; with
//     a wrong/empty secret every authenticated request fails.
//   - Auth.CredentialMasterKey: decrypts provider credentials pushed by
//     Hub; without it no upstream provider call can be made.
//   - Redis.Addrs: response cache, rate limit, idempotency, quota.
//   - MQ.Driver (+ MQ.NATS.URL when nats): traffic_event publish to Hub.
//   - Registry.NexusHubURL: AIG registers as a Thing on boot.
//
// Redis.Addrs accepts either yaml OR env (REDIS_ADDRS) — env-merge
// happens inside redisfactory.New at wiring time, not config.Load, so
// validate checks both.
func validate(cfg *Config) error {
	if cfg.PublicURL == "" {
		return fmt.Errorf("publicURL is required (reported to Thing Registry as staticInfo; admin UI uses it to render AI Gateway URLs)")
	}
	if cfg.Database.URL == "" {
		return fmt.Errorf("database.url is required")
	}
	if cfg.Auth.InternalServiceToken == "" {
		return fmt.Errorf("auth.internalServiceToken is required (env INTERNAL_SERVICE_TOKEN; must match Hub)")
	}
	if cfg.Auth.HMACSecret == "" && cfg.Auth.HMACKeyMap == "" {
		return fmt.Errorf("an HMAC secret is required: set ADMIN_KEY_HMAC_SECRET (single) or ADMIN_KEY_HMAC_KEY_MAP (versioned \"[*]vN:secret\" keyring); hashes VK + Admin API keys before DB lookup; [MUST MATCH] Control Plane")
	}
	if cfg.Auth.CredentialMasterKey == "" && cfg.Auth.CredentialKeyMap == "" {
		// Either the single master key OR the multi-key map satisfies the
		// requirement: CREDENTIAL_KEY_MAP is documented as standalone
		// ("takes precedence when present"), and wiring builds a map-only
		// decryptor correctly — but validate() previously demanded the single
		// key unconditionally, making map-only mode non-bootable.
		return fmt.Errorf("a credential decryption key is required: set CREDENTIAL_ENCRYPTION_KEY (single hex key) or CREDENTIAL_KEY_MAP (multi-key \"v1:hex64,v2:hex64\"); both decrypt Hub-pushed provider credentials")
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
		return fmt.Errorf("registry.nexusHubUrl is required (AI Gateway registers as a Thing on boot)")
	}
	return nil
}

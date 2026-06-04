// Package config loads and validates the control-plane configuration from
// a YAML file and environment variable overrides.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/redisfactory"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore/spillfactory"
)

// Config holds all control-plane configuration.
type Config struct {
	// ID is an optional operator-supplied stable identifier for this
	// service instance. When non-empty it is sent to the Hub as
	// `physicalId` at register time and persisted into
	// `thing.physical_id`, giving ops a stable handle independent of the
	// auto-derived thing_id. Leave blank when the auto-derived id is good
	// enough.
	ID string `yaml:"id,omitempty"`
	// PublicURL is the externally-reachable base URL clients use to
	// reach this service (scheme + host[:port], no trailing slash).
	// Prod: "https://nexus.example.com"; dev: "http://localhost:3001".
	// Reported to the Thing Registry as part of staticInfo so the
	// agent-setup page (and any other page that needs an
	// environment-aware URL) renders from real config rather than
	// hardcoded hostnames.
	PublicURL   string              `yaml:"publicURL"`
	Server      ServerConfig        `yaml:"server"`
	Database    DatabaseConfig      `yaml:"database"`
	Redis       redisfactory.Config `yaml:"redis"`
	Log         LogConfig           `yaml:"log"`
	BFF         BFFConfig           `yaml:"bff"`
	Registry    RegistryConfig      `yaml:"registry"`
	Auth        AuthConfig          `yaml:"auth"`
	Crypto      CryptoConfig        `yaml:"crypto"`
	Agent       AgentConfig         `yaml:"agent"`
	Otel        OtelConfig          `yaml:"otel"`
	MQ          MQConfig            `yaml:"mq"`
	AuthServer  AuthServerConfig    `yaml:"authServer"`
	AIGuard     AIGuardConfig       `yaml:"aiGuard"`
	HTTPClients HTTPClientsConfig   `yaml:"httpClients"`
	// Spill is the YAML-side spillstore configuration. CP reads this so
	// the GetTrafficEvent detail handler can resolve out-of-band body
	// payloads (large captured request/response bodies that were spilled
	// by the data plane). Disabled when omitted; the same shape is used
	// by ai-gateway / compliance-proxy / agent on the producer side.
	Spill spillfactory.FactoryConfig `yaml:"spill"`
	// CostPolicy mirrors Hub's billed-cost policy so the admin UI can
	// render the correct "internal ops included/excluded" hint without a
	// Hub round-trip on every drawer open. Operators MUST keep both yamls
	// in sync — drift causes the UI label to misrepresent Hub's rollup.
	CostPolicy CostPolicyConfig `yaml:"costPolicy"`
}

// CostPolicyConfig surfaces the same toggles the Hub scheduler reads, so the
// CP admin API can return them to the UI for display-purpose hints.
type CostPolicyConfig struct {
	// ExcludeInternalOpsFromBilledCost — see SchedulerConfig in
	// packages/nexus-hub/internal/config/config.go for the canonical
	// description. Default false: include internal ops in billed cost.
	ExcludeInternalOpsFromBilledCost bool `yaml:"excludeInternalOpsFromBilledCost"`
}

// AuthServerConfig holds settings for the OAuth/OIDC auth server mounted by
// Control Plane (JWKS, discovery, /oauth/* endpoints).
type AuthServerConfig struct {
	// Issuer is the canonical iss claim used in discovery and signed tokens.
	// Must be set in production; a zero value makes DiscoveryHandler 500 and
	// is considered a wiring bug.
	Issuer string `yaml:"issuer"`
	// KeystoreDir is the on-disk directory that holds RSA signing keys (one
	// PEM per kid). Defaults to ".nexus/authkeys" when unset. Relative paths
	// are resolved against the process working directory at startup.
	KeystoreDir string `yaml:"keystoreDir"`
	// RevocationIntrospectURL overrides the introspect endpoint the admin
	// revocation checker calls when the MQ stream is unavailable. Defaults to
	// Issuer + "/oauth/introspect" when empty.
	RevocationIntrospectURL string `yaml:"revocationIntrospectUrl"`
	// RevocationReplayURL overrides the catchup endpoint the admin revocation
	// checker polls on MQ reconnect. Defaults to Issuer + "/api/admin/revocations"
	// when empty.
	RevocationReplayURL string `yaml:"revocationReplayUrl"`
}

// MQConfig holds message queue connection configuration.
type MQConfig struct {
	Driver string `yaml:"driver"`
	NATS   struct {
		URL string `yaml:"url"`
	} `yaml:"nats"`
}

// OtelConfig holds OpenTelemetry settings.
type OtelConfig struct {
	Endpoint    string `yaml:"endpoint"`
	ServiceName string `yaml:"serviceName"`
}

// ServerConfig controls the HTTP listener.
type ServerConfig struct {
	Port            int           `yaml:"port"`
	ShutdownTimeout time.Duration `yaml:"shutdownTimeout"`
	// AdvertiseHost is the host portion of the URL Hub uses to reach this
	// service's /metrics + /debug/runtime endpoints (registered via
	// thingclient as `metricsUrl`). Empty defaults to 127.0.0.1, which is
	// only correct when Hub and Control Plane run on the same host. Set
	// explicitly in non-localhost deployments.
	AdvertiseHost string `yaml:"advertiseHost"`
}

// DatabaseConfig holds the PostgreSQL DSN and pgxpool tunables (uniform with
// nexus-hub, ai-gateway, compliance-proxy).
type DatabaseConfig struct {
	URL             string        `yaml:"url"`
	MaxConns        int32         `yaml:"maxConns"`
	MinConns        int32         `yaml:"minConns"`
	MaxConnLifetime time.Duration `yaml:"maxConnLifetime"`
}

// LogConfig controls logging behaviour.
type LogConfig struct {
	Level        string `yaml:"level"`        // debug, info, warn, error
	Format       string `yaml:"format"`       // json, text
	File         string `yaml:"file"`         // optional: tee logs to this file (see also env LOG_FILE)
	StackOnError bool   `yaml:"stackOnError"` // attach goroutine stack on error-level logs (env LOG_STACK_ON_ERROR)
}

// BFFConfig holds addresses for data-plane services that the BFF proxies to.
// URLs stay in yaml (service-discovery shape, not secrets); the api token is
// env-only per the "Secrets are env-only" binding and must match what
// compliance-proxy itself reads from the same env var (single source of
// truth: a 403 between CP and compliance-proxy means these two values
// drifted apart).
type BFFConfig struct {
	ComplianceProxyURL        string `yaml:"complianceProxyUrl"`
	AIGatewayURL              string `yaml:"aiGatewayUrl"`
	ComplianceProxyRuntimeURL string `yaml:"complianceProxyRuntimeUrl"`
	ComplianceProxyAPIToken   string `yaml:"-"` // env COMPLIANCE_PROXY_API_TOKEN — shared with compliance-proxy/runtimeapi auth
}

// RegistryConfig holds Hub connection settings for thingclient registration.
// Symmetric with the same-named block on ai-gateway and compliance-proxy.
// Moved out of BFFConfig (which carries downstream-call URLs) because
// "where I register" is semantically different from "where I forward
// admin UI calls to".
type RegistryConfig struct {
	NexusHubURL string `yaml:"nexusHubUrl"`
}

// AIGuardConfig holds Control Plane–side AI Guard settings. Backend-side
// settings (judge model, prompt template, in-DB judge timeout) live in
// the ai_guard_config table and are edited via the admin UI; the values
// below are infrastructure knobs that only affect how Control Plane
// itself talks to ai-gateway.
type AIGuardConfig struct {
	// DispatchTimeoutSec is the http.Client.Timeout used by the
	// /api/admin/ai-guard/dry-run handler when posting to ai-gateway
	// /v1/ai-guard/classify. Should be greater than the in-DB
	// ai_guard_config.timeout_ms (judge-call budget) plus a small
	// network/serde slack — otherwise this client fires before the
	// judge call can return and surfaces "Client.Timeout exceeded
	// while awaiting headers" instead of the real backend error.
	DispatchTimeoutSec int `yaml:"dispatchTimeoutSec"`
}

// HTTPClientsConfig groups Control Plane's named HTTP clients. Each
// purpose has its own timeout because the call patterns differ
// (admin-UI passthrough vs. control-plane → Hub control RPC vs. ...).
type HTTPClientsConfig struct {
	// Hub is the timeout for the typed CP → Nexus Hub client (registry,
	// jobs, scheduler, things). Used for control-plane operations that
	// may run a few seconds on Hub side; not interactive UI passthrough.
	Hub HTTPClientConfig `yaml:"hub"`
	// HubProxy is the timeout for the /api/admin/hub/* proxy passthrough
	// the admin UI uses to read Hub data. Interactive — keep short so
	// the UI doesn't hang on a misbehaving Hub.
	HubProxy HTTPClientConfig `yaml:"hubProxy"`
	// ComplianceProxyAdmin is the timeout for the /api/admin/proxy/*
	// passthrough to compliance-proxy's runtime admin API. Interactive.
	ComplianceProxyAdmin HTTPClientConfig `yaml:"complianceProxyAdmin"`
}

// HTTPClientConfig is the minimal shape: a single timeout knob.
type HTTPClientConfig struct {
	TimeoutSec int `yaml:"timeoutSec"`
}

// AuthConfig holds authentication settings.
//
// All fields are env-only per the "Secrets are env-only" binding (CLAUDE.md).
// yaml:"-" prevents a stale yaml field from silently overriding the env value.
type AuthConfig struct {
	HMACSecret           string `yaml:"-"` // env ADMIN_KEY_HMAC_SECRET — hashes VK/Admin API keys
	InternalServiceToken string `yaml:"-"` // env INTERNAL_SERVICE_TOKEN — thingclient + hubclient; must match Hub
}

// CryptoConfig holds credential encryption settings.
//
// EncryptionKey / EncryptionPassphrase / EncryptionSalt / CredentialKeyMap
// are secrets (env-only). Production is a feature flag — stays in yaml.
type CryptoConfig struct {
	EncryptionKey        string `yaml:"-"`          // env CREDENTIAL_ENCRYPTION_KEY
	EncryptionPassphrase string `yaml:"-"`          // env CREDENTIAL_ENCRYPTION_PASSPHRASE
	EncryptionSalt       string `yaml:"-"`          // env CREDENTIAL_ENCRYPTION_SALT
	CredentialKeyMap     string `yaml:"-"`          // env CREDENTIAL_KEY_MAP — "v1:hex,v2:hex"; precedence over single key
	Production           bool   `yaml:"production"` // feature flag; safe in yaml
}

// AgentConfig holds agent-related settings.
type AgentConfig struct {
	CADir string `yaml:"caDir"`
}

// Load reads config from the given YAML file, then applies environment
// variable overrides. Missing file is not an error — defaults are used.
// Load reads + parses the YAML at path, applies env-var overrides, and
// validates business-required fields. Mirrors the Hub canonical loader
// (defaults → yaml → applyEnvOverrides → validate) so all four services
// share one shape — see docs/developers/architecture/cross-cutting/
// foundation/service-bootstrap-config-architecture.md §3.
func Load(path string) (*Config, error) {
	cfg := defaults()

	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	if err == nil {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
	}

	applyEnvOverrides(cfg)
	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	return cfg, nil
}

func defaults() *Config {
	return &Config{
		Server: ServerConfig{
			Port:            3001,
			ShutdownTimeout: 10 * time.Second,
		},
		Database: DatabaseConfig{
			MaxConns:        25,
			MinConns:        5,
			MaxConnLifetime: 300 * time.Second,
		},
		// CP Redis defaults intentionally leave Addrs nil so a missing
		// yaml redis block leaves CP in no-Redis fallback mode. Operators
		// opt in by populating yaml or REDIS_ADDRS.
		Redis: redisfactory.Config{
			Mode:         redisfactory.ModeStandalone,
			DialTimeout:  5 * time.Second,
			ReadTimeout:  3 * time.Second,
			WriteTimeout: 3 * time.Second,
			PoolTimeout:  4 * time.Second,
		},
		Log: LogConfig{
			Level:  "info",
			Format: "json",
		},
		BFF: BFFConfig{
			ComplianceProxyURL:        "http://127.0.0.1:3040",
			AIGatewayURL:              "http://127.0.0.1:3050",
			ComplianceProxyRuntimeURL: "http://127.0.0.1:3040",
		},
		Registry: RegistryConfig{
			NexusHubURL: "http://127.0.0.1:3060",
		},
		Agent: AgentConfig{
			CADir: ".agent-ca",
		},
		AuthServer: AuthServerConfig{
			KeystoreDir: ".nexus/authkeys",
			// Issuer has no safe default — production deployments must set it.
		},
		AIGuard: AIGuardConfig{
			// 60s covers the default ai_guard_config.timeout_ms (30s)
			// plus marshal/unmarshal + network slack with comfortable margin.
			DispatchTimeoutSec: 60,
		},
		HTTPClients: HTTPClientsConfig{
			Hub:                  HTTPClientConfig{TimeoutSec: 30},
			HubProxy:             HTTPClientConfig{TimeoutSec: 10},
			ComplianceProxyAdmin: HTTPClientConfig{TimeoutSec: 10},
		},
	}
}

func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("CONTROL_PLANE_PORT"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &cfg.Server.Port)
	}
	if v := os.Getenv("CONTROL_PLANE_PUBLIC_URL"); v != "" {
		cfg.PublicURL = v
	}
	if v := os.Getenv("DATABASE_URL"); v != "" {
		cfg.Database.URL = v
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		cfg.Log.Level = v
	}
	if v := os.Getenv("COMPLIANCE_PROXY_URL"); v != "" {
		cfg.BFF.ComplianceProxyURL = v
	}
	if v := os.Getenv("AI_GATEWAY_URL"); v != "" {
		cfg.BFF.AIGatewayURL = v
	}
	if v := os.Getenv("NEXUS_HUB_URL"); v != "" {
		cfg.Registry.NexusHubURL = v
	}

	// Crypto
	if v := os.Getenv("CREDENTIAL_ENCRYPTION_KEY"); v != "" {
		cfg.Crypto.EncryptionKey = v
	}
	if v := os.Getenv("CREDENTIAL_ENCRYPTION_PASSPHRASE"); v != "" {
		cfg.Crypto.EncryptionPassphrase = v
	}
	if v := os.Getenv("CREDENTIAL_ENCRYPTION_SALT"); v != "" {
		cfg.Crypto.EncryptionSalt = v
	}
	if v := os.Getenv("CREDENTIAL_KEY_MAP"); v != "" {
		cfg.Crypto.CredentialKeyMap = v
	}

	// Agent
	if v := os.Getenv("AGENT_CA_DIR"); v != "" {
		cfg.Agent.CADir = v
	}

	// BFF proxy tokens
	if v := os.Getenv("COMPLIANCE_PROXY_RUNTIME_URL"); v != "" {
		cfg.BFF.ComplianceProxyRuntimeURL = v
	}
	if v := os.Getenv("COMPLIANCE_PROXY_API_TOKEN"); v != "" {
		cfg.BFF.ComplianceProxyAPIToken = v
	}

	// OTEL
	if v := os.Getenv("OTEL_ENDPOINT"); v != "" {
		cfg.Otel.Endpoint = v
	}
	if v := os.Getenv("OTEL_SERVICE_NAME"); v != "" {
		cfg.Otel.ServiceName = v
	}

	if v := os.Getenv("MQ_DRIVER"); v != "" {
		cfg.MQ.Driver = v
	}
	if v := os.Getenv("NATS_URL"); v != "" {
		cfg.MQ.NATS.URL = v
	}

	// Auth — internal service token (shared with nexus-hub).
	if v := os.Getenv("ADMIN_KEY_HMAC_SECRET"); v != "" {
		cfg.Auth.HMACSecret = v
	}
	if v := os.Getenv("INTERNAL_SERVICE_TOKEN"); v != "" {
		cfg.Auth.InternalServiceToken = v
	}

	// Crypto — production flag (replaces NODE_ENV==production).
	if v := os.Getenv("CONTROL_PLANE_CRYPTO_PRODUCTION"); v == "true" || v == "1" {
		cfg.Crypto.Production = true
	}

	// Auth server overrides (issuer and keystore directory).
	if v := os.Getenv("AUTH_SERVER_ISSUER"); v != "" {
		cfg.AuthServer.Issuer = v
	}
	if v := os.Getenv("AUTH_SERVER_KEYSTORE_DIR"); v != "" {
		cfg.AuthServer.KeystoreDir = v
	}
}

// validate enforces the business-required configuration set for the
// Control Plane. Required-set mirrors the cross-service contract in
// docs/developers/architecture/cross-cutting/foundation/
// service-bootstrap-config-architecture.md §5:
//
//   - PublicURL: reported as Thing staticInfo; admin UI uses it to render
//     CP-facing URLs (agent-setup page, integration help cards).
//   - Database.URL: every CP admin handler is DB-bound.
//   - Auth.InternalServiceToken: shared with Hub; mismatch → all
//     thingclient/hubclient calls 403.
//   - Redis.Addrs: session store, IAM cache, quota counters, rate
//     limiting. CP has no in-memory fallback.
//   - MQ.Driver (+ MQ.NATS.URL when nats): admin-audit / desired-state
//     events. Without it Hub never sees CP-side writes.
//   - Registry.NexusHubURL: CP registers as a Thing on boot; no Hub URL
//     means no registration, no shadow, no config sync.
//
// Redis.Addrs accepts either yaml OR env (REDIS_ADDRS) — env-merge
// happens inside redisfactory.New at wiring time, not config.Load, so
// validate checks both.
func validate(cfg *Config) error {
	if cfg.PublicURL == "" {
		return fmt.Errorf("publicURL is required (reported to Thing Registry as staticInfo; admin UI uses it to render CP-facing URLs)")
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
		return fmt.Errorf("registry.nexusHubUrl is required (Control Plane registers as a Thing on boot)")
	}
	return nil
}

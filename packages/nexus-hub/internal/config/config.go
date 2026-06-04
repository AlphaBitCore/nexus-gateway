// Package config loads and validates the Nexus Hub configuration from
// a YAML file and environment variable overrides.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/redisfactory"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore/spillfactory"
)

// HubConfig is the top-level configuration for the Nexus Hub.
type HubConfig struct {
	// PublicURL is the externally-reachable base URL clients use to
	// reach this service (scheme + host[:port], no trailing slash).
	// Prod: "https://hub.example.com"; dev: "http://localhost:3060".
	// Reported to the Thing Registry as part of staticInfo so the CP
	// admin API can surface it to the UI without hardcoded hostnames.
	PublicURL  string              `yaml:"publicURL"`
	Server     ServerConfig        `yaml:"server"`
	Database   DatabaseConfig      `yaml:"database"`
	Redis      redisfactory.Config `yaml:"redis"`
	MQ         MQConfig            `yaml:"mq"`
	Consumers  ConsumerConfig      `yaml:"consumers"`
	Scheduler  SchedulerConfig     `yaml:"scheduler"`
	Auth       AuthConfig          `yaml:"auth"`
	AuthServer AuthServerConfig    `yaml:"authServer"`
	AgentCA    AgentCAConfig       `yaml:"agentCA"`
	OTEL       OTELConfig          `yaml:"otel"`
	Log        LogConfig           `yaml:"log"`
	Hub        HubIdentity         `yaml:"hub"`
	// Spill configures out-of-band body storage. Hub uses it on the
	// agent_audit handler to spill large agent-uploaded payloads (agents
	// send raw bytes; Hub decides inline vs spill based on size). All
	// services in the deployment must point at the same backend so CP's
	// read path resolves refs produced anywhere.
	Spill spillfactory.FactoryConfig `yaml:"spill"`
}

// ServerConfig controls the HTTP/WebSocket server.
type ServerConfig struct {
	Port            int           `yaml:"port"`
	ReadTimeout     time.Duration `yaml:"readTimeout"`
	WriteTimeout    time.Duration `yaml:"writeTimeout"`
	ShutdownTimeout time.Duration `yaml:"shutdownTimeout"`
}

// DatabaseConfig holds PostgreSQL connection settings.
type DatabaseConfig struct {
	URL      string `yaml:"url"`
	MaxConns int32  `yaml:"maxConns"`
	MinConns int32  `yaml:"minConns"`
}

// MQConfig holds message queue settings.
type MQConfig struct {
	Driver string     `yaml:"driver"`
	NATS   NATSConfig `yaml:"nats"`
}

// NATSConfig holds NATS JetStream connection settings.
type NATSConfig struct {
	URL string `yaml:"url"`
}

// ConsumerConfig controls Hub MQ consumers (db-writer, siem-forwarder, metrics).
type ConsumerConfig struct {
	Enabled       bool               `yaml:"enabled"`
	BatchSize     int                `yaml:"batchSize"`
	FlushInterval time.Duration      `yaml:"flushInterval"`
	SIEM          SIEMConsumerConfig `yaml:"siem"`
}

// SIEMConsumerConfig controls the Hub's SIEM forwarder consumer.
type SIEMConsumerConfig struct {
	Enabled       bool              `yaml:"enabled"`
	URL           string            `yaml:"url"`
	Headers       map[string]string `yaml:"headers"`
	Format        string            `yaml:"format"`
	BatchSize     int               `yaml:"batchSize"`
	FlushInterval time.Duration     `yaml:"flushInterval"`
	EventTypes    []string          `yaml:"eventTypes"`
}

// SchedulerConfig controls whether this Hub instance runs scheduled jobs.
type SchedulerConfig struct {
	Enabled                  bool              `yaml:"enabled"`
	DriftCheckInterval       time.Duration     `yaml:"driftCheckInterval"`
	IdentityEnrichInterval   time.Duration     `yaml:"identityEnrichInterval"`
	OverrideExpiryInterval   time.Duration     `yaml:"overrideExpiryInterval"`
	AuditChainVerifyInterval time.Duration     `yaml:"auditChainVerifyInterval"`
	Retention                RetentionConfig   `yaml:"retention"`
	Intervals                JobIntervalConfig `yaml:"intervals"`
	// AlertEval tunes the alerteval streaming engine. Active only when
	// Scheduler.Enabled is true; the engine registers itself as the named
	// scheduler.Job "alerteval-engine".
	AlertEval AlertEvalConfig `yaml:"alertEval"`
	// EnableAgentRollup gates whether ThingRollup5mJob processes source=agent
	// rows from traffic_event. At fleet scale (~10K agents) aggregating every
	// agent into central per-Thing rollup tables blows up row counts and DB
	// load; agents instead compute their own rollups locally (see
	// packages/agent/internal/observability/localrollup). The toggle defaults to false so
	// Hub never builds per-agent series unless an operator opts in for a
	// small / forensics-heavy deployment. compliance-proxy and ai-gateway
	// (single-digit instance counts) are always rolled up centrally — only
	// agent is gated. Agent traffic_event rows are still ingested for
	// audit/compliance/fleet aggregation regardless of this flag.
	EnableAgentRollup bool `yaml:"enableAgentRollup"`

	// ExcludeInternalOpsFromBilledCost controls whether L2 embedding +
	// ai-guard classifier costs are EXCLUDED from MetricBilledCostUSD
	// (the quota-bearing total). Default false: include them — operator
	// pays for L2 embedding + classifier calls regardless of who triggered
	// them, so by default they count toward the customer's quota.
	// Operators whose pricing model absorbs those costs (or who run
	// internal-only deployments) set this true to keep customer quotas
	// unaffected. Internal-ops costs are also always emitted on the
	// dedicated MetricEmbeddingCostUSD / MetricAIGuardCostUSD series
	// regardless of this toggle, so ops dashboards see the breakdown
	// either way.
	ExcludeInternalOpsFromBilledCost bool `yaml:"excludeInternalOpsFromBilledCost"`
}

// AlertEvalConfig holds knobs for the streaming alert evaluator engine.
// Per-rule thresholds + windows live in AlertRule.params (admin-editable
// at runtime); only engine-wide tuning belongs here.
type AlertEvalConfig struct {
	// EngineTickSec is the per-tick cadence in seconds. Defaults to 5.
	EngineTickSec int `yaml:"engineTickSec"`
}

// RetentionConfig controls per-table retention (in days) for the
// data-retention and rollup-retention jobs. Zero disables a table.
// The metric_rollup_* tier chain is owned by the rollup-retention job via the
// Rollup*Days fields; data-retention covers traffic_event + AdminAuditLog.
type RetentionConfig struct {
	TrafficEventDays        int `yaml:"trafficEventDays"`
	TrafficEventPayloadDays int `yaml:"trafficEventPayloadDays"`
	AdminAuditLogDays       int `yaml:"adminAuditLogDays"`
	Rollup5mDays            int `yaml:"rollup5mDays"`
	Rollup1hDays            int `yaml:"rollup1hDays"`
	Rollup1dDays            int `yaml:"rollup1dDays"`
	Rollup1moDays           int `yaml:"rollup1moDays"`
	// OpsRawDays is the retention horizon (in days) for the partitioned
	// metric_ops_raw table. The ops-raw-partition job drops whole-day
	// partitions older than this. Defaults to 30. Env:
	// NEXUS_HUB_SCHEDULER_OPS_RAW_DAYS.
	OpsRawDays int `yaml:"opsRawDays"`
}

// JobIntervalConfig holds per-job intervals. Defaults match the values CP
// used prior to the consolidation; overriding keeps ops in control of each
// cadence without touching code.
type JobIntervalConfig struct {
	MetricsRollup             time.Duration `yaml:"metricsRollup"`
	DataRetention             time.Duration `yaml:"dataRetention"`
	Rollup5m                  time.Duration `yaml:"rollup5m"`
	Merge1h                   time.Duration `yaml:"merge1h"`
	Merge1d                   time.Duration `yaml:"merge1d"`
	Merge1mo                  time.Duration `yaml:"merge1mo"`
	RollupCorrection          time.Duration `yaml:"rollupCorrection"`
	RollupRetention           time.Duration `yaml:"rollupRetention"`
	QuotaAlertCheck           time.Duration `yaml:"quotaAlertCheck"`
	VKExpiry                  time.Duration `yaml:"vkExpiry"`
	ExemptionGC               time.Duration `yaml:"exemptionGC"`
	ThingOfflineAlerts        time.Duration `yaml:"thingOfflineAlerts"`
	ProviderUnavailableAlerts time.Duration `yaml:"providerUnavailableAlerts"`
	AgentCertExpiry           time.Duration `yaml:"agentCertExpiry"`
	CredentialStale           time.Duration `yaml:"credentialStale"`
	// OpsRollup5m is the cadence for the metric_ops_raw → metric_ops_rollup_5m
	// aggregation (the tier that owns the raw read). Defaults to 1 minute.
	OpsRollup5m time.Duration `yaml:"opsRollup5m"`
	// OpsRollup1h is the cadence for the metric_ops_rollup_5m → metric_ops_rollup_1h
	// cascade. Defaults to 5 minutes — the natural seal margin for the hourly
	// bucket plus headroom for late-arriving 5m rollups.
	OpsRollup1h time.Duration `yaml:"opsRollup1h"`
	// OpsRawPartition is the cadence for the ops-raw-partition maintenance job
	// (pre-create upcoming day partitions, drop aged ones). Defaults to 6h.
	OpsRawPartition time.Duration `yaml:"opsRawPartition"`
	// OpsRollup1d is the cadence for metric_ops_rollup_1h → metric_ops_rollup_1d.
	// Defaults to 1 hour.
	OpsRollup1d time.Duration `yaml:"opsRollup1d"`
	// OpsRollup1mo is the cadence for metric_ops_rollup_1d → metric_ops_rollup_1mo.
	// Defaults to 24 hours.
	OpsRollup1mo time.Duration `yaml:"opsRollup1mo"`
	// OpsRetention is the cadence for purging aged ops-metric raw + rollup +
	// diag-event rows per metric_ops_retention_config. Defaults to 24 hours.
	OpsRetention time.Duration `yaml:"opsRetention"`
	// CacheQualityMonitor is the cadence for checking normaliser error rates
	// and auto-reverting to dry-run on regression. Defaults to 5 minutes.
	CacheQualityMonitor time.Duration `yaml:"cacheQualityMonitor"`
	// ProviderHealthRollup is the cadence for recomputing ProviderHealth from
	// traffic_event. Defaults to 5 minutes; window covers the last 30 minutes.
	ProviderHealthRollup time.Duration `yaml:"providerHealthRollup"`
	// CredentialExpiry is the cadence for the credential expiry check job.
	// Defaults to 1 hour.
	CredentialExpiry time.Duration `yaml:"credentialExpiry"`
	// CredentialStatsFlush is the cadence for draining per-credential Redis
	// usage stats into the Credential table. Defaults to 60 seconds.
	CredentialStatsFlush time.Duration `yaml:"credentialStatsFlush"`
	// CredentialCircuitFlush is the cadence for draining per-credential Redis
	// circuit-state transitions into the Credential table. Defaults to 30 seconds.
	CredentialCircuitFlush time.Duration `yaml:"credentialCircuitFlush"`
	// CredentialHealthRollup is the cadence for recomputing per-credential
	// health classification from the last 5 minutes of traffic_event rows.
	// Defaults to 5 minutes.
	CredentialHealthRollup time.Duration `yaml:"credentialHealthRollup"`
	// CredentialReliabilityAlerts is the cadence for raising / resolving
	// credential.circuit_open, credential.health_unavailable, and
	// credential.health_degraded_sustained alerts. Defaults to 60 seconds.
	CredentialReliabilityAlerts time.Duration `yaml:"credentialReliabilityAlerts"`
	// CredentialRetire is the cadence for the credential retire lifecycle job.
	// Defaults to 1 hour.
	CredentialRetire time.Duration `yaml:"credentialRetire"`
	// SemanticCacheReindex is the cadence for the Hub-side blue/green
	// Valkey vector index swap job. Runs frequently (default 5s) because it
	// is a no-op when fingerprints already match and must be responsive to
	// embedding model changes without waiting for a long cron interval.
	SemanticCacheReindex time.Duration `yaml:"semanticCacheReindex"`
}

// AuthConfig holds authentication settings.
//
// InternalServiceToken is env-only per the "Secrets are env-only" binding
// (CLAUDE.md). yaml:"-" prevents a stale yaml field from silently
// overriding the env value.
type AuthConfig struct {
	InternalServiceToken string `yaml:"-"` // env INTERNAL_SERVICE_TOKEN — shared with all other services
}

// AuthServerConfig holds settings describing the shared OAuth/OIDC auth
// server (mounted by Control Plane). These fields are deployment-level
// shared-infrastructure URLs (not secrets) and stay in yaml. The env
// override names use the canonical AUTH_SERVER_* prefix and are
// symmetric across Hub (verifier) and CP (issuer).
type AuthServerConfig struct {
	// JWKSURL is the URL of the auth server's JWKS endpoint used to verify
	// enrollment JWTs in enterprise-login mode. Required when device auth
	// mode is "enterprise-login"; Hub logs a startup WARN when empty.
	JWKSURL string `yaml:"jwksURL"`
	// Issuer is the OAuth issuer string written into the `iss` claim of
	// enrollment JWTs (cfg.AuthServer.Issuer on the CP side). Hub pins
	// this value via jwt.WithIssuer so a third-party signer that also
	// happens to publish keys on JWKSURL cannot impersonate the auth
	// server. Required alongside JWKSURL. [MUST MATCH CP <-> Hub].
	Issuer string `yaml:"issuer"`
	// URL is the auth server base URL (e.g. "https://nexus.example.com")
	// returned by GET /api/public/agent-bootstrap so agents can discover
	// the SSO enrollment endpoint without per-device configuration.
	// Operators set it once at the Hub layer; the same value applies
	// fleet-wide.
	URL string `yaml:"url"`
}

// AgentCAConfig holds Agent Certificate Authority settings.
type AgentCAConfig struct {
	CertFile string `yaml:"certFile"`
	KeyFile  string `yaml:"keyFile"`
	Dir      string `yaml:"dir"`
}

// OTELConfig holds OpenTelemetry settings.
type OTELConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Endpoint string `yaml:"endpoint"`
}

// LogConfig controls logging behavior.
type LogConfig struct {
	Level        string `yaml:"level"`
	Format       string `yaml:"format"`
	File         string `yaml:"file"`         // optional: tee logs to this file (see also env LOG_FILE)
	StackOnError bool   `yaml:"stackOnError"` // attach goroutine stack on error-level logs (env LOG_STACK_ON_ERROR)
}

// HubIdentity identifies this Hub instance in the thing table.
type HubIdentity struct {
	ID            string `yaml:"id"`
	AdvertiseAddr string `yaml:"advertiseAddr"`
	// AllowedOrigins is the production origin allowlist for the Hub
	// WebSocket endpoint (e.g. cluster DNS names). Localhost origins are
	// always allowed; this list is additive for service-to-service traffic
	// arriving from non-loopback hostnames. Browsers are still rejected
	// unless their origin is explicitly listed.
	AllowedOrigins []string `yaml:"allowedOrigins"`
}

// Load reads configuration from a YAML file, then applies environment
// variable overrides. Missing file is not an error — defaults are used.
func Load(path string) (*HubConfig, error) {
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

func defaults() *HubConfig {
	hostname, _ := os.Hostname()
	return &HubConfig{
		Server: ServerConfig{
			Port:            3060,
			ReadTimeout:     30 * time.Second,
			WriteTimeout:    30 * time.Second,
			ShutdownTimeout: 15 * time.Second,
		},
		Database: DatabaseConfig{
			URL:      "postgres://postgres:postgres@localhost:5432/nexus_gateway?sslmode=disable",
			MaxConns: 20,
			MinConns: 5,
		},
		Redis: redisfactory.Config{
			Mode:         redisfactory.ModeStandalone,
			Addrs:        []string{"localhost:6379"},
			DB:           0,
			DialTimeout:  5 * time.Second,
			ReadTimeout:  3 * time.Second,
			WriteTimeout: 3 * time.Second,
			PoolTimeout:  4 * time.Second,
		},
		MQ: MQConfig{
			Driver: "nats",
			NATS:   NATSConfig{URL: "nats://localhost:4222"},
		},
		Consumers: ConsumerConfig{
			Enabled:       true,
			BatchSize:     100,
			FlushInterval: 5 * time.Second,
		},
		Scheduler: SchedulerConfig{
			Enabled:                  true,
			DriftCheckInterval:       60 * time.Second,
			IdentityEnrichInterval:   5 * time.Minute,
			OverrideExpiryInterval:   60 * time.Second,
			AuditChainVerifyInterval: 1 * time.Hour,
			Retention: RetentionConfig{
				TrafficEventDays:        90,
				TrafficEventPayloadDays: 30,
				AdminAuditLogDays:       365,
				Rollup5mDays:            7,
				Rollup1hDays:            90,
				Rollup1dDays:            365,
				Rollup1moDays:           1825,
				OpsRawDays:              30,
			},
			Intervals: JobIntervalConfig{
				MetricsRollup:               time.Hour,
				DataRetention:               24 * time.Hour,
				Rollup5m:                    60 * time.Second,
				Merge1h:                     5 * time.Minute,
				Merge1d:                     time.Hour,
				Merge1mo:                    24 * time.Hour,
				RollupCorrection:            24 * time.Hour,
				RollupRetention:             24 * time.Hour,
				QuotaAlertCheck:             60 * time.Second,
				VKExpiry:                    time.Hour,
				ExemptionGC:                 5 * time.Minute,
				ThingOfflineAlerts:          60 * time.Second,
				ProviderUnavailableAlerts:   60 * time.Second,
				OpsRollup5m:                 time.Minute,
				OpsRollup1h:                 5 * time.Minute,
				OpsRollup1d:                 time.Hour,
				OpsRollup1mo:                24 * time.Hour,
				OpsRetention:                24 * time.Hour,
				OpsRawPartition:             6 * time.Hour,
				CacheQualityMonitor:         5 * time.Minute,
				ProviderHealthRollup:        5 * time.Minute,
				CredentialExpiry:            time.Hour,
				CredentialStatsFlush:        60 * time.Second,
				CredentialCircuitFlush:      30 * time.Second,
				CredentialHealthRollup:      5 * time.Minute,
				CredentialReliabilityAlerts: 60 * time.Second,
				CredentialRetire:            time.Hour,
				SemanticCacheReindex:        5 * time.Second,
			},
		},
		Log: LogConfig{
			Level:  "info",
			Format: "json",
		},
		Hub: HubIdentity{
			ID: "hub-" + hostname,
		},
	}
}

func applyEnvOverrides(cfg *HubConfig) {
	if v := os.Getenv("NEXUS_HUB_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Server.Port = p
		}
	}
	if v := os.Getenv("DATABASE_URL"); v != "" {
		cfg.Database.URL = v
	}
	if v := os.Getenv("NEXUS_HUB_PUBLIC_URL"); v != "" {
		cfg.PublicURL = v
	}
	if v := os.Getenv("MQ_DRIVER"); v != "" {
		cfg.MQ.Driver = v
	}
	if v := os.Getenv("NATS_URL"); v != "" {
		cfg.MQ.NATS.URL = v
	}
	if v := os.Getenv("NEXUS_HUB_SCHEDULER_ENABLED"); v != "" {
		cfg.Scheduler.Enabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("INTERNAL_SERVICE_TOKEN"); v != "" {
		cfg.Auth.InternalServiceToken = v
	}
	if v := os.Getenv("AUTH_SERVER_JWKS_URL"); v != "" {
		cfg.AuthServer.JWKSURL = v
	}
	if v := os.Getenv("AUTH_SERVER_ISSUER"); v != "" {
		cfg.AuthServer.Issuer = v
	}
	if v := os.Getenv("AUTH_SERVER_URL"); v != "" {
		cfg.AuthServer.URL = v
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		cfg.Log.Level = v
	}
	if v := os.Getenv("LOG_FORMAT"); v != "" {
		cfg.Log.Format = v
	}
	if v := os.Getenv("NEXUS_HUB_ID"); v != "" {
		cfg.Hub.ID = v
	}
	if v := os.Getenv("NEXUS_HUB_ADVERTISE_ADDR"); v != "" {
		cfg.Hub.AdvertiseAddr = v
	}
	if v := os.Getenv("NEXUS_HUB_ALLOWED_ORIGINS"); v != "" {
		cfg.Hub.AllowedOrigins = splitAndTrim(v)
	}
	if v := os.Getenv("AGENT_CA_CERT_FILE"); v != "" {
		cfg.AgentCA.CertFile = v
	}
	if v := os.Getenv("AGENT_CA_KEY_FILE"); v != "" {
		cfg.AgentCA.KeyFile = v
	}
	if v := os.Getenv("AGENT_CA_DIR"); v != "" {
		cfg.AgentCA.Dir = v
	}

	// Retention (days) — zero disables per-table purge.
	parseIntEnv("NEXUS_HUB_RETENTION_TRAFFIC_EVENT_DAYS", &cfg.Scheduler.Retention.TrafficEventDays)
	parseIntEnv("NEXUS_HUB_RETENTION_TRAFFIC_EVENT_PAYLOAD_DAYS", &cfg.Scheduler.Retention.TrafficEventPayloadDays)
	parseIntEnv("NEXUS_HUB_RETENTION_ADMIN_AUDIT_DAYS", &cfg.Scheduler.Retention.AdminAuditLogDays)
	parseIntEnv("NEXUS_HUB_RETENTION_ROLLUP_5M_DAYS", &cfg.Scheduler.Retention.Rollup5mDays)
	parseIntEnv("NEXUS_HUB_RETENTION_ROLLUP_1H_DAYS", &cfg.Scheduler.Retention.Rollup1hDays)
	parseIntEnv("NEXUS_HUB_RETENTION_ROLLUP_1D_DAYS", &cfg.Scheduler.Retention.Rollup1dDays)
	parseIntEnv("NEXUS_HUB_RETENTION_ROLLUP_1MO_DAYS", &cfg.Scheduler.Retention.Rollup1moDays)
	parseIntEnv("NEXUS_HUB_SCHEDULER_OPS_RAW_DAYS", &cfg.Scheduler.Retention.OpsRawDays)

	// Per-job intervals.
	parseDurationEnv("NEXUS_HUB_SCHEDULER_METRICS_ROLLUP_INTERVAL", &cfg.Scheduler.Intervals.MetricsRollup)
	parseDurationEnv("NEXUS_HUB_SCHEDULER_DATA_RETENTION_INTERVAL", &cfg.Scheduler.Intervals.DataRetention)
	parseDurationEnv("NEXUS_HUB_SCHEDULER_ROLLUP_5M_INTERVAL", &cfg.Scheduler.Intervals.Rollup5m)
	parseDurationEnv("NEXUS_HUB_SCHEDULER_MERGE_1H_INTERVAL", &cfg.Scheduler.Intervals.Merge1h)
	parseDurationEnv("NEXUS_HUB_SCHEDULER_MERGE_1D_INTERVAL", &cfg.Scheduler.Intervals.Merge1d)
	parseDurationEnv("NEXUS_HUB_SCHEDULER_MERGE_1MO_INTERVAL", &cfg.Scheduler.Intervals.Merge1mo)
	parseDurationEnv("NEXUS_HUB_SCHEDULER_ROLLUP_CORRECTION_INTERVAL", &cfg.Scheduler.Intervals.RollupCorrection)
	parseDurationEnv("NEXUS_HUB_SCHEDULER_ROLLUP_RETENTION_INTERVAL", &cfg.Scheduler.Intervals.RollupRetention)
	parseDurationEnv("NEXUS_HUB_SCHEDULER_QUOTA_ALERT_INTERVAL", &cfg.Scheduler.Intervals.QuotaAlertCheck)
	parseDurationEnv("NEXUS_HUB_SCHEDULER_VK_EXPIRY_INTERVAL", &cfg.Scheduler.Intervals.VKExpiry)
	parseDurationEnv("NEXUS_HUB_SCHEDULER_EXEMPTION_GC_INTERVAL", &cfg.Scheduler.Intervals.ExemptionGC)
	parseDurationEnv("NEXUS_HUB_SCHEDULER_THING_OFFLINE_ALERTS_INTERVAL", &cfg.Scheduler.Intervals.ThingOfflineAlerts)
	parseDurationEnv("NEXUS_HUB_SCHEDULER_PROVIDER_UNAVAILABLE_ALERTS_INTERVAL", &cfg.Scheduler.Intervals.ProviderUnavailableAlerts)
	parseDurationEnv("NEXUS_HUB_SCHEDULER_OPS_ROLLUP_5M_INTERVAL", &cfg.Scheduler.Intervals.OpsRollup5m)
	parseDurationEnv("NEXUS_HUB_SCHEDULER_OPS_ROLLUP_1H_INTERVAL", &cfg.Scheduler.Intervals.OpsRollup1h)
	parseDurationEnv("NEXUS_HUB_SCHEDULER_OPS_RAW_PARTITION_INTERVAL", &cfg.Scheduler.Intervals.OpsRawPartition)
	parseDurationEnv("NEXUS_HUB_SCHEDULER_OPS_ROLLUP_1D_INTERVAL", &cfg.Scheduler.Intervals.OpsRollup1d)
	parseDurationEnv("NEXUS_HUB_SCHEDULER_OPS_ROLLUP_1MO_INTERVAL", &cfg.Scheduler.Intervals.OpsRollup1mo)
	parseDurationEnv("NEXUS_HUB_SCHEDULER_OPS_RETENTION_INTERVAL", &cfg.Scheduler.Intervals.OpsRetention)
	parseDurationEnv("NEXUS_HUB_SCHEDULER_CACHE_QUALITY_MONITOR_INTERVAL", &cfg.Scheduler.Intervals.CacheQualityMonitor)
	parseDurationEnv("NEXUS_HUB_SCHEDULER_CREDENTIAL_EXPIRY_INTERVAL", &cfg.Scheduler.Intervals.CredentialExpiry)
	parseDurationEnv("NEXUS_HUB_SCHEDULER_CREDENTIAL_STATS_FLUSH_INTERVAL", &cfg.Scheduler.Intervals.CredentialStatsFlush)
	parseDurationEnv("NEXUS_HUB_SCHEDULER_CREDENTIAL_CIRCUIT_FLUSH_INTERVAL", &cfg.Scheduler.Intervals.CredentialCircuitFlush)
	parseDurationEnv("NEXUS_HUB_SCHEDULER_CREDENTIAL_HEALTH_ROLLUP_INTERVAL", &cfg.Scheduler.Intervals.CredentialHealthRollup)
	parseDurationEnv("NEXUS_HUB_SCHEDULER_CREDENTIAL_RELIABILITY_ALERTS_INTERVAL", &cfg.Scheduler.Intervals.CredentialReliabilityAlerts)
	parseDurationEnv("NEXUS_HUB_SCHEDULER_CREDENTIAL_RETIRE_INTERVAL", &cfg.Scheduler.Intervals.CredentialRetire)
}

func parseIntEnv(key string, dst *int) {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}

func parseDurationEnv(key string, dst *time.Duration) {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			*dst = d
		}
	}
}

// splitAndTrim splits a comma-separated list and trims whitespace from each
// entry, discarding empty results. Used for env-var lists like
// NEXUS_HUB_ALLOWED_ORIGINS where an empty value must produce an empty slice
// (so we never silently admit unintended origins).
func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func validate(cfg *HubConfig) error {
	if cfg.PublicURL == "" {
		return fmt.Errorf("publicURL is required (reported to Thing Registry as staticInfo; admin UI uses it to render Hub endpoint)")
	}
	if cfg.Database.URL == "" {
		return fmt.Errorf("database.url is required")
	}
	if cfg.Auth.InternalServiceToken == "" {
		return fmt.Errorf("auth.internalServiceToken is required")
	}
	if cfg.Hub.ID == "" {
		return fmt.Errorf("hub.id is required")
	}
	// Redis env load happens inside InitRedis (after Load returns), so check
	// yaml AND env here. Mirrors the pattern InitRedis uses internally.
	if len(cfg.Redis.Addrs) == 0 && os.Getenv("REDIS_ADDRS") == "" {
		return fmt.Errorf("redis.addrs is required (set in yaml or via REDIS_ADDRS env)")
	}
	if cfg.MQ.Driver == "" {
		return fmt.Errorf("mq.driver is required (e.g. \"nats\")")
	}
	if cfg.MQ.Driver == "nats" && cfg.MQ.NATS.URL == "" {
		return fmt.Errorf("mq.nats.url is required when mq.driver=\"nats\"")
	}
	return nil
}

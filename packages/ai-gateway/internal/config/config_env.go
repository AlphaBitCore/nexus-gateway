package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		cfg.Database.URL = v
	}
	if v := os.Getenv("AI_GATEWAY_AUDIT_SPOOL_DIR"); v != "" {
		cfg.Audit.SpoolDir = v
	}
	if v := os.Getenv("AI_GATEWAY_AUDIT_SPOOL_MAX_FILE_MB"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &cfg.Audit.SpoolMaxFileMB)
	}
	if v := os.Getenv("AI_GATEWAY_AUDIT_SPOOL_MAX_TOTAL_MB"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &cfg.Audit.SpoolMaxTotalMB)
	}
	if v := os.Getenv("AI_GATEWAY_AUDIT_MAX_QUEUED_RECORDS"); v != "" {
		// best-effort: a malformed value leaves the default (10000) in place.
		_, _ = fmt.Sscanf(v, "%d", &cfg.Audit.MaxQueuedRecords)
	}
	if v := os.Getenv("AI_GATEWAY_AUDIT_MEM_MAX_BYTES"); v != "" {
		cfg.Audit.MemMaxBytes = v
	}
	if v := os.Getenv("AI_GATEWAY_AUDIT_LOSS_MODE"); v != "" {
		cfg.Audit.LossMode = v
	}
	if v := os.Getenv("AI_GATEWAY_AUDIT_COMPRESS"); v != "" {
		cfg.Audit.Compress = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("AI_GATEWAY_AUDIT_COMPRESS_MIN_BYTES"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &cfg.Audit.CompressMinBytes)
	}
	if v := os.Getenv("AI_GATEWAY_AUDIT_COMPRESS_LEVEL"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &cfg.Audit.CompressLevel)
	}
	if v := os.Getenv("AI_GATEWAY_AUDIT_SPILL_RECOVERY_INTERVAL_MS"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &cfg.Audit.SpillRecoveryIntervalMs)
	}
	if v := os.Getenv("AI_GATEWAY_AUDIT_SPILL_RECOVERY_PACE_MS"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &cfg.Audit.SpillRecoveryPaceMs)
	}
	if v := os.Getenv("AI_GATEWAY_PUBLIC_URL"); v != "" {
		cfg.PublicURL = v
	}
	// ADMIN_KEY_HMAC_SECRET / CREDENTIAL_ENCRYPTION_KEY / CREDENTIAL_KEY_MAP are
	// crown jewels resolved through the SecretCustody loader in Load(),
	// not read raw here, so they can be KMS-wrapped at rest.
	if v := os.Getenv("AI_GATEWAY_PORT"); v != "" {
		// best-effort: a malformed env var leaves the default port in place,
		// which is the right fallback during local dev.
		_, _ = fmt.Sscanf(v, "%d", &cfg.Server.Port)
	}
	if v := os.Getenv("AI_GATEWAY_HOST"); v != "" {
		cfg.Server.Host = v
	}
	if v := os.Getenv("AI_GATEWAY_REQUEST_READ_BUF_KB"); v != "" {
		// best-effort: a malformed value leaves the default (64) in place.
		_, _ = fmt.Sscanf(v, "%d", &cfg.Server.RequestReadBufKB)
	}
	// Upstream HTTP connection-pool tunables. The per-host idle cap is the dominant
	// one under high concurrency: when concurrent in-flight requests exceed it, each
	// request beyond the cap dials a FRESH upstream connection (and the conn is closed
	// rather than pooled on return), so the per-request TCP/TLS setup churns and tail
	// latency climbs as concurrency rises — the throughput-decays-under-load signature.
	// Size MaxIdleConnsPerHost ≥ peak concurrency to a hot upstream so warm conns are
	// reused instead of churned. Best-effort parse; malformed leaves the default.
	if v := os.Getenv("AI_GATEWAY_UPSTREAM_MAX_IDLE_CONNS_PER_HOST"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &cfg.Upstream.MaxIdleConnsPerHost)
	}
	if v := os.Getenv("AI_GATEWAY_UPSTREAM_MAX_IDLE_CONNS"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &cfg.Upstream.MaxIdleConns)
	}
	if v := os.Getenv("AI_GATEWAY_UPSTREAM_MAX_CONNS_PER_HOST"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &cfg.Upstream.MaxConnsPerHost)
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		cfg.Log.Level = v
	}
	if v := os.Getenv("LOG_FORMAT"); v != "" {
		cfg.Log.Format = v
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
	switch os.Getenv("AI_GATEWAY_CORS_ENABLED") {
	case "true", "1":
		cfg.CORS.Enabled = true
	case "false", "0":
		cfg.CORS.Enabled = false
	}
	if v := os.Getenv("AI_GATEWAY_CORS_ALLOWED_ORIGINS"); v != "" {
		cfg.CORS.AllowedOrigins = strings.Split(v, ",")
	}
	switch os.Getenv("AI_GATEWAY_CACHE_ENABLED") {
	case "true", "1":
		cfg.Cache.Enabled = true
	case "false", "0":
		cfg.Cache.Enabled = false
	}
	if v := os.Getenv("AI_GATEWAY_CACHE_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Cache.TTL = d
		}
	}
	if v := os.Getenv("AI_GATEWAY_CACHE_PREFIX"); v != "" {
		cfg.Cache.Prefix = v
	}
	if v := os.Getenv("OTEL_ENDPOINT"); v != "" {
		cfg.Otel.Endpoint = v
	}
	if v := os.Getenv("OTEL_SERVICE_NAME"); v != "" {
		cfg.Otel.ServiceName = v
	}
}

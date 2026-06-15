package config

// This file holds the connection-pool / spill tuning config types that the AI
// Gateway sizes for high concurrency. Defaults are set in defaults() and env
// overrides in applyEnvOverrides(), both in config.go.

// DatabaseConfig is the PostgreSQL connection configuration.
type DatabaseConfig struct {
	URL string `yaml:"url"`

	// MaxConns caps the pgx connection pool. The steady-state hot path does
	// NOT touch Postgres: VK / provider / model / credential lookups are
	// served from in-memory config caches (internal/cache/layer), rate
	// limit / quota / response cache from Redis, routing rules from an
	// in-memory cache, and traffic_event rows are published to NATS — not
	// written here. Postgres is a COLD-PATH backstop, hit only on a config
	// cache miss / TTL expiry / snapshot reload. The pool is therefore
	// sized to absorb a cold-cache burst (many concurrent first-use VK
	// misses after startup or a Hub invalidation) without serializing —
	// not to serve every request. Default 25: well above the pgx fallback
	// of max(4, NumCPU), which would serialize a cold burst, but not the
	// oversized pool a per-request DB path would need. Coordinate across
	// services — the sum of every service's MaxConns must stay below
	// Postgres `max_connections`. 0 falls back to the pgx default.
	MaxConns int32 `yaml:"maxConns"`

	// MinConns keeps a small floor of warm idle connections so the first
	// cold-cache misses do not each pay TCP+TLS connect latency. Kept low
	// (default 5) because the pool is mostly idle in steady state — a high
	// warm floor would pin Postgres backends for nothing. 0 keeps no floor.
	MinConns int32 `yaml:"minConns"`

	// MaxConnLifetimeSec recycles a pooled connection after this many
	// seconds, bounding the blast radius of a half-open connection behind
	// a load balancer / NAT idle timeout. Default 300. 0 keeps the pgx
	// default (no max lifetime).
	MaxConnLifetimeSec int `yaml:"maxConnLifetimeSec"`
}

// AuditConfig configures the durable spill for the traffic_event audit
// pipeline. When the in-memory record buffer is full after the backpressure
// window, overflow records are written as NDJSON to SpoolDir instead of being
// dropped, and an operator/sweeper can re-ingest them once the pipeline
// recovers.
type AuditConfig struct {
	// SpoolDir is the on-disk spill root. Empty disables disk spill (a
	// genuine overflow then becomes a loud, counted drop). Default
	// "/var/lib/nexus/audit-spool" — writable on the appliance (the
	// ai-gateway unit's ReadWritePaths). Env: AI_GATEWAY_AUDIT_SPOOL_DIR.
	SpoolDir string `yaml:"spoolDir"`
	// SpoolMaxFileMB caps a single spool file before rotation. Default 64.
	SpoolMaxFileMB int `yaml:"spoolMaxFileMb"`
	// SpoolMaxTotalMB caps the total on-disk spool; writes past it are
	// refused (loud) rather than filling the disk. Default 512.
	SpoolMaxTotalMB int `yaml:"spoolMaxTotalMb"`
}

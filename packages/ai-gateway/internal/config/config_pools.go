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
	// SpoolMaxFileMB caps a single spool file before rotation. Default 256. The
	// recovery sweeper drains one sealed file at a time, so this trades file count
	// against per-file replay granularity. Env: AI_GATEWAY_AUDIT_SPOOL_MAX_FILE_MB.
	SpoolMaxFileMB int `yaml:"spoolMaxFileMb"`
	// SpoolMaxTotalMB caps the total on-disk spool; writes past it are refused
	// (counted drop) rather than filling the disk. This is the no-loss buffer for
	// spill-defer, so it must absorb a real burst: at 8k RPS with ~700B/record a
	// few hundred MB overflows in seconds. Default 51200 (50 GiB) — disk is cheap
	// relative to dropping audit. Env: AI_GATEWAY_AUDIT_SPOOL_MAX_TOTAL_MB; size to
	// the data disk.
	SpoolMaxTotalMB int `yaml:"spoolMaxTotalMb"`

	// MaxQueuedRecords caps the audit Writer's in-heap record buffer; overflow
	// spills durably to SpoolDir (never a silent drop). Each queued record pins
	// its pooled ~50 KB request/response body until marshaled, so this bound is
	// the primary control over the audit side-path's gw heap: 10000 (default)
	// holds the body pool near ~1 GB under a slow-publish burst, vs ~5 GB at the
	// former hard-coded 50000, at the same measured spill rate. Raise it on a
	// memory-rich box wanting extra absorption headroom, lower it on a constrained
	// one. 0 falls back to the 10000 default. Env: AI_GATEWAY_AUDIT_MAX_QUEUED_RECORDS.
	MaxQueuedRecords int `yaml:"maxQueuedRecords"`

	// LossMode is the audit overflow policy. Durable audit is a product promise +
	// a compliance requirement, so the DEFAULT is "block" — no-loss back-pressure:
	// when the in-heap buffer is full the gateway slows the request path until the
	// audit pipeline drains, never dropping a record (NATS disk storage is the
	// burst buffer). The lossy modes are an explicit opt-out for callers that do
	// NOT need compliance audit and prefer raw throughput:
	//   - "block" (default): back-pressure, no loss.
	//   - "spill":  async durable NDJSON spill off the request path; bounded drop
	//               only if the spill is also saturated.
	//   - "spillblock": like "spill", but when the spill channel is also saturated
	//               it back-pressures the request path (bounded by backpressureMaxWait
	//               → durable spill) instead of dropping — lossless up to disk-write
	//               success, throttling ingest to the disk rate under overload.
	//   - "drop":   counted bounded drop on overflow; max throughput, lossy.
	// Empty / unrecognised → "block" (audit must not silently turn lossy from a
	// config typo). Env: AI_GATEWAY_AUDIT_LOSS_MODE.
	LossMode string `yaml:"lossMode"`

	// Compress enables end-to-end zstd compression of large captured bodies on
	// the audit side-path: the producer compresses (off the request path, in the
	// async marshal worker), the body rides the NATS wire compressed, the Hub
	// persists the compressed bytes verbatim (no decompress on ingest), and only
	// the Control-Plane view layer decompresses. Captured bodies are JSON/text
	// and compress ~3-10x; the audit pipeline is disk-I/O-bound at the NATS
	// broker, so shrinking each record's bytes is the direct lever on publish
	// throughput. Default true. Env: AI_GATEWAY_AUDIT_COMPRESS (0/false to disable).
	Compress bool `yaml:"compress"`

	// CompressMinBytes is the smallest captured body worth compressing; below it
	// the zstd frame + base64 overhead can exceed the saved bytes. 0 falls back
	// to the 1024 default. Env: AI_GATEWAY_AUDIT_COMPRESS_MIN_BYTES.
	CompressMinBytes int `yaml:"compressMinBytes"`

	// CompressLevel is the zstd encoder level (klauspost EncoderLevelFromZstd:
	// 1=fastest, 3=default, higher=better ratio/slower). 0 falls back to the
	// library default. Env: AI_GATEWAY_AUDIT_COMPRESS_LEVEL.
	CompressLevel int `yaml:"compressLevel"`

	// SpillRecoveryIntervalMs is the period of the background sweeper that replays
	// sealed spool files back into the MQ queue (the drain half of spill-defer), so
	// a record that overflowed to disk still reaches the queryable store. A durable
	// spool that never reaches Postgres is a silent data-availability gap, so this
	// is ON by default (2000 ms) whenever SpoolDir is set. Set to a negative value
	// to DISABLE recovery (spool then drains out-of-band only); 0 falls back to the
	// 2000 ms default. Env: AI_GATEWAY_AUDIT_SPILL_RECOVERY_INTERVAL_MS.
	SpillRecoveryIntervalMs int `yaml:"spillRecoveryIntervalMs"`

	// SpillRecoveryPaceMs throttles the sweeper between files so the drain yields
	// the box to the gateway's core request path (spill-defer: drain when there is
	// headroom). 0 falls back to the 50 ms default. Recovery is disabled via
	// SpillRecoveryIntervalMs (negative), not via the pace.
	// Env: AI_GATEWAY_AUDIT_SPILL_RECOVERY_PACE_MS.
	SpillRecoveryPaceMs int `yaml:"spillRecoveryPaceMs"`
}

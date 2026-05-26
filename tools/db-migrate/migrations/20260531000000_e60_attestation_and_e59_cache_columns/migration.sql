-- E60-S5 + E59-S1 bundle: traffic_event attestation columns +
-- the four cache detail columns whose schema.prisma rows shipped
-- with E59-S1 but never had a migration file generated.
--
-- E60 agent attestation passthrough (architecture
-- docs/developers/architecture/services/agent/agent-attestation-architecture.md § 5).
-- Populated by compliance-proxy when a verified X-Nexus-Attestation
-- header lets CP transparently tunnel a CONNECT instead of running
-- its own MITM + hook pipeline. Both columns nullable so historical
-- rows (and future MITM rows) stay clean for SQL filters that ignore
-- the attestation slice.
--
-- E59-S1 cache detail (architecture
-- docs/developers/architecture/cross-cutting/storage/cache-multi-tier-architecture.md § 6.4):
-- four columns that drive the audit drawer's "Cache Status" detail
-- panel — the unified `cache_status` column already exists; these
-- four surface the layered detail. UI filters bind only to
-- `cache_status`; these four are NEVER used as filter values.

ALTER TABLE traffic_event
    -- E60 attestation
    ADD COLUMN IF NOT EXISTS attestation_verified BOOLEAN,
    ADD COLUMN IF NOT EXISTS attestation_agent_id TEXT,
    -- E59-S1 cache detail (cache_status already exists)
    ADD COLUMN IF NOT EXISTS gateway_cache_status      TEXT,
    ADD COLUMN IF NOT EXISTS gateway_cache_skip_reason TEXT,
    ADD COLUMN IF NOT EXISTS gateway_cache_kind        TEXT,
    ADD COLUMN IF NOT EXISTS provider_cache_status     TEXT;

-- Partial index on the rare-true side: "show attested rows" filter on
-- the Traffic dashboard reads this slice without scanning every row.
CREATE INDEX IF NOT EXISTS idx_traffic_event_attestation_verified
    ON traffic_event (created_at DESC)
    WHERE attestation_verified = true;

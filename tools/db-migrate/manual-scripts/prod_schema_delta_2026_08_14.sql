-- prod_schema_delta_2026_08_14.sql
--
-- Brings the prod database up to the schema that main@d89cc2ef6 expects,
-- WITHOUT the destructive half of `prisma db push`.
--
-- Derived from the authoritative preview:
--   cd tools/db-migrate && npx prisma migrate diff \
--     --from-config-datasource --to-schema schema --script
-- run against prod over an SSH tunnel on 2026-08-14. Every statement below is
-- an ADD / ALTER TYPE / CREATE. Nothing is dropped.
--
-- DELIBERATELY OMITTED from the generated diff, and why:
--
--   DROP INDEX "thing_tags_gin_idx"
--       Created by schema-extras.sql; the Prisma model graph cannot express a
--       GIN index, so the diff reports it as surplus on every run. Executing it
--       would drop a live index on every deploy.
--
--   DROP TABLE "traffic_event_normalized"  (+ its FK)
--   ALTER TABLE "Provider"            DROP COLUMN "raw_body_spill_enabled"
--   ALTER TABLE "interception_domain" DROP COLUMN "raw_body_spill_enabled"
--       Real removals in the model graph, but hygiene rather than a runtime
--       requirement: no code at this revision reads any of the three. Staging
--       has been running this same revision for days with all three still
--       present. Prod holds 27,670 rows in traffic_event_normalized (last write
--       2026-06-23); dropping them is irreversible and buys nothing this
--       deploy needs, so it is left for a dedicated cleanup with its own
--       backup.
--
-- Idempotent: every statement is IF NOT EXISTS / IF EXISTS guarded, so a second
-- run is a no-op. Safe to re-run after a partial failure.
--
-- Run schema-extras.sql AFTER this script — it supplies the GIN index, the
-- two traffic_event error-governance partial indexes, and the
-- request/response_hooks_us CHECK constraints.

\set ON_ERROR_STOP on

BEGIN;

-- ---------------------------------------------------------------------------
-- Model: audio pricing + AP-2 parameter-constraint columns.
-- Read by the public model-detail endpoint (GET /api/v1/open/models/{id}) that
-- returns at this revision, and by audio cost stamping.
-- ---------------------------------------------------------------------------
ALTER TABLE "Model"
  ADD COLUMN IF NOT EXISTS "audioInputPricePerMillion"           DECIMAL(65,30),
  ADD COLUMN IF NOT EXISTS "audioOutputPricePerMillion"          DECIMAL(65,30),
  ADD COLUMN IF NOT EXISTS "cachedAudioInputReadPricePerMillion" DECIMAL(65,30),
  ADD COLUMN IF NOT EXISTS "family"                              TEXT,
  ADD COLUMN IF NOT EXISTS "minOutputTokens"                     INTEGER,
  ADD COLUMN IF NOT EXISTS "requiredModalities"                  TEXT[] DEFAULT ARRAY[]::TEXT[],
  ADD COLUMN IF NOT EXISTS "temperatureMax"                      DOUBLE PRECISION,
  ADD COLUMN IF NOT EXISTS "temperatureMin"                      DOUBLE PRECISION;

-- ---------------------------------------------------------------------------
-- traffic_event: caller-tag / session / router-cost / artifact columns.
-- The gateway writes these on the audit path as soon as traffic carries an
-- OpenAI `user` field — existing traffic, not opt-in — so these are required
-- before the new binaries serve, not optional.
-- All nullable with no default: metadata-only, no table rewrite.
-- ---------------------------------------------------------------------------
ALTER TABLE "traffic_event"
  ADD COLUMN IF NOT EXISTS "artifact_refs"         TEXT,
  ADD COLUMN IF NOT EXISTS "compliance_coverage"   TEXT,
  ADD COLUMN IF NOT EXISTS "embedding_provider_id" TEXT,
  ADD COLUMN IF NOT EXISTS "end_user_id"           TEXT,
  ADD COLUMN IF NOT EXISTS "router_cost_usd"       DECIMAL(20,10),
  ADD COLUMN IF NOT EXISTS "router_provider_id"    TEXT,
  ADD COLUMN IF NOT EXISTS "session_id"            TEXT;

-- ---------------------------------------------------------------------------
-- vendor_bill_reconciliation: split of our-side spend into vendor vs
-- internal-ops. NOT NULL DEFAULT 0 — a constant default, so PostgreSQL stores
-- it in the catalog and does not rewrite the table.
-- ---------------------------------------------------------------------------
ALTER TABLE "vendor_bill_reconciliation"
  ADD COLUMN IF NOT EXISTS "our_internal_ops_usd" DECIMAL(20,10) NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS "our_vendor_spend_usd" DECIMAL(20,10) NOT NULL DEFAULT 0;

-- ---------------------------------------------------------------------------
-- RevokedToken.targetJti: uuid -> text. This one is a live bug fix, not
-- cosmetics. An access token's jti is 16 random bytes in base64url, never a
-- UUID, so every revocation insert was rejected with SQLSTATE 22P02 while the
-- RFC 7009 endpoint returned 200 anyway — revoked access tokens stayed valid
-- until expiry. Postgres accepts the widening without a USING clause and it
-- cannot fail on existing rows, because the column is empty exactly where the
-- bug applied.
--
-- NOTE: staging has NOT had this applied (still uuid, index absent), so this
-- statement is prod moving ahead of stg rather than catching up to it.
-- ---------------------------------------------------------------------------
ALTER TABLE "RevokedToken" ALTER COLUMN "targetJti" TYPE TEXT;

COMMIT;

-- ---------------------------------------------------------------------------
-- Indexes. Outside the transaction so a failure here does not roll back the
-- column work above. traffic_event carries ~80k rows, so a plain build takes
-- well under a second and CONCURRENTLY is unnecessary.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS "gateway_async_job" (
    "provider_id"     TEXT NOT NULL,
    "id"              TEXT NOT NULL,
    "endpoint_type"   TEXT NOT NULL,
    "virtual_key_id"  TEXT NOT NULL,
    "organization_id" TEXT NOT NULL,
    "project_id"      TEXT,
    "credential_id"   TEXT,
    "model_id"        TEXT NOT NULL,
    "requested_units" DOUBLE PRECISION NOT NULL,
    "status"          TEXT NOT NULL,
    "cost_reconciled" BOOLEAN NOT NULL DEFAULT false,
    "submit_trace_id" TEXT NOT NULL,
    "created_at"      TIMESTAMPTZ(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "completed_at"    TIMESTAMPTZ(3),
    "canceled_at"     TIMESTAMPTZ(3),
    "expires_at"      TIMESTAMPTZ(3),
    CONSTRAINT "gateway_async_job_pkey" PRIMARY KEY ("provider_id","id")
);

CREATE INDEX IF NOT EXISTS "gateway_async_job_virtual_key_id_status_idx"
    ON "gateway_async_job" ("virtual_key_id", "status");
CREATE INDEX IF NOT EXISTS "RevokedToken_targetJti_idx"
    ON "RevokedToken" ("targetJti");
CREATE INDEX IF NOT EXISTS "traffic_event_end_user_id_timestamp_idx"
    ON "traffic_event" ("end_user_id", "timestamp");
CREATE INDEX IF NOT EXISTS "traffic_event_session_id_timestamp_idx"
    ON "traffic_event" ("session_id", "timestamp");

-- ---------------------------------------------------------------------------
-- Verification. Every count must be the number in the comment.
-- ---------------------------------------------------------------------------
SELECT 'Model new cols (expect 8)' AS check, count(*) AS n
  FROM information_schema.columns
 WHERE table_name = 'Model'
   AND column_name IN ('audioInputPricePerMillion','audioOutputPricePerMillion',
                       'cachedAudioInputReadPricePerMillion','family','minOutputTokens',
                       'requiredModalities','temperatureMax','temperatureMin')
UNION ALL
SELECT 'traffic_event new cols (expect 7)', count(*)
  FROM information_schema.columns
 WHERE table_name = 'traffic_event'
   AND column_name IN ('artifact_refs','compliance_coverage','embedding_provider_id',
                       'end_user_id','router_cost_usd','router_provider_id','session_id')
UNION ALL
SELECT 'vendor_bill new cols (expect 2)', count(*)
  FROM information_schema.columns
 WHERE table_name = 'vendor_bill_reconciliation'
   AND column_name IN ('our_internal_ops_usd','our_vendor_spend_usd')
UNION ALL
SELECT 'targetJti is text (expect 1)', count(*)
  FROM information_schema.columns
 WHERE table_name = 'RevokedToken' AND column_name = 'targetJti' AND data_type = 'text'
UNION ALL
SELECT 'gateway_async_job exists (expect 1)', count(*)
  FROM information_schema.tables WHERE table_name = 'gateway_async_job'
UNION ALL
SELECT 'new indexes (expect 4)', count(*)
  FROM pg_indexes
 WHERE indexname IN ('gateway_async_job_virtual_key_id_status_idx','RevokedToken_targetJti_idx',
                     'traffic_event_end_user_id_timestamp_idx','traffic_event_session_id_timestamp_idx');

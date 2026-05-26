-- E61 internal-ops cost accounting: dedicated columns for known internal
-- model calls (L2 embedding lookup, ai-guard classify) plus a catch-all
-- JSONB for future hook-type model calls so we never have to migrate
-- again to ship a new internal-ops cost type.
--
-- Rationale (per architecture review 2026-05-21): gateway makes "internal"
-- model calls that aren't the primary request but cost real money.
-- Existing pattern is per-row dedicated columns (estimated_cost_usd,
-- reasoning_cost_usd, cache_write_cost_usd, embedding_cost_usd). We
-- continue that pattern for ai-guard and add an open-ended JSONB so
-- hook-side LLM calls (prompt-shield, custom hooks) can record their
-- own line items without schema churn.
--
-- All columns nullable: pre-existing rows + non-touching code paths see
-- NULL, never zero, so analytics distinguishes "didn't run" from "ran
-- and was free".

ALTER TABLE "traffic_event"
    ADD COLUMN "ai_guard_cost_usd"       NUMERIC(20, 10),
    ADD COLUMN "internal_ops_breakdown"  JSONB;

-- No index — these are detail columns surfaced per-row, not aggregated at
-- query time (rollups SUM the existing primary cost columns plus these
-- as part of an ORG/VK filter the indexes already cover).

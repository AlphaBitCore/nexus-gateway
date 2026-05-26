-- E61 — Semantic Cache: time_sensitive_overrides JSONB column
--
-- Fix #3 — Time-Sensitive Patterns admin overrides persistence
--   Adds a JSONB column that stores admin-edited rules so they survive page
--   reloads. The Handler GET merges seed rules with DB overrides (DB wins per
--   ID); PUT/POST/DELETE update the JSONB blob then push merged to Hub shadow.
--
-- The earlier draft of this migration also added an `org_id` column + widened
-- the PK to (id, org_id) NULLS NOT DISTINCT for forward-compat with a SaaS
-- multi-tenant offering (Fix #37, FR-8.7). That work was reverted in
-- 2026-05-20: Nexus ships as a single-tenant on-prem product and the SaaS
-- direction was dropped. Independent of that, Prisma's @@id forbids
-- referencing optional fields, and Postgres PRIMARY KEY forbids NULL — the
-- proposed schema could not validate.
-- The directory name keeps the "_overrides_org" suffix only because rewriting
-- migration directory names invalidates the order Prisma tracks them in.
--
-- Idempotent: every statement uses IF NOT EXISTS / IF EXISTS guards so the
-- migration can be re-applied without error on a DB that already has the
-- column.
--
-- Pre-GA: no backward compatibility (CLAUDE.md development-phase policy).

ALTER TABLE semantic_cache_config
    ADD COLUMN IF NOT EXISTS time_sensitive_overrides JSONB NOT NULL DEFAULT '{"rules":[]}';

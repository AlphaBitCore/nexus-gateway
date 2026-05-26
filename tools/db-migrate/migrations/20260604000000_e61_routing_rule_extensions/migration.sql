-- E61 — Routing Rule: defaultExtensions JSONB column
--
-- Task D-3: adds a JSONB column for per-rule default nexus.ext.* overrides.
-- The gateway merges these into every canonical request body before the
-- provider adapter runs, so admins can set e.g. cohere input_type=search_document
-- on a rule without requiring every API client to send nexus.ext.cohere.input_type.
--
-- NULL = no rule-level extension defaults (current behaviour).
-- Shape: { "cohere": { "input_type": "search_document", ... }, ... }
--
-- Idempotent: ADD COLUMN IF NOT EXISTS guards re-application.
-- Pre-GA: no backward compatibility (CLAUDE.md development-phase policy).

ALTER TABLE "RoutingRule"
    ADD COLUMN IF NOT EXISTS "defaultExtensions" JSONB;

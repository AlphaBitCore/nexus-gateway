-- E48 S5 — traffic_event passthrough audit columns.
--
-- Every traffic_event row that fired any emergency-passthrough bypass
-- (E48 S4) carries the canonical-order flag set + the operator's
-- justification on these two columns. Empty / no-bypass rows leave
-- both NULL so the partial index stays compact.
--
-- passthrough_flags shape: TEXT[] with canonical-order values from
-- passthrough.Config.Flags() — currently one or more of
-- {bypassHooks, bypassCache, bypassNormalize}.

ALTER TABLE "traffic_event"
  ADD COLUMN "passthrough_flags"  TEXT[],
  ADD COLUMN "passthrough_reason" TEXT;

-- Partial index optimised for the common operator triage query
-- "show every request that fired any bypass since timestamp X".
-- Excludes the vast majority (no-bypass) rows so the index stays
-- proportional to the size of the incident window.
CREATE INDEX "traffic_event_passthrough_active_idx"
  ON "traffic_event" ("timestamp" DESC)
  WHERE "passthrough_flags" IS NOT NULL;

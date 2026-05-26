# E41-S1 — Schema additions (v2)

## Story
As a developer, I need the Credential and traffic_event tables to carry
the durable state needed for multi-window health, dominant-error
attribution, trend signal, per-credential threshold overrides, and a
sustained-degraded alert horizon, with an index optimised for the
rollup query.

## Migrations
The single migration `20260513*_e41_v2_credential_state_v2` does all of:

- **Credential** — drops `circuitAuthFailsSnapshot` (redundant with live
  Redis counter); renames `healthSampleCount` → `healthSamplesObserved`;
  renames `healthSuccessRate` → `healthSuccessRate5m`; adds
  `healthSuccessRate1h`, `healthDominantError`, `healthTrend`,
  `healthStatusChangedAt`, `reliabilityOverrides` (JSONB), plus the
  `Credential_healthStatus_idx` for the reliability-alerts sweep.
- **traffic_event** — drops the v1 full-table `(credential_id, timestamp)`
  secondary index; installs a partial index matching the rollup-job
  predicate: `(timestamp DESC, credential_id) WHERE source='ai-gateway'
  AND credential_id IS NOT NULL`.

## Acceptance Criteria
- `\d "Credential"` shows the new columns and lacks `circuitAuthFailsSnapshot`.
- `\d traffic_event` shows only `traffic_event_credential_health_rollup_idx`
  (the v1 full index is gone).
- All Go code paths that referenced the v1 field names build cleanly
  against the new struct shape.
- The migration is idempotent.

## Files Touched
- `tools/db-migrate/schema.prisma`
- `tools/db-migrate/migrations/20260513*_e41_v2_credential_state_v2/migration.sql`
- `packages/control-plane/internal/store/credential.go`
- `packages/ai-gateway/internal/store/credential.go`

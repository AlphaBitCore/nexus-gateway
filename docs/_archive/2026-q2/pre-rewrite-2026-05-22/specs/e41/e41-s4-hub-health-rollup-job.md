# E41-S4 — Hub credential-health-rollup (v2: multi-window + dominant error + trend)

## Story
As an operator, I want per-credential health to be computed from
`traffic_event` with attribution to *why* a credential is failing and a
trend signal that tells me whether things are getting better or worse.

## Tasks

### T4.1 — Single-query multi-window aggregate
`COUNT(*) FILTER (WHERE timestamp >= $short_start)` and the long-window
equivalents in a single GROUP BY pass. 429 responses are excluded; auth
failures, rate-limited (already excluded), upstream 5xx, timeout, and
non-auth 4xx are tallied per category for dominant-error attribution.

### T4.2 — Partial index
`traffic_event_credential_health_rollup_idx (timestamp DESC,
credential_id) WHERE source='ai-gateway' AND credential_id IS NOT NULL`.

### T4.3 — Classification
`classifyShort` (pure function) maps `(samples, success)` against the
effective Thresholds: `unknown` (0 samples) / `collecting` (< min) /
`healthy` (≥ healthy%) / `degraded` (≥ degraded%) / `unavailable`
(below degraded%).

### T4.4 — Dominant error
`dominantErrorOf` picks the failure category that exceeds 50 % of
failures; otherwise `mixed`. Pure function.

### T4.5 — Trend
`classifyTrend` compares short rate vs. long rate. Delta ≥ 5 pp →
`degrading`/`improving`; otherwise `stable`.

### T4.6 — Batch UPDATE
Single `UPDATE … FROM (VALUES …)` round. Rows whose status actually
changed get `healthStatusChangedAt = NOW()` (drives sustained-degraded
alert); unchanged rows keep their prior timestamp via the COALESCE
guard.

### T4.7 — Prometheus metrics
`nexus_hub_credential_health_rollup_*` (cycles, updated, candidates,
transitions, duration).

## Acceptance Criteria
- 4-sample credential ends up classified as `collecting`.
- 20 samples with 19 successes → `healthy`.
- 20 samples with 18 successes → `degraded`.
- All-401 credential → `unavailable` with `dominantError=auth_fail`.
- 60-second status flip increments `transitions_total{from,to}`.
- 429-only credential is NOT in the rollup result set.

## Files Touched
- `packages/nexus-hub/internal/jobs/credential_health_rollup.go`
- `packages/nexus-hub/internal/jobs/credential_health_rollup_test.go`

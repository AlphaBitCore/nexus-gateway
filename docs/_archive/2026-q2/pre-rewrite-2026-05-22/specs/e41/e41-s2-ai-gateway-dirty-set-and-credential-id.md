# E41-S2 — AI Gateway: credstate package, dirty-set marking, dynamic thresholds, observability

## Story
As an AI Gateway operator, I need the credential stats buffer to:
- read all Redis key names, field names, state enums, and threshold
  defaults from a single shared package (`packages/shared/schemas/credstate`);
- resolve effective Thresholds per call (global Hub config × per-cred
  override × shipped defaults);
- mark `cred:circuit:dirty` on every state transition (and only state
  transitions), so the Hub circuit-flush job has at-least-once delivery
  semantics;
- emit Prometheus metrics on every attempt, transition, and Redis
  failure.

## Tasks

### T2.1 — New shared package `packages/shared/schemas/credstate`
Single source of truth for `cred:stats:*` / `cred:circuit:*` key prefixes,
field names, state enums, dominant-error enums, `Thresholds` struct +
`DefaultThresholds`, `Merge`, and `Validate`. Imported by `credstats`,
`credpool`, Control Plane handlers, and Nexus Hub jobs.

### T2.2 — Rewrite `credstats.Buffer` constructor + RecordAttempt
- `New(rdb, logger, resolver, metrics)` injects a `ThresholdsResolver`
  function and an optional `*Metrics`.
- `RecordAttempt` resolves per-credential Thresholds, then dispatches:
  - stats pipeline always runs;
  - circuit transitions mark dirty inline (auth-fail open via Lua,
    rate-limit open, recovery close);
  - sub-threshold `auth_fails` increments stay quiet (no dirty mark).

### T2.3 — `credpool` uses credstate
Delete the duplicated `circuitDirtySet` constant + "circular import"
comment. The auto-promote-to-half-open path in `CircuitReader` and
`BulkCircuitStates` SADDs to `credstate.CircuitDirtySet` directly.

### T2.4 — Wire reliability config in `cmd/ai-gateway/main.go`
- New `reliability_config.go` owns the global `Thresholds` snapshot
  via `atomic.Pointer`, with `Reload` reading `system_metadata.gateway.credential_reliability.config`.
- `Resolve(credID)` composes `Default × global × per-cred override`.
- `OnConfigChanged` adds a `credential_reliability` case that reloads
  on Hub push.

### T2.5 — Prometheus metrics
`credstats.NewMetrics(reg)` registers:
- `nexus_ai_gateway_credstats_attempts_total{class}`
- `nexus_ai_gateway_credstats_circuit_transitions_total{to,reason}`
- `nexus_ai_gateway_credstats_auth_fail_increments_total`
- `nexus_ai_gateway_credstats_redis_write_failures_total{stage}`
- `nexus_ai_gateway_credstats_redis_write_seconds`

## Acceptance Criteria
- Unit tests assert `auth_fails` increments below threshold do NOT
  populate `cred:circuit:dirty`; the open transition does.
- A 429 alone opens the circuit and marks dirty.
- A recovery 2xx on a HALF_OPEN circuit DELs the hash and marks dirty.
- A per-credential `authFailThreshold=1` override opens the circuit on
  the very first 401 (proves the resolver flow).

## Files Touched
- `packages/shared/schemas/credstate/credstate.go`
- `packages/shared/schemas/credstate/credstate_test.go`
- `packages/ai-gateway/internal/credentials/stats/buffer.go`
- `packages/ai-gateway/internal/credentials/stats/buffer_test.go`
- `packages/ai-gateway/internal/credentials/pool/pool.go`
- `packages/ai-gateway/internal/store/credential.go`
- `packages/ai-gateway/cmd/ai-gateway/main.go`
- `packages/ai-gateway/cmd/ai-gateway/reliability_config.go`

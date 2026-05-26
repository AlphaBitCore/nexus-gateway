# E41-S3 — Hub credential-circuit-flush (v2: at-least-once + rehydrate + metrics)

## Story
As a Hub operator, I want the circuit-state durability path to survive a
process crash mid-flush without losing pending transitions, to recover
gracefully from a wiped Redis, and to be fully observable in Prometheus.

## Tasks

### T3.1 — In-flight set pattern
Each cycle SMOVEs the current `cred:circuit:dirty` cohort into
`cred:circuit:in_flight:{hubID}`, processes the working set, and DELs
the in-flight set only when every UPDATE succeeded. Partial failures
leave the working set for the next cycle.

### T3.2 — Reclaim before claim
On entry to each cycle, the job inspects its own in-flight set and
SMOVEs any leftover entries back to `cred:circuit:dirty` before claiming
the fresh cohort. Empty in steady state.

### T3.3 — First-run rehydrate from DB
`sync.Once`-guarded path queries `Credential WHERE circuitState !=
'closed'` and restores Redis hashes only when the Redis key is missing.
Skips `rate_limit` circuits whose `circuitNextProbeAt` has elapsed.

### T3.4 — Prometheus metrics
`NewCircuitFlushMetrics(reg)` registers cycles, flushed, reclaimed,
rehydrate outcomes, dirty-set size gauge, duration histogram, and
transition counters under `nexus_hub_credential_circuit_flush_*`.

### T3.5 — Wire in `main.go`
Pass `cfg.Hub.ID` for the per-Hub in-flight set namespace and a
non-nil metrics collector.

## Acceptance Criteria
- Two parallel writes to `cred:circuit:dirty` mid-cycle both end up
  persisted (the late one rolls over to the next cycle).
- Killing the Hub between SMOVE and UPDATE leaves the in-flight set
  intact; the next start reclaims it and re-flushes.
- Wiping Redis between cycles and restarting Hub: non-closed
  credentials reappear in Redis on the first post-restart cycle except
  rate_limit circuits past cooldown.
- `nexus_hub_credential_circuit_flush_cycles_total{outcome="ok"}`
  increments per cycle.

## Files Touched
- `packages/nexus-hub/internal/jobs/credential_circuit_flush.go`
- `packages/nexus-hub/cmd/nexus-hub/main.go`

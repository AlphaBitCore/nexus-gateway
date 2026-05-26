# E41-S6 — Alerting integration (state-table rules)

## Story
As an operator, I want Hub to raise actionable alerts when a credential
opens its circuit, becomes unavailable, or stays degraded for a
meaningful period, and to resolve them automatically once the situation
clears — without me having to watch the UI.

## Tasks

### T6.1 — Three new rule rows in `AlertRule`
Seeded in `tools/db-migrate/seed/seed-alerting.ts`:

- `credential.circuit_open` — severity `HIGH`, cooldown 300 s.
- `credential.health_unavailable` — severity `HIGH`, cooldown 300 s.
- `credential.health_degraded_sustained` — severity `MEDIUM`, cooldown
  1800 s.

### T6.2 — New `CredentialReliabilityAlertsJob`
Class-1 (state-table) job. Cadence 60 s. For every enabled credential:
- If `circuitState != 'closed'` → raise circuit_open.
- If `healthStatus = 'unavailable'` → raise health_unavailable.
- If `healthStatus = 'degraded'` and `NOW() - healthStatusChangedAt
  ≥ HealthSustainedDegradedSeconds` → raise health_degraded_sustained.

Auto-resolves alerts whose target credential no longer satisfies the
firing predicate, using the standard `alerting.Raiser.Resolve` flow.

### T6.3 — Wire into Hub `main.go`
Registered alongside the other Class-1 alert jobs inside
`if cfg.Scheduler.Enabled { … }`; receives the shared
`ReliabilityThresholdsLoader` so the sustained-degraded horizon honours
global config + future per-credential override semantics.

## Acceptance Criteria
- A credential with `circuitState='open'` fires the alert on the next
  cycle.
- Resetting the circuit (via admin API) auto-resolves on the cycle
  after `credential-circuit-flush` propagates closed state to DB.
- A credential held at `healthStatus='degraded'` for 16 minutes fires
  the sustained alert (default 15 min horizon); flipping to `healthy`
  resolves it.

## Files Touched
- `tools/db-migrate/seed/seed-alerting.ts`
- `packages/nexus-hub/internal/jobs/credential_reliability_alerts.go`
- `packages/nexus-hub/cmd/nexus-hub/main.go`
- `packages/nexus-hub/internal/config/config.go`

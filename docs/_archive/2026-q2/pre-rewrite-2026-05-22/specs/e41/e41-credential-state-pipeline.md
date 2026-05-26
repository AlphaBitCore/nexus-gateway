# E41 — Credential State Persistence & Reliability Pipeline

## Background

The AI Gateway has long buffered per-credential usage stats and circuit
breaker state in Redis, with periodic flush to PostgreSQL by Nexus Hub.
A 2026-05 audit surfaced four gaps in the v1 design (shipped earlier in
this epic):

1. Circuit-breaker state was never persisted to the Credential table.
2. No per-credential health classification existed.
3. `traffic_event.credential_id` was buried inside the `identity` JSONB.
4. The cache → DB pipeline was undocumented.

A follow-up review on the v1 ship surfaced six more issues that block
the design from being "production-grade":

1. Hub jobs had no Prometheus metrics.
2. The dirty-set flush was best-effort (a Hub crash between SREM and
   UPDATE silently lost transitions).
3. The Redis key constants were duplicated across `credstats` and
   `credpool` (with a comment blaming "circular import").
4. Health was coarse (no dominant-error attribution, no trend).
5. Thresholds were hardcoded, not admin-tunable.
6. No alerting integration — operators discovered credential issues by
   visiting the UI.

E41 v2 closes all ten gaps end-to-end.

## Glossary

- **Circuit breaker** — per-credential state machine: `closed → open`
  on 3 consecutive 401/403 *or* any single 429; `open → half_open`
  after the rate-limit cooldown; `half_open → closed` on any success.
- **Health classification** — periodic rollup over `traffic_event` that
  assigns one of `healthy / degraded / unavailable / unknown / collecting`
  to each credential.
- **Dominant error** — the failure category that accounts for >50 % of
  failures in the health window (`auth_fail / rate_limit / upstream_5xx
  / timeout / client_error / mixed / none`).
- **Trend** — direction of change between the 5 min and 1 h success
  rates (`improving / stable / degrading`).
- **Dirty set** — Redis SET (`cred:stats:dirty`, `cred:circuit:dirty`)
  of credential IDs with unflushed changes. Drained by the matching
  Hub flush job.
- **In-flight set** — per-Hub working set (`cred:circuit:in_flight:{id}`)
  used during a flush cycle so a crash mid-flush does not lose
  transitions.
- **Effective thresholds** — the resolved Thresholds for a credential
  after composing defaults × Hub-global × per-credential override.

## Personas

- **Operator / SRE** — needs to see live and durable circuit state in
  one place, with alerts when a credential opens or stays degraded.
- **Provisioning admin** — needs to test a new credential against its
  provider before relying on it.
- **Compliance reviewer** — needs Credential snapshots that include the
  circuit history (opened-at, reason).
- **Data-plane engineer** — needs no synchronous DB calls on the request
  path; observability that distinguishes "Redis down" from "DB down".

## Functional Requirements

### F1. Circuit-state persistence (Must)
- F1.1 — `Credential.circuitState / circuitReason / circuitOpenedAt /
  circuitNextProbeAt` track the durable view; `circuitAuthFailsSnapshot`
  is dropped.
- F1.2 — AI Gateway marks `cred:circuit:dirty` only on state transitions
  (open / close / auto half-open). Sub-threshold `auth_fails` increments
  stay quiet.
- F1.3 — Hub `credential-circuit-flush` job (default 30 s) drains the
  dirty set with **at-least-once** semantics via a per-Hub in-flight set.
- F1.4 — On first run after start the job rehydrates Redis from DB,
  skipping rate-limit circuits whose cooldown has elapsed.

### F2. Multi-window health classification (Must)
- F2.1 — `Credential.healthStatus` accepts `collecting` (samples > 0
  but below the min-samples threshold).
- F2.2 — `Credential.healthSuccessRate5m / healthSuccessRate1h /
  healthSamplesObserved` carry the dual-window data.
- F2.3 — `Credential.healthDominantError` carries the dominant-error
  category; `Credential.healthTrend` the trend signal.
- F2.4 — `Credential.healthStatusChangedAt` is set to NOW only when
  the status actually changes, so the sustained-degraded alert is
  driven by real horizon.
- F2.5 — `traffic_event` has a partial index optimised for the rollup
  query.

### F3. Per-credential thresholds + global config (Must)
- F3.1 — `Credential.reliabilityOverrides` (JSONB) carries an optional
  partial Thresholds.
- F3.2 — `system_metadata.gateway.credential_reliability.config` carries
  the global admin override.
- F3.3 — AI Gateway resolves effective Thresholds per call:
  defaults × global × per-credential override (`credstate.Thresholds.Merge`).
- F3.4 — Hub jobs read the global thresholds on every cycle.
- F3.5 — Both layers validate (degraded < healthy ≤ 100, all fields
  positive) on save.

### F4. Reliability alerts (Must)
- F4.1 — Three new AlertRule rows:
  `credential.circuit_open`, `credential.health_unavailable`,
  `credential.health_degraded_sustained`.
- F4.2 — A new Hub `credential-reliability-alerts` job (default 60 s)
  raises and resolves them based on persisted Credential state.

### F5. Synchronous probe (Must)
- F5.1 — AI Gateway exposes `POST /internal/v1/credentials/{id}/probe`
  that resolves the credential, decrypts, runs `adapter.Probe` with a
  configurable timeout (default 5 s, capped 30 s), and returns
  `{ ok, latencyMs, detail, error, providerName, adapterType, probedAt }`.
- F5.2 — Control Plane exposes `POST /api/admin/credentials/{id}/probe`
  with IAM gating + audit; proxies the body verbatim to AI Gateway.
- F5.3 — No `traffic_event` row is written.

### F6. Observability (Must)
- F6.1 — `credstats.Buffer` emits Prometheus counters / histogram for
  attempts, transitions, auth-fail increments, Redis write failures,
  pipeline latency.
- F6.2 — Hub `credential-circuit-flush` emits cycles / flushed /
  reclaimed / rehydrate-outcome / dirty-set-size / duration / transition
  metrics.
- F6.3 — Hub `credential-health-rollup` emits cycles / candidates /
  updated / transitions / duration metrics.

### F7. Admin UI (Must)
- F7.1 — List page replaces separate Circuit + Health columns with a
  single "Reliability" column + hover-card popover.
- F7.2 — Detail page has a new Reliability tab with 8 s polling, the
  Test Credential button, and the per-credential overrides editor.
- F7.3 — Settings page has a Credential Reliability tab for the global
  Thresholds.

### F8. Documentation (Must)
- F8.1 — Canonical doc `docs/developers/architecture/control-plane/credentials-architecture.md` reflects the
  v2 architecture.
- F8.2 — `docs/users/product/architecture.md` lists the new Redis namespaces and
  the in-flight pattern.
- F8.3 — SDD E41 S1-S6 rewritten.
- F8.4 — OpenAPI `docs/users/api/openapi/admin/e41-s5-admin-credentials-state.yaml`
  reflects the new shape.

## Non-Functional Requirements

- **NFR1 — Hot path latency**: No synchronous DB call on the AI Gateway
  request path. Buffer writes stay on Redis with a 200 ms pipeline.
- **NFR2 — DB write amplification**: All three flushes scale with
  changed-credential count, not credential population.
- **NFR3 — Recovery**: A Hub crash mid-flush re-processes its in-flight
  set on the next cycle (at-least-once). A wiped Redis is reconstructed
  on the next first-run rehydrate.
- **NFR4 — Multi-Hub safety**: Single-active-scheduler model continues
  to apply; new jobs sit inside the existing `cfg.Scheduler.Enabled`
  guard (no advisory locks).
- **NFR5 — Observability**: Every job emits cycles_total, duration
  histogram, and at least one outcome label.
- **NFR6 — Tunability**: Every threshold lives in code only as the
  shipped default; runtime values come from the Hub-global config and/or
  per-credential override.

## Priority

- F1 – F8: **Must**

## Out of Scope

- A `credential_circuit_event` audit table for retention of every
  transition. `traffic_event` reconstructs this if compliance asks.
- Selective half-open throttling (e.g. "send 10 % traffic for 5 min,
  then 25 %"). The current model is full open / half_open / closed.
- Replacing provider-level health (`ProviderHealth` is a separate
  table maintained by `provider-health-rollup`).

## Acceptance Criteria

- Hub restart with intact Redis: every open / half_open circuit remains.
- Hub restart with `FLUSHDB`: every persisted non-closed circuit is
  rehydrated on the next cycle, except rate-limit circuits past
  cooldown.
- A credential held degraded for 16 minutes fires
  `credential.health_degraded_sustained` (default 15 min horizon);
  flipping to healthy resolves it.
- Setting `authFailThreshold=1` on one credential opens its circuit on
  the very first 401 (proves resolver composition).
- The probe endpoint returns a usable result against a real provider
  within the configured timeout.
- `go test -race ./packages/{ai-gateway,nexus-hub,shared,control-plane}/...`
  green; `npx vitest run` green; `npx tsc --noEmit` clean.

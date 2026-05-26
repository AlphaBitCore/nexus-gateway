# E34 Story 3 — Hub-side alert evaluators (3 new rules + 4 in-process migrations)

**Epic:** 34 — Routing engine ID hygiene + alert centralization
**Story:** 3
**Status:** Draft — 2026-04-29
**Requirements:** chat 2026-04-29 — admin wants three new alert rules (hook reject, VK traffic spike, login failure flood) and confirms the project rule that **all alert evaluation runs in Hub** based on `traffic_event` + `AdminAuditLog`.
**OpenAPI:** N/A (no admin API surface change — `AlertRule.params` schema is the only contract and it lives in the rule registry)

## User Story

> **As a** platform admin watching for cost-leak attacks and operational incidents,
> **I want** Hub to centrally evaluate alert rules from `traffic_event` + `AdminAuditLog` so I can change rule logic in one place,
> **so that** new rule types do not require touching multiple data-plane services and Pre-GA migrations stay contained to Hub.

## Context — audit of existing rule placement (chat 2026-04-29)

| Rule ID | Today's evaluator | Today's source | This story |
|---|---|---|---|
| `quota.threshold` | Hub `quota_alert_check.go` | `metric_ops_*` rollup | unchanged |
| `quota.vk_expiring` | Hub `vk_expiry.go` | `VirtualKey` table (state) | unchanged |
| `thing.offline` | Hub `thing_offline_alerts.go` | `thing` table (last_seen) | unchanged |
| `provider.unavailable` | Hub `provider_unavailable_alerts.go` | `ProviderHealth` table | unchanged |
| `system.channel_test` | (none — synthetic) | N/A | unchanged |
| `proxy.high_error_rate` | **ai-gateway in-process** ring | local 5xx counter | **migrate to Hub job** (T6) |
| `proxy.cost_spike` | **ai-gateway in-process** | `db.CostSumSince` (already DB) | **migrate to Hub job** (T6) |
| `proxy.hook_failure_rate` | **compliance-proxy in-process** | local hook fail counter | **migrate to Hub job** (T6) |
| `proxy.hook_timeout_rate` | **compliance-proxy in-process** | local hook timeout counter | **migrate to Hub job** (T6) |
| `hook.reject_rate` | — (new) | `traffic_event.{request,response}_hook_decision` | **new Hub job** (T2) |
| `vk.traffic_spike` | — (new) | `traffic_event` per-vk window aggregation | **new Hub job** (T3) |
| `auth.login_failure_rate` | — (new) | `AdminAuditLog.action='admin.login.failed'` | **new Hub job** (T4) — depends on CP audit emission (T5) |

State-table rules (`vk_expiring`, `thing.offline`, `provider.unavailable`) **stay on state-table sources** by design — those events are not naturally first-class events; the row is the source of truth. `quota.threshold` reads `metric_ops_*` rollup which is already derived from `traffic_event`, so it satisfies the "event-derived" intent indirectly.

## Decisions locked in chat (2026-04-29)

1. **Plan B**: migrate the 4 in-process evaluators to Hub jobs based on `traffic_event`; keep the 3 state-table rules where they are.
2. **VK spike algorithm = (D)**: relative multiplier *and* absolute floor — `(last alertWindow req) > spikeMultiplier × (avg over baselineWindow by alertWindow chunks) AND (last alertWindow req) ≥ absFloorReq`. Default `spikeMultiplier=10, baselineWindowSec=3600, alertWindowSec=300, absFloorReq=50`. Cost double-signal (`useCost: bool`) optional but **not on by default** — req count alone is enough for v1; cost can be the v2 enhancement.
3. **VK spike target = vkID**. Cold-start: skip evaluation when the VK has < 24 h of traffic_event history (baseline insufficient).
4. **Hook reject rule scope = global per-thing** (not per-hook); details payload includes the breakdown `{ rejectByHook: { name: count } }` so the inbox shows which hook caused the spike.
5. **Login failure evaluator = Hub job**, sourceType `auth`. Depends on a small CP-side change (T5) to emit `admin.login.failed` audit rows — currently the login handler returns `errInvalidCredentials` without calling `h.Audit.LogObserved`.
6. **All four migrations (T6) bundle together** — single PR removing the in-process evaluators alongside the new Hub jobs. No "phase 1 keeps both running" period (per `feedback_no_backcompat_dev_phase`).

## Out of scope

- Cost double-signal for VK spike (`useCost: true`) — defer to v2 once req-count signal is validated in dev.
- VK spike per org / per project rolling — start with per-VK only. If false positives drown the inbox, add a second rule `org.traffic_spike` later.
- UI surface for `requestedModelLiterals`-style rule authoring of the new rules — admin uses default params; per-rule param editor already exists from earlier alerting work.
- Alert routing/dispatch changes — Hub `Raiser` API already covers the contract.

## Tasks

### T1. Rule registry — 3 new rule definitions (lockstep TS + Go)

Files:

- `tools/db-migrate/seed/seed-alerting.ts` — append 3 entries
- `packages/nexus-hub/internal/alerts/engine/rules/builtin.go` — append 3 entries (lockstep enforced by `TestBuiltinRulesMatchSeed`)

Three new entries (TS + Go versions identical except severity casing):

```ts
{
  id: 'hook.reject_rate',
  displayName: 'Hook Reject Rate',
  sourceType: 'proxy',
  defaultSeverity: 'HIGH',
  requiresAck: false,
  enabled: true,
  cooldownSec: 300,
  params: { thresholdPct: 5, windowSec: 300, minSamples: 20, decisionTypes: ['reject_hard', 'reject_soft'] },
  paramsSchema: {
    type: 'object',
    properties: {
      thresholdPct: { type: 'integer', minimum: 1, maximum: 100 },
      windowSec: { type: 'integer', minimum: 60 },
      minSamples: { type: 'integer', minimum: 1 },
      decisionTypes: { type: 'array', items: { type: 'string', enum: ['reject_hard', 'reject_soft', 'modify'] } },
    },
    required: ['thresholdPct', 'windowSec', 'minSamples'],
  },
},
{
  id: 'vk.traffic_spike',
  displayName: 'VK Traffic Spike',
  sourceType: 'proxy',
  defaultSeverity: 'CRITICAL',
  requiresAck: true,
  enabled: true,
  cooldownSec: 600,
  params: { spikeMultiplier: 10, baselineWindowSec: 3600, alertWindowSec: 300, absFloorReq: 50, coldStartHours: 24 },
  paramsSchema: {
    type: 'object',
    properties: {
      spikeMultiplier: { type: 'number', minimum: 2 },
      baselineWindowSec: { type: 'integer', minimum: 300 },
      alertWindowSec: { type: 'integer', minimum: 60 },
      absFloorReq: { type: 'integer', minimum: 1 },
      coldStartHours: { type: 'integer', minimum: 0 },
    },
    required: ['spikeMultiplier', 'baselineWindowSec', 'alertWindowSec', 'absFloorReq'],
  },
},
{
  id: 'auth.login_failure_rate',
  displayName: 'Login Failure Flood',
  sourceType: 'auth',
  defaultSeverity: 'HIGH',
  requiresAck: false,
  enabled: true,
  cooldownSec: 300,
  params: { thresholdCount: 20, windowSec: 300, groupBy: 'ip' },
  paramsSchema: {
    type: 'object',
    properties: {
      thresholdCount: { type: 'integer', minimum: 1 },
      windowSec: { type: 'integer', minimum: 60 },
      groupBy: { type: 'string', enum: ['ip', 'email', 'all'] },
    },
    required: ['thresholdCount', 'windowSec', 'groupBy'],
  },
},
```

### T2. Hub job — `hook_reject_alerts.go` (rule 1 evaluator)

File: `packages/nexus-hub/internal/jobs/hook_reject_alerts.go` (new)

- Pattern: copy structure from `quota_alert_check.go` (Run loop, Phase A/B/C with raise + resolve hysteresis).
- Tick interval: 60 s (rules with this kind of cadence in the codebase use 60–120 s).
- SQL (window = `params.windowSec` seconds):
  ```sql
  SELECT
    COALESCE(source_process, source) AS thing_key,
    COUNT(*) FILTER (WHERE request_hook_decision = ANY($2) OR response_hook_decision = ANY($2)) AS rejects,
    COUNT(*) AS total
  FROM traffic_event
  WHERE timestamp > NOW() - ($1 || ' seconds')::interval
  GROUP BY thing_key
  HAVING COUNT(*) >= $3
  ```
- For each (thing_key, rejects, total) row: `pct = rejects / total * 100`. Fire when `pct >= thresholdPct`.
- TargetKey: `thing:<thing_key>` (so the same thing dedups across runs).
- Details: `{ pct, rejects, total, windowSec, thresholdPct, decisionTypes }`. Add `rejectByHook` only if cheap (one extra SQL); the SDD permits a follow-up issue if the per-hook breakdown query is too expensive.
- Auto-resolve (Phase C): walk firing alerts; if their thing_key is no longer in the result set, or pct dropped below `thresholdPct - 1` (1-pp hysteresis), resolve with reason `auto`.

### T3. Hub job — `vk_traffic_spike_alerts.go` (rule 2 evaluator)

File: `packages/nexus-hub/internal/jobs/vk_traffic_spike_alerts.go` (new)

- Tick interval: 60 s.
- For every `entity_type = 'vk' AND entity_id IS NOT NULL`:
  1. **Cold-start gate**: `SELECT MIN(timestamp) FROM traffic_event WHERE entity_id = $vk`. If `NOW() - MIN < coldStartHours h`, skip.
  2. **Last alert window**: `SELECT COUNT(*) FROM traffic_event WHERE entity_id = $vk AND timestamp > NOW() - alertWindowSec * INTERVAL '1 second'`.
  3. **Baseline (per alertWindow chunk avg over baselineWindow)**:
     ```sql
     SELECT AVG(c) FROM (
       SELECT COUNT(*) AS c
       FROM traffic_event
       WHERE entity_id = $vk
         AND timestamp BETWEEN NOW() - (baselineWindowSec + alertWindowSec) * INTERVAL '1 second'
                          AND NOW() -  alertWindowSec                       * INTERVAL '1 second'
       GROUP BY date_trunc_sec(alertWindowSec, timestamp)
     ) b
     ```
     (Use `to_timestamp(floor(extract(epoch from timestamp) / alertWindowSec) * alertWindowSec)` if no helper exists — it's a one-liner.)
  4. Fire iff `lastWindowReq >= absFloorReq AND lastWindowReq > spikeMultiplier * baselineAvg`.
- TargetKey: `vk:<entity_id>`.
- Details: `{ lastReq, baselineAvg, spikeMultiplier, alertWindowSec, baselineWindowSec, absFloorReq, vkName }`.
- Resolve: when next tick's lastWindowReq drops below `spikeMultiplier × baselineAvg` (no extra hysteresis — alertWindow is short enough that flapping is bounded by `cooldownSec=600`).
- **Performance**: this loops over enabled VKs. Cap with `LIMIT 1000` and add a `WHERE enabled = true` on the VK list query; if the dev DB has more than 1k VKs we'll need an index strategy in v2 (note in code).

### T4. Hub job — `login_failure_flood_alerts.go` (rule 3 evaluator)

File: `packages/nexus-hub/internal/jobs/login_failure_flood_alerts.go` (new)

- Tick interval: 60 s.
- Aggregate query (groupBy = `ip`):
  ```sql
  SELECT "sourceIp" AS group_key, COUNT(*) AS failures
  FROM "AdminAuditLog"
  WHERE action = 'admin.login.failed'
    AND timestamp > NOW() - ($1 || ' seconds')::interval
    AND "sourceIp" IS NOT NULL
  GROUP BY "sourceIp"
  HAVING COUNT(*) >= $2
  ```
- For `groupBy = 'email'`: group by `actorLabel` instead. For `'all'`: a single COUNT over the window without GROUP BY (one alert).
- TargetKey: `login:<groupBy>:<group_key>` (or `login:all` for `'all'`).
- Details: `{ failures, windowSec, groupBy, groupKey }`.

**Depends on T5** — without T5 the table has zero matching rows and the rule would never fire, which violates "Real implementation only" (the rule would be a placeholder). Land T5 first or in the same PR.

### T5. CP — emit `admin.login.failed` audit row on credential failure

File: `packages/control-plane/internal/authserver/login/password.go` (and any other login entrypoint that returns `errInvalidCredentials` without auditing — search for `errInvalidCredentials` usages).

- After every credential-mismatch return, call `h.Audit.LogObserved(ctx, audit.Entry{Action: "admin.login.failed", ActorLabel: <attempted email>, SourceIP: <c.RealIP()>, ...})`.
- Be careful not to leak whether the email exists: log `actorLabel = <email-as-typed>` regardless of user existence. Server-side timing-safe behavior is unchanged.
- Also audit `admin.login.succeeded` (zero cost, observability win) — currently grep finds no audit emission from login at all.

### T6. Migrate 4 in-process evaluators to Hub (delete old, add new)

**Sub-tasks (all in the same PR per pre-GA no-backcompat rule):**

- T6a. `proxy.high_error_rate` → Hub job `high_error_rate_alerts.go`
  - SQL: `SELECT thing_key, COUNT(*) FILTER (WHERE status_code >= 500) / NULLIF(COUNT(*), 0) AS pct FROM traffic_event WHERE timestamp > NOW() - $1 INTERVAL GROUP BY thing_key HAVING COUNT(*) >= $2`.
  - Delete `packages/ai-gateway/internal/alerting/checks_high_error_rate.go` + its test + the eval.Register call in `cmd/ai-gateway/main.go`.

- T6b. `proxy.cost_spike` → Hub job `cost_spike_alerts.go`
  - SQL already lives in `db.CostSumSince` — port it to Hub `cost_sum_since.go` and call from the new job.
  - Delete `packages/ai-gateway/internal/alerting/checks_cost_spike.go` + test + eval.Register.

- T6c. `proxy.hook_failure_rate` → Hub job `hook_failure_alerts.go`
  - SQL: count traffic_event rows where the relevant hook decision indicates an error/exception (the existing `details` JSON or a new flag — needs verification of what compliance-proxy currently writes).
  - Delete the in-process check from compliance-proxy.

- T6d. `proxy.hook_timeout_rate` → Hub job `hook_timeout_alerts.go`
  - Same as T6c but for timeout decisions.
  - Delete the in-process check.

- T6e. After deletion, `packages/ai-gateway/internal/alerting/` may have nothing left except the Evaluator interface + the cost-spike DB helper that moved to Hub. **Delete the package entirely if empty** — pre-GA rule. Same for compliance-proxy's check files.

### T7. main.go wire-up + tests

Files:

- `packages/nexus-hub/cmd/nexus-hub/main.go` — register 7 new jobs (3 from T2/T3/T4 + 4 from T6a–d). Match the existing `jobs.NewXxx(...)` registration block style.
- `packages/nexus-hub/internal/jobs/<each_new_job>_test.go` — table-driven tests using a fake `alertRaiser` and either an in-memory pool or a real test DB if the rest of the jobs package uses one (check `quota_alert_check_test.go` for the pattern).
- `packages/nexus-hub/internal/alerts/engine/rules/registry_test.go` — should automatically detect the new rule IDs in BuiltinRules; if there's a `TestBuiltinRulesMatchSeed`, it'll need the TS seed updated in T1.

### T8. Verify

- (AC1) `go test -race -count=1 ./packages/nexus-hub/... ./packages/ai-gateway/... ./packages/compliance-proxy/...` — green.
- (AC2) DB seed reset + verify 12 rules in `AlertRule` (was 9 + 3 new).
- (AC3) Restart Hub + ai-gateway + compliance-proxy. Confirm no in-process evaluator threads start (grep logs for "alertclient" / "evaluator" startup messages — should now only come from Hub).
- (AC4) Live trigger: send 30 hook-rejected requests through the gateway (use rule-pack hook with deny rule); within 60 s a `hook.reject_rate` alert appears via `GET /api/admin/alerts`. Same for spike (synthetic burst of requests on one VK) and login flood (30 wrong-password POSTs from the same IP).
- (AC5) After T6 migration: `lsof -nP -iTCP:3050,3040 -sTCP:LISTEN` still shows ai-gateway/compliance-proxy alive (only the alerting subsystem is gone).

## Acceptance Criteria summary

- **AC1** All Go tests green (router + new Hub jobs + migrated jobs).
- **AC2** 12 rules in `AlertRule` table after seed reset.
- **AC3** No data-plane process holds an `alerting.Evaluator`.
- **AC4** Live triggers prove all 3 new rules fire end-to-end.
- **AC5** Data-plane services still run normally (no regression beyond removing the alerting subsystem).
- **AC6** TS seed and Go BuiltinRules pass `TestBuiltinRulesMatchSeed` lockstep.

## Notes for reviewer

- This story preserves the state-table rules (`vk_expiring`, `thing.offline`, `provider.unavailable`) on their existing data sources because the underlying signal *is* a state, not an event stream — fabricating event-derived equivalents would be over-architecture without changing behavior.
- `proxy.cost_spike` migration in T6b is the only one that already had a DB-backed evaluator (`db.CostSumSince`); the move from ai-gateway to Hub is a code relocation, not an algorithm change. Behavior should be identical.
- VK spike (T3) is the only rule that needs **per-VK enumeration**. If dev has many disabled / archived VKs, gate the loop on `WHERE enabled = true`. Index hint: existing `traffic_event_entity_idx` (verify) already covers `WHERE entity_id = $1 AND timestamp >`; if not, add an index — but only after the Run timing actually shows it being slow.
- T5 (CP login audit emission) is a tiny change but a real correctness gap — even without T4 it should ship, because zero login audit is itself a security-posture problem.

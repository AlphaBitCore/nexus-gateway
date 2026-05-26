# Hub Alerting — Streaming Evaluator Engine (alerteval)

**Date:** 2026-04-29
**Status:** Draft — supersedes the per-job approach in `docs/sdd/e34-s3-hub-side-alert-evaluators.md` (which will be rewritten to match this design).
**Inherits:** `docs/superpowers/specs/2026-04-21-unified-alerting-design.md` — the alert data model (`AlertRule`, `Alert`, `AlertChannel`), the `Raiser` API, and the dispatcher are already locked there. This spec only redesigns the **evaluator** side.
**Related prior art:** `docs/superpowers/specs/2026-04-16-scheduler-single-instance-design.md` for the "enable on exactly one instance" pattern.

## 1. Problem Statement

The unified alerting design (`2026-04-21`) defined 9 rules and split evaluators into two camps:

- **Hub-evaluated rules**, each implemented as a 60s job that queries Postgres (`quota_alert_check.go`, `vk_expiry.go`, `thing_offline_alerts.go`, `provider_unavailable_alerts.go`).
- **Data-plane-raised rules**, each implemented as an in-process ring counter inside ai-gateway / compliance-proxy that POSTs to Hub's `/internal/alerts/raise` (`proxy.high_error_rate`, `proxy.cost_spike`, `proxy.hook_failure_rate`, `proxy.hook_timeout_rate`).

This story (E34-S3) needs to add **3 new rules** and address the locked architectural rule that **all alert evaluation must run in Hub**:

1. `hook.reject_rate` — % of `traffic_event` rows where `request_hook_decision` or `response_hook_decision` is a reject decision, per thing, per window.
2. `vk.traffic_spike` — per-VK request count in the last `alertWindowSec` exceeds `spikeMultiplier × baselineAvg(baselineWindowSec)` AND `≥ absFloorReq`.
3. `auth.login_failure_rate` — count of `AdminAuditLog` rows with `action = 'admin.login.failed'` in the last `windowSec`, grouped by `sourceIp` / `actorLabel` / all.

The straight-line migration (per the original SDD) is to write 7 new 60s Hub jobs (3 new + 4 migrated from data-plane). That works, but:

- **DB scan cost** — 7 jobs each running a `WHERE timestamp > NOW() - windowSec GROUP BY thing_key` on `traffic_event` every 60s. As traffic_event grows, this is constant baseline DB load that scales linearly with the number of event-stream rules.
- **Latency floor 60s** — the next "p95 latency spike" or "queue stall" rule we want to add will be 5-10s granular; the per-job DB pattern can't hit that.
- **Boilerplate per rule** — each job re-implements Phase A (aggregate) / Phase B (raise) / Phase C (resolve with hysteresis) / cooldown. The 8th and 9th rule will copy the same template again.

The MQ topology already gives us a better path: traffic events and admin-audit events flow through NATS JetStream queues that Hub subscribes to (consumer group `hub-db-writer` writes them into `traffic_event` / `AdminAuditLog`; another group `hub-siem` forwards them to SIEM). Adding a third consumer group (`hub-alerting`) gives Hub the in-flight stream — alerts can update in-memory ring buffers in O(1) per event, fire within seconds, and never touch the `traffic_event` table for evaluation.

## 2. Scope and Non-Goals

### In scope (this design)

- **Class 4 rules only** — the 7 event-stream rules (3 new + 4 migrated). They get a new shared engine.
- **Single-instance enforcement** for **all** alerting subsystems on Hub (Engine + the 4 existing DB-job evaluators + the rollup-derived `quota.threshold` job + synthetic `system.channel_test`). One config flag, one enforcement point.
- **Migration path** for the 4 data-plane in-process evaluators (`proxy.*`) to the Engine, including deletion of the old in-process code (pre-GA, no parallel period).
- **Cold-start handling** — restart of the alerting Hub instance must not produce false-fires from partially-warm windows.

### Out of scope (explicitly)

- **Runtime toggle of `Scheduler.Enabled`** — this flag is **YAML / env only**, read once at Hub startup. There is no admin API, no UI control, no `PATCH /api/admin/hub/scheduler/enabled`. To move scheduler+alerting to a different Hub instance: edit YAML on both old and new instances, restart both. Avoiding a runtime toggle eliminates "switchover gap" and "double-active" failure modes; the operator cost (~1 min restart) is a worthwhile trade. Auto-failover is the HA epic's job, not this story's.
- **Class 1 rules** — `quota.vk_expiring`, `thing.offline`, `provider.unavailable`. Their signal is a state-table row, not an event; they continue as DB-query Hub jobs.
- **Class 2 rule** — `system.channel_test` (synthetic). No evaluator at all.
- **Class 3 rule** — `quota.threshold`. Reads `metric_ops_*` rollup, which is itself derived from `traffic_event` by a separate 5-minute rollup job. The cadence matches the rollup natural beat; rewriting it as streaming is over-engineering.
- **Hub HA / leader election** — pre-GA, the alerting instance is **chosen by config** (mirrors `2026-04-16-scheduler-single-instance-design.md`). Auto-failover is a future epic.
- **Restart-time DB replay** — the alerting instance does NOT replay events from `traffic_event` after restart. Cold-start gating below is the entire restart story.
- **Cost double-signal for VK spike** — req-count only for v1 (per the existing SDD).
- **Per org / per project rollup of VK spike** — per-VK only for v1.
- **Sub-second granularity** — Engine ticks at 5–10s, not finer.

## 3. Architectural Decisions (locked)

| ID | Decision | Reason |
|---|---|---|
| A1 | **All Hub scheduled jobs (operational + alerting) are gated by the existing `cfg.Scheduler.Enabled` flag, set to `true` on exactly one Hub instance.** This includes: rollup (`metrics_rollup`, `rollup_5m`, `rollup_correction`), retention (`data_retention`, `rollup_retention`, `ops_retention`), audit chain verify, drift detector, identity enricher, exemption GC, the 4 existing alerting state-table jobs, and the new alerteval Engine. **No new flag.** | Mirror existing scheduler-single-instance pattern. The current Hub `main.go` (lines 324–425) already wraps every scheduled job in `if cfg.Scheduler.Enabled { ... }`; the alerteval Engine joins this same gate. Adding a separate `alerting.enabled` would create operator confusion ("which flag do I set?") and a new failure mode ("scheduler enabled but alerting disabled, or vice versa"). |
| A2 | **No anonymous jobs anywhere on Hub.** Every long-running goroutine that does evaluator / scheduler / consumer work registers with a stable name in either `sched.Register(...)` (the existing scheduler registry) or the consumer manager. The alerteval Engine registers as job name `alerteval-engine`; each Aggregator inside is enumerable via Engine introspection (see §11 diag endpoint). | Operators need a complete inventory of "what is this Hub instance running?" without reading source. Already the convention for current Hub jobs (`metrics_rollup`, `data_retention`, `rollup_5m`, etc. — all named). Anonymous goroutines = future ops mystery. |
| A3 | The Engine is one new package (`packages/nexus-hub/internal/alerts/eval/`) that owns: MQ subscription for the `hub-alerting` consumer group, rule registry walk, in-memory window state, tick loop, raise/resolve via `alerting.Raiser`. | Single package = single contract. Future class-4 rules plug into this engine; future authors don't have to copy a 60s job template. |
| A4 | The Engine subscribes to a **new consumer group** `hub-alerting` on the same 4 MQ queues (`nexus.event.{ai-traffic, compliance, agent, admin-audit}`). Independent of `hub-db-writer` and `hub-siem` — fan-out is JetStream's job. | Existing fan-out precedent (`hub-siem` already does this). Failure to ack on the alerting side does not block the db writer. |
| A5 | Engine tick = **5 seconds** (configurable, but default 5s). Each tick walks every registered Aggregator and asks "any samples to consider?". Sample evaluation against threshold + raise/resolve is synchronous within the tick. | Sub-minute alerts on burst patterns (`vk.traffic_spike`, login flood). 5s is the minimum window many existing alert systems use; further down requires per-event eval which is overkill. |
| A6 | Window state is a **per-Aggregator, per-target ring buffer of 1-second buckets** sized to the rule's longest configured window. Buckets store `(count, sum)` for univariate aggregators or `(num, denom)` for ratios. | 1s bucket granularity is plenty for alerting (no rule needs finer). Ring buffer is O(1) update per event, O(N) eval per tick where N = bucket count (max ~3600 for a 1h baseline). |
| A7 | Cold-start gate per Aggregator: until `(now - aggregatorStartTime) >= rule.minWarmupSec`, **skip evaluation** (no fire, no resolve). `minWarmupSec` defaults to the rule's window (or `baselineWindow + alertWindow` for baseline-comparison rules). | Restart-tolerance without DB replay. Trade-off: the alerting node going down means a blackout window equal to the longest configured alerting window. Acceptable for pre-GA; HA epic addresses it later. |
| A8 | **Every rule's `AlertRule.params` is re-read from DB on every tick** by both the new Engine *and* (going forward) the existing class-1/2/3 jobs. Threshold/window-size changes propagate within one tick. Schema validated by the same JSON Schema already in `AlertRule.paramsSchema` (validation happens on admin write — `validateParamsAgainstSchema` in `handlers_admin.go:691`, using `github.com/santhosh-tekuri/jsonschema/v5`). | Operators tune live; no Hub restart for param changes. **This decision also closes a current correctness gap** documented in §7.6: 4 of the migrating rules currently consume hardcoded values from data-plane YAML (params edits in admin UI did nothing); 1 state-table rule (`provider.unavailable`) reads params but doesn't apply them. The Engine's per-tick reload makes "edit in UI → live in 5s" the contract. |
| A9 | All 4 in-process data-plane evaluators (`proxy.*`) and their wiring (`eval.Register`, `alerting.Evaluator` interface, ring counters, `internal/alerting` packages in ai-gateway and compliance-proxy) are **deleted in the same PR** that lands the Engine for those rules. No parallel "phase 1 keeps both" period. | CLAUDE.md rule: pre-GA + no installed user base. Two evaluators racing on the same rule = duplicate alerts. Single-PR cutover. |
| A10 | Rule definitions stay split across `tools/db-migrate/seed/seed-alerting.ts` (UI-visible defaults) and `packages/nexus-hub/internal/alerts/engine/rules/builtin.go` (Go runtime registry). The existing `TestBuiltinRulesMatchSeed` lockstep test gates them. | Inherited from unified-alerting design. No change. |
| A11 | The Engine uses the **existing** `alerting.Raiser` (`packages/nexus-hub/internal/alerts/engine/raiser.go`) for `Raise(ctx, RaiseInput)` and `Resolve(ctx, ResolveInput)`. No new alert raise path. | The dispatcher / suppression / dedup logic on top of Raiser is unchanged. Engine just feeds Raiser. |

## 4. High-Level Architecture

```
Hub instance(s) ─────────────────────────────────────────────────────────────
│
├─ Always-on (every Hub instance)
│   ├─ Thing Registry / Device Shadow / Config Sync / Admin API
│   ├─ TrafficEventWriter   (consumer group: hub-db-writer)
│   ├─ AdminAuditWriter     (consumer group: hub-db-writer)
│   ├─ SIEMForwarder        (consumer group: hub-siem)        — optional
│   └─ MetricsRollup / DataRetention / RollupCorrection / SiemBridge
│
└─ Scheduler+alerting instance only  ──  if cfg.Scheduler.Enabled  ──  exactly one Hub
    ├─ Class 1 jobs (DB query, unchanged)
    │   ├─ vk_expiry.go
    │   ├─ thing_offline_alerts.go
    │   └─ provider_unavailable_alerts.go
    ├─ Class 3 job (rollup-derived, unchanged)
    │   └─ quota_alert_check.go         — reads metric_ops_*
    ├─ Class 2 (synthetic, unchanged)
    │   └─ channel_test handler
    └─ alerteval.Engine (NEW) — class 4 (event-stream) rules
        ├─ subscribe MQ (consumer group: hub-alerting)
        │     - nexus.event.ai-traffic
        │     - nexus.event.compliance
        │     - nexus.event.agent
        │     - nexus.event.admin-audit
        ├─ event router → fans out to registered Aggregators
        ├─ Aggregator instances (one per active class-4 rule)
        │     ├─ hook.reject_rate           — RatioInWindow
        │     ├─ vk.traffic_spike           — CompareToBaseline
        │     ├─ auth.login_failure_rate    — CountInWindow
        │     ├─ proxy.high_error_rate      — RatioInWindow
        │     ├─ proxy.cost_spike           — SumInWindow
        │     ├─ proxy.hook_failure_rate    — RatioInWindow
        │     └─ proxy.hook_timeout_rate    — RatioInWindow
        └─ tick loop (5s) → for each Aggregator: walk samples → threshold → Raiser
```

The single-instance gate is at `cmd/nexus-hub/main.go`:

```go
// existing
ar := alerting.NewRaiser(...)
ad := alerting.NewDispatcher(...)
adminAlertsHandler := alerting.NewHandler(ar, ad, ...)   // serves /api/admin/alerts (always on; read-only inspection works on every instance)

// (Inside the existing `if cfg.Scheduler.Enabled { ... }` block — no new gate.
// All operational jobs (rollup, retention, etc.) already register here today.)
if cfg.Scheduler.Enabled {
    // ... existing operational job registrations: rollup, retention, audit_chain_verify, ...

    // Existing alerting state-table jobs (class 1 + 3, unchanged)
    sched.Register(jobs.NewVKExpiry(...))
    sched.Register(jobs.NewQuotaAlertCheck(...))
    sched.Register(jobs.NewThingOfflineAlerts(...))
    sched.Register(jobs.NewProviderUnavailableAlerts(...))

    // NEW: alerteval Engine (class 4 — event-stream rules)
    eng := alerteval.NewEngine(...)
    eng.Register(aggregators.NewHookRejectRate())
    eng.Register(aggregators.NewVKTrafficSpike())
    eng.Register(aggregators.NewLoginFailureFlood())
    eng.Register(aggregators.NewProxyHighErrorRate())
    eng.Register(aggregators.NewProxyCostSpike())
    eng.Register(aggregators.NewProxyHookFailureRate())
    eng.Register(aggregators.NewProxyHookTimeoutRate())
    sched.Register(eng)              // job name: alerteval-engine
}
```

Read-only inspection (`GET /api/admin/alerts` and friends) stays on every instance because it just reads `alert` table — no need to gate.

## 5. Engine Internals

### 5.1 Package layout

```
packages/nexus-hub/internal/alerts/eval/
├── engine.go             — Engine struct, lifecycle (Start / Stop), MQ subscribe, tick loop
├── event.go              — Event types: TrafficEvent, AuditEvent (decoded from MQ payloads)
├── aggregator.go         — Aggregator interface
├── window.go             — Window primitive (1s-bucket ring buffer), thread-safe per Aggregator
├── runtime.go            — runtime state per (Aggregator, target_key): window + lastFiredAt + cooldownUntil
├── raise.go              — fire/resolve adapter from Aggregator output → alerting.Raiser
├── aggregators/          — concrete rule implementations
│   ├── count_in_window.go        — generic helper (used by login_failure_flood, others)
│   ├── ratio_in_window.go        — generic helper (4 rules)
│   ├── sum_in_window.go          — generic helper (cost_spike)
│   ├── compare_to_baseline.go    — generic helper (vk.traffic_spike)
│   ├── hook_reject_rate.go       — wires RatioInWindow to traffic events
│   ├── vk_traffic_spike.go       — wires CompareToBaseline + cold-start gate
│   ├── login_failure_flood.go    — wires CountInWindow to audit events
│   ├── proxy_high_error_rate.go  — wires RatioInWindow
│   ├── proxy_cost_spike.go       — wires SumInWindow
│   ├── proxy_hook_failure.go     — wires RatioInWindow
│   └── proxy_hook_timeout.go     — wires RatioInWindow
└── engine_test.go / *_test.go    — unit tests (table-driven, in-memory MQ fake)
```

### 5.2 The `Aggregator` interface

```go
type Aggregator interface {
    // RuleID matches AlertRule.id (e.g. "hook.reject_rate"). Engine looks up
    // this rule on every tick to read params and check enabled.
    RuleID() string

    // Sources lists which MQ queue subjects this aggregator wants events from.
    // The Engine routes incoming events to aggregators based on subject.
    Sources() []string  // subset of: nexus.event.{ai-traffic, compliance, agent, admin-audit}

    // OnEvent is called for every matching event. Must be O(log N) at worst.
    // Implementations extract the target_key, decide if the event "counts", and
    // update their internal Window via the supplied Runtime handle.
    OnEvent(rt *Runtime, evt Event)

    // MinWarmupSec returns how long the aggregator must run before firing is
    // allowed (cold-start gate). Reads rule params via rt.Params (allows
    // dynamic windows). Returning 0 disables the gate.
    MinWarmupSec(params RuleParams) int

    // Tick is called every Engine tick (default 5s). Aggregator inspects its
    // window state per target_key, evaluates threshold, and emits Decisions
    // (fire / resolve / hold). Engine handles the actual Raiser call.
    Tick(rt *Runtime, params RuleParams, now time.Time) []Decision
}

type Decision struct {
    Action     DecisionAction  // Fire | Resolve | Hold
    TargetKey  string
    Severity   alerting.Severity   // optional override; nil = use rule default
    Details    map[string]any
    Reason     string
    SourceType string                // denormalized from rule (proxy/auth/etc)
}
```

`Runtime` is per-Aggregator state owned by the Engine: a map `targetKey → *Window` plus `lastFireAt[targetKey]`, plus the rule's current params snapshot. Aggregator methods receive it as a handle so all per-target bookkeeping is abstracted away.

### 5.3 Window primitive

```go
// Window is a fixed-bucket ring buffer keyed by 1-second epoch.
// Each bucket stores (a, b) — interpretation depends on aggregator type:
//   - CountInWindow:  (count, _)
//   - RatioInWindow:  (numerator, denominator)
//   - SumInWindow:    (sum_x, count) — count for sanity, sum_x is the metric
type Window struct {
    buckets   []bucketAB        // size = max window seconds + 1
    headEpoch int64             // unix-seconds for buckets[head]
    head      int
    mu        sync.Mutex
}

func (w *Window) Add(at time.Time, a, b float64) { /* advances head if needed; adds to current bucket */ }
func (w *Window) Sum(window time.Duration, now time.Time) (a, b float64) { /* walks last N buckets */ }
```

For aggregators with both alertWindow and baselineWindow (only `vk.traffic_spike`), the Window is sized to `baselineWindow + alertWindow` and the Aggregator computes both views by summing different sub-ranges.

### 5.4 Tick loop pseudocode

```
every 5 seconds {
    rules = db.LoadAlertRules(activeIDs)   // single SELECT for all engine rules
    for each Aggregator agg in registry {
        rule = rules[agg.RuleID()]
        if rule == nil || !rule.Enabled { continue }
        rt = engine.RuntimeFor(agg)
        rt.Params = rule.Params
        if (now - rt.StartTime) < agg.MinWarmupSec(rule.Params) { continue }   // cold-start gate
        decisions = agg.Tick(rt, rule.Params, now)
        for each d in decisions {
            if d.Action == Fire {
                if now < rt.CooldownUntil[d.TargetKey] { continue }
                raiser.Raise(...)
                rt.CooldownUntil[d.TargetKey] = now + rule.CooldownSec
            } else if d.Action == Resolve {
                raiser.Resolve(... TargetKey=d.TargetKey, Reason="auto" ...)
            }
        }
    }
}
```

### 5.5 Per-rule mapping

| Rule ID | Aggregator type | Source queue | OnEvent counts ... | Tick threshold | TargetKey |
|---|---|---|---|---|---|
| `hook.reject_rate` | RatioInWindow | ai-traffic, compliance, agent | num: rows where `request_hook_decision OR response_hook_decision IN params.decisionTypes`; denom: all rows | `denom >= minSamples AND num/denom*100 >= thresholdPct` | `thing:<thing_key>` |
| `vk.traffic_spike` | CompareToBaseline | ai-traffic, compliance, agent | count: rows where `entity_type='vk'` (per `entity_id`) | `lastWindowCount >= absFloorReq AND lastWindowCount > spikeMultiplier × avg(baseline buckets)` | `vk:<entity_id>` |
| `auth.login_failure_rate` | CountInWindow | admin-audit | count: rows where `action='admin.login.failed'` (per groupBy key) | `count >= thresholdCount` | `login:<groupBy>:<key>` (`login:all` for `groupBy=all`) |
| `proxy.high_error_rate` | RatioInWindow | ai-traffic, compliance, agent | num: rows where `status_code >= 500`; denom: all rows | same shape as hook.reject_rate | `thing:<thing_key>` |
| `proxy.cost_spike` | SumInWindow | ai-traffic | sum: `cost_usd` per VK | `sum >= thresholdUsd` | `vk:<entity_id>` |
| `proxy.hook_failure_rate` | RatioInWindow | compliance | num: rows where any hook decision indicates exec failure (TBD field — see §7.3); denom: all rows where hooks ran | same shape | `thing:<thing_key>` |
| `proxy.hook_timeout_rate` | RatioInWindow | compliance | num: rows where any hook decision indicates timeout; denom: all rows where hooks ran | same shape | `thing:<thing_key>` |

### 5.6 Resolve / hysteresis

Each Aggregator's `Tick` walks all live target_keys (those with non-zero buckets in the last window). For each, if the threshold check now returns false **and** there is currently a firing alert for `(rule, targetKey)`, emit `Decision{Action: Resolve}`. The Engine looks up the firing alert via `Raiser.Resolve(ctx, ResolveInput{RuleID, TargetKey, Reason: "auto"})`.

Hysteresis is built into the threshold check at the aggregator level: e.g. RatioInWindow uses `pct >= thresholdPct` for fire and `pct < thresholdPct - 1` for resolve. `vk.traffic_spike` uses no hysteresis margin — its alertWindow + cooldown bound the flap rate.

### 5.7 Cold-start gate behavior

On Engine start, every Aggregator's Runtime gets `StartTime = now`. The Tick loop checks `(now - StartTime) >= agg.MinWarmupSec(params)` before invoking `agg.Tick`. Default MinWarmupSec values:

- CountInWindow / RatioInWindow / SumInWindow: `params.windowSec`
- CompareToBaseline (vk.traffic_spike): `params.baselineWindowSec + params.alertWindowSec`

This means a Hub restart causes a blackout window equal to the longest configured alerting window for each rule. With the SDD's defaults (max baselineWindow = 3600s = 1h for vk.traffic_spike; 300s for others), the worst case is 1h of `vk.traffic_spike` blindness after restart. Operators who care can override `coldStartHours` per rule.

A future extension (out of scope here) is to seed the Window from `traffic_event` at start. The hook is `Aggregator.PreloadFrom(db, since)` — leave a `// TODO(alerteval): seed window from DB` comment in the engine `Start` for the day we want it.

## 6. Single-Instance Enforcement (reuse existing `Scheduler.Enabled`)

### 6.1 Config — no new flag

The Hub already has `cfg.Scheduler.Enabled` (and env `SCHEDULER_ENABLED=true`) which gates **every** scheduled job currently registered in `cmd/nexus-hub/main.go:324–425` — that's `metrics_rollup`, `rollup_5m`, `rollup_correction`, `merge_1h/1d/1mo`, `data_retention`, `rollup_retention`, `ops_retention`, `audit_chain_verify`, `drift_detector`, `override_expiry`, `identity_enricher`, `auth_cleanup`, `enrollment_token_cleanup`, `stale_thing`, `exemption_gc`, `diag_mode_expiry`, `vk_expiry`, `quota_alert_check`, `thing_offline_alerts`, `provider_unavailable_alerts`, `siem_bridge`. The new alerteval Engine joins the same gate.

**No new `alerting.Enabled`** is added. The operator sets `scheduler.enabled: true` on **exactly one** Hub instance, and that one instance runs all scheduled work — operational and alerting. Other instances stay scheduler-off.

The single tunable that `alerting`-specific is added to existing `SchedulerConfig`:

```go
type SchedulerConfig struct {
    Enabled bool `yaml:"enabled"`            // existing
    // ... existing interval fields ...

    // AlertEval section: tuning for the alerteval Engine. Active only when
    // Scheduler.Enabled is true (same gate as all other scheduled jobs).
    AlertEval AlertEvalConfig `yaml:"alertEval"`
}

type AlertEvalConfig struct {
    EngineTickSec int `yaml:"engineTickSec"` // default 5
    // (further tunables — eviction policy, MQ ack timeout — added later if needed)
}
```

YAML on the elected scheduler+alerting instance:

```yaml
scheduler:
  enabled: true
  alertEval:
    engineTickSec: 5
```

Other instances either omit `scheduler.enabled` (defaults false) or set it to `false`. **No runtime toggle exists** — see §2 Out of scope.

### 6.2 Main.go gate

The existing `if cfg.Scheduler.Enabled { ... }` block (currently `cmd/nexus-hub/main.go:324`) is extended to also build + register the alerteval Engine alongside the other jobs:

```go
if cfg.Scheduler.Enabled {
    // ... existing job registrations: rollup, retention, audit_chain_verify, ... ...

    // Existing alerting state-table jobs (class 1 + 3, unchanged)
    sched.Register(jobs.NewVKExpiry(...))
    sched.Register(jobs.NewQuotaAlertCheck(...))
    sched.Register(jobs.NewThingOfflineAlerts(...))
    sched.Register(jobs.NewProviderUnavailableAlerts(...))

    // NEW: alerteval Engine (class 4 — event-stream rules)
    eng := alerteval.NewEngine(alerteval.EngineConfig{
        TickSec:      cfg.Scheduler.AlertEval.EngineTickSec,
        Raiser:       raiser,
        AlertStore:   alertStore,
        MQConsumer:   mqConsumer,
        Pool:         dbPool,
        Logger:       logger,
        OpsReg:       opsReg,
    })
    eng.Register(aggregators.NewHookRejectRate())
    eng.Register(aggregators.NewVKTrafficSpike())
    eng.Register(aggregators.NewLoginFailureFlood())
    eng.Register(aggregators.NewProxyHighErrorRate())
    eng.Register(aggregators.NewProxyCostSpike())
    eng.Register(aggregators.NewProxyHookFailureRate())
    eng.Register(aggregators.NewProxyHookTimeoutRate())
    sched.Register(eng)              // alerteval-engine appears in scheduler's job list
}
```

Read-only alerting handlers (`GET /api/admin/alerts*`) stay always-on — they query the `alert` table.

Startup log on the elected instance:

```
INFO scheduler enabled — N jobs registered (rollup_5m, rollup_correction, ..., alerteval-engine, ...)
INFO alerteval engine: registered 7 aggregators (hook.reject_rate, vk.traffic_spike, ...)
INFO alerteval engine: subscribed to 4 MQ queues (consumer group: hub-alerting)
```

Other instances:

```
INFO scheduler disabled — running as non-scheduler instance (api, mq writers, siem only)
```

### 6.3 ServiceInstance role (deferred)

The CP scheduler-single-instance design added a `ServiceInstance.role` column to support BFF proxy forwarding. Hub doesn't need that yet — admin alert mutations all flow through CP, and CP's BFF can hit any Hub instance for read APIs. If future work requires "trigger this alerting job from UI", we add `role = 'scheduler'` to Hub ServiceInstance rows then. Out of scope for E34-S3.

## 7. Rule-Specific Implementation Notes

### 7.1 `hook.reject_rate`

- `params.decisionTypes` is a list of strings; default `['reject_hard', 'reject_soft']`.
- An event "counts as a reject" if **either** `request_hook_decision IN params.decisionTypes` OR `response_hook_decision IN params.decisionTypes` (logical OR — a request rejected on the way in or the response rejected on the way out both contribute).
- Total count is all `traffic_event` rows, regardless of hook decision (denominator).
- TargetKey: `thing:<source_process>` if non-empty, else `thing:<source>` (matches existing `thing_key` extraction in `metrics_rollup.go`).
- Details payload includes `rejectByHook` ONLY if cheap to compute from the event stream. In streaming we don't naturally know per-hook breakdown unless events carry it. **Decision:** v1 omits `rejectByHook`; the alert details show `(rejects, total, pct)` only. Per-hook breakdown becomes a follow-up issue once we have a clearer picture of what `request_hook_decision` JSON shape carries.

### 7.2 `vk.traffic_spike`

- Aggregator type `CompareToBaseline`. Window sized to `baselineWindowSec + alertWindowSec` (default 3600 + 300 = 3900s = ~65 min).
- **Cold-start gate** = `baselineWindowSec + alertWindowSec` = ~65 min after Engine start. Override per rule via `params.coldStartHours` (overrides the gate to `coldStartHours * 3600` if non-zero).
- TargetKey: `vk:<entity_id>`. Engine maintains a Runtime per VK that has shown traffic; idle VKs cost no memory.
- **Memory bound:** if 100k VKs all see continuous traffic, that's 100k × 3900 × 16 bytes ≈ 6 GB. **Cap:** Engine evicts a VK's Runtime when its Window has been zero for 2 × `baselineWindowSec`. Document this in the rule.
- Per the SDD: cost double-signal is out of scope for v1.

### 7.3 `proxy.hook_failure_rate` / `proxy.hook_timeout_rate`

**Correction from earlier draft (chat 2026-04-29):** The audit confirmed the top-level `request_hook_decision` / `response_hook_decision` columns are a 3-value enum (`APPROVE | REJECT_HARD | REJECT_SOFT`) — they don't carry execution-status. Hook stage-level execution detail already lives in the `request_hooks_pipeline` / `response_hooks_pipeline` JSONB column as an array of `HookExecRecord` (`packages/ai-gateway/internal/observability/audit/audit.go:31`):

```go
type HookExecRecord struct {
    Stage      string // "request" | "response" | "connection"
    Order      int
    HookID     string
    Name       string
    Decision   string  // same APPROVE/REJECT_* enum
    Reason     string
    ReasonCode string
    LatencyMs  int
    Error      string  // populated when hook errored — runtime error or timeout
}
```

Per-stage `Error` is the canonical signal. Decision rules:

- **proxy.hook_failure_rate** (RatioInWindow):
  - num: events where `request_hooks_pipeline` OR `response_hooks_pipeline` contains any record with `Error != ""`
  - denom: all events where any hook ran (pipeline length > 0)
- **proxy.hook_timeout_rate** (RatioInWindow):
  - num: events where any record has `Error` containing the canonical timeout marker (`context.DeadlineExceeded` / "deadline exceeded" / similar — pinned at impl time by reading the actual timeout error string emitted by `runRequestHooks` / `runResponseHooks`)
  - denom: same as above

Implementation note for the Engine: the Aggregator gets the decoded `traffic_event` payload at OnEvent time and walks the JSONB array in-process — no new column needed. The 1-bytes-extra cost is reading the JSON field that's already on the event.

**Optional future hardening (not this story):** add a structured `TimedOut bool` field to `HookExecRecord` so we don't have to pattern-match on the `Error` string. Defer until a real bug shows the string-matching is brittle.

### 7.4 `proxy.cost_spike`

Aggregator type `SumInWindow` with `cost_usd` per `entity_id` (VK). Window = `params.windowSec` (default 300s, threshold default `$10`). Source queue = ai-traffic only (cost is meaningful only on the gateway's metering side; compliance-proxy doesn't compute cost; agent-side traffic isn't billable).

### 7.5 `auth.login_failure_rate` (depends on T5)

Source queue = `nexus.event.admin-audit`. Filter: `action == "admin.login.failed"`.

`groupBy` choices:
- `ip` → TargetKey `login:ip:<sourceIp>`. Skip events with empty `sourceIp` (they would all collapse to the same key and produce a misleading alert).
- `email` → TargetKey `login:email:<actorLabel>`. `actorLabel` is the email as typed by the attacker; do not look up if the email exists (timing-safety preserved by CP).
- `all` → TargetKey `login:all`. Single per-window aggregate.

**Depends on T5 (CP-side audit emission)** — currently `packages/control-plane/internal/authserver/login/password.go` returns `errInvalidCredentials` without auditing, so the source events don't exist yet. T5 lands in this story.

### 7.6 Per-rule params application audit (current-state gaps fixed by this design)

Audit triggered by user question 2026-04-29: "are each rule's params actually applied?". Findings on the **pre-Engine** state:

| Rule | Params today | Actually applied? | Why this design fixes it |
|---|---|---|---|
| `quota.threshold` | `thresholds: number[]` | ✅ Yes — `quota_alert_check.go` reads via `parseAlertThresholds(p.AlertThresholds)` and compares cost % against each threshold | No change — keep as is |
| `quota.vk_expiring` | `warnDays: number[]` | ✅ Yes | No change |
| `thing.offline` | `offlineAfterSec`, `excludeKinds` | ✅ Yes — `parseThingOfflineParams(rule.Params)` at the top of each `evaluate()` call | No change |
| `provider.unavailable` | `minDownSec`, `recoverySec` | ⚠️ **Half-bug** — `provider_unavailable_alerts.go:92` parses them then logs "informational only — they are not applied". Admin can edit values in UI, no behavioral change. | **Fixed in this PR** as a small bonus: wire `minDownSec` / `recoverySec` into the actual debounce check. ~30 lines. |
| `proxy.high_error_rate` | `thresholdPct`, `windowSec`, `minSamples` | ❌ **Not applied** — `NewHighErrorRateCheck(rec, thresholdPct, windowSec, minSamples, ...)` takes them as constructor args from data-plane YAML, then never re-reads. Admin UI edits to `AlertRule.params` do **nothing**. | Fixed by T6a — Aggregator reads `rule.Params` per tick (A8) |
| `proxy.cost_spike` | `thresholdUsd`, `windowSec` | ❌ Same hardcoded-at-startup pattern in `NewCostSpikeCheck(...)` | Fixed by T6b |
| `proxy.hook_failure_rate` | `thresholdPct`, `windowSec`, `minSamples` | ❌ Same hardcoded pattern in compliance-proxy | Fixed by T6c |
| `proxy.hook_timeout_rate` | `thresholdPct`, `windowSec`, `minSamples` | ❌ Same | Fixed by T6d |

**Decision (added to T-list):** include the `provider.unavailable` params-application fix in this PR — it's a 30-line change in the same package that's already being touched, and leaving a known params-not-applied gap behind contradicts A8's "edit in UI → live in 5s" contract for the rest of the rule set.

**Distinct paramsSchema audit:** Each rule's `paramsSchema` in `seed-alerting.ts` is genuinely rule-specific (different field names + types). The 3 `proxy.*` rate rules share the same shape (`thresholdPct + windowSec + minSamples`) which is correct — they all measure rate-based events with the same controls. No copy-paste-with-wrong-fields gaps found.

### 7.7 New `traffic_event` failure-classification columns: `error_code` + `error_reason`

**Triggered by user (chat 2026-04-29):** today's `traffic_event` has `status_code` (HTTP success/failure) and `request_hook_decision` (hook outcome) but **no structured classification of why a non-2xx response happened, and no human-readable explanation**. ai-gateway's `writeDetailedError(w, status, code, message, hint)` already constructs all three pieces (`code` = enum, `message` = human-readable detail, `hint` = end-user remediation guidance) but only writes them into the HTTP response body, never into the audit record. Result: `status_code = 429` is ambiguous (Nexus-side rate-limit vs. upstream 429), and ops looking at a traffic row sees only "429" with no idea why.

**Decision:** add **two** nullable columns to `traffic_event`:

- `error_code TEXT` — structured enum classification, machine-readable (`RATE_LIMITED` / `QUOTA_EXCEEDED` / `ROUTING_NO_MATCH` / `AUTH_INVALID` / `AUTH_KEY_EXPIRED` / `USAGE_QUERY_FAILED` / etc.). Source: existing `code` argument to `writeDetailedError`.
- `error_reason TEXT` — human-readable failure description, populated for ops to skim without looking up the code dictionary. Source: existing `message` argument.

`hint` (end-user remediation guidance like "Reduce request frequency or contact admin") is **not** persisted — it belongs in the response body for the API caller, not in the audit trail.

compliance-proxy and agent leave both columns NULL — their failure semantics are different (encoded in `bump_status` / hook decisions). If at some future point they need their own structured classification, they can populate the same two columns with their own enum vocabulary; no schema change.

**Migration:** 2 columns added, no backfill (pre-GA, no historical contract).

**Code change footprint:**
- `tools/db-migrate/schema.prisma` — add `error_code String? @map("error_code")` and `error_reason String? @map("error_reason")` on `traffic_event`
- New Prisma migration: `ALTER TABLE traffic_event ADD COLUMN error_code TEXT NULL, ADD COLUMN error_reason TEXT NULL`
- `packages/ai-gateway/internal/observability/audit/audit.go` — add `ErrorCode string` and `ErrorReason string` to `audit.Record`; write both into row mapper
- `packages/ai-gateway/internal/handler/proxy.go:1582` — `writeDetailedErr` already takes `code` and `message`; add `rec.ErrorCode = code` and `rec.ErrorReason = message` (2 lines total)
- Hub-side decoder in `packages/nexus-hub/internal/jobs/consumer/traffic.go` — add 2 field decodes
- Hub-side `alerteval.Event` struct — carries `ErrorCode string` and `ErrorReason string` so Aggregators can filter on `ErrorCode` and operator drill-downs (alerts inbox detail view) show the human-readable reason

#### Success / failure definition (contract)

`status_code` is the canonical success/failure signal (HTTP standard: 2xx = success, ≥ 400 = failure). `error_code` is the **structured classification of failure reason**; `error_reason` is the **human-readable explanation**. The three together let machines filter cleanly AND let humans understand at a glance.

| State | `status_code` | `error_code` | `error_reason` | Meaning |
|---|---|---|---|---|
| Clean success | `2xx` | NULL | NULL | All phases passed; upstream returned normally |
| Nexus pre-flight reject | non-2xx (4xx / 429) | `RATE_LIMITED` / `QUOTA_EXCEEDED` / `ROUTING_NO_MATCH` / `AUTH_*` | "VK exceeded 60 req/min", "Quota policy 'eng' over $1000 cap", "no rule matches model `gpt-4-foo`", "API key expired 2 days ago" | Nexus rejected before reaching upstream; both classification AND human reason known |
| Upstream provider failure | `≥ 500` (or upstream-mapped 4xx) | NULL | NULL | Nexus passed all checks; upstream provider returned an error (caller can read the upstream's response body for the reason) |
| Soft compliance warn | `2xx` | NULL | NULL (but `request_hook_decision = REJECT_SOFT`) | Hook flagged but let through; request succeeded |
| Cache hit | `2xx` | NULL | NULL (but `cache_status = HIT`) | Success, no upstream call |

**The "fully successful request" predicate (for SLO / success-rate alerts and rollup billed-cost) is:**

```sql
status_code BETWEEN 200 AND 299 AND error_code IS NULL
```

`error_reason` is **not** in the predicate — it's purely for human consumption (alerts inbox detail view, traffic page row drill-down, ops grepping logs).

A view or generated column for "is_success" is **not** added now (YAGNI — the predicate is a 2-clause check, not worth a schema artifact yet). If multiple analytics queries grow that need it, revisit.

#### What this unlocks (P0 alerts that were impossible before)

| Rule (future, not this story) | Predicate |
|---|---|
| `proxy.rate_limit_exceeded` | `error_code = 'RATE_LIMITED'` per VK / per source_ip |
| `proxy.quota_runtime_exceeded` | `error_code = 'QUOTA_EXCEEDED'` (distinguishes from `quota.threshold` which is the soft 80%/95% rollup-based warning) |
| `proxy.routing_no_match` | `error_code = 'ROUTING_NO_MATCH'` |
| `auth.invalid_key_burst` | `error_code IN ('AUTH_INVALID','AUTH_KEY_EXPIRED')` per source_ip |
| `provider.upstream_error` | `status_code >= 500 AND error_code IS NULL` (clean separation from Nexus-side failures) |

These move into Appendix A as P0 candidates for the next story; this story does not implement them, but the schema + plumbing land here so they're a one-aggregator-file follow-up.

#### Backwards compatibility

Pre-GA, no concern. ai-gateway emits both columns going forward; compliance-proxy / agent leave them NULL. Aggregators treat NULL as "unclassified" — no false-positive on the new alert types listed above. Existing analytics that query `traffic_event` keep working unchanged (the columns are nullable and additive).

### 7.8 Rollup correctness — billed vs gross cost / tokens (current-state bugs fixed by this design)

**Triggered by user (chat 2026-04-29):** the rollup should only count successful requests when computing token usage / quota; failed requests must be excluded. Audit found 3 distinct bugs in the rollup chain plus 1 inconsistency in the runtime quota counter.

#### Bug audit

| ID | Bug | Location | Severity | Impact |
|---|---|---|---|---|
| **B1** | `rollup_5m.go:399-406` adds `prompt_tokens` / `completion_tokens` / `total_tokens` / `estimated_cost_usd` for **every** `traffic_event` row, including rate-limit / quota / hook reject / 5xx failures. No `status_code` filter, no `cache_status = 'HIT'` exclusion. | `packages/nexus-hub/internal/jobs/rollup_5m.go` | High | Usage analytics report gross numbers; quota.threshold over-fires; ai-gateway startup `usage_cache.Backfill` warm-loads gross cost into Redis cache. |
| **B2** | Streaming Reconcile path (`proxy.go:1255`) updates the runtime quota counter when `quotaDecision.Allowed`, **without checking `rec.StatusCode < 400`**. Non-streaming path (`proxy.go:1473`) checks both. | `packages/ai-gateway/internal/handler/proxy.go:1255` | Medium | Streaming requests that errored mid-flight still increment Redis quota counter — VK appears closer to budget than it actually is. |
| **B3** | `quota_alert_check.go:389` reads `MetricEstimatedCostUSD` (the gross metric from B1) to evaluate quota.threshold. | `packages/nexus-hub/internal/jobs/quota_alert_check.go` | Medium | quota.threshold 80% / 95% alerts fire prematurely (gross cost > billed cost). |
| **B4** | If ai-gateway writes a non-zero `estimated_cost_usd` on cache hits, those rows get **double-counted**: once in `MetricEstimatedCostUSD` (gross) AND once in `MetricCacheSavedCostUSD` (savings indicator). | `packages/ai-gateway/internal/handler/proxy.go` (cache hit branch) and `rollup_5m.go:394-406` | Medium (depends on current behavior) | Verify at impl time: if cache hits write 0, no fix needed; if non-zero, either change ai-gateway to write 0 or change rollup to exclude cache hits from EstimatedCostUSD. |

#### Fix design — split metrics into "gross" (existing, unchanged) vs "billed" (new)

Instead of changing the existing `MetricEstimatedCostUSD` semantics (which would break analytics that knowingly want gross), introduce **new explicitly-named metrics** that ALL quota-related readers switch to.

```go
// packages/shared/runtime/metrics/names.go (or wherever rollup metric constants live) — append:
MetricBilledCostUSD  = "billed_cost_usd"      // Sum where status_code 2xx AND error_code IS NULL AND cache_status != 'HIT'
MetricBilledTokens   = "billed_total_tokens"  // Same predicate
```

Existing constants stay (no breaking change to gross-aware analytics):

- `MetricEstimatedCostUSD` — **gross** (every row, no filter). Useful for "what would have been billed without failures / cache". Document it as gross in a code comment.
- `MetricWastedCostUSD` — failed (status >= 400). Unchanged.
- `MetricCacheSavedCostUSD` — cache-hit cost. Unchanged.
- `MetricPromptTokens` / `MetricCompletionTokens` / `MetricTotalTokens` — gross. Document.

`rollup_5m.go` change (~15 lines around line 399):

```go
// Existing — keep, but document in a comment that it's GROSS (every row).
add(metrics.MetricPromptTokens, float64(derefInt5m(promptTokens)))
add(metrics.MetricCompletionTokens, float64(derefInt5m(completionTokens)))
add(metrics.MetricTotalTokens, float64(derefInt5m(totalTokens)))
cost := derefFloat5m(estimatedCost)
add(metrics.MetricEstimatedCostUSD, cost)
if sc >= 400 {
    add(metrics.MetricWastedCostUSD, cost)
}

// NEW — billed-only (used by quota readers).
isSuccess := sc >= 200 && sc < 300 && deref5m(errorCode) == ""
isCacheHit := derefBool5m(cacheHit)
if isSuccess && !isCacheHit {
    add(metrics.MetricBilledCostUSD, cost)
    add(metrics.MetricBilledTokens, float64(derefInt5m(totalTokens)))
}
```

The SQL in `rollup_5m.go:160` also needs `error_code` added to the SELECT projection so the goroutine sees the new column.

#### Reader changes (cut over to billed metrics)

- `packages/nexus-hub/internal/jobs/quota_alert_check.go:389` — change `Metrics: []string{metrics.MetricEstimatedCostUSD}` → `Metrics: []string{metrics.MetricBilledCostUSD}`. Also rename internal local variable `costs` → `billedCosts` for clarity.
- `packages/ai-gateway/internal/pipeline/quota/usage_cache.go::Backfill:158` — change SQL `WHERE "metricName" = 'estimated_cost_usd'` → `WHERE "metricName" = 'billed_cost_usd'`. Verify: `metric_rollup_1h` is the layer Backfill reads (it should auto-roll up the new BilledCostUSD via `rollup_merge.go` because the merge sums every metric name forward).

#### Streaming Reconcile fix (B2)

`packages/ai-gateway/internal/handler/proxy.go:1255` — change:

```go
// Before
if h.deps.QuotaEngine != nil && quotaDecision != nil && quotaDecision.Allowed {

// After (matches non-streaming path at :1473)
if h.deps.QuotaEngine != nil && quotaDecision != nil && quotaDecision.Allowed && rec.StatusCode < 400 {
```

#### Cache-hit cost handling (B4 verification)

Implementation step (not design): grep `cache_status = HIT` write path in `packages/ai-gateway/internal/handler/proxy.go` to see what `rec.EstimatedCostUSD` is when cache hit. Two fixes:

- **If non-zero** (currently emits the "would-have-cost"): change writer to set `rec.EstimatedCostUSD = 0` on cache hit AND the existing `MetricCacheSavedCostUSD` accumulator already captures the indicator value.
- **If zero**: no cache-side fix needed; the `if isSuccess && !isCacheHit` guard is belt-and-suspenders.

Pick the right one at impl time and document the choice in code comment + `MetricCacheSavedCostUSD` docstring.

#### Rollup-merge auto-propagation

`rollup_merge.go` does `INSERT INTO target_table SELECT FROM source_table` grouped by metric_name; new metrics flow through automatically. No merge code change needed — confirmed by reading the join's SQL shape (`metric_name` is one of the GROUP BY columns).

#### What does NOT change

- `MetricRequestCount` — counts ALL requests (success-rate analytics need the denominator). Unchanged.
- `MetricStatus2xxCount` / `4xxCount` / `5xxCount` — already split by status. Unchanged.
- `MetricLatencySum` / `MetricLatencyHistogram` — latency is meaningful for both success and failure cases (slow failures matter). Unchanged.
- `MetricHookAllowCount` / `HookDenyCount` / `HookErrorCount` — hook decision counts are independent of HTTP status. Unchanged.
- Existing `metric_rollup_5m` / `metric_rollup_1h` / `metric_rollup_1d` / `metric_rollup_1mo` rows — no backfill (pre-GA; old data is dev fixtures only). New metric rows start appearing after deploy.

#### Backwards compat / rollback

Pre-GA, no installed user base → if BilledCostUSD vs EstimatedCostUSD divergence is unexpectedly large in dev, easiest rollback is `git revert` of this section's changes; the new metric rows are leftover but harmless (no reader queries them anymore).

## 8. Named Jobs and Operator Visibility (no anonymous goroutines)

### 8.1 Job-naming discipline (project rule)

Hub already has a strict convention: **every long-running scheduled goroutine registers in `sched.Register(job)` with a stable, machine-readable name**. The current Hub `main.go` registers ~20 jobs this way (`metrics_rollup`, `data_retention`, `rollup_5m`, `rollup_correction`, `audit_chain_verify`, `vk_expiry`, `quota_alert_check`, etc.). The CP UI Infrastructure → Jobs page (`packages/control-plane-ui/src/pages/infrastructure/InfraJobsPage.tsx`) reads this list via `GET /api/admin/jobs` (which proxies to `GET /api/hub/jobs`).

The alerteval Engine joins this convention. **Single registration:**

- Job name: `alerteval-engine`
- Registered via `sched.Register(eng)` in `cmd/nexus-hub/main.go` (inside the existing `if cfg.Scheduler.Enabled` block)
- Implements the same `sched.Job` interface other jobs already implement (`Name()`, `Run(ctx)`, `Trigger(ctx)`, `Status() JobStatus`)
- Internal Aggregators are **not** separately registered as jobs — they are state inside the Engine. Operator visibility is via the Engine's job-detail response (§8.2).

### 8.2 Engine introspection via existing job-detail endpoint

`GET /api/hub/jobs/alerteval-engine` returns a `JobStatus` payload extended with Engine-specific metadata. The CP UI Job detail drawer renders the JSON generically; no UI change needed.

```json
{
  "name": "alerteval-engine",
  "lastRunAt": "2026-04-29T13:42:05Z",
  "lastRunDurationMs": 12,
  "lastError": null,
  "tickIntervalSec": 5,
  "engine": {
    "mqLagSec": 0.4,
    "subscribedQueues": [
      "nexus.event.ai-traffic",
      "nexus.event.compliance",
      "nexus.event.agent",
      "nexus.event.admin-audit"
    ],
    "consumerGroup": "hub-alerting",
    "aggregators": [
      {
        "ruleId": "hook.reject_rate",
        "warmupRemainingSec": 0,
        "trackedTargets": 12,
        "lastTickFireDecisions": 0,
        "lastTickResolveDecisions": 1
      },
      {
        "ruleId": "vk.traffic_spike",
        "warmupRemainingSec": 0,
        "trackedTargets": 487,
        "lastTickFireDecisions": 0,
        "lastTickResolveDecisions": 0
      }
      // ... 5 more
    ]
  }
}
```

Operator value: SSH-free answer to "what is this Hub instance running, and is alerting actually keeping up?". Answers cover:

- Is alerting active on this Hub? (presence of `alerteval-engine` in jobs list)
- Which rules are wired into the engine? (aggregator list)
- Is MQ falling behind? (`mqLagSec`)
- How many entities per rule are being tracked? (`trackedTargets`)
- Did the last tick fire/resolve anything? (`lastTickFireDecisions` / `lastTickResolveDecisions`)
- Is the cold-start gate still active after a recent restart? (`warmupRemainingSec > 0`)

### 8.3 "Which Hub instance is the scheduler/alerting one?" — surfaced in Nodes page

The `GET /api/admin/jobs` endpoint already proxies to whichever Hub instance is the elected scheduler. CP UI Infrastructure → Nodes page reads the Thing Registry; we extend its row payload to include `schedulerEnabled: bool` so operators can see at a glance "Hub instance `hub-prod-1` has scheduler enabled, the others don't". The badge is read-only (per §2 Out of scope: no runtime toggle).

Implementation: Hub's existing `ServiceInstance` registration (`cmd/nexus-hub/main.go:222` already passes `SchedulerEnabled: cfg.Scheduler.Enabled` to the registration call) — exposing it on the Hub Things list response is a one-field addition. **Out of scope for this story** if the Hub Things list response doesn't currently carry it; tracked as a follow-up cosmetic improvement.

## 9. Testing Strategy

Test categories:

1. **Window primitive unit tests** — `window_test.go`. Verify ring buffer correctness across head rotation, sparse buckets, sum-over-window.
2. **Aggregator unit tests** — one per Aggregator. Inject events through `OnEvent`, advance fake clock, call `Tick`, assert Decisions. No DB, no MQ.
3. **Engine integration test** — `engine_test.go`. Use the in-memory `mq.Consumer` fake (already exists in `shared/mq`), seed `AlertRule` rows, register 1-2 aggregators, push events through fake MQ, verify Raiser is called with expected args. Use `alerting.NewRaiser` with a fake store.
4. **Per-rule end-to-end test** — for each of 7 rules, in `aggregators/<name>_test.go`: build the canonical event stream that should fire, the one that shouldn't, verify thresholds.
5. **Cold-start gate test** — verify Tick produces no Decisions before warmup elapses.
6. **Cooldown test** — Fire Decision, then second Fire within `cooldownSec` of first does NOT call Raiser.
7. **Hysteresis test** — Fire then drop just below threshold (still in hysteresis band) does not Resolve.
8. **Lockstep test** — `TestBuiltinRulesMatchSeed` continues to gate that `seed-alerting.ts` and `builtin.go` agree on the rule list.

Live verification (per the SDD AC4):

- `hook.reject_rate`: 30 hook-reject requests through gateway → alert within 10s.
- `vk.traffic_spike`: temporarily set `coldStartHours: 0` on the rule via `PUT /api/admin/alerts/rules/:id`, fire 100 VK requests, observe alert.
- `auth.login_failure_rate`: 30 wrong-password POSTs from one IP → alert within 10s.

## 10. Migration Plan (T6 single-PR cutover)

The PR that lands the Engine for `proxy.high_error_rate / cost_spike / hook_failure_rate / hook_timeout_rate` also:

1. **Deletes** `packages/ai-gateway/internal/alerting/` (its `Evaluator` interface, `checks_high_error_rate.go`, `checks_cost_spike.go`, ring counter helpers, all tests).
2. **Deletes** `packages/compliance-proxy/internal/alerting/` (similar — verify scope by `grep -rn "internal/alerting" packages/compliance-proxy`).
3. **Removes** the `eval.Register` calls and `alerting.Evaluator` field from `packages/ai-gateway/cmd/ai-gateway/main.go` and `packages/compliance-proxy/cmd/compliance-proxy/init.go`.
4. **Removes** the `shared/alertclient` import from data-plane services (the Engine raises directly via Hub-internal `Raiser`; data-plane no longer needs the HTTP path for these 4 rules). **Mandatory pre-step:** `grep -rn "shared/alertclient" packages/` — if any other consumer exists, keep the package and only remove the now-dead callsites.
5. **No new top-level enum values** for hook decisions (correction from earlier draft): `request_hook_decision` / `response_hook_decision` stay 3-valued (`APPROVE | REJECT_HARD | REJECT_SOFT`). `proxy.hook_failure_rate` / `proxy.hook_timeout_rate` derive their signal from existing `HookExecRecord.Error` inside the per-stage `request_hooks_pipeline` / `response_hooks_pipeline` JSONB arrays — see §7.3.

No phased compatibility. Rollback = `git revert` per CLAUDE.md.

## 11. Files Changed (summary)

### New files

- `packages/nexus-hub/internal/alerts/eval/engine.go`
- `packages/nexus-hub/internal/alerts/eval/event.go`
- `packages/nexus-hub/internal/alerts/eval/aggregator.go`
- `packages/nexus-hub/internal/alerts/eval/window.go`
- `packages/nexus-hub/internal/alerts/eval/runtime.go`
- `packages/nexus-hub/internal/alerts/eval/raise.go`
- `packages/nexus-hub/internal/alerts/eval/aggregators/*.go` (10 files: 4 helpers + 7 rule wirings; some helpers shared between rules)
- `packages/nexus-hub/internal/alerts/eval/*_test.go` (per file)
- `packages/nexus-hub/internal/alerts/eval/aggregators/*_test.go`

### Modified files

#### Engine + alerting

- `packages/nexus-hub/internal/config/config.go` — add `AlertEvalConfig` struct **nested inside the existing `SchedulerConfig`** (no new top-level config section, no new env flag). Default `EngineTickSec = 5`.
- `packages/nexus-hub/cmd/nexus-hub/main.go` — extend the existing `if cfg.Scheduler.Enabled { ... }` block with the alerteval Engine wireup (`alerteval.NewEngine(...)` + 7 `eng.Register(...)` calls + `sched.Register(eng)`). No new gate.
- `packages/nexus-hub/nexus-hub.dev.yaml` — add `scheduler.alertEval.engineTickSec: 5` (the top-level `scheduler.enabled` already exists from prior work).
- `packages/nexus-hub/internal/jobs/provider_unavailable_alerts.go` — wire `minDownSec` and `recoverySec` (currently parsed but not applied per §7.6) into the actual debounce check.
- `packages/nexus-hub/internal/alerts/engine/rules/builtin.go` — append 3 new rule definitions (T1)
- `tools/db-migrate/seed/seed-alerting.ts` — append 3 new rule definitions (T1, lockstep)
- `packages/control-plane/internal/authserver/login/password.go` — emit `admin.login.failed` audit on credential failure; emit `admin.login.succeeded` on success (T5)

#### `error_code` + `error_reason` schema (§7.7)

- `tools/db-migrate/schema.prisma` — `traffic_event` model: add `error_code String? @map("error_code")` and `error_reason String? @map("error_reason")`
- New Prisma migration: `ALTER TABLE traffic_event ADD COLUMN error_code TEXT NULL, ADD COLUMN error_reason TEXT NULL`
- `packages/ai-gateway/internal/observability/audit/audit.go` — `audit.Record` struct: add `ErrorCode string` + `ErrorReason string`; row mapper writes both columns
- `packages/ai-gateway/internal/handler/proxy.go:1582` — `writeDetailedErr`: 2 lines added (`rec.ErrorCode = code` + `rec.ErrorReason = message`)
- `packages/nexus-hub/internal/jobs/consumer/traffic.go` — TrafficEventMessage decode: 2 field decodes; INSERT statement adds 2 columns
- `packages/shared/traffic/...` (path TBD at impl) — if a shared event message struct exists, add the 2 fields there too

#### Rollup correctness — billed metrics (§7.8)

- `packages/shared/runtime/metrics/names.go` (or wherever `MetricEstimatedCostUSD` is defined) — append `MetricBilledCostUSD = "billed_cost_usd"` and `MetricBilledTokens = "billed_total_tokens"` constants
- `packages/nexus-hub/internal/jobs/rollup_5m.go` — SELECT statement (line 160) adds `error_code` to projection; aggregator (line ~399) adds the new `if isSuccess && !isCacheHit` branch that accumulates BilledCostUSD + BilledTokens; existing gross accumulators kept with a comment clarifying gross semantics
- `packages/nexus-hub/internal/jobs/quota_alert_check.go:389` — switch `Metrics: []string{metrics.MetricEstimatedCostUSD}` → `metrics.MetricBilledCostUSD`; rename local var for clarity
- `packages/ai-gateway/internal/pipeline/quota/usage_cache.go::Backfill` (line ~158) — switch `WHERE "metricName" = 'estimated_cost_usd'` → `'billed_cost_usd'`
- `packages/ai-gateway/internal/handler/proxy.go:1255` — streaming Reconcile gate: add `&& rec.StatusCode < 400` to match non-streaming path at :1473
- `packages/ai-gateway/internal/handler/proxy.go` (cache hit branch) — verify and document: cache hits write `rec.EstimatedCostUSD = 0` (or rollup explicitly excludes cache hits from EstimatedCostUSD; pick the consistent path at impl)

### Deleted files

- `packages/ai-gateway/internal/alerting/` (entire package, after verifying no other consumer)
- `packages/compliance-proxy/internal/alerting/` (entire package, similar verification)
- `packages/shared/alertclient/` (only if no other rule uses it; **mandatory** `grep -rn "shared/alertclient" packages/` step before deletion)

## 12. Acceptance Criteria

- **AC1** — Hub Go tests green with `-race -count=1` (alerteval package unit + integration; existing alerting package still passes; lockstep test still passes).
- **AC2** — DB seed reset produces 12 `AlertRule` rows (was 9).
- **AC3** — `grep -rn "alerting.Evaluator\|eval.Register" packages/ai-gateway packages/compliance-proxy` returns no matches after migration.
- **AC4** — Live trigger: 30 hook-rejects → `hook.reject_rate` alert fires within 10s (Engine tick = 5s, so within 2 ticks). Same for login flood (30 wrong-password POSTs from same IP). VK spike verified with `coldStartHours: 0` override on the rule.
- **AC5** — `lsof -nP -iTCP:3050,3040 -sTCP:LISTEN` confirms ai-gateway / compliance-proxy still healthy after migration.
- **AC6** — Multi-instance dev simulation (run two Hub processes on different ports, only one with `scheduler.enabled: true`): trigger an alert condition and verify only one Raiser invocation occurred (no duplicate alert rows in DB).
- **AC7** — Restart the scheduler+alerting Hub: confirm logs show "alerteval engine: cold-start gate active" and no alerts fire for at least `windowSec` of the shortest active rule (default 300s = 5min).
- **AC8** — `GET /api/admin/jobs` (CP UI Infrastructure → Jobs page) lists `alerteval-engine` alongside other Hub jobs; `GET /api/admin/jobs/alerteval-engine` returns the engine block from §8.2 with the 7 aggregator entries enumerated.
- **AC9** — `provider.unavailable` rule's `minDownSec` / `recoverySec` params are now actually applied (audit fix from §7.6): edit `minDownSec` from 60 to 120 in admin UI, observe that a provider that goes down for 90s no longer fires an alert (it must stay down ≥120s now). Reverts to fire after 120s.
- **AC10** — Edit `proxy.high_error_rate` `params.thresholdPct` from 5 to 50 in admin UI; observe that within one Engine tick (≤5s) the new threshold is in effect (rule no longer fires at 10% error rate, must reach 50%). Tests the `params reread per tick` contract (A8) end-to-end.
- **AC11** — `error_code` + `error_reason` write-through: trigger a rate-limit on a VK with low RPM cap; query the resulting `traffic_event` row → `status_code = 429`, `error_code = 'RATE_LIMITED'`, `error_reason` = the human-readable message (e.g. "rate limit exceeded"). Repeat for `QUOTA_EXCEEDED` (over-budget VK), `ROUTING_NO_MATCH` (unknown model), `AUTH_KEY_EXPIRED` (expired VK). Compliance-proxy traffic_event rows: both columns NULL.
- **AC12** — Rollup correctness — billed-cost predicate: in any 5-minute bucket, `SUM(MetricBilledCostUSD) <= SUM(MetricEstimatedCostUSD)` always holds. Specifically: produce 1 successful request (cost $X) + 1 hook-rejected request (cost $0) + 1 cache-hit (cost $Y if non-zero) → after rollup tick: `BilledCostUSD = X`, `EstimatedCostUSD = X + Y` (or `X` if cache hits write 0).
- **AC13** — Quota.threshold no longer over-fires: set a VK's monthly cost limit to $10. Generate $9 of successful traffic + $5 of hook-rejected traffic. `quota.threshold` 80% alert (trigger at $8) **does** fire (real successful spend = $9, crosses 80%). It would have fired earlier under the old (gross) reading at $14. Verify the new BilledCostUSD reading drives the alert.
- **AC14** — Streaming Reconcile filter (B2 fix): trigger a streaming request that fails mid-flight (kill upstream connection or send malformed body that triggers 5xx after stream start). Verify Redis `usage:virtual_key:<id>:<period>` counter does **not** increment for this request. Same scenario before the fix would have incremented the counter.

## 13. Risks and Mitigations

| Risk | Mitigation |
|---|---|
| Memory growth from per-VK Runtime in `vk.traffic_spike` | Eviction after 2 × baselineWindowSec of zero traffic per VK. Document expected memory cost. |
| Engine consumer falls behind under burst (NATS lag) | NATS JetStream consumer group provides lag metrics. Add `mq.lag_seconds{group="hub-alerting"}` to `opsmetrics`. Alert via existing infrastructure if lag > 30s. |
| Cold-start blackout after restart | Documented; operators can preempt by increasing redundancy of alerting Hub instance (e.g. systemd restart-on-failure). HA epic addresses true elimination. |
| Aggregator + DB-job race on the same rule during migration | Single-PR cutover (A9). No transitional period. |
| `params` JSON Schema drift between `seed-alerting.ts` and Go validator | Existing lockstep test enforces ID match. Schema content is currently validated only at admin write time. Future improvement: emit Go validator from the same schema. Out of scope. |
| `error_code` / `error_reason` columns missing on pre-existing traffic_event rows | Pre-GA: no historical rows worth preserving. Migration adds nullable columns; readers treat NULL as "unclassified" (matches the upstream-provider-error semantic case). No backfill. |
| Cache hit cost double-counting (B4) — depends on what ai-gateway writes for `estimated_cost_usd` on cache hits | Verify at impl: if non-zero, change ai-gateway writer to 0; if already 0, the `if !isCacheHit` guard in §7.8 is belt-and-suspenders. Either way: lands a single behavior, not two. |
| BilledCostUSD readers initially see 0 because metric only starts being written post-deploy | Cold-start window equals max rollup chain (5min + merge cycles ≤ 1h). Documented in `usage_cache.Backfill` log message. Operators who want immediate values can manually trigger rollup via the existing `POST /api/admin/jobs/rollup-5m/trigger` endpoint. |

## 14. Open Questions

None blocking implementation. Items to revisit post-merge:

- Engine introspection beyond what §8.2 already specifies (e.g. per-target_key drill-down at the JSON level, not just counts) — wait for first real ops incident to see what's actually missing.
- Should the eviction policy (2 × baselineWindowSec idle) be configurable? Probably yes once we have one customer with > 10k VKs; not now.
- Should `scheduler.enabled` be renamed to something like `runner.enabled` to reflect that it now gates alerting too? Cosmetic; defer until somebody is genuinely confused.

## 15. Dependencies on Other Work

- Inherits the unified-alerting design (`2026-04-21`) — no changes needed there.
- **Reuses** the scheduler-single-instance pattern (`2026-04-16`) — explicitly: shares the **same `cfg.Scheduler.Enabled` flag**. No new flag introduced.
- Pairs with the existing rule-pack hook architecture (`2026-04-14-hook-architecture-redesign.md`) — `request_hook_decision` / `response_hook_decision` payload shape is set there. This design does NOT extend the top-level decision enum; it derives hook failure/timeout signals from the already-existing per-stage `HookExecRecord.Error` field inside `request_hooks_pipeline` / `response_hooks_pipeline` JSONB.
- Pairs with the existing metrics rollup design (`2026-04-15-metrics-rollup-design.md`) — `MetricEstimatedCostUSD` semantics defined there. This design adds two new metric names (`MetricBilledCostUSD`, `MetricBilledTokens`) without changing the existing constants; the rollup chain (5m → 1h → 1d → 1mo) auto-propagates new metric names because `metric_name` is a GROUP BY dimension in `rollup_merge.go`.
- Pairs with the existing quota system design (`2026-04-15-quota-system-redesign.md` + `2026-04-16-quota-system-fixes-design.md`) — quota.threshold reader and `usage_cache.Backfill` reader switch from gross `MetricEstimatedCostUSD` to billed `MetricBilledCostUSD`; runtime `Reconcile` streaming branch gains the same `status_code < 400` filter the non-streaming branch already has.
- CP UI Infrastructure → Jobs page (`InfraJobsPage.tsx`) and the BFF proxy (`admin_hub_proxy.go`) already exist — `alerteval-engine` flows into them automatically once `sched.Register(eng)` is called. **No new admin API**, no new UI page.

## 16. Appendix A — Future Rule Candidates (out of scope for E34-S3)

Surfaced by the 2026-04-29 codebase exploration. **Not** included in this story — listed here so the next story (E34-S4 or E35) can pick from a vetted backlog. All P0/P1 items use existing data sources (zero new instrumentation).

### P0 — High-impact, immediate signals

| # | Rule ID | Signal source | Aggregator type | What it catches |
|---|---|---|---|---|
| 1 | `proxy.rate_limit_exceeded` | `error_code = 'RATE_LIMITED'` per VK / per source_ip (unlocked by §7.7 schema) | CountInWindow | Caller hitting rate-limit cap repeatedly — VK leak / runaway client |
| 2 | `proxy.quota_runtime_exceeded` | `error_code = 'QUOTA_EXCEEDED'` per VK (unlocked by §7.7) | CountInWindow | Hard-quota rejections; complements `quota.threshold` (soft 80%/95% rollup-based) |
| 3 | `proxy.routing_no_match` | `error_code = 'ROUTING_NO_MATCH'` (unlocked by §7.7) | CountInWindow | Customer config broken; routing engine can't pick a target |
| 4 | `auth.invalid_key_burst` | `error_code IN ('AUTH_INVALID','AUTH_KEY_EXPIRED')` per source_ip (unlocked by §7.7) | CountInWindow | Brute-force VK guessing or expired-key client |
| 5 | `provider.upstream_error` | `status_code >= 500 AND error_code IS NULL` per provider (unlocked by §7.7) | RatioInWindow | Clean separation of upstream provider errors from Nexus-side rejects |
| 6 | `provider.high_latency_percentile` | `traffic_event.latency_ms` per provider | CompareToBaseline (p95 multiplier) | Provider degradation before full outage |
| 7 | `model.rate_limited_responses` | `traffic_event.status_code = 429 AND error_code IS NULL` per model (must exclude Nexus-side rate-limit) | CountInWindow | Upstream provider 429-throttling |
| 8 | `credential.auth_failures_cascade` | `traffic_event.status_code IN (401,403) AND error_code IS NULL` per credential_id | RatioInWindow | Credential rotation needed / upstream perm revocation |

### P1 — Operational awareness

| # | Rule ID | Signal source | Aggregator type | What it catches |
|---|---|---|---|---|
| 5 | `vk.latency_degradation` | `traffic_event.latency_ms` per VK, p95(5m) vs baseline(1h) | CompareToBaseline | VK-specific slowness |
| 6 | `vk.token_usage_spike` | `traffic_event.total_tokens` per VK vs `VirtualKey.rateLimitRpm` | SumInWindow + RatioInWindow | Token quota exhaustion ahead of cost-spike |
| 7 | `vk.budget_utilization_rate` | cumulative `traffic_event.estimated_cost_usd` vs `VirtualKey.budgetLimitUsd` (90% soft alert) | RatioInWindow | Burn-rate visibility before quota hard-cuts |
| 8 | `compliance.hook_execution_timeout_surge` | `traffic_event.response_hooks_pipeline` JSON timeout count per `hook_id` | CountInWindow | Hook infra degradation |
| 9 | `compliance.payload_capture_failure_rate` | `traffic_event_payload.{request,response}_truncated` ratio | RatioInWindow | Spillstore down → audit trail broken |
| 10 | `agent.cert_expiration_imminent` | `ThingAgent.cert_expires_at < NOW() + 30 days` | state-poll (class 1, NOT engine) | Surprise mTLS disconnects |
| 11 | `credential.stale_last_success` | `Credential.lastSuccessAt < NOW() - 7 days AND enabled = true` | state-poll (class 1, NOT engine) | Latent auth issue masked by no traffic |

### Sequencing recommendation

- E34-S3 (this story) ships the Engine + 7 rules.
- E34-S4 (proposed) bulk-adds 1–9 above (the engine-compatible ones); each new rule = ~50 lines of glue.
- Class-1 rules (10, 11) follow the existing state-table job pattern, not Engine — separate story or fold into S4 as standalone job files.

P2-tier candidates (12 more) are documented in the 2026-04-29 exploration agent's report; not reproduced here to keep this section focused on actionable next-PR work.

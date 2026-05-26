---
doc: alerting-architecture
area: cross-cutting
service: observability
tier: 1
updated: 2026-05-20
---

# Alerting Architecture

> **Tier 1 architecture doc.** Read this before adding a new alert rule, alert channel, or evaluating aggregator. The Go BuiltinRules registry, the DB `AlertRule` rows, and the lockstep concern are the three things to know.

Alerts are how the platform pages humans. The pipeline is streaming over the same MQ that carries traffic and ops metrics; rule evaluation is in Hub; channel dispatch is fan-out.

---

## 1. Two parallel rule sources

| Source | Where | Edited by |
|---|---|---|
| Go `BuiltinRules` registry | `packages/nexus-hub/internal/alerts/engine/rules/builtin.go` | Engineering (compile-time) |
| DB `AlertRule` rows | `tools/db-migrate/seed/data/seed-baseline.sql` (seed) + admin UI | Admins (runtime) |

Both sources feed the same evaluator. They serve different needs:

- **Builtin rules** — invariants we want to ship with every install (e.g., provider-unavailability, hub-db-down). Compile-time means we cannot accidentally delete them via SQL.
- **DB rules** — tunables admins control (specific thresholds, customer-specific routes, custom aggregator parameters).

### The drift concern (binding)

The design is **not** "make them identical" (the two serve different purposes). It is **"every builtin's seed counterpart must exist with the binding name; admin can change params but not delete builtin-shipped rules"**.

The weaker half of that lockstep is enforced: `packages/nexus-hub/internal/alerts/engine/rules/builtin_seed_lockstep_test.go` asserts that every Go `BuiltinRules.ID` appears as an `AlertRule` row in `tools/db-migrate/seed/data/seed-baseline.sql`. The seed is allowed to carry additional admin-managed rules (DB-only) — these do not require Go counterparts. The full bidirectional design (with admin-only params + builtin-only schemas) is future work.

## 2. The evaluator

Hub runs the evaluator (`packages/nexus-hub/internal/alerts/eval/`). It subscribes to MQ streams:

- `nexus.traffic` — per-request events.
- `nexus.ops_metrics` — per-Thing metric samples.
- Job-emitted audit events (cross-ref `jobs-architecture.md`).

Each rule registers one or more **aggregators**. An aggregator is a streaming function (count/avg/percentile over a sliding window) keyed by labels (org_id, provider, model, …).

```go
type Aggregator interface {
    Ingest(event Event)
    Snapshot() Reading
}
```

When `Snapshot().Value > Threshold` and the rule's dedup/silence window allows, the rule **fires**.

## 3. Aggregator catalog

Current aggregators in `packages/nexus-hub/internal/alerts/eval/aggregators/` (19 production files):

| Aggregator | Aggregates |
|---|---|
| `auth_invalid_key_burst` | Invalid VK/API-key auth failures per source. |
| `compliance_hook_execution_timeout_surge` | Hook executions that exceeded the per-hook timeout. |
| `compliance_payload_capture_failure_rate` | Compliance-proxy payload-capture failures vs total intercepts. |
| `credential_auth_failures_cascade` | Cascading auth failures on a single provider credential. |
| `hook_reject_rate` | Hook-reject decisions per total hook executions. |
| `login_failure_flood` | Failed admin logins per actor/IP window. |
| `model_rate_limited_responses` | Upstream 429 responses per `(provider, model, source)`. |
| `provider_high_latency_percentile` | p95/p99 upstream latency per provider. |
| `provider_upstream_error` | Upstream 5xx / connection errors per provider. |
| `proxy_cost_spike` | Sudden cost rise per VK or org. |
| `proxy_high_error_rate` | AI-gateway 4xx/5xx ratio. |
| `proxy_hook_failure_rate` | Hook errors (not rejects) per total hook executions. |
| `proxy_hook_timeout_rate` | Hook timeouts per total hook executions. |
| `proxy_quota_runtime_exceeded` | Runtime quota exhaustion events. |
| `proxy_rate_limit_exceeded` | Gateway-side rate limit rejections. |
| `proxy_routing_no_match` | Requests that hit zero matching routing rules. |
| `vk_latency_degradation` | Per-VK latency rise vs baseline. |
| `vk_token_usage_spike` | Token-count spike per VK. |
| `vk_traffic_spike` | Request-count spike per VK. |

Adding a new aggregator: implement the `Aggregator` interface, register in `aggregators.Init()`, write tests.

## 4. Rule shape

```go
type Rule struct {
    Name           string
    Aggregator     string
    Threshold      float64
    Window         time.Duration
    Labels         map[string]string    // narrows the aggregator (e.g., provider="openai")
    Severity       Severity             // info | warning | critical | page
    Channels       []string             // channel IDs
    SilenceWindow  time.Duration        // dedup window
    Description    string               // shown in alert payload
    RunbookURL     string               // for the responder
}
```

Each rule is independently configurable via the admin UI (or seed). Multiple rules can share an aggregator (e.g., quota-threshold-warning and quota-threshold-critical use the same aggregator with different thresholds).

## 5. Channel model

Channels are destinations for alerts. Today:

- **Webhook** — POST JSON to a configured URL. Most flexible; bridges to PagerDuty, Slack, etc.
- **SIEM** — fire-and-forget to the configured SIEM bridge.
- **Email** — SMTP relay.

Per-channel config includes:

- Headers / auth (for webhook).
- Severity filter (per-channel; e.g., a Slack channel only gets `warning+`).
- Active hours / on-call rotation (planned, not in scope today).

A channel can be **tested** from the admin UI; the test emits a dummy alert and reports delivery success/failure.

## 6. Alert inbox

Every fired alert inserts into the `Alert` Prisma model (Hub-managed; channel fan-out is recorded on `AlertDispatch`). The CP UI surfaces unread alerts on the dashboard.

The IAM catalog (`packages/control-plane/internal/identity/iam/managed.go`) ships exactly two alert-admin actions:

- `admin:alert.read` — list / view alerts and rules.
- `admin:alert.update` — create / update / acknowledge / mute / channel-test. All write operations roll up into this single action code; finer-grained verbs (mute, unmute, snooze, resolve, test-channel) are intentionally not separate IAM actions.

State transitions are audit-logged on the `admin:alert.update` action (cross-ref `admin-audit-log-coverage.md`).

## 7. Sample alert payload

Webhook channel:

```json
{
  "alert_id": "01HXYZ...",
  "rule_name": "model.rate_limited",
  "severity": "warning",
  "fired_at": "2026-05-16T...",
  "summary": "openai gpt-4o rate-limit responses crossed 50/min",
  "value": 73,
  "threshold": 50,
  "window": "1m",
  "labels": { "provider": "openai", "model": "gpt-4o", "source": "upstream" },
  "samples": [
    { "request_id": "...", "trace_id": "...", "emitted_at": "..." },
    ...
  ],
  "runbook": "https://docs/operators/ops/runbooks/upstream-429.md"
}
```

Samples carry `request_id` so the responder can dive into the unified audit timeline.

## 8. Failure modes

| Failure | Behaviour |
|---|---|
| MQ consumer lag | Surfaced through the standard MQ outbox / dispatcher metrics (`mq-architecture.md`) and the runtime introspection surface; a dedicated builtin rule is not wired today. |
| Channel webhook unreachable | Retry with backoff (3 tries); after DLQ-style discard, emit `alert.dispatch_failed` audit. |
| Rule eval error | Logged; metric increments; rule auto-disabled after N consecutive errors. |
| Builtin↔DB drift (cross-ref §1) | Detect during admin review of any alert-related PR. Replacement lockstep design pending. |
| Alert storm | Per-rule silence window dedups. Cross-rule storm protection: a Hub-wide circuit breaker mutes channels above N alerts/min (admin-tunable). |

## 9. Operational concerns

- **Tuning thresholds** — admins do this through the UI. Test changes by sending synthetic events via the test harness.
- **Adding a builtin rule** — implement aggregator + rule in Go + add seed counterpart + alert smoke test. Document expected threshold range.
- **Migration of a rule from DB-only to builtin** — admin UI keeps the DB row; Go ships the builtin version which the seeder rewrites to the canonical params.

## 10. Sources

- `packages/nexus-hub/internal/alerts/eval/` — evaluator runtime.
- `packages/nexus-hub/internal/alerts/eval/aggregators/` — per-aggregator implementations.
- `packages/nexus-hub/internal/alerts/engine/rules/builtin.go` — Go `BuiltinRules` registry.
- `packages/nexus-hub/internal/alerts/engine/rules/builtin_seed_lockstep_test.go` — Go↔seed lockstep test (weaker half — see §1).
- `packages/nexus-hub/internal/alerts/engine/` — channel dispatch + inbox.
- `packages/nexus-hub/internal/alerts/client/` — Hub-internal outbound HTTP alert client with disk-spool resilience.
- `tools/db-migrate/seed/data/seed-baseline.sql` — DB `AlertRule` baseline rows.
- `tools/db-migrate/schema.prisma` — `AlertRule`, `AlertChannel`, `Alert`, `AlertDispatch` Prisma models.

## 11. Cross-references

- `mq-architecture.md` — `nexus.traffic` + `nexus.ops_metrics` streams.
- `audit-pipeline-architecture.md` — alert lifecycle emits audit.
- `jobs-architecture.md` — job-emitted events feed alerts.
- `error-taxonomy-architecture.md` — `ErrorClass` drives `model_rate_limited_responses`.
- `credentials-architecture.md` — credential-health rules.
- `quota-architecture.md` — quota threshold rules.
- `multi-endpoint-coordination-architecture.md` §8 — alert evaluation as a golden flow.

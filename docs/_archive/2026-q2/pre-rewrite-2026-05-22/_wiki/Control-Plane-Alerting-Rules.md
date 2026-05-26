# Control Plane Alerting Rules

*Audience: operators configuring alerts and contributors adding new alert rules.*

The alerting pipeline pages humans when a metric crosses a threshold. Rules evaluate in Hub against streaming events from the MQ and fire to configured channels (webhook, SIEM, email). Two parallel rule sources coexist: Go built-in rules compiled into Hub, and DB `AlertRule` rows managed by admins at runtime.

---

## Two rule sources

| Source | Where | Edited by |
|---|---|---|
| Go `BuiltinRules` registry | `packages/nexus-hub/internal/alerts/engine/rules/builtin.go` | Engineering (compile-time) |
| DB `AlertRule` rows | `tools/db-migrate/seed/data/seed-baseline.sql` + admin UI | Admins (runtime) |

Both sources feed the same evaluator. Built-in rules cover platform invariants that ship with every install. DB rules let admins tune thresholds, window sizes, and channel bindings without a code change.

Every Go `BuiltinRules.ID` must appear as an `AlertRule` row in the seed. The seed may carry additional admin-managed rules without Go counterparts. The lockstep test `builtin_seed_lockstep_test.go` enforces this one-way assertion.

## Evaluation model

Hub runs the evaluator in `packages/nexus-hub/internal/alerts/eval/`. It subscribes to:

- `nexus.traffic` — per-request events
- `nexus.ops_metrics` — per-service metric samples
- Job-emitted audit events

Each rule registers one or more **aggregators** — streaming functions (count, average, percentile) over a sliding window, keyed by labels (org, provider, model). When `Aggregator.Snapshot().Value > Threshold` and the rule's dedup/silence window allows, the rule fires.

## Aggregator catalog (19 aggregators)

| Aggregator | What it measures |
|---|---|
| `auth_invalid_key_burst` | Invalid VK auth failures per source |
| `compliance_hook_execution_timeout_surge` | Hook executions that exceeded timeout |
| `compliance_payload_capture_failure_rate` | Compliance-proxy capture failures vs intercepts |
| `credential_auth_failures_cascade` | Cascading auth failures on a single provider credential |
| `hook_reject_rate` | Hook-reject decisions per total hook executions |
| `login_failure_flood` | Failed admin logins per actor/IP window |
| `model_rate_limited_responses` | Upstream 429 responses per (provider, model, source) |
| `provider_high_latency_percentile` | p95/p99 upstream latency per provider |
| `provider_upstream_error` | Upstream 5xx / connection errors per provider |
| `proxy_cost_spike` | Sudden cost rise per VK or org |
| `proxy_high_error_rate` | AI Gateway 4xx/5xx ratio |
| `proxy_hook_failure_rate` | Hook errors per total hook executions |
| `proxy_hook_timeout_rate` | Hook timeouts per total hook executions |
| `proxy_quota_runtime_exceeded` | Runtime quota exhaustion events |
| `proxy_rate_limit_exceeded` | Gateway-side rate limit rejections |
| `proxy_routing_no_match` | Requests that hit zero matching routing rules |
| `vk_latency_degradation` | Per-VK latency rise vs baseline |
| `vk_token_usage_spike` | Token-count spike per VK |
| `vk_traffic_spike` | Request-count spike per VK |

## Rule shape

Each rule specifies an aggregator, threshold, window, labels, severity, channels, silence window, and an optional runbook URL. Multiple rules can share an aggregator with different thresholds (for example, a `warning` and a `critical` on the same quota aggregator).

## Channel model

Channels are alert destinations:

- **Webhook** — POST JSON to a configured URL. Bridges to PagerDuty, Slack, or any HTTP receiver.
- **SIEM** — fire-and-forget to the SIEM bridge.
- **Email** — SMTP relay.

Each channel carries a severity filter. A Slack channel receiving only `warning+` is a common setup. Channels can be tested from the admin UI; the test emits a dummy alert with a recorded result in the admin audit.

## Alert lifecycle in the UI

Fired alerts appear in the Alerts Inbox at `/alerts`. From there, operators can:

- **Acknowledge** — mark as reviewed.
- **Mute** — suppress future firings for this rule for a configured duration.
- **Snooze** — same as mute but with a reminder.
- **Resolve** — close manually if the condition is already clear.

All lifecycle transitions emit `admin:alert.update` audit rows.

## Sample webhook payload

```json
{
  "alert_id": "01HXYZ...",
  "rule_name": "model.rate_limited",
  "severity": "warning",
  "fired_at": "2026-05-16T10:00:00Z",
  "summary": "openai gpt-4o rate-limit responses crossed 50/min",
  "value": 73,
  "threshold": 50,
  "window": "1m",
  "labels": { "provider": "openai", "model": "gpt-4o" },
  "samples": [
    { "request_id": "...", "trace_id": "...", "emitted_at": "..." }
  ],
  "runbook": "https://docs/operators/ops/runbooks/upstream-429.md"
}
```

The `samples` array carries `request_id` so the responder can drill straight into the audit timeline.

## Failure modes

| Failure | Behaviour |
|---|---|
| Channel webhook unreachable | Retry with backoff (3 attempts); then discard + `alert.dispatch_failed` audit |
| Rule eval error | Rule auto-disabled after N consecutive errors; alert fires on the rule itself |
| Alert storm | Hub-wide circuit breaker mutes channels above N alerts/min (admin-tunable) |
| MQ lag | `mq_consumer_lag_high` rule self-monitors |

---

## Canonical docs

- [`alerting-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/observability/alerting-architecture.md) — evaluator, aggregators, channel model, failure modes
- [`alerts.md` (cp-ui features)](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/features/cp-ui/alerts.md) — Inbox, Rules, Channels UI surfaces
- [`alert-evaluation.md` (flow)](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/features/flows/alert-evaluation.md) — end-to-end alert evaluation sequence

**Adjacent wiki pages**: [Control Plane Audit Log](Control-Plane-Audit-Log) · [Control Plane SIEM Bridge](Control-Plane-SIEM-Bridge) · [Control Plane Infrastructure Pages](Control-Plane-Infrastructure-Pages) · [Feature Audit And SIEM](Feature-Audit-And-SIEM) · [Observability Stack](Observability-Stack)

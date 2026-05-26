# Flow — Alert evaluation

## What this flow accomplishes

A metric crosses a threshold (provider 429 burst / agent offline / quota near exhaustion / job stalled); rule evaluator detects; the right humans get paged or alerted.

## Actors

Emitter (any Thing) · MQ · Hub alerteval · Alert channels · Operators.

## Sequence

1. **Emitter** produces events: `traffic_event` (per request), `ops_metrics` sample, or job-emitted audit.
2. **MQ** streams (`nexus.traffic`, `nexus.ops_metrics`) carry the events.
3. **Hub alerteval** consumes the streams. For each rule:
   - Updates its **aggregator** (a streaming function: count, avg, percentile over a sliding window, keyed by labels).
   - On each tick: `Aggregator.Snapshot() > rule.Threshold` AND dedup window allows → **rule fires**.
4. **Fired alert** → insert `alert_inbox` row (CP UI surface) → publish to `nexus.alerts` MQ subject.
5. **Alert-dispatcher** consumer fans out to configured channels (webhook / SIEM / email) per per-channel severity filter.
6. **Operator** receives the alert via channel:
   - Webhook → PagerDuty / Slack / custom handler.
   - SIEM → external SIEM platform.
   - Email → SMTP relay.
7. **Operator → CP UI → Alerts → Inbox** → triage → ack / mute / snooze / resolve. Each lifecycle transition emits `admin_audit`.
8. **If the underlying condition clears** and `auto_resolve` is configured for the rule, the alert auto-resolves.

## Sample alert payload (webhook)

```json
{
  "alert_id": "01HXYZ...",
  "rule_name": "model.rate_limited",
  "severity": "warning",
  "fired_at": "...",
  "summary": "openai gpt-4o rate-limit responses crossed 50/min",
  "value": 73,
  "threshold": 50,
  "window": "1m",
  "labels": { "provider": "openai", "model": "gpt-4o", "source": "upstream" },
  "samples": [ {"request_id": "...", "trace_id": "..."}, ... ],
  "runbook": "https://docs/operators/ops/runbooks/upstream-429.md"
}
```

`samples` carries `request_id` so the responder can dive straight into the unified audit timeline.

## Failure modes

- **MQ lag in alerteval** — `mq_consumer_lag_high` alert rule self-monitors.
- **Channel webhook unreachable** — best-effort single attempt per fired alert. `DispatcherImpl.Dispatch` (`packages/nexus-hub/internal/alerts/engine/dispatcher.go:52-78`) calls each enabled channel's `Sender.Send` exactly once; on failure it persists an `AlertDispatch` row with `success=false`, the upstream `statusCode` (when one was received), and the `errorMsg` for operator visibility via the dispatches list UI. There is no in-process retry loop and no separate "dispatch_failed" audit event — the failure row IS the audit record. Operators who need at-least-once delivery should layer their own retry on the receiving webhook (PagerDuty's own EventV2 ingest handles this) or watch the dispatches UI for a follow-up incident.
- **Rule eval error** — auto-disable after N consecutive errors; emit alert.
- **Alert storm** — Hub-wide circuit breaker mutes channels above N alerts/min.
- **Builtin↔DB drift** — non-fatal but breaks expectations; binding manual review per `alerting-architecture.md` §1.

## Verification

```bash
# 1) Send a stream of 429-inducing requests (in a test env) to trip `model.rate_limited`.

# 2) Confirm alert in inbox:
cp_curl /api/admin/alerts | jq '.[0]'

# 3) Confirm channel fire:
# (depends on channel; check the receiving service)

# 4) Inspect samples; click a request_id link to the unified audit timeline.
```

## References

- `docs/developers/architecture/cross-cutting/observability/alerting-architecture.md` — evaluator + aggregators + channels.
- `docs/developers/architecture/cross-cutting/foundation/multi-endpoint-coordination-architecture.md` §8 — flow diagram.
- `docs/users/features/cp-ui/alerts.md` — inbox / rules / channels surfaces.
- `docs/developers/architecture/cross-cutting/safety/error-taxonomy-architecture.md` §8 — what aggregators consume.
- `project_alerting_builtin_drift_2026_05_15` (memory) — builtin/DB drift concern.

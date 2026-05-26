# Alerts section — CP-UI feature doc

> Audience: ops and on-call. This section is the operational layer for alert rules and dispatch channels.

## Pages in this section

| Page | Path | IAM action | Purpose |
|---|---|---|---|
| Alerts Inbox | `/alerts` | (alert.read) | Currently-firing + recently-resolved alerts; ack / mute / snooze actions |
| Rules | `/alerts/rules` | (alert-rule.read) | DB-managed AlertRule CRUD; pairs with Go BuiltinRules |
| Channels | `/alerts/channels` | (alert-channel.read) | Webhook / SIEM / Email destinations + per-channel severity filter + test |

## Common workflows

- **Triage a firing alert** — Inbox → expand row → see labels (provider, model, org, request samples) → click a sample `request_id` to drill into the unified audit timeline.
- **Mute noisy rule for a window** — Inbox → "Mute" → choose duration → reason. Audit row records the mute (`admin:alert.mute`).
- **Tune a rule threshold** — Rules → select → edit threshold or window → save. Re-evaluation begins on next aggregator tick.
- **Add a new webhook channel** — Channels → new → URL + headers/auth → severity filter → "Test" emits a synthetic alert. Test result audited.
- **Map a builtin rule to a custom channel** — Rules → select builtin (read-only on threshold but channels editable) → bind to custom channel.

## Key API endpoints

```
/api/admin/alerts             [GET]; POST /:id/ack; POST /:id/mute; POST /:id/snooze; POST /:id/resolve
/api/admin/alert-rules        [GET/POST/PUT/DELETE]
/api/admin/alert-channels     [GET/POST/PUT/DELETE]; POST /:id/test
```

## Failure modes & gotchas

- **Builtin↔DB drift (binding concern)** — DB AlertRule rows can drift from Go BuiltinRules; on 2026-05-15 prod had 30 vs 27 (memory `project_alerting_builtin_drift_2026_05_15`). Replacement lockstep design pending. **Manual review required** when touching either side.
- **Channel webhook unreachable** — auto-retry 3x with backoff; subsequent failures discard with audit. Surface in UI as "delivery failed".
- **Alert storm** — Hub-wide circuit breaker mutes channels above N alerts/min (admin-tunable). Surface in UI as a banner.
- **Test channel from UI** — emits a dummy payload with `_test: true`; receivers should filter.

## Architecture references

- `docs/developers/architecture/cross-cutting/observability/alerting-architecture.md` — evaluator + aggregators + channels
- `docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md` — alert events emit `admin_audit`
- `docs/developers/architecture/cross-cutting/foundation/jobs-architecture.md` — job-emitted alerts
- `docs/developers/architecture/cross-cutting/safety/error-taxonomy-architecture.md` — `ErrorClass` drives `model_rate_limited_responses` aggregator

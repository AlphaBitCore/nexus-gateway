# Alerting architecture

## 1. Intro

Alerting is the operational nerve of Nexus Gateway: a uniform path that turns "something measurable went outside its band" into a persisted `Alert` row and a fanned-out notification to one or more channels (webhook, Slack, email, PagerDuty). The same path handles operational signals (a Thing went offline, a credential's circuit breaker opened) and business signals (a virtual key's cost spiked, an upstream provider's 5xx rate crossed threshold).

The subsystem answers four questions for operators:

1. **What's broken right now?** — the unresolved `Alert` rows with `state = FIRING` or `ACKNOWLEDGED`, surfaced by the AlertBell badge and the Alerts list page.
2. **Who's been notified about it, and did the delivery succeed?** — per-channel `AlertDispatch` rows attached to the alert.
3. **What rule fired this, and what knob do I turn to make it more or less sensitive?** — the `AlertRule` registry, mutable per-tenant via the admin API.
4. **Where do these notifications go?** — `AlertChannel` rows, filtered by severity and source type.

Scope: alerting lives entirely inside `nexus-hub` (the Hub is the single producer of alerts and the single owner of the channel fan-out). The Control Plane exposes a thin admin proxy for the Hub endpoints; the AI Gateway, Compliance Proxy, and Agent emit traffic / audit events to MQ, but they never call the alerting engine directly.

## 2. Anchor packages

- `tools/db-migrate/schema.prisma` — `AlertRule`, `Alert`, `AlertChannel`, `AlertDispatch` models; `AlertSeverity` and `AlertState` enums.
- `packages/nexus-hub/internal/alerts/engine/` — domain types, the `Raiser` that owns the FIRING-state dedup invariant, the `Store` for persistence, the `DispatcherImpl` for channel fan-out, and the 14 admin HTTP handlers.
- `packages/nexus-hub/internal/alerts/engine/rules/builtin.go` — the Go-side source of truth for built-in rule definitions, consumed at Hub startup by `rules.NewRegistry`.
- `packages/nexus-hub/internal/alerts/engine/senders/` — one file per channel type: `webhook.go`, `slack.go`, `email.go`, `pagerduty.go`.
- `packages/nexus-hub/internal/alerts/eval/` — the streaming evaluator engine that subscribes to MQ traffic events and ticks aggregators on a fixed interval.
- `packages/nexus-hub/internal/alerts/eval/aggregators/` — one file per streaming rule; each owns its own ring-buffer / window logic and emits `Decision{Fire,Resolve}` per tick.
- `packages/nexus-hub/internal/jobs/defs/health/` and `packages/nexus-hub/internal/jobs/defs/quota/` — schedule-driven jobs that raise the state-poll alerts (quota, thing offline, provider unavailable, credential reliability, credential staleness, agent cert expiration, VK expiry).
- `packages/nexus-hub/cmd/nexus-hub/wiring/alerts.go` + `alerteval.go` — boot-time wiring that constructs the Store, sender registry, dispatcher, raiser, rules registry, and the streaming engine.
- `packages/nexus-hub/internal/jobs/defs/alert_raiser.go` — the narrow `AlertRaiser` interface that the schedule-driven jobs depend on.
- `packages/control-plane/internal/observability/alerts/handler/` — the CP admin forwarder (every endpoint proxies to Hub, gated by `iam.ResourceAlert.Action(...)`).
- `packages/control-plane-ui/src/pages/alerts/` and `packages/control-plane-ui/src/api/services/alerts/` — the operator-facing UI (Alerts list, detail drawer, Rules editor, Channels editor) and the typed client.
- `packages/shared/identity/iam/catalog_data.go` — the canonical `alert` resource (verbs: CRUD plus `Acknowledge`).

## 3. `AlertRule` shape

An `AlertRule` row is a tunable template, not a per-tenant alert instance. Each row carries:

- `id` — stable string key (e.g. `quota.threshold`, `proxy.cost_spike`). Producers (the streaming aggregator or a schedule-driven job) hand-pick this ID when calling `Raise`.
- `displayName` — human label shown in the rules table.
- `sourceType` — coarse category used by channel filtering. The canonical set lives in `alerting.AllSourceTypes` (`packages/nexus-hub/internal/alerts/engine/types.go`): `quota`, `proxy`, `thing`, `provider`, `auth`, `system`. The Prisma schema doc-comment on `AlertRule.sourceType` mirrors this set, and `TestBuiltinRuleSourceTypesAreCanonical` keeps the three layers in lockstep.
- `defaultSeverity` — the `AlertSeverity` enum (`CRITICAL` / `HIGH` / `MEDIUM` / `LOW` / `INFO`) used when the producer does not override per-firing.
- `requiresAck` — when true, the alert stays in `ACKNOWLEDGED` after a human ack and must be explicitly resolved; when false, auto-resolve flows are allowed.
- `enabled` — boolean kill switch. `Raise()` short-circuits with no row and no dispatch when the rule is disabled, so the operator UI's disable toggle is the fastest way to silence a noisy rule.
- `params` — rule-specific JSON (e.g. `{thresholdPct: 20, windowSec: 300, minSamples: 10}` for hook reject rate); the producer reads this on every tick / poll so admin edits propagate without a restart.
- `paramsSchema` — JSON Schema describing `params`. The admin UPDATE endpoint validates incoming params against this schema before writing.
- `cooldownSec` — minimum quiet window between dispatches for the same `(ruleID, targetKey)`. The Raiser checks this against the *most recent* firing (including resolved ones), so it survives ack/resolve cycles and process restarts. In-memory cooldown inside the streaming engine is a perf shortcut; the DB-backed check is the source of truth.
- `group_id_filter` — optional `DeviceGroup` foreign key. When set, the rule only fires for targets whose key has a `thing:<id>` prefix AND `<id>` is a member of the group (matched against `DeviceGroupMembership` plus the smart-group cache). NULL means fleet-wide.

`Alert` rows (the firing instances) carry a denormalised `severity`, `state` (`FIRING` / `ACKNOWLEDGED` / `RESOLVED`), `targetKey`, `targetLabel`, free-form `details` JSON, `firedAt` / `lastSeenAt`, and a monotonic `duplicateCount` that increments on repeat raises during dedup. Ack and resolve writes capture `acknowledgedBy` / `acknowledgedAt` and `resolvedBy` / `resolvedAt` / `resolvedReason` so the audit trail is intrinsic to the row.

## 4. Builtin rule set

`rules.BuiltinRules` is a single Go slice; `rules.NewRegistry(BuiltinRules)` consumes it at Hub boot and exposes a `Lookup(id)` interface used by the `POST /alerts/rules/:id/reset` endpoint to restore code-owned defaults.

The 30 rules currently in the Go source, grouped by `sourceType`:

| Source     | Rule ID                                       | Default severity | Default cooldown | Producer kind        |
| ---------- | --------------------------------------------- | ---------------- | ---------------- | -------------------- |
| `quota`    | `quota.threshold`                             | high (ack)       | 300 s            | schedule poll        |
| `quota`    | `quota.vk_expiring`                           | medium           | 86400 s          | schedule poll        |
| `proxy`    | `proxy.hook_failure_rate`                     | high             | 300 s            | streaming aggregator |
| `proxy`    | `proxy.hook_timeout_rate`                     | medium           | 300 s            | streaming aggregator |
| `proxy`    | `proxy.high_error_rate`                       | high             | 300 s            | streaming aggregator |
| `proxy`    | `proxy.cost_spike`                            | critical (ack)   | 3600 s           | streaming aggregator |
| `proxy`    | `proxy.rate_limit_exceeded`                   | high             | 300 s            | streaming aggregator |
| `proxy`    | `proxy.quota_runtime_exceeded`                | high             | 300 s            | streaming aggregator |
| `proxy`    | `proxy.routing_no_match`                      | medium           | 600 s            | streaming aggregator |
| `proxy`    | `hook.reject_rate`                            | high             | 300 s            | streaming aggregator |
| `proxy`    | `vk.traffic_spike`                            | critical (ack)   | 600 s            | streaming aggregator |
| `proxy`    | `vk.latency_degradation`                      | medium           | 600 s            | streaming aggregator |
| `proxy`    | `vk.token_usage_spike`                        | medium           | 600 s            | streaming aggregator |
| `proxy`    | `compliance.hook_execution_timeout_surge`     | medium           | 300 s            | streaming aggregator |
| `proxy`    | `compliance.payload_capture_failure_rate`     | medium           | 600 s            | streaming aggregator |
| `auth`     | `auth.login_failure_rate`                     | high             | 300 s            | streaming aggregator |
| `auth`     | `auth.invalid_key_burst`                      | high             | 300 s            | streaming aggregator |
| `provider` | `provider.unavailable`                        | critical (ack)   | 600 s            | schedule poll        |
| `provider` | `provider.upstream_error`                     | high             | 300 s            | streaming aggregator |
| `provider` | `provider.high_latency_percentile`            | medium           | 600 s            | streaming aggregator |
| `provider` | `model.rate_limited_responses`                | medium           | 300 s            | streaming aggregator |
| `provider` | `credential.auth_failures_cascade`            | high             | 600 s            | streaming aggregator |
| `provider` | `credential.expiring`                         | medium           | 86400 s          | schedule poll        |
| `provider` | `credential.stale_last_success`               | low              | 86400 s          | schedule poll        |
| `provider` | `credential.circuit_open`                     | high             | 300 s            | schedule poll        |
| `provider` | `credential.health_unavailable`               | high             | 300 s            | schedule poll        |
| `provider` | `credential.health_degraded_sustained`        | medium           | 1800 s           | schedule poll        |
| `thing`    | `thing.offline`                               | high             | 300 s            | schedule poll        |
| `thing`    | `agent.cert_expiration_imminent`              | medium           | 86400 s          | schedule poll        |
| `system`   | `system.channel_test`                         | info             | 0 s              | synthetic (UI test)  |

Builtin/seed lockstep is enforced by `TestBuiltinRulesAppearInSeed` and `TestSeedRulesAppearInBuiltin` in `packages/nexus-hub/internal/alerts/engine/rules/builtin_seed_lockstep_test.go` — adding a rule in one place without the other breaks the build.

## 5. Evaluation loop

Two complementary producers populate `Alert` rows, both routed through the same `Raiser`:

### 5a. Streaming evaluator (`alerteval.Engine`)

A scheduler job (`alerteval-engine`, default tick 5 s, configurable via `Scheduler.AlertEval.EngineTickSec`) that subscribes to MQ subjects under consumer group `hub-alerting`:

- `nexus.event.ai-traffic` — AI Gateway requests.
- `nexus.event.compliance` — Compliance Proxy traffic.
- `nexus.event.agent` — Agent-forwarded traffic.
- `nexus.event.admin-audit` — admin-audit events (e.g. login failure flood, invalid key burst).

The consumer group is independent of `hub-db-writer` (which feeds the `TrafficEvent` table) and `hub-siem` (the SIEM bridge); JetStream fan-out delivers each message to all three groups.

The engine registers 19 `Aggregator` implementations at boot. Each aggregator owns:

- `RuleID()` — the `AlertRule.id` it produces.
- `OnEvent(rt, evt)` — called per inbound MQ message; updates the aggregator's in-memory ring buffer / counter / sample window.
- `Tick(rt, params, now)` — called once per engine tick with the live `params` JSON; returns zero or more `Decision{Action: Fire|Resolve, TargetKey, Severity, Message, Details}` values.
- `MinWarmupSec(params)` — cold-start gate so the engine does not fire before it has enough samples after process start.

The engine reloads rule rows from the DB on every tick so admin edits to `params` propagate within one tick. Disabled rules skip both `OnEvent` and `Tick`. Cooldown is enforced per-`(rule, target)` in the in-memory `Runtime` for hot-path efficiency, with the DB-backed check in the `Raiser` as the source of truth.

The MQ consume goroutines are bound to a long-lived `consumeCtx` that is intentionally decoupled from the per-tick scheduler context. The scheduler cancels the per-tick context the moment `Run()` returns, so binding the consume loop to it would kill the goroutines after the first tick and silently freeze MQ ingestion. The `Engine` therefore owns its own cancellable context that lives until `Stop()` (or process shutdown) and re-subscribes on transient MQ errors with a 5 s backoff. Any unexpected return from `Consume` is logged at `error` level with the subject name so a silent wedge is impossible.

### 5b. Schedule-driven state pollers

Rules whose source data is "current state of a DB table" (rather than "rate of an event stream") run as standalone scheduler jobs that depend on the narrow `defs.AlertRaiser` interface (`Raise` + `Resolve`):

- `quota-alert-check` — raises `quota.threshold` from current quota counters.
- `vk-expiry` — raises `quota.vk_expiring`.
- `thing-offline-alerts` — raises `thing.offline` from `last_seen_at` timestamps in the `Thing` table.
- `provider-unavailable-alerts` — raises `provider.unavailable` from accumulated provider health.
- `credential-reliability-alerts` — raises `credential.circuit_open`, `credential.health_unavailable`, `credential.health_degraded_sustained` from persisted reliability state on the `Credential` table.
- `credential-stale-alerts` — raises `credential.stale_last_success` from `Credential.lastSuccessAt`, auto-resolves when usage recovers.
- `agent-cert-expiration-alerts` — raises `agent.cert_expiration_imminent` ahead of mTLS cert expiry.

Each job is registered in the standard Hub job registry and runs on its own interval (independent of the 5 s alerteval tick). State-poll jobs typically auto-call `Resolve` when the underlying condition clears, so an operator does not need to ack every transient blip.

### 5c. Fire / dedup / resolve semantics

The single entry point is `Raiser.Raise(ctx, RaiseInput{RuleID, TargetKey, TargetLabel, Severity, Message, Details})`. It:

1. Loads the rule by ID. Unknown ID → error. Disabled rule → silent drop (no row, no dispatch).
2. If the rule has a `group_id_filter` and the `TargetKey` is `thing:<id>`, checks membership in `DeviceGroupMembership` (live, expiry-respecting) plus the smart-group cache (`device_group_membership_cache`). Non-member or non-device target → silent drop.
3. Acquires `pg_advisory_xact_lock(hashtextextended(ruleID + ":" + targetKey, 0))` inside a transaction. Advisory locks serialise concurrent raises even when no row yet exists for the pair (a plain `SELECT ... FOR UPDATE` cannot lock a row that does not yet exist).
4. Reads the most recent `Alert` for this `(rule, target)`. If one exists AND it is `FIRING` OR within `cooldownSec` of `firedAt`, increment `duplicateCount` and bump `lastSeenAt` — no fresh row, no dispatch.
5. Otherwise, INSERT a fresh `Alert` row with `state=FIRING, duplicateCount=1`.
6. After COMMIT, if a fresh row was inserted, hand it to `Dispatcher.Dispatch` in a detached goroutine. A slow or failing channel never blocks persistence.

`Raiser.Resolve(ctx, ruleID, targetKey, reason)` transitions all `FIRING` and `ACKNOWLEDGED` rows for the `(rule, target)` pair to `RESOLVED` via `Store.ResolveByRuleTarget`, which hardcodes `resolvedBy = "system"` and writes the supplied reason. Manual operator-driven resolves go through the admin endpoint (§7) and call `Store.ResolveAlert`, a separate path that stamps the actor identity from the admin session.

## 6. Notification channels

`AlertChannel` rows are the operator-managed delivery targets. Schema:

- `name` — human label.
- `type` — one of `webhook`, `slack`, `email`, `pagerduty` (the four kinds wired into the sender registry at boot).
- `enabled` — kill switch.
- `severities` — string array of severity names. Empty array means "match all". The dispatcher compares case-insensitively so a row saved as `"Critical"` still matches an alert with severity `"critical"`.
- `sourceTypes` — string array of `AlertRule.sourceType` values. Empty array means "match all".
- `config` — type-specific JSON; secret fields are masked on read by `maskChannelConfig` and round-tripped through `mergeMaskedSecrets` so the UI can PATCH a partial update without re-entering secrets.

Sender config shapes:

- **Webhook** — `{url, headers?}`. POSTs the JSON-serialised `Alert` body. Any HTTP status ≥ 300 is treated as failure.
- **Slack** — `{webhookUrl?, botToken?, channel?}`. The incoming-webhook path is preferred when `webhookUrl` is set (no bot token required); otherwise the dispatcher falls back to `chat.postMessage` with `botToken + channel`.
- **Email** — `{host, port, from, to, username?, password?}`. `to` accepts a comma-separated list. Uses `net/smtp` with optional PLAIN auth.
- **PagerDuty** — `{routingKey}`. POSTs to the Events API v2 with `event_action: trigger` and `dedup_key = ruleID|targetKey`, so PagerDuty itself collapses repeated firings into a single incident.

`DispatcherImpl.Dispatch(ctx, alert)` walks every enabled channel, applies the severity + sourceType filter, resolves the channel type to its registered `Sender`, and calls `Send`. Each attempt writes one `AlertDispatch` row (success or failure, with HTTP status code and error message), so the admin UI can surface delivery problems. A missing sender registration also writes a failure row rather than silently dropping.

IAM verbs — channels are read/created/updated/deleted under the canonical `alert` resource: `alert.read`, `alert.create`, `alert.update`, `alert.delete`. The synthetic-test endpoint (`POST /alerts/channels/:id/test`) uses `alert.update`.

## 7. Admin CRUD

Hub exposes 14 endpoints under `/api/v1/admin/alerts/*`, gated by the inter-service `ServiceAuth` token (only the Control Plane is allowed to call them). The Control Plane proxies each one at the matching `/api/admin/alerts/*` path, applying IAM gating with `iam.ResourceAlert.Action(verb)`:

| Method | CP path                                    | Hub path (same suffix)                  | IAM verb       |
| ------ | ------------------------------------------ | --------------------------------------- | -------------- |
| GET    | `/api/admin/alerts`                        | `/api/v1/admin/alerts`                  | `alert.read`   |
| GET    | `/api/admin/alerts/:id`                    | `/api/v1/admin/alerts/:id`              | `alert.read`   |
| POST   | `/api/admin/alerts/:id/ack`                | `/api/v1/admin/alerts/:id/ack`          | `alert.acknowledge` |
| POST   | `/api/admin/alerts/:id/resolve`            | `/api/v1/admin/alerts/:id/resolve`      | `alert.acknowledge` |
| GET    | `/api/admin/alerts/rules`                  | `/api/v1/admin/alerts/rules`            | `alert.read`   |
| GET    | `/api/admin/alerts/rules/:id`              | `/api/v1/admin/alerts/rules/:id`        | `alert.read`   |
| PUT    | `/api/admin/alerts/rules/:id`              | `/api/v1/admin/alerts/rules/:id`        | `alert.update` |
| POST   | `/api/admin/alerts/rules/:id/reset`        | `/api/v1/admin/alerts/rules/:id/reset`  | `alert.update` |
| GET    | `/api/admin/alerts/channels`               | `/api/v1/admin/alerts/channels`         | `alert.read`   |
| POST   | `/api/admin/alerts/channels`               | `/api/v1/admin/alerts/channels`         | `alert.create` |
| GET    | `/api/admin/alerts/channels/:id`           | `/api/v1/admin/alerts/channels/:id`     | `alert.read`   |
| PUT    | `/api/admin/alerts/channels/:id`           | `/api/v1/admin/alerts/channels/:id`     | `alert.update` |
| DELETE | `/api/admin/alerts/channels/:id`           | `/api/v1/admin/alerts/channels/:id`     | `alert.delete` |
| POST   | `/api/admin/alerts/channels/:id/test`      | `/api/v1/admin/alerts/channels/:id/test`| `alert.update` |

The parametric `/:id` routes are registered after the static `/rules` and `/channels` siblings so Echo's matcher does not bind `/rules` against `/:id`.

Hub's `ListAlerts` accepts the multi-value categorical filter set (`state`, `severity`, `sourceType`, `ruleId`) as repeated query parameters; within a dimension values are OR'd, across dimensions they are AND'd. Time bounds are `since` / `until` (RFC 3339), pagination is `offset` / `limit`. The CP forwarder passes the query string verbatim. `GetAlert` returns the flat `Alert` shape plus a `dispatches[]` array of every delivery attempt.

`UpdateRule` accepts `displayName`, `defaultSeverity`, `requiresAck`, `enabled`, `params`, `cooldownSec` (and optionally `paramsSchema` for advanced operators) and validates `params` against the rule's stored `paramsSchema` before writing. `UpdateChannel` is a partial patch — nil fields preserve the existing value, sensible for the list page's one-click enable/disable toggle.

`ResetRule` restores code-owned defaults from `rules.BuiltinRules` via the in-process `RuleRegistry.Lookup`. Rules absent from `BuiltinRules` return 404 from this endpoint (see §10).

`ChannelTest` raises a synthetic alert against rule `system.channel_test` with the target channel as the only delivery surface — the operator can verify wiring without waiting for a real incident.

There is no Redis-pub/sub or shadow-push notification here: rule and channel edits are read on the next engine tick (≤ 5 s) by the streaming evaluator and on the next poll by each state-poll job. The Raiser reads rules fresh from `Store.GetRule` on every `Raise` call, so disable-the-rule takes effect immediately for the next firing.

## 8. CP-UI surface

All UI lives under `packages/control-plane-ui/src/pages/alerts/`:

- **AlertBell** (`components/alerts/AlertBell.tsx`) — header-bar badge showing the count of `FIRING` alerts; click navigates to the list page.
- **AlertListPage** (`pages/alerts/list/`) — paginated table with the multi-value filter chips (state / severity / sourceType / rule), bound to `GET /api/admin/alerts`.
- **AlertDetailDrawer** (`pages/alerts/detail/`) — side drawer for `GET /api/admin/alerts/:id`; renders the alert metadata, the `details` JSON via per-rule custom renderers in `pages/alerts/detailRenderers/`, and the dispatch history.
- **AlertRulesListPage** (`pages/alerts/rules/`) — table of every rule with severity, enabled toggle, source type, cooldown, and a Reset action that calls `POST .../reset`.
- **AlertRuleEditPage** (`pages/alerts/rules/`) — rule editor; the `params` form is rendered from `paramsSchema` via per-rule custom editors in `pages/alerts/ruleEditors/` when a specialised UI exists, otherwise via the generic schema-driven form.
- **AlertChannelsListPage** and **AlertChannelEditPage** (`pages/alerts/channels/`) — channel CRUD; secrets are sent and returned in masked form so the same form round-trips a partial PATCH without re-entering the URL / bot token / SMTP password / routing key.

The typed client (`api/services/alerts/alerts.ts`) is the only place CP-UI code talks to the alerting surface; useApi query keys are domain-prefixed `['admin', 'alerts', ...]` for cache invalidation hygiene.

## 9. Audit + observability

- **Admin audit** — every CP mutation that returns 2xx writes an `admin_audit` entry via the standard CP `audit.Writer`. The entry stamps `resource = "alert"`, the appropriate verb, the entity ID (rule ID, channel ID, or alert ID), and `afterState = {hubPath, method, subEntity}` so the audit trail records which sub-entity (alert vs rule vs channel) was touched without copying request bodies (which can carry secrets).
- **Intrinsic alert audit** — ack and resolve writes are persisted into the `Alert` row itself: `acknowledgedBy` / `acknowledgedAt` and `resolvedBy` / `resolvedAt` / `resolvedReason`. The Raiser logs a structured `alert resolved` slog entry with `ruleId`, `targetKey`, `reason`, and affected row count when a resolve hits any rows.
- **Dispatch trail** — every delivery attempt produces an `AlertDispatch` row including HTTP status (when present) and the error message on failure, so a misconfigured channel surfaces in the UI on the next alert.
- **MQ consumer health** — the streaming engine logs at boot the count of subjects subscribed and aggregators registered; any unexpected return from the MQ consume loop is logged at `error` level with the subject name and the backoff before resubscribe, so a silent wedge in the consumer becomes immediately visible.

## 10. Code issues found

**Severity casing mismatch (currently handled, fragile).** `BuiltinRules` uses lowercase `Severity` constants (`"critical"`, `"high"`, ...), the Prisma `AlertSeverity` enum uses uppercase (`"CRITICAL"`, `"HIGH"`, ...). The boundary lives in two helpers in `packages/nexus-hub/internal/alerts/engine/store.go` — `dbSeverity` (Go-typed → uppercase string for DB writes) and `goSeverity` (DB string → typed `Severity` via `ParseLoose`) — plus `ParseLoose` in `types.go` for free-form inputs (e.g. `Channel.severities[]`). The dispatcher's `matchesSeverity` does a case-insensitive compare. Any new code path that writes severity to a DB column or compares against the channel filter must route through these helpers rather than re-implement the casing rule.

**`ListChannels` skips disabled channels for fan-out but exposes them in the admin list (intentional, worth noting).** `DispatcherImpl.Dispatch` calls `Store.ListEnabledChannels`, so a disabled channel never receives an alert. The admin `ListChannels` endpoint returns every channel including disabled ones so operators can re-enable them. The two paths use different store methods; do not collapse them.

**Test-channel uses a real Raiser path (intentional, worth knowing).** `POST /alerts/channels/:id/test` raises a synthetic alert via the normal Raiser → Dispatcher path against rule `system.channel_test` (`cooldownSec = 0`, enabled by default). It produces a real `Alert` row, a real `AlertDispatch` row, and a real network call to the channel's endpoint. The cooldown of 0 is deliberate so consecutive tests during channel setup are not silently coalesced into a duplicate count.

## References

- `tools/db-migrate/schema.prisma`
- `tools/db-migrate/seed/data/seed-baseline.sql`
- `packages/nexus-hub/internal/alerts/engine/types.go`
- `packages/nexus-hub/internal/alerts/engine/raiser.go`
- `packages/nexus-hub/internal/alerts/engine/dispatcher.go`
- `packages/nexus-hub/internal/alerts/engine/store.go`
- `packages/nexus-hub/internal/alerts/engine/handlers_admin.go`
- `packages/nexus-hub/internal/alerts/engine/rules/builtin.go`
- `packages/nexus-hub/internal/alerts/engine/rules/registry.go`
- `packages/nexus-hub/internal/alerts/engine/senders/webhook.go`
- `packages/nexus-hub/internal/alerts/engine/senders/slack.go`
- `packages/nexus-hub/internal/alerts/engine/senders/email.go`
- `packages/nexus-hub/internal/alerts/engine/senders/pagerduty.go`
- `packages/nexus-hub/internal/alerts/eval/engine.go`
- `packages/nexus-hub/internal/alerts/eval/aggregator.go`
- `packages/nexus-hub/internal/alerts/eval/runtime.go`
- `packages/nexus-hub/internal/alerts/eval/aggregators/`
- `packages/nexus-hub/internal/jobs/defs/alert_raiser.go`
- `packages/nexus-hub/internal/jobs/defs/quota/quota_alert_check.go`
- `packages/nexus-hub/internal/jobs/defs/expiry/vk_expiry.go`
- `packages/nexus-hub/internal/jobs/defs/health/thing_offline_alerts.go`
- `packages/nexus-hub/internal/jobs/defs/health/provider_unavailable_alerts.go`
- `packages/nexus-hub/internal/jobs/defs/health/credential_reliability_alerts.go`
- `packages/nexus-hub/internal/jobs/defs/health/credential_stale_alerts.go`
- `packages/nexus-hub/internal/jobs/defs/health/agent_cert_expiration_alerts.go`
- `packages/nexus-hub/cmd/nexus-hub/wiring/alerts.go`
- `packages/nexus-hub/cmd/nexus-hub/wiring/alerteval.go`
- `packages/nexus-hub/internal/handler/routes.go`
- `packages/nexus-hub/internal/config/config.go`
- `packages/control-plane/internal/observability/alerts/handler/handler.go`
- `packages/control-plane/internal/observability/alerts/handler/alerts.go`
- `packages/shared/identity/iam/catalog_data.go`
- `packages/control-plane-ui/src/api/services/alerts/alerts.ts`
- `packages/control-plane-ui/src/components/alerts/AlertBell.tsx`
- `packages/control-plane-ui/src/pages/alerts/list/AlertListPage.tsx`
- `packages/control-plane-ui/src/pages/alerts/detail/AlertDetailDrawer.tsx`
- `packages/control-plane-ui/src/pages/alerts/rules/AlertRulesListPage.tsx`
- `packages/control-plane-ui/src/pages/alerts/rules/AlertRuleEditPage.tsx`
- `packages/control-plane-ui/src/pages/alerts/channels/AlertChannelsListPage.tsx`
- `packages/control-plane-ui/src/pages/alerts/channels/AlertChannelEditPage.tsx`

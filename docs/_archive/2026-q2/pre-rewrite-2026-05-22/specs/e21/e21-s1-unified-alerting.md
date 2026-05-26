# E21 Story 1 — Unified Alerting Pipeline

**Epic:** 21 — Unified Alerting
**Story:** 1
**Status:** Implemented — 2026-04-22
**Design:** `docs/_archive/2026-q2/brainstorms/2026-04-21-unified-alerting-design.md`
**OpenAPI:** `docs/users/api/openapi/admin/e21-s1-unified-alerting.yaml`
**Runbook:** `docs/operators/ops/runbooks/alerts.md`

> Epic 21 was driven directly from a design spec. No standalone `docs/developers/specs/e21-*.md` exists; the design doc linked above supplies the motivation, scope, and non-functional constraints that would normally appear there.

## Context

Before this epic, three independent alerting mechanisms operated in isolation with no shared storage, no shared dispatch, and no unified operator interface:

| System | Producer | Storage | Dispatch | Lifecycle |
|---|---|---|---|---|
| Quota alerts | `QuotaAlertCheckJob` in Hub (cron, 1/min) | `quota_alert` PG table | None — UI-read only | `active` → `acknowledged` → `resolved` (manual) |
| Proxy runtime alerts | `compliance-proxy` in-process evaluator | In-memory ring buffer, max 100 rows, lost on restart | Webhook/Slack/Email/PagerDuty via shadow `alert_channels` | `firing` → `resolved` (auto, no ack) |
| Hub webhook evaluator | `nexus-hub/internal/alerting/evaluator.go` | Stateless, cooldown map in memory | Single webhook URL | Fire-and-forget, no lifecycle |

Operators using both the quota-monitoring and compliance-proxy features had to configure Slack separately in two places, view three different pages to get a full picture of system health, and could not reconstruct proxy alert history after a proxy restart.

This epic deletes all three mechanisms and replaces them with a single Hub-owned pipeline:

- **Durable storage** — four new Postgres tables (`alert_rule`, `alert`, `alert_channel`, `alert_dispatch`) owned by Hub. Alerts survive service restarts.
- **Rule registry** — one row per rule identity. Rule `id` is the evaluator selector (`quota.threshold`, `proxy.hook_failure_rate`, `thing.offline`, etc.). Operator-editable knobs (thresholds, windows) live in `AlertRule.params`; the Go code owns the param schema via `paramsSchema` so UI can render a generic editor without code changes.
- **Severity × source-type channel routing** — a single `AlertChannel` row covers any source type and any severity range. One Slack channel can receive only critical proxy alerts; one email can receive all quota alerts regardless of severity.
- **Disk spool for data-plane producers** — `shared/spool` and `shared/alertclient` give compliance-proxy and ai-gateway a crash-safe outbound path. Envelopes are fsynced to disk on enqueue and deleted only after Hub returns HTTP 2xx, so a Hub restart loses no alerts.
- **New producers from day one** — `thing.offline` and `provider.unavailable` prove the rails are generic and not quota-specific.
- **Unified UI** — `/alerts` inbox, `/alerts/rules` editor, `/alerts/channels` CRUD under a new top-level "Alerts" nav section.

The `quota_alert` table and all shadow config keys for channels and thresholds (`alert_channels`, `alert_thresholds`, `alert_custom_checks`) are deleted without data migration per the pre-GA dev-phase policy.

## User Story

**As a** platform operations engineer or compliance admin,
**I want** a single alert inbox, rule registry, and channel configuration page backed by durable storage,
**so that** I can see all system health events in one place, configure one Slack target for all critical alerts, and review the complete dispatch history for any past alert — even after a data-plane service restart.

## Tasks

### T1 — Schema Migration

**Delivered:** One Prisma migration `20260421_unified_alerting` in `tools/db-migrate/schema.prisma`.

- DROP `model QuotaAlert` and its backing table.
- ADD `model AlertRule` (id, displayName, sourceType, defaultSeverity, requiresAck, enabled, params JSON, paramsSchema JSON, cooldownSec, timestamps).
- ADD `model Alert` (id UUID, ruleId FK, sourceType denorm, targetKey, targetLabel, severity, state, message, details JSON, firedAt, lastSeenAt, duplicateCount, ack/resolve audit columns).
- ADD `model AlertChannel` (id UUID, name, type, enabled, severities string[], sourceTypes string[], config JSON, timestamps).
- ADD `model AlertDispatch` (id UUID, alertId FK cascade, channelId no-FK, channelName denorm, success, statusCode?, errorMsg?, attemptedAt).
- ADD Prisma enums `AlertSeverity` (CRITICAL HIGH MEDIUM LOW INFO) and `AlertState` (FIRING ACKNOWLEDGED RESOLVED).
- Seed file `tools/db-migrate/prisma/seed.ts` inserts 9 built-in `AlertRule` rows: 8 operator-visible rules (`quota.threshold`, `quota.vk_expiring`, `proxy.hook_failure_rate`, `proxy.hook_timeout_rate`, `proxy.high_error_rate`, `proxy.cost_spike`, `thing.offline`, `provider.unavailable`) plus 1 synthetic `system.channel_test` row consumed by the channel-test endpoint to satisfy the `alert_dispatch.alert_id` FK.

### T2 — Shared Packages: `shared/spool` and `shared/alertclient`

**Delivered:** Two new packages in `packages/shared/`.

`packages/shared/spool/spool.go` — generic append-only disk queue `Spool[T any]`:
- `Enqueue(item T) error` — marshals to JSON, writes to a per-envelope file under `dir/name/`, fsyncs before returning.
- `Drain(ctx, send func(T) error) (int, error)` — iterates envelope files in creation order; calls `send`; deletes file only on `nil` return.
- `PendingCount() int` — count of unacknowledged envelope files.
- LRU eviction when total spool size exceeds `maxBytes` (default 50 MB); oldest files evicted, counter incremented.
- Corrupt envelopes (JSON parse failure) are skipped with a `slog.Warn` and a `*_spool_corrupt_total` counter.

`packages/shared/alertclient/client.go` — `Client` struct:
- `Fire(ctx, AlertEnvelope) error` — POST to `{hubBaseURL}/api/v1/alerts/raise` with 5s timeout; on 2xx returns nil; on any error or non-2xx writes to spool and returns nil (non-blocking to caller).
- `Resolve(ctx, ruleID, targetKey, reason) error` — POST to `/api/v1/alerts/resolve`; same error path.
- `ReplayPending(ctx) (int, error)` — calls `spool.Drain` forwarding each envelope to Hub.
- Wired to `thingclient.OnReconnect` (drain on resume) and a 30s ticker.
- Prometheus metrics per namespace: `*_alertclient_pending_alerts` (gauge), `*_alertclient_fire_total{result=success|spool|drop}` (counter), `*_alertclient_replay_total{result=success|fail}` (counter).

`packages/shared/alertclient/types.go` — `AlertEnvelope` and `ResolveRequest` structs (JSON field names: `ruleId`, `targetKey`, `targetLabel`, `severity`, `message`, `details`, `firedAt`).

**Note on scope:** The plan spec proposed extracting break-glass into `shared/spool`. After implementation, break-glass's latest-wins semantics differed enough from alert-queue fan-in semantics that the two are kept separate. Break-glass stays in `packages/compliance-proxy/internal/runtimeapi/break_glass.go`. `shared/spool` is new, purpose-built for `alertclient`.

### T3 — Hub Core: Raiser, Resolver, Dispatcher, Senders

**Delivered:** Core alerting pipeline in `packages/nexus-hub/internal/alerts/engine/`.

`raiser.go` — `Raise(ctx, RaiseInput) error`:
- `SELECT ... FOR UPDATE` the latest FIRING or ACKNOWLEDGED row for `(ruleId, targetKey)`.
- If none or last=RESOLVED: `INSERT` new row, state=FIRING, dispatch to channels.
- If FIRING: `UPDATE lastSeenAt, duplicateCount++` — no re-dispatch (dedupe).
- If ACKNOWLEDGED: `INSERT` new row (re-fire after ack → fresh incident) + dispatch.
- Full operation in one transaction to prevent duplicate inserts under concurrent Raise.

`raiser.go` — `Resolve(ctx, ruleID, targetKey, reason) error`:
- `UPDATE alert SET state=RESOLVED ...` where `state IN (FIRING, ACKNOWLEDGED)`.
- No dispatch on resolve (v1).

`dispatcher.go` — `Dispatch(ctx, alert Alert) error`:
- SELECT all enabled channels from DB.
- Skip channel if `severities` non-empty and `alert.severity` not in list.
- Skip channel if `sourceTypes` non-empty and `alert.sourceType` not in list.
- Call `Sender.Send(ctx, channel, alert)`; `INSERT alert_dispatch` row regardless of success/failure.

`senders/` — four `Sender` implementations, each with 10s timeout:
- `webhook.go` — POST JSON body to `config.url`; optional `config.headers` map forwarded verbatim.
- `slack.go` — POST to `config.url` (Slack Incoming Webhook URL); body uses `blocks` attachment format.
- `email.go` — SMTP with optional TLS; fields: `host`, `port`, `username`, `smtpPassword` (masked in responses), `from`, `to[]`, `subject`.
- `pagerduty.go` — PagerDuty Events API v2; fields: `routingKey` (masked), `dedupKey` from `alert.id`.

Secret masking: `handlers_admin.go:maskChannelConfig` masks `botToken`, `smtpPassword`, `routingKey`, and any header with name containing "authorization", "token", or "secret". Mask pattern: `xxxx-••••-` + last 4 chars. PUT round-trips preserve the original secret via `mergeMaskedSecrets`.

`rules/` — `RuleRegistry` maps rule `id` → `RuleDefault` (code-owned defaults for the reset endpoint). `rules/builtin.go` declares all 9 built-in rules including their `paramsSchema`.

`store.go` — pgx CRUD for all four tables: `ListAlerts`, `GetAlert`, `ListDispatchesByAlert`, `AcknowledgeAlert`, `ResolveAlert`, `ListRules`, `GetRule`, `UpdateRule`, `InsertChannel`, `GetChannel`, `ListChannels`, `UpdateChannel`, `DeleteChannel`, `InsertAlert`, `InsertDispatch`.

### T4 — Hub Admin API: 14 Endpoints

**Delivered:** `packages/nexus-hub/internal/alerts/engine/handlers_admin.go` implementing `AdminHandlers` (Echo handler struct).

14 registered endpoints under `/api/v1/admin/alerts/*`:

| Method | Path | Handler | Notes |
|---|---|---|---|
| GET | `/admin/alerts` | `ListAlerts` | Multi-value query params: `state[]`, `severity[]`, `sourceType[]`, `ruleId[]`, `since` RFC3339, `until` RFC3339, `offset`, `limit` |
| GET | `/admin/alerts/:id` | `GetAlert` | Returns `{alert, dispatches[]}` |
| POST | `/admin/alerts/:id/ack` | `AckAlert` | Body `{reason?}`; actor from `X-Nexus-Actor-User-Id` header |
| POST | `/admin/alerts/:id/resolve` | `ResolveAlert` | Body `{reason?}`; actor from header |
| GET | `/admin/alerts/rules` | `ListRules` | Returns `{rules[]}` |
| GET | `/admin/alerts/rules/:id` | `GetRule` | Returns single `AlertRule` |
| PUT | `/admin/alerts/rules/:id` | `UpdateRule` | Partial update; `params` validated against `paramsSchema` via `jsonschema/v5` |
| POST | `/admin/alerts/rules/:id/reset` | `ResetRule` | Restores to code-declared defaults from `RuleRegistry` |
| GET | `/admin/alerts/channels` | `ListChannels` | Returns `{channels[]}` with secrets masked |
| POST | `/admin/alerts/channels` | `CreateChannel` | Returns 201 + created channel (secrets masked) |
| GET | `/admin/alerts/channels/:id` | `GetChannel` | Secrets masked |
| PUT | `/admin/alerts/channels/:id` | `UpdateChannel` | Merges masked secrets from existing |
| DELETE | `/admin/alerts/channels/:id` | `DeleteChannel` | Returns 204 |
| POST | `/admin/alerts/channels/:id/test` | `ChannelTest` | Inserts synthetic `system.channel_test` alert, dispatches, writes `alert_dispatch`, then immediately resolves the synthetic alert so it does not appear in the inbox |

Internal endpoints (`handlers_internal.go`, registered at Hub root):
- `POST /api/v1/alerts/raise` — decodes `alertclient.AlertEnvelope`, calls `raiser.Raise`. Returns 200 on success.
- `POST /api/v1/alerts/resolve` — decodes `alertclient.ResolveRequest`, calls `raiser.Resolve`. Returns 204 on success.

IAM actions seeded in Control Plane:
- `admin:ReadAlerts` — roles: `super_admin`, `admin`, `compliance_admin`, `viewer`
- `admin:ManageAlerts` — roles: `super_admin`, `compliance_admin`
- Deleted: `admin:ReadQuotaAlerts`, `admin:ManageQuotaAlerts`, proxy alert-rule/channel actions under `admin:ManageCompliance`

CP BFF thin-forward in `packages/control-plane/internal/handler/admin_alerts.go`:
- Forwards all `/api/admin/alerts/*` requests to Hub after IAM gate.
- Injects `X-Nexus-Actor-User-Id` header from session.

### T5 — Producer Migration and New Producers

**Delivered:** Three existing/new producers calling the Hub alerting pipeline.

`QuotaAlertCheckJob` (`packages/nexus-hub/internal/jobs/quota_alert_check.go`):
- Replaced `UPSERT quota_alert` with `alerting.Raise(ctx, "quota.threshold", ...)`.
- Added second pass: for each currently-FIRING `quota.threshold` alert, recompute usage; if below threshold → `alerting.Resolve(...)`.
- `targetKey` format: `org:{orgId}|period:{yyyy-mm}` or `vk:{vkId}|period:{yyyy-mm}`.
- `quota.vk_expiring` split into its own rule with 30/15/7/1 day thresholds from `params`.

`compliance-proxy` evaluator (`packages/compliance-proxy/internal/alerting/evaluator.go`):
- Kept: CheckFunc registration, cooldown map, periodic scheduler.
- Replaced: local dispatcher + in-memory ring buffer → `alertclient.Fire` / `alertclient.Resolve`.
- Deleted: `channels.go`, `channel_senders.go`, `rebuild_test.go`, `channels_test.go`.
- `targetKey` format: `proxy:{nodeID}` (node-wide) or `proxy:{nodeID}|hook:{hookID}` (per-hook).
- Stable `ruleId` values: `proxy.hook_failure_rate`, `proxy.hook_timeout_rate`, `proxy.high_error_rate`, `proxy.cost_spike`.

AI Gateway (`packages/ai-gateway/internal/alerting/`):
- Wired `alertclient.Client` for quota and hook-related producers.

New Hub producers:
- `thing.offline` (`packages/nexus-hub/internal/jobs/thing_offline_alerts.go`) — scans `thing.lastSeenAt < now - 5m AND state='online'`; raises per thing; auto-resolves when `lastSeenAt` within window.
- `provider.unavailable` (`packages/nexus-hub/internal/jobs/provider_unavailable_alerts.go`) — watches `provider_health.status IN ('unhealthy','degraded')` for ≥ configured seconds (default 120); auto-resolves when `status='healthy'` for ≥60s (flap protection).

### T6 — UI Rebuild

**Delivered:** Four new pages under `packages/control-plane-ui/src/pages/alerts/`, new API service, route config update, nav restructure.

`packages/control-plane-ui/src/api/services/alerts.ts` — `alertsApi` covering all 14 admin endpoints. TypeScript types: `Alert`, `AlertListResponse`, `AlertDispatch`, `AlertDetailResponse`, `AlertRule`, `AlertChannel`, `ListAlertsParams`. Query params built via `buildListQuery` emitting repeated `state=firing&state=acknowledged` pairs matching Hub's `url.Query()["state"]` multi-value reading.

`AlertListPage.tsx` — DataTable (State • Severity • SourceType • Rule • Target • FiredAt • Actions), filter row, 15s auto-refresh, right-side `AlertDetailDrawer` on row click. `queryKey: ['admin', 'alerts', 'inbox', ...stateVars]`.

`AlertDetailDrawer.tsx` — per-rule body renderer via `detailRenderers` map; dispatch history table; Ack/Resolve action buttons.

`AlertRulesListPage.tsx` + `AlertRuleEditPage.tsx` — enabled toggle, cooldownSec, requiresAck, defaultSeverity controls. `params` edited via dedicated React editors (`ruleEditors` map) for `quota.threshold`, `quota.vk_expiring`, `proxy.hook_failure_rate`, `proxy.hook_timeout_rate`; all other rules use the generic JSON-schema-driven editor. Reset button calls `POST /rules/:id/reset`.

`AlertChannelsListPage.tsx` + `AlertChannelEditPage.tsx` — full CRUD, secrets column shows masked preview (e.g. `xoxb-••••••••abc123`), Test button calls `POST /:id/test` and shows success/error toast.

Route config (`shellRouteConfig.tsx`):
- Removed: `proxy/alerts` route and `alerts` overview route.
- Added: `alerts` section with sub-routes `alerts` (inbox), `alerts/rules`, `alerts/rules/:id`, `alerts/channels`, `alerts/channels/new`, `alerts/channels/:id/edit`.

Nav restructure: removed `proxy.alerts`, `compliance.alertChannels`, `compliance.alertRules` entries; added top-level `alerts` section with `inbox`, `rules`, `channels` entries.

i18n: added `pages:alerts.*` keys (inbox, rules, detail, channels, ruleEditors, detailRenderers) and `nav:alerts.*` in all three locales (en/zh/es); deleted stale `pages:alertCenter.*`, `pages:proxy.alertHistory.*`, `pages:compliance.alertChannels.*`, `pages:compliance.alertRules.*` sections. Three-locale key-count parity verified.

### T7 — Cleanup

**Delivered:** Dead code and configuration removed in the same PR as the new implementation (no phased deletion).

Deleted:
- `shared/configtypes/alert_channel.go` — shadow channel model superseded by Hub DB.
- Shadow config keys `alert_channels`, `alert_thresholds`, `alert_custom_checks` removed from `shared/configtypes` and Hub config emitter.
- `compliance-proxy/internal/alerting/channels.go`, `channel_senders.go`, and associated tests.
- `quota_alert` Prisma model and its Prisma-generated Go struct.
- CP handlers `admin_quota_alerts.go` and `admin_compliance_alert_*`.

### T8 — Testing and Documentation

**Delivered:** Unit tests, integration test, E2E test harness, this SDD, the OpenAPI spec, and the operational runbook.

Unit tests: all new Go packages have `_test.go` files using `go test -race -count=1`. Table-driven tests for raiser dedup semantics (new row / UPDATE / re-fire after ack), dispatcher severity × sourceType routing, spool enqueue/drain/crash recovery, alertclient success/spool/replay paths, each sender (using `httptest.Server`).

Integration test: `packages/nexus-hub/internal/alerts/engine/integration_test.go` — real Postgres, no mocks:
1. `POST /raise` → one FIRING row.
2. Same `(ruleId, targetKey)` again → same row, `duplicateCount=2`.
3. After ack, `POST /raise` → second row inserted.
4. `POST /resolve` → state becomes RESOLVED.
5. Dispatcher routing: critical-only channel ignores medium alert; empty-sourceType channel accepts all.
6. `alert_dispatch` rows written for both success and failure paths.

E2E test: `packages/nexus-hub/test/e2e/unified_alerting_test.go` using the public `testharness` package:
- Spawns compliance-proxy + Hub + mock Slack webhook (`httptest.Server`).
- Injects `hook_failure_rate` via test-only seam.
- Asserts: one FIRING row, Slack received one POST, `alert_dispatch` row with `success=true`, `GET /api/admin/alerts` returns the alert, ack → state=ACKNOWLEDGED.
- Resilience scenario: Hub down → proxy fires → spool has pending → Hub up → `thingclient.OnReconnect` triggers drain → Hub DB has the alert.

## Acceptance Criteria

1. `GET /api/admin/alerts` returns alerts from all source types (quota, proxy, thing, provider) in a single list with consistent shape.
2. `POST /api/v1/alerts/raise` with the same `(ruleId, targetKey)` twice while FIRING increments `duplicateCount` and does not re-dispatch channels.
3. `POST /api/v1/admin/alerts/:id/ack` transitions state to ACKNOWLEDGED; subsequent `POST /raise` for the same target creates a new FIRING row.
4. Channel with `severities: ["critical"]` does not receive a dispatch row for a `severity: "medium"` alert.
5. Channel with empty `severities` and empty `sourceTypes` receives dispatches for all alerts regardless of source type or severity.
6. `DELETE /api/admin/alerts/channels/:id` succeeds and returns 204; previously written `alert_dispatch` rows referencing the deleted channel remain intact (no cascading delete on channel).
7. `POST /api/admin/alerts/channels/:id/test` writes an `alert_dispatch` row and returns `{"success": true, "statusCode": ..., "dispatchId": ...}` when the channel sender succeeds. The synthetic alert does not appear in `GET /api/admin/alerts` (auto-resolved immediately).
8. Compliance-proxy `hook_failure_rate` evaluator fires → `shared/alertclient.Fire` POSTs to Hub → FIRING row exists in DB → Slack channel receives the POST (E2E scenario).
9. Hub unreachable → proxy fires → spool file written → Hub restarts → `thingclient.OnReconnect` triggers `ReplayPending` → Hub DB has the alert (E2E resilience scenario).
10. `GET /api/admin/alerts/rules` lists all 8 operator-visible seeded rules (the synthetic `system.channel_test` row is filtered out by the handler); `PUT /api/admin/alerts/rules/:id` with an invalid params body (schema violation) returns 400.
11. `POST /api/admin/alerts/rules/:id/reset` restores `params`, `cooldownSec`, `requiresAck`, `defaultSeverity`, and `displayName` to code-declared defaults.
12. Channel `config` response always masks `botToken`, `smtpPassword`, `routingKey`, and `Authorization`-class headers; PUT round-trip preserves the original secret.
13. UI: three-locale (en/zh/es) i18n key counts match; no stale `alertCenter.*` or `proxy.alertHistory.*` keys present.
14. Integration test suite passes under `go test -race -count=1 ./packages/nexus-hub/internal/alerts/engine/...`.
15. E2E suite passes under `go test -race -count=1 ./packages/nexus-hub/test/e2e/...`.

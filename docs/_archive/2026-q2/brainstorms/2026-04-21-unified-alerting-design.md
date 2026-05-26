# Unified Alerting Architecture — Design

- **Date:** 2026-04-21
- **Status:** Draft → ready for implementation plan
- **Scope:** Consolidate three parallel alert mechanisms (quota, proxy-runtime, Hub webhook evaluator) into one Hub-owned pipeline and set up rails for future alert sources (thing-offline, provider-unavailable, audit-anomaly).
- **Predecessor signal:** 404 on `GET /api/admin/proxy/alerts` — compliance-proxy `/alerts` was deleted in the runtimeapi-slimming refactor (`2026-04-20-runtimeapi-slimming-design.md` line 134) without the UI or CP BFF being cleaned up. This design supersedes that follow-up rather than restoring the dead path.

## 1. Context and Problem Statement

### 1.1 Today's fragmentation

Three unrelated alert mechanisms coexist, each with its own lifecycle, storage, and dispatch:

| System | Producer | Storage | Dispatch | Lifecycle |
| --- | --- | --- | --- | --- |
| Quota alerts | `QuotaAlertCheckJob` in Hub (cron, 1/min) | `quota_alert` PG table | None (UI-read only) | `active` → `acknowledged` → `resolved` (manual) |
| Proxy runtime alerts | `compliance-proxy` in-process evaluator (periodic) | In-memory ring buffer, max 100, lost on restart | Webhook/Slack/Email/PagerDuty via shadow `alert_channels` | `firing` → `resolved` (auto, when CheckFunc flips) |
| Hub webhook evaluator | `nexus-hub/internal/alerting/evaluator.go` | Stateless, cooldown map in memory | Single webhook URL | No lifecycle — fire-and-forget |

Plus **unaddressed signals** with no alert pipeline today:
- Stale Things (Thing `lastSeen > 5 min` — lives in Thing Registry, nobody alerts)
- `provider_health.status = 'degraded' | 'unhealthy'` (table exists, read by nothing)
- Audit anomalies (reserved by jobs but not wired)

### 1.2 Why unify

- **User-facing consistency:** operator sees one inbox, not three pages with three shapes. Today: `/alerts` (quota), `/proxy/alerts` (404 dead), `/compliance/alert-channels` + `/compliance/alert-rules` (proxy config). Post-migration: `/alerts` (inbox), `/alerts/rules`, `/alerts/channels`.
- **Rule/channel reuse:** quota and proxy alerts currently cannot share channels. Operators configure Slack in two places.
- **Deterministic storage:** proxy alerts vanish on restart; incident review is impossible. All alerts must be durable.
- **Rails for new producers:** once the pipeline exists, adding `thing.offline` or `provider.unavailable` is a handler change, not a new subsystem.

### 1.3 Scope (agreed in brainstorm)

**In scope:**
1. New Hub-owned alert tables + APIs.
2. Migrate the two existing concrete producers: `QuotaAlertCheckJob` and `compliance-proxy` evaluator.
3. Ship 2 new producers to prove rails are generic: `thing.offline` and `provider.unavailable`.
4. New unified UI: `/alerts` (inbox), `/alerts/rules`, `/alerts/channels`.
5. Extract `shared/spool` and `shared/alertclient` for reuse by data-plane services.

**Out of scope:**
- Historical data migration from `quota_alert` rows — per dev-phase policy we DROP the table.
- Deep-link from traffic/audit tab to "create alert from this event" affordances.

## 2. Architecture Decisions (locked)

Decisions reached through 7 rounds of brainstorm Q&A. Each drove a concrete section below.

| # | Decision | Value | Rationale |
| --- | --- | --- | --- |
| D1 | Scope shape | Build rails + migrate existing + 2 new producers | Rails unprovable without ≥2 non-quota real producers |
| D2 | Topology | Hub evaluates DB-readable checks; services raise via HTTP for in-process signals | Keep quota in Hub (already there, DB-local); keep hook-latency in proxy (data lives there) |
| D3 | Rule config | Identity in code (built-in rule types), knobs in DB (params JSON) — **one row per rule** | Operators edit thresholds without redeploy; code owns rule type surface |
| D4 | Lifecycle | Per-rule `requiresAck` flag; re-firing after ack creates new rows | Quota needs ack (persistence); proxy transient fires auto-resolve |
| D5 | Transport | HTTP POST + local disk spool (break-glass pattern) | Reuse proven break-glass pending-buffer; avoid NATS coupling for simple outbound signal |
| D6 | Dispatch | Channels route by severity × sourceType (both empty = all) | Operators want "PagerDuty critical-only" and "Slack for quota only" |
| D7 | UI | Single inbox + `/alerts/rules` + `/alerts/channels` under new top-level "Alerts" nav | Matches mental model: one place for "what just fired" |

## 3. Data Model

All in `tools/db-migrate/schema.prisma`; one migration `20260421_unified_alerting`.

### 3.1 `alert_rule`

One row per rule identity; `ruleType` matches a Go-registered evaluator; `params` holds user-editable knobs.

```prisma
model AlertRule {
  id              String        @id               // ruleId = rule-type identifier, e.g. "quota.threshold". Matches the evaluator registered in Go code.
  displayName     String
  sourceType      String                          // "quota" | "proxy" | "thing" | "provider" | "audit" | "system"
  defaultSeverity AlertSeverity
  requiresAck     Boolean       @default(false)
  enabled         Boolean       @default(true)
  params          Json                            // rule-type-specific knobs (thresholds, windows, exclusions)
  paramsSchema    Json                            // JSON schema describing params; used by UI generic fallback editor to render + validate
  cooldownSec     Int           @default(300)     // producer-side throttle — minimum seconds between two CheckFunc fires for the same (rule, target) while condition persists
  createdAt       DateTime      @default(now())
  updatedAt       DateTime      @updatedAt

  alerts Alert[]
  @@index([sourceType, enabled])
}
```

Per D3, rule identity lives in code — `id` string is both the primary key and the evaluator selector. No separate "ruleType" column; adding one would let DB and code disagree.

### 3.2 `alert`

One row per firing instance; re-firing after ack creates a new row, firing-without-ack updates `lastSeenAt` + `duplicateCount`.

```prisma
model Alert {
  id             String         @id @default(uuid())
  ruleId         String
  rule           AlertRule      @relation(fields: [ruleId], references: [id])
  sourceType     String                     // denormalized from rule for query speed
  targetKey      String                     // e.g. "orgId:xxx|period:2026-04" or "thing:agent-123" — producer's choice
  targetLabel    String                     // human-readable, for UI (denormalized at raise time)
  severity       AlertSeverity
  state          AlertState     @default(FIRING)   // FIRING | ACKNOWLEDGED | RESOLVED
  message        String                     // short summary for list
  details        Json                       // rule-specific payload (thresholds, counts, sample evidence)
  firedAt        DateTime
  lastSeenAt     DateTime                   // updated on subsequent fires while still FIRING
  duplicateCount Int            @default(1)
  acknowledgedBy String?
  acknowledgedAt DateTime?
  resolvedAt     DateTime?
  resolvedBy     String?                    // nullable — "system" for auto-resolve
  resolvedReason String?                    // "auto" | "manual" | "rule-disabled"

  dispatches AlertDispatch[]
  @@index([state, sourceType, firedAt])
  @@index([ruleId, targetKey, state])       // dedupe lookup on raise
  @@index([firedAt])
}

enum AlertSeverity { CRITICAL HIGH MEDIUM LOW INFO }
enum AlertState    { FIRING ACKNOWLEDGED RESOLVED }
```

### 3.3 `alert_channel`

Moved from Hub shadow config into Hub DB — channels are Hub-local, never pushed to data-plane services.

```prisma
model AlertChannel {
  id          String   @id @default(uuid())
  name        String
  type        String                       // "webhook" | "slack" | "email" | "pagerduty"
  enabled     Boolean  @default(true)
  severities  String[]                     // empty = all; otherwise subset of AlertSeverity values lowercased
  sourceTypes String[]                     // empty = all
  config      Json                         // type-specific; secrets masked in API responses
  createdAt   DateTime @default(now())
  updatedAt   DateTime @updatedAt
}
```

### 3.4 `alert_dispatch`

Audit of every send attempt; per (alert, channel).

```prisma
model AlertDispatch {
  id         String       @id @default(uuid())
  alertId    String
  alert      Alert        @relation(fields: [alertId], references: [id], onDelete: Cascade)
  channelId  String                            // no FK — channels can be deleted but audit stays
  channelName String                           // denormalized
  success    Boolean
  statusCode Int?
  errorMsg   String?
  attemptedAt DateTime    @default(now())

  @@index([alertId])
  @@index([attemptedAt])
}
```

### 3.5 What gets deleted

- `model QuotaAlert` — DROP; no data migration.
- Shadow config keys `alert_channels`, `alert_thresholds`, `alert_custom_checks` — deleted from `shared/configtypes` and Hub config emitter. Channels live in DB now; thresholds live in `AlertRule.params`.

## 4. Execution Flow

### 4.1 DB-readable producers (Hub-evaluated)

Example: quota threshold check.

```
Hub scheduler (cron 1/min)
  → runs built-in evaluator "quota.threshold"
  → for each QuotaOverride / QuotaPolicy row: compute usage%
  → if crosses threshold: alerting.Raise(ctx, "quota.threshold", targetKey, sev, msg, details)
  → if back below threshold (and FIRING row exists): alerting.Resolve(ctx, "quota.threshold", targetKey, reason="auto")
```

Also registered: `quota.vk_expiring` (30/15/7/1 days out), `thing.offline` (lastSeen > 5 min), `provider.unavailable` (ProviderHealth.status in unhealthy for N min).

### 4.2 In-process producers (data-plane-raised)

Example: compliance-proxy hook failure rate.

```
compliance-proxy in-process evaluator (periodic)
  → CheckFunc observes hook_failure_rate > threshold for window
  → alertclient.Fire(ctx, AlertEnvelope{ruleId:"proxy.hook_failure_rate", targetKey:"proxy:node-A", sev, msg, details})
     → POST http://hub:8080/api/v1/alerts/raise with bearer token (Thing cert auth)
     → 5s timeout
     → on success: done
     → on error/5xx: write envelope to local spool, return nil (non-blocking)
  → CheckFunc flips back (below threshold): alertclient.Resolve(...)
```

Spool drain triggers:
1. `thingclient.OnReconnect` callback (reuses break-glass hook-in point).
2. Periodic ticker (default 30s).
3. Explicit `ReplayPending` call from readyz handler or manual.

Spool guarantees: append-only file per envelope, fsync on enqueue, crash-safe drain (envelope deleted only after HTTP 2xx), max size 50MB (oldest evicted + counter metric), corrupted envelopes skipped with warning.

### 4.3 Raise semantics (Hub-side)

Given `Raise(ctx, ruleId, targetKey, sev, msg, details)`:

1. `SELECT ... FOR UPDATE` existing row `WHERE ruleId=? AND targetKey=? AND state IN (FIRING, ACKNOWLEDGED)` ordered by firedAt DESC LIMIT 1.
2. Dispatch on result:
   - **No row** OR **last row state = RESOLVED**: `INSERT` new row, state=FIRING, duplicateCount=1. Dispatch to channels.
   - **Last row state = FIRING**: `UPDATE` lastSeenAt=NOW(), duplicateCount++. **Do not** re-dispatch — the existing FIRING row already notified operators; dedupe happens here, not in the dispatcher.
   - **Last row state = ACKNOWLEDGED**: `INSERT` new row (per D4 — re-firing after ack creates fresh incident). Dispatch.
3. Entire operation wrapped in one transaction to prevent duplicate inserts under concurrent raise.

Note on cooldown: `AlertRule.cooldownSec` is a **producer-side** throttle (how often a periodic evaluator re-fires `Raise` for the same target while the condition persists). It does **not** apply at the dispatcher, which sees only successful INSERTs and dispatches each once.

### 4.4 Resolve semantics (Hub-side)

Given `Resolve(ctx, ruleId, targetKey, reason)`:

1. `UPDATE alert SET state=RESOLVED, resolvedAt=NOW(), resolvedBy='system', resolvedReason=? WHERE ruleId=? AND targetKey=? AND state IN (FIRING, ACKNOWLEDGED)`.
2. No dispatch on resolve (v1); future knob on rule can opt-in "notify on resolve".

### 4.5 Dispatcher

After a new row is inserted via Raise:

```
dispatcher.Dispatch(alert):
  channels = SELECT * FROM alert_channel WHERE enabled=true
  for each channel:
    if channel.severities not empty AND alert.severity not in channel.severities: skip
    if channel.sourceTypes not empty AND alert.sourceType not in channel.sourceTypes: skip
    sender = senderRegistry[channel.type]
    err = sender.Send(ctx, alert, channel.config)
    INSERT alert_dispatch(alertId, channelId, success, statusCode, errorMsg)
```

Senders registered in `nexus-hub/internal/alerting/senders/`: `webhook`, `slack`, `email`, `pagerduty`. Each is a `Sender` interface impl with 10s timeout.

## 5. HTTP Surfaces

### 5.1 Hub internal (service-to-service, mTLS + Thing cert)

| Method | Path | Purpose | Caller |
| --- | --- | --- | --- |
| POST | `/api/v1/alerts/raise` | Raise one alert envelope | `alertclient` from data-plane |
| POST | `/api/v1/alerts/resolve` | Resolve matching FIRING/ACKED alerts | `alertclient` |

Request envelope:
```json
{
  "ruleId": "proxy.hook_failure_rate",
  "targetKey": "proxy:node-A|rule:external-pii-scan",
  "targetLabel": "Node A — External PII Scan hook",
  "severity": "high",
  "message": "42% of hook invocations failed in last 5m",
  "details": { "failures": 42, "total": 100, "windowSec": 300 }
}
```

### 5.2 Hub admin (UI-facing via CP BFF)

All routed through `nexus-hub/internal/alerting/admin_handlers.go`; CP pass-through in `packages/control-plane/internal/handler/admin_alerts.go`.

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/admin/alerts` | List (filters: state, severity, sourceType, ruleId, targetSearch, timeRange) + pagination |
| GET | `/api/admin/alerts/:id` | Detail incl. dispatch history |
| POST | `/api/admin/alerts/:id/ack` | Acknowledge; body `{ reason? }` |
| POST | `/api/admin/alerts/:id/resolve` | Manual resolve; body `{ reason? }` |
| GET | `/api/admin/alerts/rules` | List rules |
| GET | `/api/admin/alerts/rules/:id` | Rule detail (incl. paramsSchema) |
| PUT | `/api/admin/alerts/rules/:id` | Update `enabled`, `params`, `cooldownSec`, `requiresAck`, `defaultSeverity` |
| POST | `/api/admin/alerts/rules/:id/reset` | Reset `params` to code-declared defaults |
| GET | `/api/admin/alerts/channels` | List channels (secrets masked) |
| POST | `/api/admin/alerts/channels` | Create |
| GET | `/api/admin/alerts/channels/:id` | Detail (secrets masked) |
| PUT | `/api/admin/alerts/channels/:id` | Update |
| DELETE | `/api/admin/alerts/channels/:id` | Delete |
| POST | `/api/admin/alerts/channels/:id/test` | Send synthetic `system.channel_test` event; does **not** write `alert` row; logs to `alert_dispatch` |

### 5.3 IAM

Two new actions in seed:
- `admin:ReadAlerts` — bound to `super_admin`, `admin`, `compliance_admin`, `viewer`
- `admin:ManageAlerts` — bound to `super_admin`, `compliance_admin`

Delete: `admin:ReadQuotaAlerts`, `admin:ManageQuotaAlerts` (if present in seed). Delete: proxy alert-rule/channel actions scoped to `admin:ManageCompliance`.

## 6. UI

### 6.1 Routes and nav

**New top-level nav section** `alerts`, replacing the scattered entries:

```
Alerts  (new section)
  └─ Inbox         → /alerts
  └─ Rules         → /alerts/rules
  └─ Channels      → /alerts/channels
```

Deleted routes: `/proxy/alerts`, `/compliance/alerts/channels*`, `/compliance/alerts/rules*`. Deleted nav entries: `proxy.alerts`, `compliance.alertChannels`, `compliance.alertRules`.

Route config changes in `packages/control-plane-ui/src/routes/shellRouteConfig.tsx`:
```
- path: 'proxy/alerts', sectionKey: 'forwardProxy', element: LazyAlertHistoryPage
- path: 'alerts', sectionKey: 'overview', element: LazyAlertCenter
+ section: 'alerts'
+   path: 'alerts',             element: LazyAlertListPage
+   path: 'alerts/rules',       element: LazyAlertRulesListPage
+   path: 'alerts/rules/:id',   element: LazyAlertRuleEditPage
+   path: 'alerts/channels',    element: LazyAlertChannelsListPage
+   path: 'alerts/channels/new',      element: LazyAlertChannelEditPage
+   path: 'alerts/channels/:id/edit', element: LazyAlertChannelEditPage
```

### 6.2 `/alerts` Inbox

- `DataTable` columns: State • Severity • SourceType (chip) • Rule • Target • FiredAt • Actions (Ack / Resolve).
- Filters row: State (Firing/Acked/Resolved/All), Severity multi-select, SourceType multi-select, RuleID search, Target text search, Time range.
- Row click opens right-side `AlertDetailDrawer`.
- 15s auto-refresh (same pattern as current `AlertHistoryPage`).
- Empty state per filter combination.
- `queryKey: ['admin', 'alerts', 'inbox', state, severities, sourceTypes, ruleId, targetSearch, fromIso, toIso, offset, limit]`.

### 6.3 `/alerts/rules`

- List view: RuleID • Display name • SourceType • Severity • Enabled toggle • RequiresAck badge • Edit.
- Per-rule edit page `/alerts/rules/:id`:
  - Knobs: `enabled`, `defaultSeverity`, `requiresAck`, `cooldownSec`.
  - `params` edited via **per-rule React editor** registered in a map: `ruleEditors: Record<string, React.FC<RuleEditorProps>>`. v1 ships editors for `quota.threshold`, `quota.vk_expiring`, `proxy.hook_failure_rate`, `proxy.hook_timeout_rate`. All other rules (including `proxy.high_error_rate`, `proxy.cost_spike`, `thing.offline`, `provider.unavailable`, `system.channel_test`) fall back to the **generic JSON editor** which renders form fields from `paramsSchema` and validates client-side. Editor and detail-renderer coverage are intentionally not one-to-one: rules with simple numeric knobs (error rate, cost threshold) get no dedicated editor because the generic one suffices, but may still warrant a dedicated detail renderer because their firing evidence is non-trivial.
- Reset button → `POST /rules/:id/reset`.
- `queryKey: ['admin', 'alerts', 'rules', 'list']` and `['admin', 'alerts', 'rules', 'detail', id]`.

### 6.4 `/alerts/channels`

- List view, CRUD buttons. Secrets column shows masked preview (e.g. `xoxb-••••••••abc123`).
- Add/Edit page: Name • Type • Enabled • Severities (multi-select) • SourceTypes (multi-select, empty = all) • Type-specific config fields.
- `Test send` button per row → calls `POST /:id/test` with a synthetic alert; result toast shows success or sender error.
- `queryKey: ['admin', 'alerts', 'channels', 'list']`.

### 6.5 Detail drawer

`AlertDetailDrawer` shows:
- Header: rule name, severity chip, state, fired/resolved timestamps.
- **Per-rule body renderer** from `detailRenderers: Record<string, React.FC<{alert: Alert}>>`. v1 ships renderers for `quota.threshold` (usage bar), `quota.vk_expiring` (key list), `proxy.hook_failure_rate` (failure count + sample error), `proxy.high_error_rate`. Fallback renders `details` as generic key-value list.
- Dispatch history table: Channel • Success • Status • ErrorMsg • AttemptedAt.
- Actions: Acknowledge (if FIRING), Resolve (if not RESOLVED), Copy as JSON.

### 6.6 i18n

**Delete entire sections** (all three locales: en/zh/es):
- `pages:alertCenter.*`
- `pages:proxy.alertHistory.*`
- `pages:compliance.alertChannels.*`
- `pages:compliance.alertRules.*`
- `nav:*` entries for removed routes

**Add** (all three locales, key-count parity enforced):
- `pages:alerts.inbox.*`
- `pages:alerts.rules.*`
- `pages:alerts.detail.*`
- `pages:alerts.channels.*`
- `pages:alerts.ruleEditors.<ruleId>.*` (per ruleId)
- `pages:alerts.detailRenderers.<ruleId>.*` (per ruleId)
- `nav:alerts.*`

Technical terms kept English across locales per project convention.

## 7. Shared Packages

### 7.1 `packages/shared/spool/` (new)

Factored out of `packages/compliance-proxy/internal/runtimeapi/bgspool.go`. Generic over item type.

```go
type Spool[T any] struct { /* dir, name, max bytes, mu, logger */ }

func NewSpool[T any](dir, name string, maxBytes int64, logger *slog.Logger) (*Spool[T], error)
func (s *Spool[T]) Enqueue(item T) error
func (s *Spool[T]) Drain(ctx context.Context, send func(T) error) (drained int, err error)
func (s *Spool[T]) PendingCount() int
```

Guarantees: append-only file-per-envelope, fsync on enqueue, LRU eviction at cap, corrupt envelopes skipped with warn log and counter, crash-safe (envelope deleted only after `send` returns nil).

Break-glass refactored to `Spool[BreakGlassReport]` in the same PR as extraction — no parallel copy of the logic.

### 7.2 `packages/shared/alertclient/` (new)

All data-plane services (ai-gateway / compliance-proxy / agent) use this:

```go
type Client struct { /* hub URL, auth, spool, logger, metrics */ }

func NewClient(cfg Config, logger *slog.Logger) (*Client, error)
func (c *Client) Fire(ctx context.Context, env AlertEnvelope) error
func (c *Client) Resolve(ctx context.Context, ruleID, targetKey, reason string) error
func (c *Client) ReplayPending(ctx context.Context) (drained int, err error)
func (c *Client) PendingCount() int
```

Wired to `thingclient.OnReconnect` (drain on resume) and a 30s ticker.

Metrics per service namespace:
- `*_alertclient_pending_alerts` (gauge)
- `*_alertclient_fire_total{result=success|spool|drop}` (counter)
- `*_alertclient_replay_total{result=success|fail}` (counter)

## 8. Producer Migration

### 8.1 QuotaAlertCheckJob (Hub)

File: `packages/nexus-hub/internal/jobs/quota_alert_check.go`.

- **Replace**: `UPSERT quota_alert` → `alerting.Raise(ctx, "quota.threshold", targetKey, sev, msg, details)`.
- **Add**: second pass — for every currently-FIRING quota.threshold alert, recompute usage; if back below threshold → `alerting.Resolve(..., reason="auto")`.
- `targetKey` format: `org:{orgId}|period:{yyyy-mm}` or `vk:{vkId}|period:{yyyy-mm}` depending on policy scope.
- `vk_expiring` check split into its own registered rule `quota.vk_expiring` with 30/15/7/1 day thresholds from `params`.

### 8.2 compliance-proxy evaluator

File: `packages/compliance-proxy/internal/alerting/evaluator.go`.

- **Keep**: CheckFunc registration mechanism, cooldown, periodic scheduler.
- **Replace**: local dispatcher + channels → `alertclient.Fire` / `alertclient.Resolve`.
- **Delete**: `channels.go`, `channel_senders.go`, `rebuild_test.go`, `channels_test.go`, in-memory ring buffer (`maxHistory`).
- Existing CheckFunc names become stable `ruleId`s: `proxy.hook_failure_rate`, `proxy.hook_timeout_rate`, `proxy.high_error_rate`, `proxy.cost_spike`.
- `targetKey` format: `proxy:{nodeID}` for node-wide, `proxy:{nodeID}|hook:{hookID}` for per-hook.

### 8.3 New producers

- `thing.offline` — Hub periodic job scanning `thing.lastSeenAt < now - 5m AND state='online'`; raises per thing. Auto-resolves when `lastSeenAt` within window again.
- `provider.unavailable` — Hub watcher on `provider_health`; raises when `status IN ('unhealthy','degraded')` for ≥ configurable seconds (default 120); auto-resolves when `status='healthy'` for ≥60s (flap protection).

## 9. Testing Strategy

### 9.1 Unit

| Package | Coverage focus |
| --- | --- |
| `shared/spool` | Enqueue/Drain/crash recovery; LRU at cap; corrupt envelope skip |
| `shared/alertclient` | Success → immediate return; 5xx → spool; reconnect → drain success/fail; metrics |
| `nexus-hub/internal/alerting/raiser` | New row when no match; UPDATE on FIRING match; INSERT new when last=ACKED; concurrent Raise is transactional |
| `nexus-hub/internal/alerting/resolver` | Updates only FIRING/ACKED; no-op on RESOLVED; sets resolvedBy=system on auto |
| `nexus-hub/internal/alerting/dispatcher` | Severity filter; sourceType filter; empty = all; dispatch failure writes `alert_dispatch` row with error |
| `nexus-hub/internal/alerting/senders/*` | Each sender: request format, secret header, timeout, error wrap (use `httptest.Server`) |
| `nexus-hub/internal/jobs/quota_alert_check` | Threshold crossing → Raise; usage drop → Resolve; multi-target independence |
| `compliance-proxy/internal/alerting/evaluator` | CheckFunc firing → `alertclient.Fire` called; cooldown respected |
| `control-plane/internal/handler/admin_alerts` | BFF forwarding; IAM denial |
| `control-plane-ui/src/pages/alerts/AlertListPage.test.tsx` | Filter interactions; ack button; queryKey shape |

### 9.2 Integration (Hub, real Postgres)

`packages/nexus-hub/internal/alerts/engine/integration_test.go`:
1. `POST /raise` → one FIRING row.
2. Same `(ruleId, targetKey)` again → same row with `duplicateCount=2`, `lastSeenAt` advanced.
3. After ack, `POST /raise` → second row.
4. `POST /resolve` → row becomes RESOLVED.
5. Dispatcher routing: critical-only channel ignores medium; empty-sourceType channel takes all; disabled channel skipped.
6. `alert_dispatch` has rows for both success and failure paths.

### 9.3 End-to-end

New harness under the existing Hub e2e convention (`packages/nexus-hub/tests/e2e/unified-alerting/`, promoting the `testharness` public package already used by break-glass outage-recovery tests):
- Spawn compliance-proxy + Hub + mock SMTP + mock Slack webhook (`httptest.Server`).
- Inject hook_failure_rate via test-only seam.
- Assert: one FIRING row, Slack got one POST, `alert_dispatch` row success, `GET /api/admin/alerts` shows it, ack → state=ACKNOWLEDGED.
- **Resilience scenario** (non-negotiable): Hub down → proxy fires → spool has pending → Hub up → `thingclient.OnReconnect` triggers drain → Hub DB has the alert.

## 10. Rollout Order

Dev-phase policy: "Phase" = order of work, not compatibility layers. No feature flags, no dual paths. Single merged migration, single merged deletion.

| Phase | Story | Title | Dep |
| --- | --- | --- | --- |
| A | S1 | Prisma schema: DROP `quota_alert`, CREATE 4 new tables, seed rules | — |
| A | S2 | Extract `shared/spool`; rewrite break-glass on top of it | — |
| A | S3 | `shared/alertclient` package | S2 |
| B | S4 | Hub raiser + resolver | S1 |
| B | S5 | Hub channel senders (webhook/slack/email/pagerduty) | S1 |
| B | S6 | Hub dispatcher (severity × sourceType routing) | S4, S5 |
| B | S7 | Hub `/api/v1/alerts/raise` + `/resolve` handlers | S4 |
| B | S8 | Hub admin `/api/v1/admin/alerts/*` handlers + IAM | S4, S6 |
| C | S9 | Migrate QuotaAlertCheckJob to Raise/Resolve; drop quota_alert writes | S4 |
| C | S10 | compliance-proxy evaluator → alertclient; delete channels.go, senders, ring buffer | S3, S7 |
| C | S11 | New producer: `thing.offline` (Hub job) | S4 |
| C | S12 | New producer: `provider.unavailable` (Hub watcher) | S4 |
| D | S13 | CP `admin_alerts` BFF handlers + IAM; delete `admin_quota_alerts` + `admin_compliance_alert_*` | S8 |
| D | S14 | UI `AlertListPage` + API service + i18n | S13 |
| D | S15 | UI `AlertRulesListPage` + `AlertRuleEditPage` + 4 rule editors + generic JSON fallback + i18n | S13 |
| D | S16 | UI `AlertChannelsPage` (CRUD + test-send) + i18n | S13 |
| D | S17 | UI `AlertDetailDrawer` + 4 detail renderers + fallback + route config + nav restructure + i18n deletions | S14, S15, S16 |
| E | S18 | Delete dead handlers, shadow config keys, old i18n sections; final cleanup sweep | All prior |
| E | S19 | E2E tests; three-language key-count parity check; manual UI smoke | S17 |
| E | S20 | OpenAPI spec + SDD `docs/sdd/` + runbook updates | S17 |

Total: 20 stories. Phase A+B stand up rails (~3 days). Phase C migrates producers (~1 day). Phase D rebuilds UI (~2 days). Phase E cleans up (~0.5 day).

## 11. Risks and Mitigations

| Risk | Likelihood | Impact | Mitigation |
| --- | --- | --- | --- |
| Hub down loses in-flight proxy alerts | Medium | High | `shared/spool` durability + replay; E2E resilience test is a gate |
| Channel misconfig spams operators | Medium | Medium | Per-rule `cooldownSec`; per-channel severity/sourceType filter; "test send" button before save |
| Params schema drift between code and DB | Low | Medium | `paramsSchema` served from code-declared source of truth; UI validates before PUT |
| Re-fire after ack creates alert fatigue | Medium | Low | Only rules with `requiresAck=true` behave this way; most proxy rules auto-resolve |
| Spool file corruption persists forever | Low | Low | Corrupt envelope skipped + counter metric; file-per-envelope limits blast radius |
| Concurrent Raise creates duplicate rows | Low | Medium | Transaction with `SELECT ... FOR UPDATE` on dedupe lookup |
| Migration drops quota_alert rows in use | — | — | Pre-GA, no users — per explicit dev-phase policy |

## 12. Open Questions

None — all brainstorm questions resolved. Any new decisions required during implementation should be raised per project "Ask the user when confirmation is needed" rule.

## 13. References

- `docs/superpowers/specs/2026-04-20-runtimeapi-slimming-design.md` — origin of the 404
- `docs/dev/thing-model.md` — Thing Registry + Shadow terminology
- `docs/dev/service-call-framework.md` — Hub-centric architecture
- `packages/compliance-proxy/internal/runtimeapi/bgspool.go` — spool pattern being extracted
- `tools/db-migrate/schema.prisma` — data model changes land here

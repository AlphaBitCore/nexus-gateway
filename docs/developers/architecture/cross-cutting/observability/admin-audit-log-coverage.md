# Admin audit log coverage

## 1. Scope

The admin audit log captures every state-mutating action performed by an administrator (or a Hub-initiated internal control action) against the Control Plane. One audit row per mutation; the row records who acted, what the action was, which entity was touched, the prior and resulting state, and a tamper-evident hash linking it to the row before it.

This document covers only the **admin audit log** — the `AdminAuditLog` table fed by `audit.Writer` from `packages/control-plane/internal/platform/audit/`. LLM request/response capture rides a separate pipeline (`traffic_event`) documented in `audit-pipeline-architecture.md`.

The IAM resource/verb catalog in `packages/shared/identity/iam/` is the single source of truth for both the IAM gate (`iamMW`) and the audit `EntityType` / `Action` strings — both derive from the same `(ResourceDef, Verb)` pair, so a route that gates on `admin:virtual-key.create` emits an audit row with `EntityType="virtual-key"`, `Action="create"`.

## 2. Anchor packages

- `packages/control-plane/internal/platform/audit/` — writer, helper, fail-observer wiring.
- `packages/shared/identity/iam/` — canonical resource/verb taxonomy.
- `packages/shared/transport/mq/` — MQ wire format.
- `packages/nexus-hub/internal/observability/consumer/` — Hub-side batched DB writer.
- `packages/nexus-hub/internal/fleet/manager/` — Hub-side direct in-transaction writer (override mutations).
- `packages/nexus-hub/internal/traffic/chain/` — hash-chain helper, shared between both writer paths.
- `tools/db-migrate/schema.prisma` — `AdminAuditLog` model.

## 3. Entry struct

`audit.Entry` is the value handlers populate. It contains:

| Field | Type | Source |
|---|---|---|
| `ActorID` | string | resolved admin session key id (set by `EntryFor` from `middleware.AdminAuthFromContext`) |
| `ActorLabel` | string | admin display name (same source as ActorID) |
| `ActorRole` | string | optional role label |
| `SourceIP` | string | `echo.Context.RealIP()` |
| `Action` | string | the bare verb (e.g. `"create"`, `"approve"`) |
| `EntityType` | string | the canonical resource name (e.g. `"virtual-key"`) |
| `EntityID` | string | the mutated row's id |
| `BeforeState` | any | optional pre-mutation snapshot — marshalled to JSONB |
| `AfterState` | any | optional post-mutation snapshot — marshalled to JSONB |
| `NexusRequestID` | string | per-request id from `middleware.NexusRequestIDFromContext` |

The Hub-side row also carries `previousHash`, `integrityHash`, `hashInput`, and a Postgres-side autoincrement `sequenceNumber`. Those columns are not part of the Entry struct — they are computed by the Hub writers under `pg_advisory_xact_lock` and never leave the wire as plaintext fields.

### Construction rule (binding)

`audit.EntryFor(c, resource, verb)` is the only allowed constructor for an Entry that carries `(EntityType, Action)`. It panics at server start if `verb` is not declared on `resource` in `iam.Catalog`. A CI consistency test in `packages/control-plane/internal/identity/iam/catalog_consistency_test.go` fails the build if any handler assigns `.Action =` or `.EntityType =` as a free-form string literal.

Two intentional exceptions construct `audit.Entry{...}` directly:

- `packages/control-plane/internal/identity/users/handler/auth_sessions.go` — `RevokeDeviceInternal` is called by Hub on device-unenroll. Actor is synthetic (`ActorID="internal"`, `ActorLabel="nexus-hub"`) so the EchoContext-driven `EntryFor` cannot supply real admin attribution. The consistency test allow-lists this filename by name; the struct literal still binds `EntityType=iam.ResourceNexusSession.Name` and `Action=iam.VerbRevoke` so the canonical invariant holds.
- `packages/control-plane/internal/identity/authserver/login/password.go` — login success/failure emits `Action="admin.login.succeeded"` / `Action="admin.login.failed"`. These are deliberately non-canonical bare strings (no `admin:<resource>.<verb>` shape) because there is no IAM-gated resource for a login attempt — the failure-rate alert rule consumes the bare action string. Both the literal-shape scan (`ae.Action = "…"` post-construction assignment) and the canonical-shape scan (`"admin:…"` colon-form literal) miss this file by design, so no allow-list entry is needed.

## 4. Persistence — `AdminAuditLog` table

Schema in `tools/db-migrate/schema.prisma`:

| Column | Type | Notes |
|---|---|---|
| `id` | uuid | primary key |
| `sequenceNumber` | int autoincrement | dense ordering for chain-verify pagination |
| `timestamp` | Timestamptz(3) | UTC; hashed value matches the `timestamp` column exactly |
| `actorId` | string | |
| `actorLabel` | string | |
| `actorRole` | string? | |
| `sourceIp` | string? | |
| `action` | string | bare verb (`create`, `approve`, `toggle`, …) |
| `entityType` | string | catalog resource name (`virtual-key`, `hook`, …) |
| `entityId` | string? | nullable for actions that do not target a single row (e.g. `hook` bulk reorder) |
| `beforeState` | Json? | |
| `afterState` | Json? | |
| `nexusRequestId` | string? | matches access logs / `x-nexus-request-id` response header |
| `clientRequestId` / `clientUserId` / `clientSessionId` | string? | optional admin-client correlation fields; `EntryFor` does not populate them, so they are NULL on rows produced by the canonical path |
| `previousHash` | string? | NULL only on the genesis row |
| `integrityHash` | string | NOT NULL — SHA-256 over the canonical hashInput |
| `hashInput` | bytes | exact bytes that were hashed; persisted so `VerifyChain` can recompute without depending on JSONB roundtrip stability |

Indexes on `timestamp`, `actorId`, `entityType`, `nexusRequestId`, `clientRequestId`, `sequenceNumber`.

The hash chain is the cryptographic spine: each row's `previousHash` equals the prior row's `integrityHash`, and the prior row's `integrityHash` is computed under a `pg_advisory_xact_lock` shared between the two writer paths so the chain cannot fork even when both writers commit concurrently.

## 5. Emit pattern

There are three places an `AdminAuditLog` row originates from:

### 5.1 CP handlers → MQ → Hub batch writer (the common path)

The handler builds an Entry with `EntryFor`, populates `EntityID`, optionally `BeforeState` and `AfterState`, then calls `Writer.LogObserved(ctx, entry)`:

```go
ae := audit.EntryFor(c, iam.ResourceVirtualKey, iam.VerbCreate)
ae.EntityID = vk.Name
ae.AfterState = map[string]any{"projectId": vk.ProjectID, "scope": vk.Scope}
h.audit.LogObserved(c.Request().Context(), ae)
```

`Writer.Log` marshals the Entry into an `mq.AdminAuditMessage`, generates a UUID, stamps a UTC timestamp, and enqueues to the `nexus.event.admin-audit` queue with a 5s detached-context timeout (so a client disconnect cannot cancel the publish). `LogObserved` swallows the returned error — failures are still surfaced via the warn-log + `FailureObserver` Prometheus counter wired in `cmd/control-plane/wiring/audit.go`. The handler does not block on MQ; the upstream mutation has already committed.

`AdminAuditWriter` in `packages/nexus-hub/internal/observability/consumer/admin_audit.go` consumes the queue under consumer group `hub-db-writer`. It batches (default 100 messages or 5s) and runs the batch inside a single Postgres transaction. For every message:

1. Marshal `BeforeState` / `AfterState` to JSONB; on marshal error the row is still inserted with NULL state and a warn log + per-action metric — never lose the chain.
2. Acquire the chain advisory lock (`pg_advisory_xact_lock`) on a stable int64 key.
3. Read the prior row's `integrityHash`, compute the new `previousHash` / `integrityHash` / `hashInput` via `chain.NextHash`.
4. `INSERT INTO "AdminAuditLog" … ON CONFLICT (id) DO NOTHING` — id is the message UUID, so a retried MQ delivery is idempotent.

On any failure (Begin / Insert / Commit), the entire batch is NAK'd back to the queue and the failure breaks out by stage in the `mq.admin_errors_total` counter (`db_begin` / `db_insert` / `db_commit`).

### 5.2 Hub direct in-transaction writer (override path)

`packages/control-plane/internal/infrastructure/infra/thing_overrides.go` forwards override mutations (`PUT/DELETE /api/admin/nodes/:id/overrides/:configKey`) straight to Hub via `hubForward`. The contract for this path is explicit: CP MUST NOT call `Writer.Log` here. Hub writes both the override row and the audit row inside the same transaction via `insertAdminAuditLog` in `packages/nexus-hub/internal/fleet/manager/override.go`, sharing the same advisory lock and `chain.NextHash` helper as the batch writer. Two parallel writes on the same mutation would break the "exactly one audit row per admin mutation" invariant.

### 5.3 Hub direct in-transaction writer (device-assignment)

`packages/nexus-hub/internal/identity/store/enrollstore/device_assignment_audit.go` provides `WriteDeviceAssignmentAudit`, called from the agent SSO enrollment handler in `packages/nexus-hub/internal/identity/handler/enroll/enrollment_handler.go` after a device row is bound to a user. It opens its own transaction, takes the same advisory lock as the batch writer, runs `chain.NextHash`, and inserts an `AdminAuditLog` row with `EntityType="device-assignment"`, `Action="device-assignment.update"`, synthetic actor (`internal` / `nexus-hub`), and the prior/new `userId` in `BeforeState` / `AfterState`. Mirrors §5.2 (override path): the binding is owned by Hub, the audit row must commit atomically with the binding, and CP MUST NOT emit a parallel row.

### 5.4 Direct `audit.Entry{}` literal

The two sites described in §3 — `RevokeDeviceInternal` and the password login emitter — produce the same wire format as `EntryFor` but skip the EchoContext-driven actor lookup.

## 6. Coverage map

Resources are listed by service. The "Audited verbs" column lists the verbs that actually produce an audit row on the live server; "Lifecycle/write verbs without emit" surfaces gaps relative to the catalog. Read-only resources (no write verbs declared in the catalog) are listed once for completeness; they are intentionally not audited.

### 6.1 Gateway service

| Resource | Catalog verbs | Audited verbs | Lifecycle/write verbs without emit |
|---|---|---|---|
| `provider` | C/R/U/D | C, U, D | — |
| `model` | C/R/U/D | C, U, D | — |
| `model-pricing` | R/C/D | C, D | — |
| `credential` | C/R/U/D + probe + rotate | C, U, D, probe, rotate | — |
| `virtual-key` | C/R/U/D + approve/reject/revoke/renew | C, U, D, approve, reject, revoke, renew | — |
| `routing-rule` | C/R/U/D + simulate | C, U, D | `simulate` (read-only what-if; explicitly not audited) |
| `quota-policy` | C/R/U/D | C, U, D | — |
| `quota-override` | C/R/U/D | C, U, D | — |
| `quota-analytics`, `analytics`, `traffic-log` | R only | — | — (read-only) |
| `prompt-cache` | R/U | U | — |
| `semantic-cache` | R/U | U | — |
| `extract-cache` | R/U | U | — |
| `passthrough` | R / write / emergency-enable | write, emergency-enable | — (`packages/control-plane/internal/governance/passthrough/handler/handler.go` emits via `emitAudit` from all five mutation sites — global/adapter/provider × write+emergency-enable; `EntityID` is the tier identifier `global` / `<adapter>` / `<provider>` and `BeforeState` / `AfterState` carry the prior and new `enabled` flag plus truncated reason/ticket) |

### 6.2 Compliance service

| Resource | Catalog verbs | Audited verbs | Lifecycle/write verbs without emit |
|---|---|---|---|
| `hook` | C/R/U/D | C, U, D | — |
| `rule-pack` | C/R/U/D + import | C, U, D, import | — |
| `compliance-exemption` | C/R/U/D + reject | C, U, D, reject | — |
| `compliance-report` | R only | — | — (read-only) |
| `interception-domain` | C/R/U/D | C, U, D | — |
| `dsar` | C/R/U/D + fulfill | C, U, fulfill | — |
| `payload-capture` | R/U | U (audited as `settings.update`) | — |
| `ai-guard-config` | R/U | U | — |
| `kill-switch` | R + toggle | toggle | — |

### 6.3 Agent service

| Resource | Catalog verbs | Audited verbs | Lifecycle/write verbs without emit |
|---|---|---|---|
| `agent-device` | C/R/U/D + force-resync + rotate | U, force-resync, rotate | C, D — no admin handler routes (devices auto-enroll via `/api/agent/sso-enroll`) |
| `device-group` | C/R/U/D | C, U, D | — |
| `device-assignment` | U only | update (Hub-side direct writer — see §5.3) | — |
| `device-defaults` | R/U | U (audited as `settings.update`) | — |
| `agent-attestation` | R/U | U (audited as `settings.update`) | — |

### 6.4 Platform service

| Resource | Catalog verbs | Audited verbs | Lifecycle/write verbs without emit |
|---|---|---|---|
| `alert` | C/R/U/D + acknowledge | C, U, D, acknowledge | — (single dispatcher in `observability/alerts/handler/handler.go` runs the emit on any 2xx Hub response, carrying the verb in `AfterState.subEntity`) |
| `observability` | R + write | write | — |
| `observability-dlq` | R + manage | manage | — (the retry endpoint at `packages/control-plane/internal/observability/dlq/handler/dlq.go` stamps an audit row on 2xx forwarded from Hub; the row's `AfterState` carries `{"action":"retry"}` and `EntityID` is the DLQ row uuid) |
| `settings` | R/U + write | U | — (`write` is reserved for future broader-blast-radius operations) |
| `diagnostic-mode` | R/U | U | — |
| `node` | C/R/U/D + force-resync + write-override | C, U, force-resync | `write-override` — by design (Hub writes the row directly per §5.2) |

### 6.5 IAM service

| Resource | Catalog verbs | Audited verbs | Lifecycle/write verbs without emit |
|---|---|---|---|
| `user` | C/R/U/D + revoke | C, U, D, revoke | — |
| `api-key` | C/R/U/D + rotate | C, U, D, rotate | — (`POST /api-keys/:id/rotate` mints a successor key and flips the predecessor to `rotating`; emitted via `audit.EntryFor(c, iam.ResourceApiKey, iam.VerbRotate)` in `packages/control-plane/internal/identity/users/handler/api_keys.go`) |
| `organization` | C/R/U/D | C, U, D | — |
| `project` | C/R/U/D | C, U, D | — |
| `iam-policy` | C/R/U/D | C, U, D | — |
| `iam-group` | C/R/U/D | C, U, D | — |
| `audit-log` | R + export | export | — (every export is itself audited so the trail is self-witnessing) |
| `revocation` | R only | — | — (read-only) |
| `nexus-session` | revoke only | revoke (admin force-logout + Hub revoke-device) | — |
| `identity-provider` | C/R/U/D + probe | C, U, D, probe | — |
| `device-enrollment` | enroll only | — | enroll runs inside `POST /api/agent/sso-enroll` and audits via the user-creation handler chain |

In addition to the catalog-derived emit, the password login path emits two non-canonical actions (`admin.login.succeeded`, `admin.login.failed`) used by the `auth.login_failure_rate` alert rule.

## 7. Catalog verbs without an emit, by design

The catalog declares one verb that intentionally produces no audit row.

`routing-rule.simulate` is a what-if computation against a candidate routing rule and a candidate request; it never mutates state. Listing it in the catalog gives the verb an IAM gate so admins can be granted "may run simulations" without being granted update. No emit is correct.

## 8. Read surface

Admin-facing endpoints in `packages/control-plane/internal/traffic/handler/traffic/traffic.go`:

- `GET /api/admin/admin-audit-logs` — list + filter; gated by `admin:audit-log.read`.
- `GET /api/admin/admin-audit-logs/export` — bounded export; gated by `admin:audit-log.export`; emits its own audit row before returning so the trail is self-witnessing.
- `GET /api/admin/me/admin-audit-logs` — current user's own actions; no IAM gate (actor filter is server-enforced from session).

Store implementation: `packages/control-plane/internal/traffic/store/trafficstore/traffic_event.go` (`ListAdminAuditLogs`, `ExportAdminAuditLogs`).

Chain integrity is verified by `audit-chain-verify` (scheduled job in `packages/nexus-hub/internal/jobs/defs/audit/audit_chain_verify.go`).

## 9. Retention

Daily purge driven by `packages/nexus-hub/internal/jobs/defs/retention/data_retention.go`:

```text
DELETE FROM "AdminAuditLog" WHERE timestamp < now() - retentionDays
```

Default `AdminAuditLogDays = 365`, overridable via:

- yaml: `scheduler.retention.adminAuditLogDays` in `packages/control-plane/control-plane.config.yaml` (and `dev` / `prod` siblings).
- env: `NEXUS_HUB_RETENTION_ADMIN_AUDIT_DAYS`.

Setting the value to `0` (or negative) disables the purge — operators retaining indefinitely for compliance can do so without code changes. The job also purges `traffic_event`, `traffic_event_payload`, and `metric_rollup_1h` independently; one table's failure does not block the others. See `data-retention-purge-architecture.md` for the broader retention model.

## 10. Failure mode

When the publish to MQ fails on the CP side, `Writer.observeFailure` runs:

- warn log `admin_audit_log_publish_failed` with `stage` in `{marshal, enqueue}`, the action, entity, and underlying error;
- per-action Prometheus counter `admin.audit_log_failed_total{action}` (wired in `cmd/control-plane/wiring/audit.go`).

The upstream mutation has already committed — there is no rollback path for the missing audit row. Operators are expected to alert on the failure counter and reconcile manually if a hard ack is required for compliance.

On the Hub batch-write side, a tx Begin / Insert / Commit failure causes the entire batch to be NAK'd back to NATS for redelivery. Per-stage metrics on `mq.admin_errors_total{error_type}` plus the `mq.admin_consumed_total` / `mq.admin_batch_flush_total{result}` / `mq.admin_batch_size` histograms drive ops dashboards.

A marshal failure on `BeforeState` or `AfterState` inside Hub still inserts the row (with the affected column NULL) — losing one state snapshot is preferable to losing the chain link and forcing a manual repair.

## 11. References

- `packages/control-plane/internal/platform/audit/writer.go`
- `packages/control-plane/internal/platform/audit/helpers.go`
- `packages/control-plane/internal/identity/iam/catalog_consistency_test.go`
- `packages/control-plane/internal/identity/authserver/login/password.go`
- `packages/control-plane/internal/identity/users/handler/auth_sessions.go`
- `packages/control-plane/internal/infrastructure/infra/thing_overrides.go`
- `packages/control-plane/internal/observability/alerts/handler/handler.go`
- `packages/control-plane/internal/traffic/handler/traffic/traffic.go`
- `packages/control-plane/internal/traffic/store/trafficstore/traffic_event.go`
- `packages/control-plane/cmd/control-plane/wiring/audit.go`
- `packages/shared/identity/iam/catalog.go`
- `packages/shared/identity/iam/catalog_data.go`
- `packages/shared/transport/mq/messages.go`
- `packages/nexus-hub/internal/observability/consumer/admin_audit.go`
- `packages/nexus-hub/internal/jobs/defs/retention/data_retention.go`
- `packages/nexus-hub/internal/jobs/defs/audit/audit_chain_verify.go`
- `packages/nexus-hub/internal/fleet/manager/override.go`
- `packages/nexus-hub/internal/traffic/chain/chain.go`
- `packages/nexus-hub/internal/config/config.go`
- `packages/nexus-hub/cmd/nexus-hub/wiring/jobs.go`
- `tools/db-migrate/schema.prisma`
- `docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md`
- `docs/developers/architecture/cross-cutting/observability/siem-bridge-architecture.md`
- `docs/developers/architecture/cross-cutting/storage/data-retention-purge-architecture.md`

---
doc: data-retention-purge-architecture
area: cross-cutting
service: storage
tier: 1
updated: 2026-05-20
---

# Data Retention & Purge Architecture

> **Tier 2 architecture doc.** Read when touching retention policy CRUD, the per-table purge jobs, or any path that determines how long Nexus keeps user content. Sister doc: `jobs-architecture.md` (the purge jobs themselves) + `pii-redaction-policy-architecture.md` (what's already scrubbed).

Nexus stores four classes of user-relevant data: **admin audit logs**, **traffic events** (every request through the platform), **traffic event payloads** (large request/response bodies), and **metric rollups** (operational telemetry). Each is governed by a knob on the single Hub-side `data-retention` job; ops-side retention has its own per-table table (`MetricOpsRetentionConfig`). DSAR-driven anonymisation is a separate flow.

---

## 1. The data classes

| Class | Where | Default retention | Knob source |
|---|---|---|---|
| `admin_audit_log` | Postgres | 365 days | `Scheduler.Retention.AdminAuditLogDays` in `nexus-hub.yaml` (`packages/nexus-hub/internal/config/config.go:355`) |
| `traffic_event` | Postgres | 90 days | `Scheduler.Retention.TrafficEventDays` |
| `traffic_event_payload` (bodies) | Postgres + S3 spillstore for large bodies | yaml-configured | `Scheduler.Retention.TrafficEventPayloadDays` |
| `metric_rollup_1h` | Postgres | yaml-configured | `Scheduler.Retention.MetricRollupDays` (the data-retention job only purges `metric_rollup_1h` today; the 5m / 1d / 1mo tables are tied to the rollup retention windows in `metrics-rollup-architecture.md` §4.4) |
| ops metrics per-layer (`metric_ops_raw`, rollups) | Postgres | per-layer | rows in `MetricOpsRetentionConfig` (one per layer) |

The default for `AdminAuditLogDays` (365 days) is aligned with compliance frameworks (SOC 2, ISO 27001) that require admin-action retention; tightening it shorter is admin-allowed but not recommended. `traffic_event_payload` retention can be set shorter than the parent `traffic_event` retention — the row stays even after the body is purged; clicking "Show body" after body expiry shows "Body expired" (the CASCADE from `traffic_event` cleans up surviving payload rows on the longer parent purge).

## 2. Configuration surfaces

- **Data-class retention** (admin audit / traffic event / payload / metric rollup) — driven from Hub yaml. There is no DB-backed per-tenant editor today; changing these values requires editing `nexus-hub.yaml` (env overrides `NEXUS_HUB_RETENTION_*` exist — see `packages/nexus-hub/internal/config/config.go:459+`).
- **Ops-metrics retention** (per layer in `MetricOpsRetentionConfig`) — DB-backed and editable via admin API at `GET / PUT /api/admin/observability/retention` (`packages/control-plane/internal/observability/retention/handler/retention.go:24-25`). PUT values are validated against per-layer min/max ranges before write.

## 3. The purge jobs (Hub scheduler)

Cross-ref `jobs-architecture.md`. Retention jobs in the Hub scheduler:

- `data-retention` — single job that sweeps the four data-class tables (`AdminAuditLog`, `traffic_event`, `traffic_event_payload`, `metric_rollup_1h`) using the four day-count knobs in `DataRetentionConfig` (a Go config struct populated from yaml — not a Prisma model). Implemented in `packages/nexus-hub/internal/jobs/defs/retention/data_retention.go`.
- Ops-retention sweeping — per-layer over ops-metric tables, driven by rows in `MetricOpsRetentionConfig`. Implemented under `packages/nexus-hub/internal/jobs/defs/retention/`.
- Spillstore S3 objects are reaped by the bucket Lifecycle rule (§8); the Hub does not currently run a separate S3 reaper job.

Each tick the job executes a single `DELETE … WHERE timestamp < cutoff` per table; per-table batching, idempotency, and an explicit `system:retention.purged` audit emit are not implemented today (the job logs deleted counts via slog instead).

## 4. Right-to-delete (GDPR / DSAR)

A user (or admin acting on behalf) can request deletion or access of their data via the DSAR endpoints:

```
POST /api/admin/dsar          { subjectId, type: "ACCESS" | "ERASURE", contact, notes }
GET  /api/admin/dsar          (list, filterable by status)
GET  /api/admin/dsar/:id
PUT  /api/admin/dsar/:id      (status / notes update)
POST /api/admin/dsar/:id/fulfill
```

IAM-gated on `admin:dsar.{read,create,update,fulfill}`. The request creates a `dsar_request` row (Prisma model `DSARRequest`; statuses `PENDING | IN_PROGRESS | COMPLETED | REJECTED`; types `ACCESS | ERASURE`) + emits an admin-audit event. The fulfillment side that actually anonymises matching rows (traffic events, audit actors, spillstore objects) is currently a manual operator step — the DSAR API tracks the request lifecycle; the per-row anonymisation job is planned. The "privileged background job" described below is the planned end state:

- For `traffic_event` rows where the user is the actor: anonymise (`user_id = NULL, actor_email = NULL`).
- For audit rows where the user is the actor of a compliance event (e.g., they accepted a policy): preserve the row but anonymise PII fields.
- For spillstore bodies: delete the S3 object; row stays but body unrecoverable.

GDPR compliance requires action within 30 days. The job runs continuously and tracks per-request status.

## 5. Anonymisation vs deletion

Some events MUST be retained for compliance even after the user is deleted (e.g., "this user agreed to TOS at time T" is a compliance record). For those:

- Replace `user_id` with a stable opaque ID derived from `HMAC(user_id, tenant_secret)`.
- Replace `email`, `name` with `<redacted>`.
- Keep the action + timestamp + outcome.

The result: the audit row is preserved as a compliance artefact but cannot be reverse-linked to a real user.

## 6. Backup interactions

Backups (cross-ref `docs/operators/ops/backup-dr.md`) inherit retention. A backup taken before a purge contains the deleted rows; restoring that backup re-introduces them. Operator process: after restoring a backup, immediately re-run purge jobs to enforce current retention.

For right-to-delete compliance, an admin who is restoring an old backup MUST also re-process all `dsar_request` rows since the backup point. The planned DSAR fulfillment job will do this automatically when it starts up after a restore.

## 7. Schema-level partitioning (optional)

For very large tenants, `traffic_event` can be partitioned by month. Purging then becomes "drop the oldest partition" — O(1) DDL operation instead of O(N) DELETE.

The partition strategy is admin-configurable per-tenant. Default: not partitioned (single-table, simpler). Tenants over ~100M `traffic_event` rows per month should enable partitioning.

## 8. Spillstore lifecycle rules

S3 bucket has S3 Lifecycle rules configured:

- `prod/*` — transition to Glacier after 30 days; expire after the per-tenant body retention.
- `dev/*` — expire after 7 days.

The Lifecycle rule is a defence-in-depth — `retention.purge.spillstore` job is the primary delete path. Lifecycle catches whatever the job missed.

## 9. Observability of retention

- `retention_rows_deleted_total{table, tenant}` — counter.
- `retention_last_run_seconds_ago{table}` — gauge; alerts on stale (job stuck).
- `retention_oldest_row_age_seconds{table, tenant}` — gauge; alerts if retention is not enforced.

The "Retention dashboard" in CP visualises per-tenant compliance state.

## 10. Operational concerns

- **Reducing retention shorter than backups exist** is a one-way operation — backups are not retroactively purged. Operators should also age out backups under the new retention.
- **Force-purge** (admin escalation): a "Purge Now" button on the retention page triggers an immediate run instead of waiting for the next scheduled tick. Audit-logged.
- **Purge job stalled**: alert + the `dsar_request` rows pile up. Manual replay path documented in `docs/operators/ops/runbooks/`.

<!-- 💡 harvest: retention bounds (min/max per table) is binding. Currently enforced server-side in admin handler validation. Could be a Cursor rule on the retention handler but only one file consumes; skipping. -->

## 11. Sources

- `packages/nexus-hub/internal/jobs/defs/retention/data_retention.go` — the `data-retention` Hub job (single job; `DataRetentionConfig` carries all four knobs).
- `packages/nexus-hub/internal/jobs/defs/retention/ops_retention.go` — per-layer ops-metrics retention sweep driven by `MetricOpsRetentionConfig`.
- `packages/control-plane/internal/observability/retention/handler/retention.go` — admin CRUD + bounds validation.
- `packages/control-plane/internal/governance/dsar/handler/` + `dsarstore/` — DSAR admin endpoints + persistence.
- `tools/db-migrate/schema.prisma` — `DSARRequest` (table `dsar_request`) + `MetricOpsRetentionConfig` (table `metric_ops_retention_config`).
- `docs/operators/ops/runbooks/retention-recovery.md` — operational runbooks (planned).

## 12. Cross-references

- `jobs-architecture.md` §5 — the purge jobs.
- `pii-redaction-policy-architecture.md` — distinguishes "redact at emit" (kept forever, scrubbed) vs "delete at retention" (gone).
- `spillstore-architecture.md` — body retention + S3 lifecycle.
- `audit-pipeline-architecture.md` §9 — retention overview from the audit pipeline's perspective.
- `tenancy-architecture.md` — per-tenant policy scoping.

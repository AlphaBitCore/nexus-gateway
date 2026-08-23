# Data retention & purge architecture

Nexus Gateway keeps audit, traffic, and metric data only as long as it is useful,
then deletes it. Retention is enforced by a set of daily Hub scheduler jobs, each
owning a distinct set of tables, plus a subject-driven right-to-erasure path that
anonymizes a person's data on request. This doc covers what gets purged, the
configuration that governs it, and the erasure flow.

The scheduler framework itself — registration, the advisory lock that guarantees
at-most-one replica runs a job per tick, intervals, and run history — is described
in [jobs-architecture.md](../foundation/jobs-architecture.md). This doc focuses on
the retention semantics layered on top.

## 1. Retention configuration

Retention windows come from two places:

- **Hub config (`RetentionConfig`)** — per-table day counts in
  `packages/nexus-hub/internal/config/config.go`, overridable per field by
  `NEXUS_HUB_RETENTION_*` environment variables. A value of zero or less disables
  purge for that table, so an operator can keep a table indefinitely.
- **`metric_ops_retention_config` table** — one row per ops-metric/diag layer
  (`layer` primary key, `retention_days`), managed through the Control Plane admin
  API and read live by the ops-retention job each run. This lets operators tune
  ops/diag retention without a redeploy.

## 2. The purge jobs

Four daily jobs divide the tables between them. Each purges its tables
independently — a failure on one table does not stop the others (the job joins the
errors and returns them together) — and each honors the "zero disables" rule.

### data-retention

`packages/nexus-hub/internal/jobs/defs/retention/data_retention.go` purges the
audit and traffic tables:

- `traffic_event_payload` (by `created_at`) — purged first, since request/response
  body blobs are the bulky part and usually have the shortest window.
- `traffic_event` (by `timestamp`) — its `ON DELETE CASCADE` to
  `traffic_event_payload` cleans up any payload rows that outlived the payload
  window.
- `AdminAuditLog` (by `timestamp`).

### rollup-retention

`packages/nexus-hub/internal/jobs/defs/rollup/rollup_retention.go` owns the entire
business-metrics rollup tier chain — `metric_rollup_5m`, `metric_rollup_1h`,
`metric_rollup_1d`, `metric_rollup_1mo` — purging each by its own per-tier day
count (finer tiers expire sooner). This is the single owner of `metric_rollup_*`
purging.

### ops-retention

`packages/nexus-hub/internal/jobs/defs/retention/ops_retention.go` reads every row
of `metric_ops_retention_config` and applies each layer's window to its matching
table. The layers cover the ops-metric raw and rollup tiers
(`metric_ops_raw`, `metric_ops_rollup_1h` / `_1d` / `_1mo`, split into runtime vs
business classes) and the diagnostic-event levels (`thing_diag_event` rows by
`info` / `warn` / `error` / `fatal`). Deletes run in chunks (looping until a chunk
comes back short) so each transaction stays small and never blocks the live
writers for long.

### job-retention

`packages/nexus-hub/internal/jobs/defs/retention/job_retention.go` keeps the
`job_run` history table bounded by count, not age: it keeps the most recent N runs
per job (matching the admin UI's paginated run-history view). It also runs once on
startup so a fresh replica prunes immediately.

## 3. Right to erasure (DSAR)

The Control Plane exposes a data-subject-request workflow under
`packages/control-plane/internal/governance/dsar/` for access and erasure
requests. Requests live in the `dsar_request` table and move through a status
machine: `PENDING` → `IN_PROGRESS` → `COMPLETED` or `REJECTED`. Every route is IAM
gated on the `dsar` resource, and create / update / fulfill are recorded in the
admin audit log.

Fulfillment first validates that the request's `subjectId` resolves to a real
`NexusUser`. A subjectId that matches no user is rejected (HTTP 422) and the
request is moved to `REJECTED` — never `COMPLETED` — so an empty access export
or a zero-row erasure cannot be mistaken for a successful operation.

A request is one of two types:

- **ACCESS** — fulfillment exports every personal-data surface for the subject —
  the `NexusUser` record, IAM group memberships, AI-gateway + agent traffic, the
  inline request/response bodies (capped), device-assignment history, and the
  assistant sessions/memory/files — and stores the export in the request's
  `outcome`. The admin-audit row records only per-surface counts (a digest),
  never the export contents, so the unbounded, never-erased audit log does not
  itself become a residual copy of the subject's PII.
- **ERASURE** — fulfillment removes the subject's personal data in a single
  transaction while preserving the rows that carry no personal data (so audit
  integrity and aggregate rollups survive). It:
  - scrubs the subject's captured bodies — `traffic_event_payload`'s
    `inline_request_body`, `inline_response_body`, and the `request_spill_ref` /
    `response_spill_ref` pointers. This is the ONLY copy: the normalized
    projection is recomputed at view time from exactly these bytes rather than
    stored beside them, so nulling them erases the prompt/response content
    outright. One erasure surface is the point — a second stored copy is a
    second thing an erasure can miss;
  - nulls the identifying columns on the subject's `traffic_event` rows —
    `entity_id`, `entity_name`, the `identity` snapshot, and `source_ip` for
    virtual-key traffic; `source_ip`, `source_process`, `entity_name`, and
    `identity` for agent traffic within its assignment windows;
  - deletes the subject's assistant data — `AssistantMemory`, `AssistantSession`,
    `AssistantFile`, `AssistantPendingConfirm`, and `AssistantChatEvent` rows
    keyed by `userId` (the chat audit-chain rows carry digests + counts only,
    but they are keyed to the subject, so erasure removes them with the
    sessions they attest);
  - nulls the persisted ACCESS export (`dsar_request.outcome`) for the same
    subject — a prior access request stored a full PII copy there, so erasure
    must scrub it for the correct ACCESS-then-ERASURE sequence to leave no
    residual personal data;
  - **deletes the account record itself (default behaviour).** Art.17 covers
    "all personal data", which on a compliance gateway includes the account
    profile and federated-identity claims, not just the traffic footprint. So the
    same transaction also removes: the subject's owned `VirtualKey` and
    `AdminApiKey` rows (their owner FKs are `ON DELETE SET NULL`, so the account
    delete alone would only orphan-null them — they are removed outright); the
    `UserFederatedIdentity` rows (external IdP subject, `externalEmail`,
    `rawClaims`) and `RefreshToken` rows for the subject; the `ScimToken` rows the
    subject *created* (whose `createdBy` is `ON DELETE RESTRICT` and would
    otherwise block the delete); and finally the `NexusUser` row itself. Its
    remaining referrers (`DeviceAssignment.userId`, `ThingDiagModeWindow.setBy`,
    `MetricOpsRetentionConfig.updatedBy`) are `ON DELETE SET NULL` and resolve
    automatically.

  **Audit-trail retention exception.** The tamper-evident admin-audit hash chain
  (`AdminAuditLog`) is the one personal-data-adjacent surface that erasure does
  **not** touch. Its rows carry no subject PII beyond an opaque actor id, and
  breaking the hash chain would destroy the tamper-evidence the gateway relies on
  as a compliance control. Retaining it is the documented accountability /
  legal-obligation basis exception (GDPR Art.17(3)(b)/(e)) to the otherwise-
  complete erasure.

  Payloads are scrubbed before the identifying columns are nulled, because the
  selection predicates depend on those columns; the account-deletion stage runs
  last (after the traffic anonymisation, which keys on `entity_id` =
  `NexusUser.id`) so it cannot strand an earlier stage. The physical spill objects
  behind any cleared spill references are not deleted by this transaction (it
  holds only a database pool); their count is reported as `spillRefsOrphaned` and
  their out-of-band deletion is tracked separately; the database retains no
  reference to them.

Erasure is keyed by the subject's user id; the per-stage counts (anonymized rows,
payloads scrubbed, orphaned spill references, assistant rows erased, prior-access
exports scrubbed, owned keys / federated identities / refresh + SCIM tokens
deleted, and whether the account record itself was deleted) are written into the
request outcome and the audit entry.

## 4. Design notes

- **Zero means keep forever.** Every per-table window treats a non-positive value
  as "disabled", so retention is opt-in per table and an operator never loses data
  by leaving a window unset.
- **One owner per table.** `metric_rollup_*` purging lives only in
  rollup-retention; data-retention covers `traffic_event*` and `AdminAuditLog`;
  ops-retention covers the `metric_ops_*` and diag tables. No table is purged by
  two jobs.
- **Chunked deletes for hot tables.** The ops tables are the highest-volume, so
  ops-retention deletes in bounded chunks to keep transactions short; the
  lower-volume audit tables purge in a single statement per table.
- **Erasure preserves only the audit/metric rows, deletes everything personal —
  including the account.** Traffic rows are anonymized in place (column-level) so
  aggregate metrics survive a subject's erasure; the subject's own content
  (payload bodies) and assistant data carry no aggregate value and are
  scrubbed/deleted outright; the account record and its identity/credential rows
  are deleted by default (full Art.17 erasure). The single retained surface is the
  tamper-evident admin-audit hash chain — see the audit-trail retention exception
  in §3.

## References

- `packages/nexus-hub/internal/config/config.go` — `RetentionConfig` + env overrides
- `packages/nexus-hub/internal/jobs/defs/retention/data_retention.go` — traffic + admin-audit purge
- `packages/nexus-hub/internal/jobs/defs/retention/ops_retention.go` — ops-metric + diag purge
- `packages/nexus-hub/internal/jobs/defs/retention/job_retention.go` — job-run history prune
- `packages/nexus-hub/internal/jobs/defs/rollup/rollup_retention.go` — metric_rollup tier purge
- `packages/nexus-hub/internal/quota/rollup/rollupstore.go` — `PurgeRollupBefore`
- `packages/control-plane/internal/governance/dsar/` — DSAR access + erasure flow
- `tools/db-migrate/schema/` — `dsar_request` (`compliance.prisma`); `metric_ops_retention_config` (`observability.prisma`)

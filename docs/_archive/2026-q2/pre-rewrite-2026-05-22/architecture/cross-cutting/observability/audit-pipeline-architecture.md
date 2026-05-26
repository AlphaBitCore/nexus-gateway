---
doc: audit-pipeline-architecture
area: cross-cutting
service: observability
tier: 1
updated: 2026-05-20
---

# Audit Pipeline Architecture (End-to-End)

> **Tier 1 architecture doc.** Read this before touching `packages/shared/audit/`, `packages/nexus-hub/internal/traffic/ingest/audit/`, the MQ audit sink consumer, the audit Postgres schema, or any audit emitter in AI Gateway / Compliance Proxy / Agent. Admin-action audit coverage matrix lives in the companion `admin-audit-log-coverage.md`. The MQ envelope used here is in `mq-architecture.md`; trace_id semantics in `trace-id-propagation-architecture.md`.

The audit pipeline is the substrate for compliance, debugging, and analytics. Every traffic-affecting event in the system ends up in either `traffic_event` (data-plane request rows) or `AdminAuditLog` (admin mutations / sensitive reads), correlated by the Nexus request id stamped at the Hub edge.

---

## 1. The two audit tables

| Table | Source | Granularity |
|---|---|---|
| `traffic_event` | AI Gateway / Compliance Proxy / Agent on every traffic-affecting call | One row per request |
| `AdminAuditLog` | Control Plane on every admin-API mutation + sensitive read | One row per admin action |

There is no separate `agent_audit` table — agent-side traffic rows land in `traffic_event` with `trafficSource = AGENT` and the agent's diagnostic / lifecycle signals (enrollment, drift, kill-switch ack) ride the diag pipeline (`DiagSilence`, `diag-event-triage-architecture.md`), not an audit table. Cross-table correlation uses the Hub-stamped Nexus request id (`AdminAuditLog.nexusRequestId`, `traffic_event.nexus_request_id`).

## 2. Event schema

The two tables have separate shapes — there is no shared canonical-field set across them. `AdminAuditLog` is the actor-action shape (see `admin-audit-log-coverage.md` §22 for the per-column listing) and is hash-chained (`previousHash`, `integrityHash`, `hashInput`). `traffic_event` is the request-row shape — the canonical producer struct is `packages/shared/audit/event_types.go::AuditEvent` (TransactionID, ConnectionID, TrafficSource, BumpStatus, hook-pipeline fields, provider/model/tokens, latency phases, request/response Body containers, attestation passthrough, source-process attribution).

Common indexed columns on `traffic_event`: trafficSource, ingressType, provider, model, latencyMs, requestHookDecision, responseHookDecision, organisation lookups, plus the spillable `Body` containers for request/response bodies (`packages/shared/audit/body.go`).

## 3. Emission

Each emitter packages an audit event and pushes through a per-service `Writer`. The producer-side library lives in `packages/shared/audit/`:

```go
// packages/shared/audit/event_types.go
type AuditEvent struct {
    ID                    string
    TransactionID         string
    ConnectionID          string
    TrafficSource         string // COMPLIANCE_PROXY, DNS_TERMINATED, AGENT
    IngressType           string
    BumpStatus            string
    // …request/response hook fields, provider/model/usage, latency,
    //   compliance tags, attestation, source-process attribution…
    RequestBody           Body   // absent | inline | spill
    ResponseBody          Body
    Timestamp             time.Time
}

// packages/shared/audit/event_types.go
type Writer interface {
    Enqueue(event AuditEvent)
    Flush(ctx context.Context) error
    Close(ctx context.Context) error
}
```

Each data-plane service supplies its own `Writer`:

- **Compliance Proxy** — `MQBatchWriter` over `nexus.event.ai-traffic`, with NDJSON spool on MQ pressure.
- **Agent** — encrypted SQLite queue + HTTP upload to Hub.
- **AI Gateway** — MQ writer in `packages/ai-gateway/internal/platform/audit/`.

Per emit, the writer:

1. Stamps `Timestamp` from the local clock.
2. Routes the request/response body via `NewInlineBody` / `NewSpillBody` (cross-ref §7).
3. `Enqueue` returns immediately; the writer batches to MQ asynchronously.
4. Falls back to per-service durable storage (NDJSON spool for cp, SQLite queue for agent) when MQ is unreachable.
5. Agent additionally falls back to HTTP `POST /api/internal/things/audit` (via `thingsAPI.AuditUpload`) when MQ is unreachable but the Hub is.

### The empty-string stamping invariant (binding)

Agent's `auditEventToMap` always sets string fields — including `""`. Hub `AuditUpload` MUST stamp-unconditionally or strip-empty for any CHECK-constrained column, or the audit pipeline stalls silently when an empty string violates a CHECK.

The fix shape: Hub-side code that consumes audit fields must either accept `""` and write NULL, or accept `""` and write `""` consistently. Inconsistent behaviour produces silent CHECK constraint failures and pipeline halt.

## 4. Transport — MQ + HTTP fallback

Primary: `nexus.event.ai-traffic` JetStream subject (cross-ref `mq-architecture.md`).

Fallback:

- AI Gateway / Compliance Proxy → NDJSON spool to local disk; re-driven into MQ on recovery.
- Agent → encrypted SQLite queue; once Hub is reachable, uploaded via HTTP `POST /api/internal/things/audit` (handled by `thingsAPI.AuditUpload` in `packages/nexus-hub/internal/traffic/ingest/audit/agent_audit.go`).

Both paths carry the **same wire shape** (`mq.TrafficEventMessage` envelope around `AuditEvent`). The Hub sink dedups based on `event_id` (cross-ref `mq-architecture.md` §5).

## 5. Ingestion (Hub audit sink)

The audit consumer in Hub reads `nexus.event.ai-traffic` and writes to Postgres:

1. Receive event from MQ (pull consumer).
2. Validate required fields.
3. Resolve `orgId` → `org_ancestor_path` (cached lookup).
4. Extract canonical typed columns onto `traffic_event`; bodies routed onto `traffic_event_payload` (inline JSONB) or via `SpillRef` to S3.
5. Bulk insert (batch size + interval tunable).
6. Ack the MQ message.
7. On batch failure: NAK; retry with backoff; DLQ after max retries.

`AdminAuditLog` rows are inserted in-transaction by the CP admin handlers (separate path — see `admin-audit-log-coverage.md`), not through this consumer.

The HTTP fallback path goes through the same write logic; both paths converge on the same insert function.

## 6. PII redaction

Sensitive payloads (request bodies, response bodies, headers known to carry secrets) pass through a redaction step at emit time. Three redaction primitives:

- **Hash** — SHA-256 the value; analytics can still detect duplicates without seeing the plaintext.
- **Token** — replace with a stable, opaque token (per-value, per-tenant).
- **Mask** — replace with `***` (the value is unrecoverable).

The redaction policy is configured per hook (cross-ref `hook-architecture.md`). When PII Detector identifies a span, the strategy in `HookConfig.onMatch.redactStrategy` controls which primitive applies.

Redaction happens **before** the event reaches MQ. The plaintext never leaves the originating Thing.

## 7. Body storage tiering — spillstore overflow

Audit rows have a hot path (Postgres) and a cold path (S3 spillstore):

- Bodies ≤ `MaxInlineBodyBytes` → inline column on `traffic_event_payload`.
- Bodies > `MaxInlineBodyBytes` → spillstore. Row stores a `SpillRef { backend, key, size, sha256, contentType, truncated }` (`packages/shared/audit/body.go`).

`MaxInlineBodyBytes` is a runtime, Hub-pushed config — `payloadcapture.DefaultMaxInlineBodyBytes` (256 KiB) is the default in `packages/nexus-hub/internal/compliance/catbagent/payload_capture.go`. The producer pulls the current value via `payloadCapture.Get().MaxInlineBodyBytes` on every emit, so admin changes propagate without a restart.

When an admin clicks "Show body" in the CP UI, the back-end generates a presigned S3 URL (cross-ref `spillstore-architecture.md` for the storage layer).

Agents use a slightly different pattern: they spill locally first (SQLCipher payload table), then upload to S3 via Hub-issued presigned URLs (`project_prod_s3_spillstore_done` records the prod flow).

## 8. Analytics queries

CP `/api/admin/analytics/*` endpoints serve the dashboard. Key query shapes:

- "Traffic by provider over time" — group by `provider` + time-bucket on `emitted_at`.
- "Top virtual keys by cost" — sum `cost_usd` group by `virtual_key_id`.
- "Hook rejection rate by hook" — count `request_hook_decision = reject` group by `hook_id`.
- "Org cost rollup" — sum across `org_ancestor_path` matches.

Indexes (anchored on `traffic_event`):

- `(emitted_at)` for time-range scans.
- `(org_id, emitted_at)` for tenant time-range.
- `(request_id)` for drill-down.
- `(virtual_key_id, emitted_at)` for VK analytics.
- `(provider, model, emitted_at)` for provider analytics.

Partitioning by month on `traffic_event` is supported but optional (single-EC2 prod doesn't need it yet).

## 9. Retention

Retention is per-table, per-tenant, configured by admin:

- `traffic_event` — typically 90 days; bodies in spillstore have separate retention.
- `AdminAuditLog` — typically 365 days (compliance requirements; the integrity hash chain means tombstoned rows would break verification — practical retention is the table's full lifetime).

A Hub scheduled job (cross-ref `jobs-architecture.md`) walks each table, deletes expired rows in batches, and emits a `retention.purged` audit event with the deleted count. Spillstore object cleanup is a separate parallel job.

Retention policy can be tenant-tunable in the future; today it is system-wide.

## 10. Failure modes

| Failure | Behaviour |
|---|---|
| MQ down | HTTP fallback. |
| HTTP fallback down (agent only) | SQLCipher local queue. |
| Hub down | Both fallbacks fail; agent buffers locally; AI GW / Compliance Proxy buffer in process memory (lossy on restart). |
| Postgres down | Hub buffers in JetStream (retention sized for 24h); resumes on Postgres return. |
| Spillstore down | Bodies are dropped (event still recorded with `body_dropped=true`). |
| `agent.empty-string` mishandling | Pipeline stalls silently; binding fix per §3. |
| DLQ growing | Alerts fire; manual replay via `tools/db-migrate/manual-scripts/`. |

## 11. Sources

- `packages/shared/audit/` — `AuditEvent` struct, `Writer` interface, `Body` discriminator, body-encoding helpers.
- `packages/nexus-hub/internal/traffic/ingest/audit/` — Hub-side ingest (`agent_audit.go` is the HTTP upload route; `helpers.go` shared helpers).
- `packages/shared/storage/spillstore/` — body overflow interface.
- `packages/shared/storage/spillstore/s3/` — S3 driver.
- `tools/db-migrate/schema.prisma` — `traffic_event`, `traffic_event_payload`, `AdminAuditLog` models.

## 12. Cross-references

- `admin-audit-log-coverage.md` — admin-action coverage matrix (companion doc).
- `mq-architecture.md` — `nexus.event.ai-traffic` JetStream subject + envelope dedup.
- `trace-id-propagation-architecture.md` — `request_id` / `trace_id` correlation.
- `hook-architecture.md` — hook decisions land here.
- `error-taxonomy-architecture.md` — `error_class` / `error_code` audit fields.
- `jobs-architecture.md` — retention purge job.
- `tenancy-architecture.md` — `org_ancestor_path` materialisation.

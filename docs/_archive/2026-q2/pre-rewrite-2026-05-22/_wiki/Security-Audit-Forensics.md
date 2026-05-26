# Security Audit Forensics

*Audience: compliance leads and security reviewers evaluating the completeness and integrity of Nexus Gateway's audit trail.*

Every AI transaction across all three traffic paths — AI Gateway, Compliance Proxy, and Desktop Agent — is recorded as a `traffic_event` row in PostgreSQL. Admin actions are recorded separately in `AdminAuditLog` with a hash chain for integrity verification. Bodies up to 256 KiB are stored inline; larger bodies overflow to S3 spillstore. External SIEM forwarding is available via HTTP webhook. This page covers what is recorded, how it is stored, retention defaults, and how to correlate events across services.

---

## The two audit tables

| Table | Source | What it captures |
|---|---|---|
| `traffic_event` | AI Gateway / Compliance Proxy / Agent on every traffic request | One row per request: source, hook decisions, cost, tokens, latency phases, body containers, provider/model, VK ID |
| `AdminAuditLog` | Control Plane on every admin-API mutation and sensitive read | One row per admin action: actor, action, changed payload, hash-chain fields |

There is no separate agent audit table. Agent traffic rows land in `traffic_event` with `trafficSource = AGENT`. The two tables are correlated by the Hub-stamped Nexus request ID (`AdminAuditLog.nexusRequestId`, `traffic_event.nexus_request_id`).

`AdminAuditLog` rows are hash-chained (`previousHash`, `integrityHash`, `hashInput`). A deleted or modified row breaks the chain; the integrity can be verified by replaying the hash sequence. This makes tampering detectable without an append-only storage backend.

## What traffic_event captures per row

Key fields on every `traffic_event` row:

- `trafficSource` — `AI_GATEWAY`, `COMPLIANCE_PROXY`, or `AGENT`
- `ingressType` — which ingress endpoint served the request
- `provider`, `model` — resolved provider and model
- `virtualKeyId` — the VK that authenticated the request (never the secret)
- `requestHookDecision`, `responseHookDecision` — hook pipeline verdicts (allow / redact / block)
- `dataClassification` — content sensitivity label assigned by the PII hook
- `costUsd` — request cost stamped from the `Model` row prices
- `latencyMs`, `upstreamTtfbMs`, `upstreamTotalMs` — latency phases
- `requestBody`, `responseBody` — body containers (inline or spill reference)
- `passthrough`, `bypassReason` — set when emergency passthrough was active

The hashed VK secret is never logged; audit rows reference only the `virtualKeyId`. The `Authorization` header value is always redacted at emit time.

## Body storage tiering

Request and response bodies follow a two-tier storage model:

- Bodies ≤ 256 KiB are stored inline in `traffic_event_payload` (PostgreSQL JSONB).
- Bodies > 256 KiB are written to S3 spillstore. The `traffic_event` row stores a `SpillRef` containing `{backend, key, size, sha256, contentType, truncated}`.

The 256 KiB threshold (`MaxInlineBodyBytes`) is a Hub-pushed runtime config; admin changes propagate without a service restart. When an admin opens "Show body" in the Control Plane UI, the backend generates a presigned S3 URL to retrieve the stored content. After the body retention period expires, "Show body" returns "Body expired".

Agents spill locally first (SQLCipher payload table), then upload to S3 via Hub-issued presigned URLs.

## PII redaction before storage

Sensitive content is scrubbed before reaching MQ — the plaintext never leaves the originating service. Three redaction primitives are available:

| Primitive | Output |
|---|---|
| Hash | SHA-256 (or HMAC-SHA-256); analytics can still detect duplicates |
| Token | Stable opaque string per value per tenant |
| Mask | `***` or `<PII redacted>`; value unrecoverable |

The PII detector hook applies the configured strategy to request and response bodies. Built-in categories include email, phone, credit card, SSN, IBAN, API key shapes, JWTs, and private-key blocks. Custom categories are configurable via Rule Packs. Regardless of hook configuration, `Authorization` header values are always masked at emit.

## SIEM forwarding

The Hub SIEM bridge fans audit and traffic events to external compliance systems. The forwarder uses an `HTTPSink` that POSTs JSON batches to a configured webhook URL. By setting different URLs and authentication headers, the same sink serves:

- Splunk HEC — `Authorization: Splunk <hec-token>`
- Datadog Logs API
- Elasticsearch `_bulk` endpoint
- Any generic HTTPS webhook receiver

The SIEM forwarder has its own queue and spool directory so SIEM outages do not affect the primary PostgreSQL audit writes.

## Retention defaults and policy

Retention is enforced by the Hub `data-retention` scheduled job, which runs and applies the configured day counts for each data class:

| Data class | Default retention |
|---|---|
| `admin_audit_log` | 365 days (compliance floor for SOC 2 / ISO 27001) |
| `traffic_event` | 90 days |
| `traffic_event_payload` (bodies) | 30 days |
| Metric rollups | 90 days |

Body retention can be shorter than row retention — the `traffic_event` row persists after the body expires. The `admin_audit_log` floor exists because its hash chain makes tombstoned rows detectable; practical retention is the table's full lifetime.

The purge job emits a `system:retention.purged` audit event with the deleted row count after each run. The Retention dashboard in the Control Plane UI visualises per-table compliance state.

## Emergency passthrough audit invariant

Emergency passthrough does not create a gap in the audit trail. Every bypassed request still emits a `traffic_event` with `passthrough=true` and a mandatory `bypass_reason` set to the reason the activating admin provided. Hook decisions are absent (the hooks did not run), but routing, cost, and latency fields are present. Activation and auto-revert both emit admin-audit events. See [Emergency Passthrough](Emergency-Passthrough).

---

## Canonical docs

- [`docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md) — end-to-end audit pipeline: emission, transport, ingestion, PII redaction, body tiering, retention
- [`docs/developers/architecture/cross-cutting/storage/data-retention-purge-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/storage/data-retention-purge-architecture.md) — data classes, per-tenant retention config, purge jobs, GDPR/DSAR flows

**Adjacent wiki pages**: [Security Compliance Posture](Security-Compliance-Posture) · [Security Threat Model](Security-Threat-Model) · [Control Plane Audit Log](Control-Plane-Audit-Log) · [Control Plane SIEM Bridge](Control-Plane-SIEM-Bridge) · [Spillstore](Spillstore)

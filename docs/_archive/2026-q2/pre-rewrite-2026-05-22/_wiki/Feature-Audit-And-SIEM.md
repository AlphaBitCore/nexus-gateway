# Feature Audit And SIEM

Every AI interaction flowing through any of the three Nexus traffic paths — AI Gateway, Compliance Proxy, or Desktop Agent — produces a row in the `traffic_event` table. Request and response bodies are captured inline for small payloads or spilled to S3 (or local filesystem) for large ones. Admin-plane mutations land in a hash-chained `AdminAuditLog` table. The SIEM bridge fans completed events out to Splunk HEC, Datadog, Elasticsearch, or any HTTPS webhook receiver.

---

## Traffic event recording

Every request produces one `traffic_event` row. Key fields:

| Field group | What it contains |
|---|---|
| Identity | `traffic_source` (AI_GATEWAY / COMPLIANCE_PROXY / AGENT), ingress type, org, project, virtual key |
| Routing | Provider, model, `routing_trace` (JSONB: strategy path, fallback attempts, smart-routing decision) |
| Hook decisions | `request_hook_decision`, `response_hook_decision`, `blocking_rule` attribution, per-stage pipeline trace |
| Usage and cost | Input tokens, output tokens, cache-read tokens, cache-creation tokens, estimated cost, cache savings |
| Latency | Request-hooks ms, upstream TTFB ms, upstream total ms, response-hooks ms, end-to-end duration ms |
| Classification | Data sensitivity label (Public / Internal / Confidential / Restricted) |
| Cache | `gateway_cache_status` (hit / miss / skipped), `provider_cache_status`, `gateway_cache_kind` |
| Body | Inline JSONB or spillstore reference (content-hashed, retrievable via presigned URL) |

Admin-plane mutations (provider updates, routing-rule changes, credential rotations, IAM policy changes, kill-switch toggles) land in `AdminAuditLog` with a hash chain (`previousHash`, `integrityHash`) for tamper evidence. Cross-table correlation uses the Hub-stamped Nexus request ID present on both tables.

## Body capture and spillstore

Body capture is configured per host (Compliance Proxy / Agent) or per provider (AI Gateway). When enabled, the canonical prompt and completion text are extracted from the provider's wire format.

Two-tier storage keeps the audit hot path bounded:

- Bodies under 256 KiB are stored inline in `traffic_event_payload` JSONB columns.
- Larger bodies are written to the spillstore (S3 in production, local filesystem in development). The row stores a content-hashed reference; the Control Plane admin UI fetches the original bytes via presigned URL for investigation.

The capture pipeline handles JSON, SSE text, multipart, and binary bytes losslessly. For streaming responses, the compliance mode determines capture behaviour:

| Mode | Audit behaviour |
|---|---|
| `passthrough` | No body capture. Metadata-only audit row. |
| `buffer_full_block` | Full body buffered before forwarding; captured once at stream end. |
| `chunked_async` | Body accumulated per chunk asynchronously; complete audit trail, no blocking. |

## SIEM forwarding

The SIEM bridge polls `traffic_event` (and admin/agent rows when configured) and POSTs JSON batches to configured channels. A single `HTTPSink` type handles all destinations by accepting different URLs and header maps:

```json
{
  "event_id": "01HXYZ...",
  "trace_id": "...",
  "request_id": "...",
  "schema_version": 3,
  "source": "nexus.ai-gateway",
  "event_type": "traffic:request",
  "severity": "info",
  "org_id": "...",
  "org_ancestor_path": ["nexus", "acme-holdings", "acme-marketing"],
  "actor": { ... },
  "payload": { ... }
}
```

Auth headers (Splunk HEC token, Datadog API key, Elasticsearch API key) are stored encrypted at rest using the same AES-256-GCM pattern as provider credentials, and decrypted at dispatch time. Batch size and flush interval are configurable (default 100 events / 5s). Retry with exponential backoff covers transient 5xx failures; persistent 4xx failures go to a dead-letter queue with an alert.

Per-channel filtering options:
- **Severity floor** — emit only `warning+` or `critical+` events.
- **Event-type filter** — include only specific types (`admin:credential.*`, `traffic:hook_reject`, etc.).
- **Tenant scope** — emit only events from a specific org subtree.

## Where it sits

- Audit event producer library: `packages/shared/audit/` (`event_types.go`, `body.go`, `Writer` interface).
- AI Gateway audit writer: `packages/ai-gateway/internal/platform/audit/`.
- SIEM bridge: `packages/nexus-hub/internal/traffic/siem/` (`bridge.go`, `classify.go`, `formatter.go`, `sink.go`).
- SIEM admin API: `packages/control-plane/internal/observability/siem/handler/`.

Transport from data-plane services to the Hub uses NATS JetStream with NDJSON spool fallback when MQ is unreachable. Agent traffic uploads via encrypted SQLite queue drained to Hub over mTLS when online.

## How to enable and configure

**Body capture**: navigate to **AI Gateway → Providers** (per provider) or **Compliance Proxy → Domains** (per host). Enable body capture and select the streaming compliance mode appropriate for the deployment's latency vs blocking requirements.

**SIEM forwarding**: navigate to **Settings → SIEM**:
1. Add a channel with the destination webhook URL and auth headers.
2. Set the event-type filter and severity floor.
3. Use the **Test** button to verify the channel receives a sample event.
4. Save. The bridge begins forwarding events within seconds.

The Traffic Monitor page surfaces audit data immediately after requests complete. The Audit Log page supports full-text search by provider, model, virtual key, organisation, hook decision, and time range.

---

## Canonical docs

- [`audit-pipeline-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md) — two audit tables, emission flow, MQ transport, spillstore integration, agent upload fallback
- [`siem-bridge-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/observability/siem-bridge-architecture.md) — HTTPSink, channel filtering, batching, retry, event payload shape, auth header encryption

**Adjacent wiki pages**: [Feature PII Redaction](Feature-PII-Redaction) · [Feature IAM And SSO](Feature-IAM-And-SSO) · [Spillstore](Spillstore) · [Control Plane Audit Log](Control-Plane-Audit-Log) · [Security Audit Forensics](Security-Audit-Forensics) · [Features Index](Features-Index)

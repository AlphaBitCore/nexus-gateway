# Control Plane SIEM Bridge

*Audience: compliance leads integrating Nexus with an external SIEM platform.*

The SIEM bridge is the outbound forwarder that ships `traffic_event` and `AdminAuditLog` rows from Nexus to external compliance systems. It supports Splunk HEC, Datadog Logs API, Elasticsearch `_bulk`, and any other receiver that accepts an HTTPS POST with a JSON body. The bridge only pushes — it does not pull from SIEMs.

---

## Architecture

The bridge runs inside Nexus Hub and polls Postgres directly (not MQ):

| Component | Role |
|---|---|
| `packages/nexus-hub/internal/traffic/siem/bridge.go` | Poll loop over `traffic_event` and `AdminAuditLog` |
| `packages/nexus-hub/internal/traffic/siem/classify.go` | Event filtering and decoration |
| `packages/nexus-hub/internal/traffic/siem/formatter.go` | JSON payload shaping |
| `packages/nexus-hub/internal/traffic/siem/sink.go` | `HTTPSink` — vendor-agnostic HTTPS POST |
| `packages/control-plane/internal/observability/siem/handler/` | CP admin API for SIEM channel management |

SIEM channel config lives in the `system_metadata` table under the key `siem.config`. There is no separate SIEM table.

## Sink model

A single `HTTPSink` type handles all vendor targets. Vendor-specific configuration is expressed through URL and HTTP headers:

| Vendor | URL | Auth header |
|---|---|---|
| Splunk HEC | `https://<host>/services/collector/event` | `Authorization: Splunk <token>` |
| Datadog | `https://http-intake.logs.datadoghq.com/v1/input` | `DD-API-KEY: <key>` |
| Elasticsearch | `https://<host>/_bulk` | `Authorization: ApiKey <key>` |
| Generic webhook | Any HTTPS URL | Any header |

Auth header values are stored encrypted at rest using the same AES-256-GCM pattern as provider credentials.

## Event payload shape

```json
{
  "event_id": "01HXYZ...",
  "trace_id": "...",
  "request_id": "...",
  "schema_version": 3,
  "emitted_at": "2026-05-16T10:00:00Z",
  "ingested_at": "2026-05-16T10:00:01Z",
  "source": "nexus.ai-gateway",
  "event_type": "admin:provider.update",
  "severity": "info",
  "org_id": "org-acme",
  "org_ancestor_path": ["nexus", "acme-holdings", "acme-marketing"],
  "actor": { ... },
  "resource": { ... },
  "payload": { ... }
}
```

`schema_version` lets receivers tolerate forward evolution. The `severity` field is derived from the event type at emit time — `system:kill_switch.activated` maps to `critical`, `admin:credential.revoke` maps to `info`. The bridge does not create new event types; it forwards what the audit pipeline already emits.

## Event filtering

Per channel, admins configure:

- **Severity floor** — forward only `warning+` or `critical+` events (default: all).
- **Event-type filter** — forward only specific event types (`admin:credential.*`, `traffic:hook_reject`, etc.).
- **Tenant scope** — forward only events from a specific org subtree.

## Batching and retry

`HTTPSink` POSTs a JSON array per batch. Default: 100 events / 5-second flush interval. Retry on failure:

| Failure | Behaviour |
|---|---|
| Channel 5xx / timeout | Retry with exponential backoff (3 attempts); then drop to in-process DLQ |
| Channel 4xx (auth, malformed) | No retry; DLQ + alert |
| DLQ full | Alert; oldest events drop |

The DLQ is in-process per channel and does not survive Hub restart. For strict at-least-once delivery, point the channel at a durable intermediate queue (e.g., a customer-managed Kafka topic).

## Multiple channels

Each tenant can configure multiple channels simultaneously. A typical setup:

- Webhook → PagerDuty for `critical` events.
- Splunk HEC → security data lake for all events.
- Datadog → ops dashboards for `warning+` events.

Channels are independent; a failing channel does not affect others.

## Test endpoint

Admin → CP UI → Infrastructure → SIEM → Channels → "Test" sends a synthetic event with `_test: true` and `event_type: "system:siem.test"`. Receivers should filter `_test=true` from compliance dashboards. The test result (delivery success/failure + latency) is recorded in the admin audit log.

## Admin UI

The SIEM bridge is managed from the Infrastructure → SIEM page (`/infrastructure/siem`), gated by `admin:audit-log.read`. Channel CRUD is at `/api/admin/siem`.

---

## Canonical docs

- [`siem-bridge-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/observability/siem-bridge-architecture.md) — bridge components, sink model, payload shape, retry, channels-per-tenant
- [`audit-pipeline-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md) — upstream pipeline that produces the events the bridge forwards

**Adjacent wiki pages**: [Control Plane Audit Log](Control-Plane-Audit-Log) · [Control Plane Alerting Rules](Control-Plane-Alerting-Rules) · [Control Plane Infrastructure Pages](Control-Plane-Infrastructure-Pages) · [Feature Audit And SIEM](Feature-Audit-And-SIEM) · [Security Audit Forensics](Security-Audit-Forensics)

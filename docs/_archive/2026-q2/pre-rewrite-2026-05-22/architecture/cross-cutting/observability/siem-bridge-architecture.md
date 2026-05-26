---
doc: siem-bridge-architecture
area: cross-cutting
service: observability
tier: 1
updated: 2026-05-20
---

# SIEM Bridge Architecture

> **Tier 2 architecture doc.** Read when touching `packages/nexus-hub/internal/traffic/siem/` or `packages/control-plane/internal/observability/siem/`. The bridge fans audit / traffic events out to external SIEM platforms (Splunk HEC, Datadog, Elastic, or any generic HTTP webhook) via a single sink type.

The SIEM bridge is the **outbound** side of the audit pipeline — internal events flow to external compliance systems for retention, SOC review, regulatory reporting. The bridge does NOT pull from SIEMs; it only pushes.

---

## 1. Where the bridge lives

| Component | Role |
|---|---|
| `packages/nexus-hub/internal/traffic/siem/` | The bridge itself — `bridge.go` polls `traffic_event` (and admin/agent rows when configured), `classify.go` filters / decorates events, `formatter.go` emits JSON, `sink.go` ships them out via `HTTPSink`. |
| `packages/control-plane/internal/observability/siem/handler/` | CP admin API for SIEM settings (GET / PUT `/api/admin/settings/siem`, POST `/api/admin/settings/siem/test`, GET `/api/admin/settings/siem/event-types`). Stores config in `system_metadata` under key `siem.config`. |

The bridge is a single Hub-side service today. There is no separate `packages/shared/siem/` package and no in-line compliance-proxy SIEM path — all SIEM forwarding goes through the Hub bridge over the same `traffic_event` table the rest of the platform reads.

## 2. Sink

There is a single sink type — `HTTPSink` (`packages/nexus-hub/internal/traffic/siem/sink.go`) — that POSTs a JSON batch to a configured webhook URL with a configurable header map. It is intentionally vendor-agnostic: by configuring different URLs + headers, the same sink serves Splunk HEC (`Authorization: Splunk <token>`), Datadog Logs API (`DD-API-KEY: <key>`), Elasticsearch `_bulk`, or any other receiver that accepts an HTTPS POST with a JSON body.

Auth, batching, retry, and TLS verification are operator-configured per channel.

## 3. Event filtering

The bridge config exposes two filters (`packages/nexus-hub/internal/traffic/siem/bridge.go`):

- `TrafficMode` — controls which subset of `traffic_event` rows are dispatched (e.g., processed vs blocked).
- `EventTypes` — explicit allowlist of classified event-type strings; non-empty allowlist filters the merged batch via `FilterByEventTypes` before formatting.

Severity floors and per-tenant scope are not part of the current filter surface; the JSON formatter ships the whole row and downstream receivers do further filtering.

## 4. Event payload shape

`Event` is `map[string]any` (`packages/nexus-hub/internal/traffic/siem/sink.go:22`); each map carries the columns the bridge selected from `traffic_event` or `AdminAuditLog` plus a classified `eventType`. The JSON formatter marshals the batch as a JSON array; the CEF and Syslog formatters lift named fields (`eventType`, `sourceIp`, `actorLabel`, `timestamp`, `hookReason`, etc. — see `packages/nexus-hub/internal/traffic/siem/formatter.go`) into their respective wire shapes.

The Hub does NOT stamp a top-level `schema_version`; receivers should rely on field presence and ignore unknown keys.

## 5. Auth

Auth headers are part of the channel's header map (e.g., `Authorization: Splunk <token>`, `DD-API-KEY: <key>`). The header map is stored alongside the URL in the `siem.config` row of `system_metadata` (`packages/control-plane/internal/observability/siem/handler/siem.go:26`). Header values are currently stored as-is in `system_metadata` JSON; if at-rest encryption of secret-valued headers is required it must be added explicitly — there is no automatic AES-GCM wrapping today.

## 6. Batching

`HTTPSink` POSTs each batch as a JSON array (or CEF / syslog text depending on the configured `Formatter`). The bridge defaults to `PollInterval = 30s` and `BatchSize = 200` (`packages/nexus-hub/internal/traffic/siem/bridge.go:86-89`); both are overridable via the `siem.config` row, and the bridge re-reads them on every poll cycle.

## 7. Failure handling

The current bridge does NOT implement per-channel retry, exponential backoff, or a DLQ; a failed POST is logged and the bridge advances the checkpoint on the next successful poll cycle. If at-least-once delivery is required for a specific receiver, point that channel at a queue the receiver owns (e.g., a customer-managed Kafka or Splunk HEC indexer with its own retry).

## 8. Test endpoint

Admin → CP UI → SIEM Settings → "Test" hits `POST /api/admin/settings/siem/test` (`packages/control-plane/internal/observability/siem/handler/siem.go:22`), which emits a synthetic event with `message = "SIEM integration test event from Nexus Gateway"` and reports delivery success / failure + latency to the admin. The test action goes through the standard IAM `settings.update` gate so it lands in `AdminAuditLog` like other settings changes.

## 9. SIEM event types vs audit event types

`traffic_event` (data plane) and `AdminAuditLog` (admin actions) rows feed the bridge — they're the same events stored in Postgres. The bridge does NOT create new event types; it dispatches what the audit pipeline already emits.

The CEF and Syslog formatters synthesise a severity from the event-type prefix (`auth.login_failure`, `iam.*`, `credential.*`, `proxy.*`, `config.*` — see `formatter.go::cefSeverity` / `syslogSeverity`); the JSON formatter ships the row unchanged.

## 10. Single shared bridge

There is one process-wide SIEM bridge today; the `siem.config` row holds a single channel URL + header map. Multi-channel and per-tenant routing are not implemented — adding them requires extending the config row plus the dispatch loop in `bridge.go`.

<!-- 💡 harvest: nothing new — the "always-strip auth on outbound" pattern echoes forward-header-allowlist; both are about trust-boundary header hygiene. No new rule. -->

## 11. Sources

- `packages/nexus-hub/internal/traffic/siem/` — `bridge.go` (poll loop), `classify.go` (filter + decorate), `formatter.go` (JSON shape), `sink.go` (`HTTPSink`).
- `packages/control-plane/internal/observability/siem/handler/` — CP admin API for SIEM settings.
- `tools/db-migrate/schema.prisma` — `traffic_event` + `AdminAuditLog` are the upstream tables; SIEM channel config lives in the `system_metadata` row keyed `siem.config`, not in a dedicated table.

## 12. Cross-references

- `audit-pipeline-architecture.md` — upstream of the bridge.
- `mq-architecture.md` — the bridge reads Postgres directly, not MQ, today.
- `alerting-architecture.md` — sibling fan-out path; the same events may also feed alerts.
- `docs/users/features/cp-ui/infrastructure.md` — SIEM admin page.

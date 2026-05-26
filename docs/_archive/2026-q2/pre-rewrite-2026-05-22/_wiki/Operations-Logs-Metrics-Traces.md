# Operations Logs Metrics Traces

*Audience: operators investigating incidents, building dashboards, or tuning observability on a Nexus Gateway deployment.*

All five Nexus Gateway services emit structured JSON logs via `slog`, expose Prometheus metrics at `/metrics`, and propagate `trace_id` through the request pipeline for cross-service correlation. The diagnostics subsystem adds a per-service "verbose telemetry" mode for incident triage. This page maps each signal source to its location and explains how they connect.

---

## Structured logs

All Go services write JSON log lines. Each line includes `time`, `level`, `msg`, and zero or more structured fields.

```json
{
  "time": "2026-05-20T10:15:30.123Z",
  "level": "INFO",
  "msg": "request completed",
  "method": "POST",
  "path": "/v1/chat/completions",
  "status": 200,
  "latency_ms": 1234,
  "trace_id": "abc123..."
}
```

### Log file locations

| Service | Production (journald) | Local dev (on-disk) |
|---|---|---|
| Nexus Hub | `journalctl -u nexus-hub` | `packages/nexus-hub/logs/nexus-hub.log` |
| Control Plane | `journalctl -u nexus-control-plane` | `packages/control-plane/logs/control-plane.log` |
| AI Gateway | `journalctl -u nexus-aigw` | `packages/ai-gateway/logs/ai-gateway.log` |
| Compliance Proxy | `journalctl -u nexus-compliance-proxy` | `packages/compliance-proxy/logs/compliance-proxy.log` |
| Desktop Agent | platform log path via `platform.DefaultPaths()` | `packages/agent/logs/agent.log` |

Override without editing YAML: `LOG_FILE` (path) and `LOG_STACK_ON_ERROR` (`true`/`1`/`yes`).

### Log levels

| Level | When to use |
|---|---|
| `debug` | Verbose tracing; request/response bodies; hook execution details |
| `info` | Normal operations: job completions, startup messages, config apply |
| `warn` | Degraded state: Valkey unreachable, hook errors (fail-open), config staleness |
| `error` | Failures requiring attention: DB write errors, cert signing failures, panic recovery |

To enable debug logging without editing YAML, set `LOG_LEVEL=debug` in the service environment before restarting.

### Key log messages

| Message pattern | Level | Service | Action |
|---|---|---|---|
| `"job panic recovered"` | error | Hub | Scheduled job crashed — check stack trace |
| `"redis get failed"` / `"redis set failed"` | warn | any | Valkey connectivity issue |
| `"compliance hook error"` | warn | proxy / agent | Hook execution failure — check hook config |
| `"NDJSON fallback"` | warn | proxy | PostgreSQL unreachable; audit in fallback mode |
| `"config_apply_success"` | info | any | Shadow config applied; no restart needed |
| `"Marked devices offline"` | info | Hub | Devices with stale heartbeats; normal maintenance sweep |
| `"Data retention purge completed"` | info | Hub | Retention job ran successfully |

### AI Gateway body-level debugging

To inspect the exact request sent to providers and the raw response received, set `log.level: "debug"` in `ai-gateway.dev.yaml`. The gateway emits these structured fields at DEBUG level:

| Log message | Key fields | When |
|---|---|---|
| `"upstream request body"` | `format`, `url`, `body` (first 8 KB) | Before the upstream HTTP call |
| `"upstream response headers"` | `format`, `status`, `stream`, `content_type` | After the upstream responds |
| `"upstream stream body"` | `format`, `bytes_captured`, `body` (first 8 KB) | When the stream body closes |
| `"outbound http"` | `url`, `status`, `req_bytes`, `resp_bytes`, `duration_ms` | After the response is sent to the caller |

---

## Prometheus metrics

All four server services expose `/metrics` on their primary port. Configure Prometheus to scrape at 15–30 second intervals.

| Service | Metrics endpoint |
|---|---|
| Nexus Hub | `http://<host>:3060/metrics` |
| Control Plane | `http://<host>:3001/metrics` |
| AI Gateway | `http://<host>:3050/metrics` |
| Compliance Proxy | `http://<host>:9090/metrics` |

### AI Gateway key metrics

Namespace: configurable (typically `nexus_aigw`).

| Metric | Type | Labels | Description |
|---|---|---|---|
| `{ns}_requests_total` | counter | provider, model, endpoint, status | Total proxy requests |
| `{ns}_request_duration_seconds` | histogram | provider, model, endpoint | Request latency |
| `{ns}_tokens_total` | counter | provider, model, direction | Tokens (prompt / completion) |
| `{ns}_cache_hits_total` | counter | — | Response cache hits |
| `{ns}_errors_total` | counter | provider, error_type | Proxy errors |

### Control Plane key metrics

Namespace: `nexus_control_plane`.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `requests_total` | counter | method, path_group, status | Total admin API requests |
| `request_duration_seconds` | histogram | method, path_group | Admin API latency |
| `auth_failures_total` | counter | type | Auth failures (missing_auth, invalid_api_key, etc.) |
| `iam_denials_total` | counter | — | IAM policy denials |

### Compliance Proxy key metrics

| Metric | Labels | Description |
|---|---|---|
| `tunnels_active` | — | Active CONNECT tunnels (gauge) |
| `tunnels_total` | result | Lifetime tunnels by outcome (accepted / denied / error) |
| `cert_cache_hits_total` | layer (l1 / l2) | Certificate cache hits |
| `cert_cache_misses_total` | — | Cache misses requiring cert signing |
| `cert_sign_ms` | — | Per-mint cert-sign latency (histogram) |
| `audit_queue_depth` | — | Current audit write queue depth (gauge) |
| `audit_ndjson_active` | — | 1 when NDJSON fallback active (gauge) |
| `redis_available` | — | Valkey reachability: 1 = reachable, 0 = unreachable |

### Critical Prometheus thresholds

| Metric | Condition | Meaning |
|---|---|---|
| `audit_ndjson_active` | = 1 | PostgreSQL unreachable; audit pipeline in fallback |
| `audit_write_errors_total` | rising | Persistent DB write failures |
| `tunnels_active` | > 80% of `maxConcurrentTunnels` | Near tunnel capacity |
| `redis_available` | = 0 | Valkey unreachable; cert cache degraded |
| `auth_failures_total` | spike | Possible brute force or misconfiguration |

---

## Trace ID propagation

Every request that enters Nexus Gateway via the AI Gateway, Compliance Proxy, or Agent carries a `trace_id`. The same `trace_id` appears in:

- The `traffic_event.trace_id` column in PostgreSQL
- Every log line emitted during that request (structured field `trace_id`)
- OTel spans (when an OTel exporter is configured)
- The audit timeline in the Control Plane UI when clicking an alert

To trace a specific request across services:

```bash
# Find traffic_event row by trace_id
docker exec $(docker ps --filter "name=postgres" -q | head -1) \
  psql -U postgres -d nexus_gateway -c \
  "SELECT id, source, target_host, status_code, latency_ms, timestamp
     FROM traffic_event
    WHERE trace_id = '<trace-id>'
    ORDER BY timestamp;"

# Find log lines for the same trace_id
journalctl -u nexus-aigw --since "1 hour ago" | grep '<trace-id>'
```

---

## Diagnostics mode

The diagnostics subsystem provides a per-service "verbose telemetry" mode that activates DEBUG logging, 100% OTel sampling, and body capture for a bounded duration (default 1 hour, max 24 hours).

Enable via the Control Plane UI at **Infrastructure → Diagnostic Mode**, or via the API:

```bash
# Enable diag mode on an AI Gateway Thing for 30 minutes
THING_ID=$(cp_curl "/api/admin/nodes" | jq -r '.[] | select(.type=="ai-gateway") | .id' | head -1)
cp_curl -X POST "/api/admin/things/$THING_ID/diagnostic-mode" \
  -H 'Content-Type: application/json' \
  -d '{"durationMinutes": 30, "reason": "investigating latency spike"}'
```

Diag mode auto-expires at the configured duration — the mode is never left on indefinitely.

### Event triage surface

All `WARN`/`ERROR` log records from every service are available in the Control Plane UI at **Infrastructure → Recent Errors**. Records are grouped by `messageHash` (a stable hash of the message template) with count and last-seen timestamp. Operators can apply silence rules to collapse known-noise messages.

Silence rules do not delete records — silenced records still land on disk. They only collapse the `/infrastructure/errors` list and suppress the alert pipeline for that message hash.

---

## Grafana dashboard suggestions

### AI Gateway performance

- Request rate by provider and model (counter rate over 5m)
- Token throughput by direction (prompt / completion tokens/s)
- Request latency by provider: p50 / p95 / p99 from `request_duration_seconds`
- Cache hit rate: `cache_hits_total / requests_total`

### Compliance proxy health

- Active tunnels gauge with `maxConcurrentTunnels` reference line
- Cert cache hit rate: `cert_cache_hits_total{layer="l1"} / (hits + misses)`
- Audit queue depth over time
- NDJSON fallback status (binary gauge)

### Infrastructure

- Valkey `redis_available` gauge per service
- NATS JetStream consumer pending count (via `/jsz` endpoint)
- PostgreSQL connection pool utilization
- Service health (`/healthz` probe status)

---

## Canonical docs

- [`monitoring.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/monitoring.md) — full Prometheus metrics catalog, health endpoints, and Grafana dashboard suggestions
- [`diag-event-triage-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/observability/diag-event-triage-architecture.md) — diagnostics subsystem architecture: slog sink, reconnect buffer, silence rules, support bundle
- [`local-dev-debugging.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/local-dev-debugging.md) — AI Gateway body-level debug fields, service log paths, admin API `cp_curl` helper

**Adjacent wiki pages**: [Operations Day 2 Cheatsheet](Operations-Day-2-Cheatsheet) · [Operations Capacity Performance](Operations-Capacity-Performance) · [Operations FAQ](Operations-FAQ) · [Observability Stack](Observability-Stack) · [Operations Runbook Index](Operations-Runbook-Index)

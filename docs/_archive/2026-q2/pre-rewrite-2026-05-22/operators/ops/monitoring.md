# Monitoring Guide

## Overview

All Nexus Gateway Go services expose Prometheus metrics and use `slog` structured logging. This guide lists the key metrics, logging format, and health check endpoints for operational monitoring.

---

## Prometheus Metrics Endpoints

| Service | Endpoint | Port |
|---------|----------|------|
| Nexus Hub | `GET /metrics` | 3060 |
| Control Plane | `GET /metrics` | 3001 |
| AI Gateway | `GET /metrics` | 3050 |
| Compliance Proxy | `GET /metrics` | 9090 |

Every Go service exposes `/metrics` (see Deployment Guide → Health Check
Endpoints "All Go services" row). Configure Prometheus to scrape these
endpoints at 15-30 second intervals.

---

## Control Plane Metrics

Namespace: `nexus_control_plane`

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `requests_total` | Counter | method, path_group, status | Total HTTP requests |
| `request_duration_seconds` | Histogram | method, path_group | Request latency distribution |
| `auth_failures_total` | Counter | type | Auth failures (missing_auth, invalid_api_key, session_lookup_error) |
| `iam_denials_total` | Counter | - | Total IAM policy denials |
| `enrollments_total` | Counter | status | Agent enrollments (success, failed, rate_limited) |

---

## Compliance Proxy Metrics

The compliance proxy uses the **dotted opsmetrics** naming convention.
The old `nexus_compliance_proxy_*` namespace prefix has been **dropped**
per the CLAUDE.md no-backcompat rule (see
`packages/compliance-proxy/internal/metrics/prometheus.go:11`). When
Prometheus scrapes the dotted names, the client library translates `.`
to `_` for the wire format, so metric line names appear as e.g.
`cert_cache_hits_total{layer="l1"}` — the canonical names below are the
source of truth (see also `docs/operators/ops/runbooks/compliance-proxy-smoke.md`
§7.2).

### Connection Metrics

| Canonical name | Type | Labels | Description |
|--------|------|--------|-------------|
| `tunnels.active` | Gauge | - | Active CONNECT tunnels |
| `tunnels.total` | Counter | result | Lifetime CONNECTs by accept / deny / error |

### TLS / Certificate Metrics

| Canonical name | Type | Labels | Description |
|--------|------|--------|-------------|
| `tls_handshake_ms` | Histogram | - | TLS bump handshake latency. Registered by `shared/tlsbump.RegisterMetrics` (E55) so the agent shares the same instrument. |
| `cert_cache.hits_total` | Counter | layer (`l1`, `l2`) | Certificate cache hits by tier |
| `cert_cache.misses_total` | Counter | - | Cache misses (full signing required) |
| `cert_cache.size` | Gauge | - | Current entry count |
| `cert_sign_ms` | Histogram | - | Per-mint cert sign latency |
| `cert_prewarm.duration_ms` | Gauge | - | Last prewarm run duration |
| `pinning.passthrough_total` | Counter | status | Pinning bypass events |

### Upstream Metrics

| Canonical name | Type | Labels | Description |
|--------|------|--------|-------------|
| `upstream_request_ms` | Histogram | - | Upstream request latency. Registered by `shared/tlsbump.RegisterMetrics` (E55) so the agent shares the same instrument. |

### Redis Health

| Canonical name | Type | Description |
|--------|------|-------------|
| `redis.available` | Gauge | 1=reachable, 0=unreachable |

### Attestation

| Canonical name | Type | Labels | Description |
|--------|------|--------|-------------|
| `attestation.verify_total` | Counter | outcome (`valid`, `missing`, `invalid_sig`, `expired`, `replayed`, `unknown_agent`, `disabled`) | Agent-attestation verification outcomes |

### Kill Switch

| Canonical name | Type | Description |
|--------|------|-------------|
| `killswitch.active` | Gauge | 0/1; flipped by the runtime kill-switch API |

### Config Cache Metrics

Config refresh observability lives in the Hub shadow audit trail; no
dedicated `config_cache_*` counters are registered in the
compliance-proxy metrics registry.

### Audit Metrics

The `audit_*` family is not registered in
`packages/compliance-proxy/internal/metrics/prometheus.go`. Persistent
DB failures surface via `WARN`/`ERROR` slog entries and the
`hooksPipeline` traffic_event column.

### Compliance Pipeline Metrics

Compliance-pipeline counters are owned by the shared
`packages/shared/policy/hooks/` package and shared verbatim across
compliance-proxy and AI gateway. Canonical names use the dotted
opsmetrics form:

| Canonical name | Type | Labels | Description |
|--------|------|--------|-------------|
| `compliance.pipeline.duration_ms` | Histogram | - | Total pipeline execution duration |
| `compliance.hook.duration_ms` | Histogram | hook | Per-hook execution duration |
| `compliance.hook.decision_total` | Counter | hook, decision | Hook decisions by hook and outcome |
| `compliance.pipeline.decision_total` | Counter | decision | Pipeline decisions (APPROVE, REJECT_HARD, REJECT_SOFT) |
| `compliance.hook.error_total` | Counter | hook | Hook execution errors |
| `compliance.hook.timeout_total` | Counter | hook | Hook timeouts |

---

## AI Gateway Metrics

Namespace: configurable (passed to `NewRecorder`)

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `{ns}_requests_total` | Counter | provider, model, endpoint, status | Total proxy requests |
| `{ns}_request_duration_seconds` | Histogram | provider, model, endpoint | Request latency |
| `{ns}_tokens_total` | Counter | provider, model, direction | Tokens processed (prompt/completion) |
| `{ns}_errors_total` | Counter | provider, error_type | Proxy errors |

---

## Key Metrics to Watch

### Critical (page immediately)

| Metric | Condition | Meaning |
|--------|-----------|---------|
| `/healthz` non-200 | Any service | Service unhealthy or shutting down |
| `tunnels.active` > 80% of `maxConcurrentTunnels` | Compliance proxy | Approaching tunnel capacity |
| `WARN` slog "NDJSON fallback" sustained | Compliance proxy | PostgreSQL likely unreachable; audit in fallback mode (no dedicated metric registered; alert on the log line) |
| `ERROR` slog "audit batch write failed" sustained | Compliance proxy | Persistent DB write failures (no dedicated metric registered; alert on the log line) |

### High (investigate within 15 min)

| Metric | Condition | Meaning |
|--------|-----------|---------|
| `redis.available` = 0 | Compliance proxy | Redis unreachable; cert cache L2 degraded (L1 LRU still serves) |
| `cert_sign_ms` P99 > 100ms | Compliance proxy | Certificate signing slow |
| `tls_handshake_ms` P99 > 250ms | Compliance proxy | TLS bump adding latency |
| `auth_failures_total` spike | Control plane | Possible brute force or misconfiguration |

### Warning (review daily)

| Metric | Condition | Meaning |
|--------|-----------|---------|
| `cert_cache.misses_total` / total > 50% | Compliance proxy | Poor cache hit rate; check Redis connectivity |
| `compliance.hook.error_total` increasing | Any | Hook execution failures |
| `compliance.hook.timeout_total` increasing | Any | Hooks exceeding timeout |

---

## Health Check Endpoints

| Service | Endpoint | Port | Healthy | Unhealthy |
|---------|----------|------|---------|-----------|
| Control Plane | `GET /healthz` | 3001 | 200 `{"status":"ok"}` | N/A (process crash = no response) |
| Compliance Proxy | `GET /healthz` | 9090 | 200 `{"status":"ok"}` | 503 `{"status":"shutting_down"}` |

### Load Balancer Configuration

Use `/healthz` (not a deep-check endpoint) for LB health checks. This prevents cascading removal of all proxy instances when a shared dependency (PostgreSQL or Redis) is temporarily down. The proxy is still functional in degraded mode.

Recommended LB health check settings:
- Interval: 5 seconds
- Timeout: 3 seconds
- Unhealthy threshold: 3 consecutive failures
- Healthy threshold: 2 consecutive successes

---

## Structured Logging

All Go services use `slog` (structured logging) with JSON output by default.

### Log Format

```json
{
  "time": "2026-04-12T10:15:30.123Z",
  "level": "INFO",
  "msg": "request completed",
  "method": "POST",
  "path": "/v1/chat/completions",
  "status": 200,
  "latency_ms": 1234
}
```

### Log Levels

| Level | Usage |
|-------|-------|
| `debug` | Verbose tracing, hook execution details |
| `info` | Normal operations, job completions, startup messages |
| `warn` | Degraded state (Redis down, hook errors with fail-open), config staleness |
| `error` | Failures requiring attention (DB write errors, cert signing failures) |

### Configuration

- **Control Plane**: `LOG_LEVEL` env var or `log.level` in YAML (default: `info`)
- **Compliance Proxy**: `logging.level` in YAML (default: `info`)

### Key Log Messages to Alert On

| Message Pattern | Level | Meaning |
|----------------|-------|---------|
| `"job panic recovered"` | error | Scheduled job crashed and recovered |
| `"redis get failed"` / `"redis set failed"` | warn | Redis connectivity issue |
| `"compliance hook error"` | warn | Hook execution failure |
| `"unknown hook implementation"` | warn | Hook config references non-existent implementation |
| `"NDJSON fallback"` | warn | Audit writing to local files (DB unreachable) |
| `"Marked devices offline"` | info | Agent devices with stale heartbeats |
| `"Data retention purge completed"` | info | Retention job completed successfully |

---

## Grafana Dashboard Suggestions

### Compliance Proxy Overview

- Active connections gauge (`tunnels.active`)
- Connection rate (`tunnels.total` by `result`)
- TLS handshake latency P50/P95/P99 (`tls_handshake_ms`)
- Certificate cache hit rate by tier (`cert_cache.hits_total{layer}` vs `cert_cache.misses_total`)
- Upstream request latency (`upstream_request_ms`)
- Kill-switch state (`killswitch.active`) and attestation outcomes (`attestation.verify_total{outcome}`)

### Compliance Pipeline

- Pipeline decision rate (APPROVE vs REJECT_HARD vs REJECT_SOFT)
- Per-hook latency distribution
- Hook error and timeout rates
- Data classification distribution

### AI Gateway

- Request rate by provider and model
- Token throughput (prompt vs completion)
- Request latency by provider P50/P95/P99
- Error rate by provider and type

### Infrastructure Health

- Redis available gauge (`redis.available`)
- PostgreSQL connection pool utilization
- NDJSON-fallback + audit-write-error trends — track via slog `WARN`/`ERROR` rate (no dedicated metric in current build)

# Status Page Metrics Enhancement — Design Spec

**Date:** 2026-04-15
**Status:** Approved
**Scope:** Enhance the `/status` Overview tab with aggregated Prometheus service metrics, Go runtime indicators, and offline instance removal.

---

## 1. Context & Motivation

Nexus Gateway runs multiple services (control-plane, ai-gateway, compliance-proxy), each with multiple instances. Every instance exposes Prometheus `/metrics` with rich operational data (request counters, latency histograms, connection gauges, Go runtime stats). However, the current Status page only shows instance health status and heartbeat — none of the Prometheus metrics are surfaced in the UI.

**Goal:** Give SRE/Ops and Platform Admins a single-pane view of service-level operational metrics and the ability to clean up stale offline instances, without requiring direct Prometheus/Grafana access.

## 2. Design Decisions (from brainstorm)

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Persona | SRE/Ops + Platform Admin | Both need metrics visibility and fleet management |
| Display granularity | Service-level aggregation cards | One card per service with key metrics; no instance-level metric drill-down in V1 |
| Data fetch strategy | Control-plane backend aggregation | Frontend cannot access other services directly; backend handles parsing, caching, timeout |
| Storage | None — real-time pull with 15s in-memory cache | Runtime snapshots are ephemeral; historical data already covered by rollup tables |
| Layout | Enhance existing Overview tab | Avoids tab proliferation; Overview = "at-a-glance" by nature |
| Offline instance removal | Single remove per instance | Remove button on each offline row; no batch removal in V1 |
| Refresh strategy | Auto-refresh every 15s | Aligned with backend cache TTL; stops when tab is not active |
| Go runtime metrics | Included | goroutines, heap, GC, threads per service |

## 3. Backend API

### 3.1 `GET /api/admin/service-metrics`

**Purpose:** Aggregate Prometheus metrics from all healthy service instances, return structured JSON.

**IAM Permission:** `admin:ReadSettings`

**Flow:**
1. Read all instances with `status=healthy` from the instance registry
2. Parallel HTTP GET to each instance's `/metrics` endpoint (3s timeout per instance)
3. Parse Prometheus exposition format using `prometheus/common/expfmt`
4. Extract whitelisted metrics only (see Section 4)
5. Aggregate by service (see Section 4.2)
6. Cache result in memory (15s TTL, singleflight to prevent thundering herd)
7. Return JSON

**Response Schema:**

```json
{
  "cachedAt": "2026-04-15T09:00:00Z",
  "fetchErrors": ["instance-id: connection refused"],
  "services": {
    "control-plane": {
      "instances": 1,
      "metrics": {
        "requests_total": 156,
        "request_duration_p50_ms": 8.2,
        "request_duration_p99_ms": 45.1,
        "auth_failures_total": 0,
        "iam_denials_total": 0
      },
      "runtime": {
        "goroutines": 27,
        "heap_alloc_mb": 1.8,
        "heap_sys_mb": 11.1,
        "gc_pause_p50_ms": 0.21,
        "gc_count": 48,
        "threads": 13
      }
    },
    "ai-gateway": {
      "instances": 1,
      "metrics": {
        "requests_total": 42,
        "request_duration_p50_ms": 520,
        "request_duration_p99_ms": 2100,
        "tokens_prompt_total": 1200,
        "tokens_completion_total": 8500,
        "errors_total": 3
      },
      "runtime": {
        "goroutines": 0,
        "heap_alloc_mb": 0,
        "heap_sys_mb": 0,
        "gc_pause_p50_ms": 0,
        "gc_count": 0,
        "threads": 0
      }
    },
    "compliance-proxy": {
      "instances": 1,
      "metrics": {
        "connections_active": 0,
        "connections_total": 15,
        "connections_rejected": 0,
        "tls_handshake_p50_ms": 5.2,
        "cert_cache_hit_rate": 0.85,
        "audit_queue_depth": 0,
        "redis_available": true
      },
      "runtime": {
        "goroutines": 27,
        "heap_alloc_mb": 1.9,
        "heap_sys_mb": 19.2,
        "gc_pause_p50_ms": 0.21,
        "gc_count": 48,
        "threads": 13
      }
    }
  }
}
```

**Error Handling:**
- Instance unreachable (timeout/refused): skip that instance, add to `fetchErrors` array
- All instances offline for a service: that service entry has `"instances": 0` and null metrics
- Cache miss + fetch in progress: subsequent requests wait via singleflight (no duplicate fetches)

### 3.2 `DELETE /api/admin/instances/:id`

**Purpose:** Remove an offline instance registration record.

**IAM Permission:** `admin:WriteSettings`

**Constraints:**
- Only allows removal of instances with `status=offline`
- If instance is healthy/degraded/unhealthy: returns `409 Conflict`
- If instance ID not found: returns `404 Not Found`
- Backend re-checks status at execution time (race condition guard)

**Response:** `204 No Content`

## 4. Prometheus Metrics Parsing

### 4.1 Whitelist

Only these metrics are extracted from Prometheus text; all others are ignored.

**Go Runtime (all services):**
- `go_goroutines` (gauge)
- `go_memstats_heap_alloc_bytes` (gauge)
- `go_memstats_heap_sys_bytes` (gauge)
- `go_gc_duration_seconds` (summary — extract quantile=0.5)
- `go_threads` (gauge)

**control-plane:**
- `nexus_control_plane_requests_total` (counter)
- `nexus_control_plane_request_duration_seconds` (histogram)
- `nexus_control_plane_auth_failures_total` (counter)
- `nexus_control_plane_iam_denials_total` (counter)

**ai-gateway** (namespace varies, match by suffix):
- `*_requests_total` (counter)
- `*_request_duration_seconds` (histogram)
- `*_tokens_total` (counter, labels: direction=prompt|completion)
- `*_errors_total` (counter)

**compliance-proxy:**
- `nexus_compliance_proxy_connections_active` (gauge)
- `nexus_compliance_proxy_connections_total` (counter, label: status)
- `nexus_compliance_proxy_tls_handshake_duration_seconds` (histogram)
- `nexus_compliance_proxy_cert_cache_hits_total` (counter)
- `nexus_compliance_proxy_cert_cache_misses_total` (counter)
- `nexus_compliance_proxy_audit_queue_depth` (gauge)
- `nexus_compliance_proxy_redis_available` (gauge)

### 4.2 Aggregation Strategy (multi-instance)

| Metric Type | Aggregation |
|-------------|-------------|
| Counter (requests_total, errors_total, tokens_total) | Sum across instances |
| Gauge (goroutines, heap_alloc, connections_active, audit_queue_depth) | Sum (total fleet view) |
| Gauge (redis_available) | All 1 → true; any 0 → false |
| Histogram (latency, handshake) | Merge buckets, compute p50/p99 from merged |
| Summary (GC pause) | Average of p50 quantile values |
| Cache hit rate | `sum(hits) / (sum(hits) + sum(misses))` |

### 4.3 Parsing Library

Use `github.com/prometheus/common/expfmt` — the official Prometheus text parser. Already an indirect dependency via `prometheus/client_golang`. No custom parsing needed.

### 4.4 Cache Implementation

```go
type metricsCache struct {
    mu        sync.RWMutex
    data      *ServiceMetricsResponse
    fetchedAt time.Time
    ttl       time.Duration // 15s
}
```

Uses `golang.org/x/sync/singleflight` to deduplicate concurrent cache-miss fetches.

## 5. Frontend Changes

### 5.1 Overview Tab Layout (enhanced)

```
Stats Row (unchanged)
  → Infrastructure Card (unchanged)
  → (NEW) Service Metrics section
      ├── control-plane card
      ├── ai-gateway card
      └── compliance-proxy card
  → Services Card (enhanced: Actions column with Remove button)
```

### 5.2 Service Metrics Cards

Three cards in a horizontal CSS Grid (3 columns, stack on narrow screens).

Each card has two sections:

**Business Metrics** (top) — service-specific key indicators:
- control-plane: Requests, Latency p50/p99, Auth Failures, IAM Denials
- ai-gateway: Requests, Latency p50/p99, Tokens (prompt/completion), Errors
- compliance-proxy: Active Connections, Total/Rejected, TLS Handshake p50, Cert Cache Hit Rate, Audit Queue, Redis Status

**Runtime** (bottom) — identical layout for all services:
- Goroutines, Heap (alloc / sys), GC Pause p50, GC Count, Threads

Card header shows service name with a colored health dot (derived from existing `services` summary) and instance count.

### 5.3 Instance Table — Actions Column

- New rightmost column: **Actions**
- Offline instances: show a "Remove" button (secondary variant, danger color)
- Non-offline instances: empty cell
- Click triggers a confirmation dialog: "Remove instance {instanceId}?"
- On confirm: `DELETE /api/admin/instances/:id` → success removes row from table, shows success toast
- On error: show error toast, no row removal

### 5.4 Auto-Refresh

- `useEffect` with `setInterval(15_000)` for `getServiceMetrics()` — runs when Overview tab is active
- Existing `listInstances()` call also refreshed on the same interval
- Tab switch away → `clearInterval` (same pattern as existing Jobs tab)
- On fetch error: retain last successful data, show "Last updated: Xs ago" indicator next to section title

### 5.5 Empty / Error States

| State | Display |
|-------|---------|
| No healthy instances (all offline) | "No healthy instances — metrics unavailable" message in metrics section |
| Partial fetch failures | Cards render available data; `fetchErrors` shown as a small warning banner above metrics section |
| API unreachable | Retain stale data with "Stale data — last updated Xs ago" warning |

## 6. File Change List

### Backend (Go)

| File | Change |
|------|--------|
| `packages/control-plane/internal/handler/admin_extras.go` | Add `ServiceMetrics` handler, `DeleteInstance` handler, register routes |
| `packages/control-plane/internal/handler/promparse.go` | **New** — Prometheus text parsing, whitelist extraction, multi-instance aggregation, singleflight cache |
| `packages/control-plane/internal/handler/promparse_test.go` | **New** — Unit tests for parsing and aggregation |

### Frontend (React/TypeScript)

| File | Change |
|------|--------|
| `packages/control-plane-ui/src/api/services/system.ts` | Add `getServiceMetrics()`, `deleteInstance(id)` |
| `packages/control-plane-ui/src/pages/status/StatusPage.tsx` | Insert Service Metrics cards in Overview tab, add Actions column to instance table |
| `packages/control-plane-ui/src/pages/status/StatusPage.module.css` | Styles for metrics cards grid, runtime section, remove button |
| `packages/control-plane-ui/src/i18n/locales/en/pages.json` | i18n keys for all new labels |
| `packages/control-plane-ui/src/i18n/locales/zh/pages.json` | Chinese translations |
| `packages/control-plane-ui/src/i18n/locales/es/pages.json` | Spanish translations |

## 7. Explicitly Out of Scope

- **No historical trend charts** — covered by existing rollup tables + Realtime tab
- **No batch offline removal** — single instance removal only in V1
- **No instance-level metric drill-down** — service-level aggregation only in V1
- **No alerting thresholds** — separate feature
- **No Prometheus metric storage in Postgres** — real-time pull only
- **No WebSocket push** — polling is sufficient for 15s refresh

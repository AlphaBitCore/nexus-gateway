# Status Page Layout Redesign — Design Spec

**Date:** 2026-04-15
**Status:** Approved
**Scope:** Redesign the `/status` Overview tab layout for clearer visual hierarchy, add collapsible service cards with instance drill-down, add `/status/services/:serviceName` detail page with metric dimension breakdown.

---

## 1. Context & Motivation

The current Overview tab stacks 4 flat blocks (Stats Row, Infrastructure Card, Service Metrics Card, Services + Instance Table) with no clear visual hierarchy. Issues:

- Service Metrics and Services Card overlap in purpose (both show per-service health)
- The instance table mixes all services together with no grouping
- No way to drill down into metric dimensions (e.g., requests by path, tokens by provider)
- Runtime metrics (goroutines, heap, GC) take up significant space but are rarely the first thing to check

## 2. Design Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Breakdown type | Expand/collapse instances + metric dimension drill-down via sub-page | Both operational views needed |
| Drill-down interaction | Sub-page at `/status/services/:name` | Data volume justifies a full page, not a popover |
| Layout core issue | Visual hierarchy — keep data, improve layers | Admin needs overview at a glance |
| Runtime metrics | On card but collapsed by default | Useful for ops but not first priority |
| Instance table | Merge into each service card's collapsible area | Eliminates mixed table, natural grouping |
| Top section (Stats + Infra) | Keep but visually de-emphasize | Still useful, but service cards are the main focus |

## 3. Overview Tab — New Layout

### 3.1 Overall Structure

```
PageHeader: "System Status"

Stats Row (de-emphasized: smaller font, muted colors)
  → 6 stat cards: Uptime, Version, Instances, Go, Log Level, Maintenance

Infrastructure Bar (de-emphasized: compact inline indicators, no Card border)
  → ● DB ok  ● Redis connected  Config v42
  → Lighter weight than current Card; single-row horizontal layout

Service Cards (page focus, full-width stacked)
  → control-plane card
  → ai-gateway card
  → compliance-proxy card
```

### 3.2 Service Card Internal Structure

Each card has 4 layers:

**Header Row:**
- Health dot (colored by overall status)
- Service name (bold)
- Instance summary: "1 healthy / 2 total"
- `[View →]` link to `/status/services/:serviceName`

**Business Metrics Row (always visible):**
- Horizontal grid of key metric items
- control-plane: Requests, p50, p99, Auth Failures, IAM Denials
- ai-gateway: Requests, p50, p99, Tokens Prompt, Tokens Completion, Errors
- compliance-proxy: Active Conn, Total Conn, Rejected, TLS p50, Cert Cache Hit, Audit Queue, Redis

**Runtime Section (collapsed by default):**
- Click "Runtime ▸" to expand
- Shows: Goroutines, Heap Alloc/Sys, GC Pause p50, GC Count, Threads
- Click again to collapse

**Instances Section (collapsed by default):**
- Click "Instances ▸" to expand
- Shows a table of ONLY this service's instances (filtered, not mixed)
- Columns: Instance ID, Status, Uptime, Last Heartbeat, Checks, Actions
- Offline instances show "Remove" button
- Click again to collapse

### 3.3 Removed from Overview

- Bottom mixed instance table — fully removed; instances live inside each service card
- Original Services Card (service summary rows) — merged into service card headers
- Original Service Metrics Card — merged into service card business metrics

### 3.4 De-emphasis of Top Section

**Stats Row:**
- Reduce `statValue` font-size from 24px to 18px
- Reduce `statLabel` from 11px to 10px
- Use `color-text-secondary` instead of `color-text` for values

**Infrastructure:**
- Change from `<Card>` to a borderless compact bar
- Horizontal inline layout: `● DB ok  ● Redis connected  Config v42`
- Font size ~12px, muted colors
- Remove tooltips and help icons (keep accessible but less prominent)

## 4. Service Detail Page — Metric Dimension Breakdown

### 4.1 Route

`/status/services/:serviceName`

Registered in `shellRouteConfig.tsx`. Accessible from the `[View →]` link on each service card.

### 4.2 Page Layout

```
← Back to Status

PageHeader: "control-plane"    ● healthy    1 instance(s)

┌─ Business Metrics (full-width, larger) ────────────────┐
│ Same aggregated metrics as Overview card, bigger layout │
└────────────────────────────────────────────────────────┘

┌─ Metric Breakdown ────────────────────────────────────┐
│ Select metric: [Requests ▾]                           │
│                                                        │
│ ┌─ Breakdown Table ───────────────────────────────┐   │
│ │ path_group                      │     count     │   │
│ │ /api/admin/analytics/cost       │         31    │   │
│ │ /api/admin/analytics/usage      │         31    │   │
│ │ /api/admin/auth/login           │          5    │   │
│ └─────────────────────────────────────────────────┘   │
└────────────────────────────────────────────────────────┘

┌─ Runtime Metrics (expanded by default) ────────────────┐
│ Goroutines 27 │ Heap 1.8/11 MB │ GC 0.21ms │ ...     │
└────────────────────────────────────────────────────────┘

┌─ Instances (expanded by default) ──────────────────────┐
│ Instance table for this service only                   │
│ instance-id │ status │ uptime │ heartbeat │ [Remove]   │
└────────────────────────────────────────────────────────┘
```

### 4.3 Metric Breakdown Data

The `/api/admin/service-metrics` response is extended with a `breakdowns` field per service. Each breakdown preserves the Prometheus label dimensions:

```json
{
  "services": {
    "control-plane": {
      "instances": 1,
      "metrics": { "requestsTotal": 156, ... },
      "breakdowns": {
        "requestsTotal": {
          "label": "path_group",
          "items": [
            { "value": "/api/admin/analytics/cost", "count": 31 },
            { "value": "/api/admin/auth/login", "count": 5 }
          ]
        },
        "requestDurationP50Ms": {
          "label": "path_group",
          "items": [
            { "value": "/api/admin/analytics/cost", "p50Ms": 8.2, "p99Ms": 25.0, "count": 31 }
          ]
        },
        "authFailuresTotal": {
          "label": "type",
          "items": [
            { "value": "missing_auth", "count": 2 }
          ]
        }
      },
      "runtime": { ... }
    }
  }
}
```

### 4.4 Breakdown Dimensions Per Service

| Service | Metric | Breakdown Label | Item Fields |
|---------|--------|-----------------|-------------|
| control-plane | requestsTotal | path_group | count |
| control-plane | requestDurationP50Ms | path_group | p50Ms, p99Ms, count |
| control-plane | authFailuresTotal | type | count |
| ai-gateway | requestsTotal | provider | count |
| ai-gateway | requestDurationP50Ms | provider | p50Ms, p99Ms, count |
| ai-gateway | tokensPromptTotal | provider | count |
| ai-gateway | tokensCompletionTotal | provider | count |
| ai-gateway | errorsTotal | provider | count |
| compliance-proxy | connectionsTotal | status | count |
| compliance-proxy | certCacheHitRate | layer | count (hits) |

The detail page shows a `<select>` dropdown to choose which metric to view. Default: first metric in the list.

## 5. Component Architecture

### 5.1 Component Split

| Component | File | Responsibility |
|-----------|------|----------------|
| `StatusPage` | `StatusPage.tsx` | Top-level: Stats Row + Infrastructure Bar + ServiceCard list |
| `ServiceCard` | `ServiceCard.tsx` **new** | Single service: header + metrics + collapsible runtime + collapsible instances |
| `ServiceDetailPage` | `ServiceDetailPage.tsx` **new** | `/status/services/:name` sub-page with breakdown |
| `MetricItem` | stays in `StatusPage.tsx` | Small helper, not worth its own file |

### 5.2 Data Flow

- `StatusPage` fetches `serviceMetrics` and `instancesData` via existing `useApi` hooks
- Passes per-service data to each `ServiceCard` as props
- `ServiceCard` receives: `serviceName`, `metricSet` (metrics + runtime + breakdowns), `instances` (filtered for this service), `onRemoveInstance`
- `ServiceDetailPage` fetches `serviceMetrics` independently (or receives via route state), reads `:serviceName` from URL params

## 6. File Change List

### Backend (Go)

| File | Change |
|------|--------|
| `packages/control-plane/internal/handler/promparse.go` | Extract with labels preserved, add `breakdowns` to response |
| `packages/control-plane/internal/handler/promparse_test.go` | Tests for breakdown extraction |

### Frontend (React/TypeScript)

| File | Change |
|------|--------|
| `packages/control-plane-ui/src/api/services/system.ts` | Add `breakdowns` to `ServiceMetricSet` type |
| `packages/control-plane-ui/src/pages/status/StatusPage.tsx` | Refactor: de-emphasize top, remove bottom table, render ServiceCard list |
| `packages/control-plane-ui/src/pages/status/ServiceCard.tsx` | **New** — Collapsible service card |
| `packages/control-plane-ui/src/pages/status/ServiceCard.module.css` | **New** — Styles for service card |
| `packages/control-plane-ui/src/pages/status/ServiceDetailPage.tsx` | **New** — Breakdown sub-page |
| `packages/control-plane-ui/src/pages/status/ServiceDetailPage.module.css` | **New** — Styles for detail page |
| `packages/control-plane-ui/src/pages/status/StatusPage.module.css` | Modify — De-emphasis styles, remove obsolete classes |
| `packages/control-plane-ui/src/routes/shellRouteConfig.tsx` | Add `/status/services/:serviceName` route |
| `packages/control-plane-ui/src/routes/lazyPages.tsx` | Add lazy import for ServiceDetailPage |
| `packages/control-plane-ui/src/i18n/locales/{en,zh,es}/pages.json` | New i18n keys for detail page, collapse labels |

## 7. Explicitly Out of Scope

- **No changes** to other Status tabs (Cache, Providers, Jobs, Realtime)
- **No instance-level** Prometheus metrics (per-instance drill-down)
- **No historical trend charts** — covered by Analytics and Realtime tab
- **No custom dimension selector** — each metric has fixed breakdown dimensions
- **No search/filter** on instance tables — not needed at current scale

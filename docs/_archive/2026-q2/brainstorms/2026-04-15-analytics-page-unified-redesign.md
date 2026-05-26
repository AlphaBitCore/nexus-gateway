# Analytics Page Unified Redesign — Design Specification

**Date:** 2026-04-15
**Status:** Approved

## 1. Problem

The `/analytics` page only shows VK (AI Gateway) traffic. Proxy, Agent, and Admin traffic are invisible. The "By Host" breakdown is empty because VK traffic has no `target_host`. The page gives an incomplete picture of platform activity.

## 2. Solution

Add a top-level **Source Filter** that controls the entire page. Summary cards, charts, and breakdown tables adapt dynamically based on the selected source.

## 3. Source Filter

Appears at the top of the page alongside the existing Time Range selector.

Options:
- **All Traffic** — no source filter, merged data across all sources
- **AI Gateway** — `source=vk`
- **Compliance Proxy** — `source=proxy`
- **Desktop Agent** — `source=agent`

Default: **All Traffic**

The selected source is passed as `?source=vk` (or empty for all) to all API calls on the page.

## 4. Summary Cards

Cards shown/hidden based on selected source:

| Card | All | VK | Proxy | Agent |
|------|-----|-----|-------|-------|
| Total Requests | show (all) | show | show | show |
| Total Tokens | show (VK portion) | show | hide | hide |
| Total Cost | show (VK portion) | show | hide | hide |
| Avg Latency | show | show | show | show |
| Error Rate | show | show | show | show |
| P95 Latency | show | show | show | show |
| Cache Hit Rate | show (VK portion) | show | hide | hide |
| Active Devices | show (Agent portion) | hide | hide | show |
| Compliance Coverage | show (Proxy portion) | hide | show | hide |

When "All Traffic" is selected, Token/Cost/Cache cards show VK data only (only source with cost), labeled as "AI Gateway" to avoid confusion. Active Devices and Compliance Coverage pull from agent/proxy data respectively.

## 5. Analytics Tab — Charts

### Cost Pie Chart
- Visible when source = All or VK
- Groups by the current groupBy dimension
- Hidden when source = Proxy or Agent (no cost data)

### Token Bar Chart
- Visible when source = All or VK
- Prompt vs Completion stacked bars
- Hidden when source = Proxy or Agent

### Traffic Trend (new)
- Always visible
- Line chart showing request_count over time for the selected source
- Replaces the old approach of only showing VK trends

## 6. Analytics Tab — Breakdown Tables

Replace the current fixed 4 tables (By User, By VK, By Device, By Host) with a **single dynamic table** controlled by a Group-By dropdown. The dropdown options change based on source:

| Source | Group-By Options |
|--------|-----------------|
| All | provider, model, user, organization, target_host, device |
| VK | provider, model, user, virtual_key, organization |
| Proxy | target_host, user, organization |
| Agent | device, target_host, user |

Table columns adapt to the selected groupBy:
- Always: Group, Requests
- When VK data present: Tokens, Cost
- When latency available: Avg Latency

Default groupBy per source:
- All → provider
- VK → provider
- Proxy → target_host
- Agent → device

## 7. Metrics Tab

Keeps its own independent Time Range selector (6h / 24h / 7d).

Add a Source selector (matching the top-level source filter). The `/metrics/aggregates` endpoint accepts `?source=` param and adjusts `SubDimension` accordingly:
- `source=vk` → `SubDimension: "source=vk"`
- `source=proxy` → `SubDimension: "source=proxy"`
- `source=agent` → `SubDimension: "source=agent"`
- empty → `SubDimension: ""` (all sources)

Charts that only apply to VK (Cost, Tokens) are hidden when source is Proxy or Agent.

## 8. Backend Changes

### API Parameter

All analytics endpoints accept a new optional query parameter `source`:
- `/api/admin/analytics/summary?source=vk`
- `/api/admin/analytics/summary?source=proxy`
- `/api/admin/analytics/summary?source=` (or omitted) — all sources

### Rollup Query Logic

In `admin_analytics_rollup.go`, update all `tryRollup*` functions:

```go
func sourceSubDimension(c echo.Context) string {
    src := c.QueryParam("source")
    if src == "" {
        return "" // no filter — all sources
    }
    return "source=" + src
}
```

Replace hardcoded `"source=vk"` with `sourceSubDimension(c)`.

### Summary Adaptation

When source is empty (all), the summary must handle mixed data:
- Token/Cost metrics only exist for `source=vk` events — these naturally sum to VK totals even without filter
- The response includes a `sourceNote` field: `"tokensCostSource": "vk"` to inform the frontend

### GroupBy Validation

The `groupBy` parameter is validated against allowed dimensions per source:
- VK: provider, model, user, virtual_key, organization, department, project
- Proxy: target_host, user, organization
- Agent: device, target_host, user
- All: union of all above

Invalid groupBy for the selected source returns 400.

### Metrics Aggregates

The `/metrics/aggregates` endpoint accepts `?source=` and passes it through to `SubDimension` in the rollup query.

## 9. Frontend Changes

### AnalyticsPage.tsx

1. Add `source` state: `useState<'' | 'vk' | 'proxy' | 'agent'>('')`
2. Add Source Filter UI (button group or dropdown) next to Time Range selector
3. Pass `source` to all API calls as query parameter
4. Conditionally show/hide Summary Cards based on source
5. Replace fixed 4 breakdown tables with dynamic single table + groupBy dropdown
6. GroupBy dropdown options derived from source
7. Charts: hide Cost/Token charts when source is proxy/agent

### MetricsRollupsSection.tsx

1. Accept optional `source` prop
2. Pass `source` to `metricsAggregates({ start, end, source })`
3. Hide Cost/Token charts when source is proxy/agent

### API Services

All analytics API methods accept optional `source` parameter and append it to query strings.

## 10. Not in Scope

- New pages or routes — this redesign is within the existing `/analytics` page
- Compliance-specific dashboards — `/proxy/compliance` remains separate
- Fleet-specific dashboards — `/fleet-overview` remains separate
- Real-time traffic stream — stays in `/traffic` page

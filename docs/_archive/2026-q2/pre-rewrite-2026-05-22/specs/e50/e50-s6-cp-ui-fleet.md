# E50 — Story 6: Control Plane UI — Fleet Surfaces + Admin API

## Context

Surfaces phase data across CP-UI fleet-facing pages: Dashboard, Analytics,
Model Detail Usage, Nodes Details Stats, and the Traffic list + detail.
Also implements the backend admin API endpoint + extensions that feed
the new widgets.

## User Story

**As a** platform operator looking at any CP-UI surface that today shows a
latency number,
**I want** to see "Us vs Upstream" framing and a Provider Leaderboard view
of which providers are slow,
**so that** I can answer "is Nexus slow?" with a chart in one click instead of
SQL gymnastics.

## Tasks

### 6.1 Backend — new endpoint `/api/admin/analytics/latency-phases`

`packages/control-plane/internal/handler/admin_analytics_latency.go` (new).
Signature (full OpenAPI in `docs/users/api/openapi/admin/e50-s6-latency-phases.yaml`):

```
GET /api/admin/analytics/latency-phases
  ?groupBy=<provider|model|virtual_key|node|host|device>
  &start=<iso8601>
  &end=<iso8601>
  &source=<all|ai-gateway|compliance-proxy|agent>
  &percentile=<p50,p95,p99>
```

Response shape:

```jsonc
{
  "rows": [
    {
      "groupKey":         "Anthropic",
      "groupLabel":       "Anthropic",     // human-readable when provider_id needs lookup
      "requestCount":     12345,
      "totalP50Ms":       1200, "totalP95Ms":  3400, "totalP99Ms":  6100,
      "usOverheadP50Ms":  45,   "usOverheadP95Ms": 80, "usOverheadP99Ms": 120,
      "upstreamTtfbP50Ms": 230, "upstreamTtfbP95Ms": 410, "upstreamTtfbP99Ms": 720,
      "upstreamTotalP50Ms": 1100, "upstreamTotalP95Ms": 3300, "upstreamTotalP99Ms": 6000,
      "requestHooksP50Ms": 12, "requestHooksP95Ms": 35,
      "responseHooksP50Ms": 8, "responseHooksP95Ms": 22
    }
  ]
}
```

Implementation: a single SQL query against `traffic_event` using
`percentile_cont` aggregates. `usOverheadP*` is computed in SQL as
`percentile_cont(...) WITHIN GROUP (ORDER BY GREATEST(0, latency_ms - upstream_total_ms))`.

IAM: gated by `admin:observability.read` (same as existing analytics endpoints).
Per CLAUDE.md IAM-audit binding, the SDD records this decision and the action
already exists in `packages/shared/security/iam/catalog_data.go`'s `observability`
resource — no new resource needed.

### 6.2 Backend — extend `summary`, `sparkline`, `by-provider`

`packages/control-plane/internal/handler/admin_analytics.go` —

- `AnalyticsSummary` adds: `usOverheadP95Ms`, `upstreamTtfbP95Ms`,
  `upstreamTotalP95Ms`.
- `SparklineResponse` per-bucket adds: `us_overhead_sum`, `us_overhead_count`,
  `upstream_ttfb_sum`, `upstream_ttfb_count`, `upstream_total_sum`,
  `upstream_total_count`.
- `ProviderBreakdown` row adds: `usOverheadP95Ms`, `upstreamTtfbP95Ms`,
  `upstreamTotalP95Ms`.

All additions are nullable JSON fields; pre-E50 clients ignore them. Update the
TypeScript types in `packages/control-plane-ui/src/api/types/analytics.ts`
accordingly.

### 6.3 Dashboard — `DashboardPage.tsx`

New 4-tile row "Latency Health" rendered beneath the existing 4-tile "System
Health" row. Each tile reuses the existing `<Card>` + `<AnimatedNumber>`
+ `<Sparkline>` components:

| Tile | Value | Sparkline data |
|---|---|---|
| Our Overhead P95 | `summary.usOverheadP95Ms` | `sparkline.bucket[*].us_overhead_sum / us_overhead_count` |
| Upstream TTFB P95 | `summary.upstreamTtfbP95Ms` | `bucket[*].upstream_ttfb_sum / count` |
| Upstream Total P95 | `summary.upstreamTotalP95Ms` | `bucket[*].upstream_total_sum / count` |
| Slowest Upstream Provider | `byProvider[0].providerName` (sorted by upstream_total_p95 DESC) | `byProvider[0].upstreamTotalP95Ms` as ms |

The "Slowest Upstream Provider" tile is a clickable nav to
`/analytics?tab=latency&groupBy=provider`.

The existing Top Providers table latency column is replaced by a `Us · Upstream`
dual-text cell (P95 values).

### 6.4 Analytics — `AnalyticsPage.tsx`

New tab "Latency" added alongside "Analytics" + "Metrics". Tab contents:

1. **KPI row** — 4 stat cards mirroring the Dashboard Latency Health row
   (totals over the selected time range).
2. **Stacked Area Time-Series** — recharts `<AreaChart>` with 5 stacked series
   (from bottom to top: request_hooks, our_other, upstream_ttfb, upstream_body,
   response_hooks). `our_other = our_overhead - hooks_total`. Tooltip shows each
   layer's ms value at the hovered bucket.
3. **Provider Leaderboard card** — table with columns
   `Provider | Requests | P50 TTFB | P95 TTFB | P50 Upstream Total | P95 Upstream Total | P95 Our`.
   Sort defaults to P95 Upstream Total DESC. Row click drills through to
   `groupBy=model&provider=<clicked>`.
4. **Breakdown table** — existing breakdown table by the current groupBy
   dimension, extended with `P95 Us` and `P95 Upstream` columns when groupBy is
   provider / model / virtual_key. Other groupBys (device / host) get only `P95 Us`.

The existing Avg Latency KPI on the Analytics tab is unchanged; the new Latency
tab is where the deep view lives.

### 6.5 Provider Detail Usage — `ProviderUsageTab.tsx`

- Replace the existing Avg Latency tile with a `Us · Upstream` split tile.
- New mini-card "Phase split" beneath the summary tiles: horizontal stacked
  bar showing P95 phase distribution for this provider over the last 30d.
- 3 breakdown tables (project / VK / model) lose the single `Avg Latency` column
  and gain `Avg Us` + `Avg Upstream` columns.

Data: the existing `useProviderDetail` hook gets extended `analyticsData.summary`
fields (per 6.2) plus a `byProject` / `byVK` / `byModel` shape with phase
fields. Backend already aggregates by these dimensions; add the phase columns
to those aggregates.

### 6.6 Nodes Details Stats — `ThingStatsTab.tsx`

Extend `thingStatsMetricCatalog.ts` for traffic-processing Thing types
(`AI_GATEWAY`, `COMPLIANCE_PROXY`, `AGENT`) with 4 new metric entries:

```ts
{ name: 'latency_us',              label: 'Our Overhead',    valueFmt: 'ms' },
{ name: 'latency_upstream_ttfb',   label: 'Upstream TTFB',   valueFmt: 'ms' },
{ name: 'latency_upstream_total',  label: 'Upstream Total',  valueFmt: 'ms' },
{ name: 'latency_hooks',           label: 'Hooks Total',     valueFmt: 'ms' },
```

The catalog already drives KPI cards, trend small-multiples, and breakdown
column visibility. Hub-side rollup populates these metrics from S5.3.

`HUB` Thing type does NOT get these metrics (no AI traffic).

### 6.7 Traffic list — `TrafficEventsPage.tsx`

Replace the existing single latency column with a `Us · Upstream` dual chip:

```tsx
<div className="latency-cell">
  <span className="us">Us {fmtMs(usOverhead)}</span>
  <span className="upstream">Upstream {fmtMs(upstreamTotal)}</span>
</div>
```

Row click opens a Detail drawer (existing pattern) with a Waterfall section
added (see 6.8).

When `upstream_total_ms` is NULL (historical row backfill failed or non-AI
traffic), render as a single chip with `Total Xms`.

### 6.8 Traffic Detail Waterfall — `TrafficEventDetailDrawer.tsx`

New `<Waterfall>` component renders a 5-segment horizontal stacked bar:

```
[ request_hooks ][ our_other ][ upstream_ttfb ][ upstream_body ][ response_hooks ]
```

A vertical tick on the boundary between `upstream_ttfb` and `upstream_body`
labels TTFB. Tooltip on each segment shows phase name + ms.

Below the bar, render the long-tail phases (auth, quota, routing, cache,
adapter) as a small table from `latencyBreakdown` JSONB.

Reuse: the existing detail drawer already renders `request_hooks_pipeline` /
`response_hooks_pipeline` JSON — keep that section unchanged.

### 6.9 i18n — three locales

Add keys to `packages/control-plane-ui/public/locales/{en,zh,es}/pages.json`:

```
analytics.latency.tab                Latency
analytics.latency.kpi.usOverhead     Our Overhead P95
analytics.latency.kpi.upstreamTtfb   Upstream TTFB P95
analytics.latency.kpi.upstreamTotal  Upstream Total P95
analytics.latency.providerLeaderboard.title  Provider Latency Leaderboard
dashboard.latencyHealth.title        Latency Health
dashboard.latencyHealth.slowestProvider  Slowest Upstream Provider
traffic.col.us                       Us
traffic.col.upstream                 Upstream
traffic.detail.waterfall.title       Phase Breakdown
traffic.detail.waterfall.reqHooks    Request Hooks
traffic.detail.waterfall.ourOther    Our Other
traffic.detail.waterfall.upstreamTtfb  Upstream TTFB
traffic.detail.waterfall.upstreamBody  Upstream Body
traffic.detail.waterfall.respHooks   Response Hooks
```

Tech terms (TTFB, Upstream, Latency) stay English across all locales per
CLAUDE.md TypeScript convention.

### 6.10 Vitest

- `<Waterfall>` component renders 5 segments with correct widths.
- Latency Health tiles render `—` when summary fields are NULL.
- Provider Leaderboard sorts by P95 Upstream Total by default.

## Acceptance Criteria

- A live ai-gateway request reflects in the Dashboard within the polling
  cycle (≤5s), Latency Health tiles populate, Top Providers shows split.
- Analytics → Latency tab renders all four widgets against the last 24h.
- Clicking Provider Leaderboard row drills through to filtered Analytics
  view.
- Provider Detail Usage shows phase split mini-card; breakdown tables show
  Us/Upstream columns.
- ThingStatsTab on an AI Gateway Thing shows the 4 new metric KPIs + trend
  charts.
- Traffic list dual chip + Detail Waterfall render against any post-S2 row;
  fall back to single chip for NULL-upstream rows.
- `npm run -w control-plane-ui test` passes.
- i18n key count matches across en/zh/es (use `/i18n-gap-check`).

## Non-Goals

- Agent UI rendering (S7).
- New IAM resource — reuses `admin:observability.read`.
- Histogram-based percentile *streaming* — V1 computes P50/P95/P99 server-side
  via `percentile_cont` per query window. If the table grows beyond
  10s of millions per window, promote to `pg_stat_statements`-style sampling
  in a follow-up.

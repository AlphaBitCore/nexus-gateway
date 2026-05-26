# E50 — Story 7: Agent Desktop UI — Single-Machine Surfaces

## Context

Surfaces phase data on the Wails desktop UI that ships inside the Nexus Agent.
The agent UI runs against the agent's local SQLite (no Hub calls), so this
story is gated on S4 (local schema + statusapi changes) being complete.

## User Story

**As an** agent end user on my laptop,
**I want** the desktop UI to show "is Nexus slow or is the network/provider
slow?" at a glance,
**so that** when my coding assistant feels sluggish I can self-diagnose without
filing a ticket.

## Tasks

### 7.1 Bridge types — `types/agent.ts`

`packages/agent/ui/frontend/src/types/agent.ts` `AgentEvent` interface gains:

```ts
upstreamTtfbMs?:    number | null;
upstreamTotalMs?:   number | null;
requestHooksMs?:    number | null;
responseHooksMs?:   number | null;
latencyBreakdown?:  Record<string, number>;
hooksPipeline?:     unknown;  // previously omitted; surface now
```

`StatsRow` interface (used by Stats page) gains `metricName` values
`latency_us`, `latency_upstream_ttfb`, `latency_upstream_total`,
`latency_hooks` (no schema change — these are new rows under the same
`metricName: string` field).

### 7.2 Overview — `Overview.tsx`

Existing 3-card protection strip (Inspected / Passthrough / Denied) gains a
4th card "Today's Latency":

```tsx
<Tile
  label={t('overview.todaysLatency')}
  primary={`Us ${fmtMs(usOverhead.avg)}`}
  secondary={`Upstream ${fmtMs(upstream.avg)}`}
/>
```

Data comes from a small extension to `StatusSnapshot.todayStats` returned by
`bridge().GetStatus()`. Adds `usOverheadAvgMs` and `upstreamTotalAvgMs` fields
populated from local SQLite SUM/COUNT over today's rows.

The Recent Activity table (last 5 events) gains a latency chip column
identical to the Traffic list pattern.

### 7.3 Stats — `Stats.tsx`

Split the existing Avg Latency KPI card into two: `Avg Us` and `Avg Upstream`
(both derived from the rollup `latency_us_sum / count` and
`latency_upstream_total_sum / count`). The 5-tile KPI row becomes 6.

The `<MiniLineChart>` gains a metric switcher above the chart:

```tsx
<select value={metric} onChange={...}>
  <option value="request_count">Requests</option>
  <option value="latency_us">Our Overhead</option>
  <option value="latency_upstream_total">Upstream</option>
  <option value="latency_both_stacked">Both (stacked)</option>
</select>
```

The breakdown table (currently `target_host` / `source_process` / `action`
dimensions × `request_count`) extends with two columns when the dimension is
`target_host`: `Avg Us` and `Avg Upstream`. This is the **per-destination
latency** view the user explicitly asked for — operators see at a glance which
destination is slow from this agent's POV.

For `source_process` and `action` dimensions, only `Avg Us` is added
(upstream latency by process / action is rarely diagnostic).

`bridge().QueryStats(filter, {metric})` is extended to accept a metric name
and the statusapi `handleQueryStats` returns the rollup rows for that metric.

### 7.4 Traffic — `Traffic.tsx`

Replace `fmtLatency(e.latencyMs)` cell with `Us · Upstream` dual chip,
matching CP-UI:

```tsx
const us = (e.latencyMs ?? 0) - (e.upstreamTotalMs ?? 0);
<div className="latency-cell">
  <span>Us {fmtMs(Math.max(0, us))}</span>
  <span>Upstream {fmtMs(e.upstreamTotalMs)}</span>
</div>
```

Row becomes clickable; click opens `<TrafficEventDetailDrawer>` (new — see
7.5).

### 7.5 Traffic Detail Drawer — `TrafficEventDetailDrawer.tsx` (new)

Wails React drawer (right-side slide-in) showing:

- Header: timestamp, source process, target host, status code, action badge
- 5-segment horizontal stacked bar (`<Waterfall>` component, ported / reused
  from CP-UI S6.8 styled for the agent's compact theme)
- Below the bar: `latency_breakdown` keys as a small label/value list
- `hooks_pipeline` JSON rendered as a per-hook table (this data was already in
  local SQLite but never exposed — bonus from S4.4)

Use existing CSS variables; no recharts (matches the agent UI's "no recharts"
constraint — the Waterfall is a plain SVG / divs).

### 7.6 i18n — three locales

Add keys to
`packages/agent/ui/frontend/src/i18n/locales/{en,es,zh}/dashboard.json`:

```
overview.todaysLatency         Today's Latency
stats.kpi.avgUs                Avg Our Overhead
stats.kpi.avgUpstream          Avg Upstream
stats.metric.requests          Requests
stats.metric.our               Our Overhead
stats.metric.upstream          Upstream
stats.metric.both              Both (stacked)
stats.col.avgUs                Avg Us
stats.col.avgUpstream          Avg Upstream
traffic.col.us                 Us
traffic.col.upstream           Upstream
traffic.detail.title           Event Details
traffic.detail.waterfall.title Phase Breakdown
traffic.detail.waterfall.intercept       Intercept
traffic.detail.waterfall.requestHooks    Request Hooks
traffic.detail.waterfall.upstreamTtfb    Upstream TTFB
traffic.detail.waterfall.upstreamBody    Upstream Body
traffic.detail.waterfall.responseHooks   Response Hooks
traffic.detail.hooksPipeline   Hook Pipeline
```

### 7.7 Vitest

- `<Waterfall>` segment widths sum to total width.
- Overview 4th tile renders `—` when `todayStats.usOverheadAvgMs` is null.
- Stats metric switcher swaps the chart series correctly.

## Acceptance Criteria

- Running the dev agent + ai-gateway combo: open the agent UI, exercise some
  traffic, then check:
  - Overview: 4-tile strip shows the new Today's Latency card.
  - Stats: 6 KPIs; switching MiniLineChart metric updates the line.
  - Stats breakdown by `target_host`: extra columns show Avg Us / Avg Upstream.
  - Traffic list: dual chip; click opens drawer with waterfall.
  - Drawer Hook Pipeline section renders per-hook list (which was invisible
    pre-S4.4).
- `npm run -w agent-ui test` (or wherever vitest is wired for the agent UI)
  passes.
- Locale key counts match across en/zh/es.

## Non-Goals

- CP-UI surfaces (S6).
- Hub API integration — agent UI stays 100% local.
- Cross-hop waterfall (combining agent + ai-gateway + provider rows for the
  same trace_id) — that's a CP-UI surface; the agent UI shows a single hop.
- Policies → Hook Detail latency rendering — recorded as Could-have C1 in
  requirements; deferred.

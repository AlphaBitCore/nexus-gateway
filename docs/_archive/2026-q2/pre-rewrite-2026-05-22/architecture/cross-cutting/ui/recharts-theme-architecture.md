---
doc: recharts-theme-architecture
area: cross-cutting
service: ui
tier: 1
updated: 2026-05-20
---

# Recharts Theme Architecture

> **Tier 2 architecture doc.** Read this when adding or editing a chart in the Control Plane UI / Agent Dashboard. The single sanctioned hex source for chart colors is `packages/ui-shared/src/theme/chartColors.ts`. The design-tokens binding (`.cursor/rules/design-tokens.mdc`) forbids hex anywhere else.

Recharts is the only place in the codebase that uses hex literals legally — and only via `chartColors.ts`. Anywhere else, hex is forbidden by the design-token binding.

---

## 1. Why hex (and only here)

Recharts components (`<Line>`, `<Bar>`, `<Pie>`, `<Area>`) accept colour props as hex strings. They don't read CSS variables, so the two-layer token system (`design-tokens-architecture.md`) doesn't reach them.

To keep theme/mode safety, we centralise the hex strings in `chartColors.ts` and **never** scatter them across chart files.

## 2. The file shape

`packages/ui-shared/src/theme/chartColors.ts` exports four palette categories plus pure helpers and React hooks. Built-in (prime-brand) hexes:

```ts
// Multi-series palette (light) — prime-brand monochrome family.
const LIGHT_SERIES = [
  '#3b518a', // prime brand
  '#647aa8', // brand steel
  '#5a607f', // prime secondary text
  '#16a34a', // success
  '#d97706', // cost / warning
  '#0891b2', // cyan
  '#7c3aed', // violet
  '#b91c1c', // danger
];

const DARK_SERIES = [
  '#8ea4da', '#b0bddc', '#8a93ad', '#4ade80',
  '#fb923c', '#22d3ee', '#a78bfa', '#f87171',
];

// Pie palettes (6 stops each).
const LIGHT_PIE = ['#3b518a', '#647aa8', '#16a34a', '#d97706', '#b91c1c', '#0891b2'];
const DARK_PIE  = ['#8ea4da', '#b0bddc', '#4ade80', '#fb923c', '#f87171', '#22d3ee'];

// Semantic per-metric colours — ChartSemanticName union (10 keys).
const LIGHT_SEMANTIC = {
  requests:        '#3b518a',
  tokens:          '#16a34a',
  cost:            '#d97706',
  errors:          '#b91c1c',
  cacheHits:       '#0d9488',
  prompt:          '#647aa8',
  completion:      '#16a34a',
  gatewaySavings:  '#059669',
  promptCache:     '#3b518a',
  totalSavings:    '#d97706',
};
// (DARK_SEMANTIC mirrors with the dark-mode values.)

// Latency-phase palette — waterfall + stacked-area phase keys mirror traffic_event.timings_ms.
const LIGHT_PHASE = {
  reqHooks:  '#647aa8',
  our:       '#9ba1ae',
  ttfb:      '#f59e0b',
  body:      '#10b981',
  respHooks: '#8b5cf6',
};
// (DARK_PHASE mirrors with the dark-mode values.)
```

Pure functions (use outside the React tree): `getSeriesColors(mode, theme?)`, `getPieColors(mode, theme?)`, `getSemanticColor(mode, key, theme?)`, `getPhaseColor(mode, phase)`, `getPhaseColors(mode)`, `getTooltipStyle(mode)`, `getAxisTickStyle(mode)`, `getGridStroke(mode)`.

React hooks (preferred inside `<ThemeProvider>` — read the active theme automatically): `useChartSeriesColors()`, `useChartPieColors()`, `useChartSemanticColor(key)`, `useChartPhaseColor(phase)`.

A theme JSON can override the built-in palettes via `ThemeConfig.charts.series` / `.pie` / `.semantic` (see `theme-architecture.md` §3e). The pure functions accept the resolved `ThemeConfig` so callers in non-React contexts can replicate the override behaviour.

## 3. The contract

- **All Recharts hex strings come from this file.** Chart components never write hex literals directly.
- **Light + dark palettes are defined in parallel** so theme flips work.
- **Series palette is ordered** — consumers index by position (so the "first" line keeps its colour across re-renders).
- **Semantic colours are named** — `ChartSemanticName` is a 10-key union: `requests`, `tokens`, `cost`, `errors`, `cacheHits`, `prompt`, `completion`, `gatewaySavings`, `promptCache`, `totalSavings`.
- **Phase colours are named** — `LIGHT_PHASE` / `DARK_PHASE` define the 5 latency-phase keys (`reqHooks`, `our`, `ttfb`, `body`, `respHooks`) that align with `traffic_event.timings_ms` — used by the waterfall + stacked-area charts.

## 4. Forbidden patterns

In any chart file:

```tsx
// ❌ inline hex
<Line stroke="#3b82f6" />

// ❌ raw rgba
<Bar fill="rgb(59, 130, 246)" />

// ❌ a hex from somewhere other than chartColors.ts
<Pie data={data} fill={someLocalConstant /* defined as '#3b82f6' */} />
```

## 5. Correct usage

```tsx
import {
  useChartSeriesColors,
  useChartSemanticColor,
  useChartPhaseColor,
} from '@nexus-gateway/ui-shared/theme/chartColors';

function RequestsChart({ data }) {
  const requestsColor = useChartSemanticColor('requests');
  return <Line stroke={requestsColor} />;
}

function MultiSeriesChart({ data, series }) {
  const colors = useChartSeriesColors();
  return (
    <>
      {series.map((s, i) => (
        <Line key={s.key} stroke={colors[i % colors.length]} />
      ))}
    </>
  );
}

function LatencyWaterfall({ phases }) {
  // Phase-keyed colour for traffic_event.timings_ms breakdowns.
  const reqHooks  = useChartPhaseColor('reqHooks');
  const ttfb      = useChartPhaseColor('ttfb');
  const body      = useChartPhaseColor('body');
  const respHooks = useChartPhaseColor('respHooks');
  // ...
}
```

## 6. Adding a new chart metric

1. Decide: ordered (multi-series) or semantic (single named metric) or phase (latency phase)?
2. Ordered → just consume `useChartSeriesColors()[i]`. No file edit needed.
3. Semantic → add a new key to `ChartSemanticName` in `ThemeConfig.ts`, then add `LIGHT_SEMANTIC[key]` + `DARK_SEMANTIC[key]` in `chartColors.ts`. Theme JSONs can override via `charts.semantic.<key>`.
4. Phase → extend `LIGHT_PHASE` + `DARK_PHASE` (no theme override path today — phase palette is built-in only).
5. Run `frontend-arch-review` skill to verify no stray hex landed elsewhere.

## 7. Enforcement

- `npm run check:design-tokens` — finds hex outside `chartColors.ts`.
- `.cursor/rules/design-tokens.mdc` — IDE-time binding.
- `frontend-arch-review` skill — comprehensive UI architecture audit including Recharts hex audit.

The check script knows `chartColors.ts` is the sanctioned location and exempts hex there.

<!-- 💡 harvest: existing rule + lint + skill cover this. No new harvest. -->

## 8. Sources

- `packages/ui-shared/src/theme/chartColors.ts` — single hex source (palettes + pure helpers + hooks).
- `packages/ui-shared/src/theme/ThemeContext.ts` — `useTheme()` hook (theme + resolvedMode), consumed by every chart hook.
- `packages/control-plane-ui/src/theme/useTheme.ts` — CP-UI re-export wrapper consumed across CP-UI pages.
- `packages/ui-shared/src/theme/ThemeConfig.ts` — `ChartSemanticName` union + `ThemeConfig.charts` override contract.
- `.cursor/rules/design-tokens.mdc` — IDE binding.
- `.claude/skills/frontend-arch-review/` — audit skill.

## 9. Cross-references

- `design-tokens-architecture.md` — parent design-token system.
- `ui-shared-architecture.md` — where `chartColors.ts` lives.
- `docs/users/features/cp-ui/overview.md` — analytics surfaces that render charts.

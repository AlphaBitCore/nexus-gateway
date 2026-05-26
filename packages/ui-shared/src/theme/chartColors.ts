// Theme-aware chart color palettes.
//
// Recharts requires JS color strings, not CSS variables — this module is the
// SINGLE legitimate source of hex literals in the UI codebase. All charts
// (CP-UI + Agent-UI) must import from here; no chart component is allowed to
// inline hex codes.
//
// Two-layer override system:
//   - Built-in palettes (LIGHT_*, DARK_*) below are the floor.
//   - Themes override via `ThemeConfig.charts.series` / `.pie` / `.semantic`
//     so a customer's brand colour reaches the charts the same way it
//     reaches every other surface (via the active theme JSON).
//
// Consumption preference:
//   - Inside a component rendered under <ThemeProvider> → use the hooks
//     (`useChartSeriesColors()` etc.) — they read the active theme.
//   - In utilities or helper functions outside the React tree → use the
//     pure functions and pass `mode` + optional `theme` explicitly.

import { useTheme } from './ThemeContext';
import type { ChartSemanticName, ThemeConfig } from './ThemeConfig';

const LIGHT_SERIES: readonly string[] = [
  '#3b518a', // prime brand
  '#647aa8', // brand steel
  '#5a607f', // prime secondary text
  '#16a34a', // success
  '#d97706', // cost / warning
  '#0891b2', // cyan
  '#7c3aed', // violet
  '#b91c1c', // danger
];

const DARK_SERIES: readonly string[] = [
  '#8ea4da', // dark brand
  '#b0bddc', // brand steel
  '#8a93ad', // prime muted text
  '#4ade80', // success
  '#fb923c', // cost / warning
  '#22d3ee', // cyan
  '#a78bfa', // violet
  '#f87171', // danger
];

const LIGHT_PIE: readonly string[] = [
  '#3b518a', '#647aa8', '#16a34a', '#d97706', '#b91c1c', '#0891b2',
];

const DARK_PIE: readonly string[] = [
  '#8ea4da', '#b0bddc', '#4ade80', '#fb923c', '#f87171', '#22d3ee',
];

const LIGHT_SEMANTIC: Record<ChartSemanticName, string> = {
  requests: '#3b518a',
  tokens: '#16a34a',
  cost: '#d97706',
  errors: '#b91c1c',
  cacheHits: '#0d9488',
  prompt: '#647aa8',
  completion: '#16a34a',
  gatewaySavings: '#059669',
  promptCache: '#3b518a',
  totalSavings: '#d97706',
};

const DARK_SEMANTIC: Record<ChartSemanticName, string> = {
  requests: '#8ea4da',
  tokens: '#4ade80',
  cost: '#fb923c',
  errors: '#f87171',
  cacheHits: '#2dd4bf',
  prompt: '#b0bddc',
  completion: '#4ade80',
  gatewaySavings: '#34d399',
  promptCache: '#8ea4da',
  totalSavings: '#fbbf24',
};

// Latency-phase palette — used by waterfall + stacked area charts.
// Phase keys mirror traffic_event.timings_ms:
//   reqHooks  → request-side hooks
//   our       → agent overhead (not attributed elsewhere)
//   ttfb      → upstream TTFB
//   body      → upstream body transfer
//   respHooks → response-side hooks (streaming + post)
const LIGHT_PHASE = {
  reqHooks:  '#647aa8', // brand steel
  our:       '#9ba1ae', // prime placeholder
  ttfb:      '#f59e0b', // amber-500
  body:      '#10b981', // emerald-500
  respHooks: '#8b5cf6', // violet-500
} as const;

const DARK_PHASE = {
  reqHooks:  '#8ea4da', // dark brand
  our:       '#8a93ad', // dark muted
  ttfb:      '#fbbf24', // amber-400
  body:      '#34d399', // emerald-400
  respHooks: '#a78bfa', // violet-400
} as const;

export type ChartColorMode = 'light' | 'dark';

export function getSeriesColors(mode: ChartColorMode, theme?: ThemeConfig): readonly string[] {
  return theme?.charts?.series?.[mode] ?? (mode === 'dark' ? DARK_SERIES : LIGHT_SERIES);
}

export function getPieColors(mode: ChartColorMode, theme?: ThemeConfig): readonly string[] {
  return theme?.charts?.pie?.[mode] ?? (mode === 'dark' ? DARK_PIE : LIGHT_PIE);
}

export function getSemanticColor(
  mode: ChartColorMode,
  key: ChartSemanticName,
  theme?: ThemeConfig,
): string {
  const override = theme?.charts?.semantic?.[key]?.[mode];
  if (override) return override;
  return mode === 'dark' ? DARK_SEMANTIC[key] : LIGHT_SEMANTIC[key];
}

export function getPhaseColor(mode: ChartColorMode, phase: keyof typeof LIGHT_PHASE): string {
  return mode === 'dark' ? DARK_PHASE[phase] : LIGHT_PHASE[phase];
}

export function getPhaseColors(mode: ChartColorMode): Record<keyof typeof LIGHT_PHASE, string> {
  return mode === 'dark' ? DARK_PHASE : LIGHT_PHASE;
}

export function getTooltipStyle(mode: ChartColorMode): Record<string, string | number> {
  return {
    background: 'var(--color-surface-overlay)',
    border: '1px solid var(--color-border)',
    borderRadius: 'var(--g-radius-md)',
    boxShadow: mode === 'dark' ? 'var(--shadow-lg)' : 'var(--shadow-sm)',
    fontSize: 13,
    color: 'var(--color-text)',
  };
}

export function getAxisTickStyle(mode: ChartColorMode): Record<string, string | number> {
  return { fontSize: 11, fill: mode === 'dark' ? 'var(--color-text-tertiary)' : 'var(--color-text-secondary)' };
}

export function getGridStroke(mode: ChartColorMode): string {
  return mode === 'dark' ? 'var(--color-border)' : 'var(--color-border-light)';
}

/** Multi-series palette (line / bar / area). Indexed by series position. */
export function useChartSeriesColors(): readonly string[] {
  const { resolvedMode, theme } = useTheme();
  return getSeriesColors(resolvedMode, theme);
}

/** Pie / donut palette. */
export function useChartPieColors(): readonly string[] {
  const { resolvedMode, theme } = useTheme();
  return getPieColors(resolvedMode, theme);
}

/** Named per-metric colour (requests, tokens, cost, errors, …). */
export function useChartSemanticColor(key: ChartSemanticName): string {
  const { resolvedMode, theme } = useTheme();
  return getSemanticColor(resolvedMode, key, theme);
}

/** Latency-phase colour for waterfall / stacked-area charts. */
export function useChartPhaseColor(phase: keyof typeof LIGHT_PHASE): string {
  const { resolvedMode } = useTheme();
  return getPhaseColor(resolvedMode, phase);
}

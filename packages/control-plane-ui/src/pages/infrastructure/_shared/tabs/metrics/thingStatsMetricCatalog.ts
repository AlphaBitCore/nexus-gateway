/**
 * thingStatsMetricCatalog — table-driven definition of which metrics, KPI
 * cards, dimension breakdowns, and time-series series each Thing type
 * produces and how to render them.
 *
 * Source of truth: the Hub aggregator (`packages/nexus-hub/internal/jobs/
 * thing_rollup_5m.go`) and the agent local aggregator (`packages/agent/
 * internal/localrollup/localrollup.go`). When new metrics are added there,
 * mirror them here so the Stats UI renders them.
 *
 * The catalog also encodes intentional differences across Thing types:
 *
 *   - **agent** — produces source_process / action / target_host dimensions;
 *     no tokens / no cost (agent does not see provider responses).
 *   - **ai-gateway** — produces provider / model dimensions plus token
 *     totals + cost; no bump / hook / action metrics.
 *   - **compliance-proxy** — produces target_host / hook dimensions plus
 *     TLS bump counters; no tokens / cost.
 *
 * The Stats UI hides KPIs / breakdowns for which the Thing type has no
 * registered entry instead of showing empty cells.
 */

export type ThingTypeKey = 'agent' | 'ai-gateway' | 'compliance-proxy';

/** Compact display units the Stats UI knows how to humanize. */
export type StatsUnit = 'count' | 'bytes' | 'ms' | 'tokens' | 'percent' | 'usd';

/**
 * One KPI card spec.
 *
 *   - `metric` — exact metricName written by the rollup pipeline.
 *   - `dimensionKey` — must match what the server returns (usually "" for
 *     globals; some KPIs live only under a specific dimension).
 *   - `derive` — for KPIs computed from multiple metrics (p95 latency,
 *     success-rate %, token totals); falls back to single-metric sum.
 */
export interface KpiSpec {
  /** Stable id used in tests + i18n key suffix. */
  id: string;
  /** Default metric name; ignored when `derive` is set. */
  metric?: string;
  dimensionKey?: string;
  unit: StatsUnit;
  /** i18n key under `pages:thingStats.kpi.<id>`. */
  labelKey: string;
  /**
   * When set, the card value is computed from one or more raw metric SUMs in
   * the response. Returning `null` hides the card (e.g. p95 with no samples).
   */
  derive?: (sumByMetric: Record<string, number>) => number | null;
}

/** Breakdown table spec — one dimensionKey -> grouped rows. */
export interface BreakdownSpec {
  /** Stable id; i18n key under `pages:thingStats.breakdown.<id>`. */
  id: string;
  /** Server-side `?dimension=` value. */
  dimensionKey: string;
  /** Metric to SUM per subDimension for the table value column. */
  metric: string;
  unit: StatsUnit;
  /** i18n key under `pages:thingStats.breakdown.<id>`. */
  titleKey: string;
  /** i18n key for the value column header. */
  valueColumnKey: string;
}

/** Time-series chart spec for the "Trend" section.
 *
 * Two modes:
 *   - **Single-metric sum** (legacy / common case): set `metric` only.
 *     Each bucket plots the per-bucket sum of that metric across
 *     dimensions. Used for `request_count`, `latency_sum`, etc.
 *   - **Multi-metric derive** (phase trends): set `metrics` to the
 *     list of metric names to collect per bucket, and `derive` to a
 *     function that turns the per-bucket sums into a single number
 *     (e.g. `avg = sum / count`). Returning null skips that bucket so
 *     the line doesn't render a zero where there's actually no data.
 *
 * Mirrors the KpiSpec.derive shape so the catalog has one consistent
 * derive grammar for "this number isn't a raw metric, compute it."
 */
export interface TrendSpec {
  /** Stable id; i18n key under `pages:thingStats.trend.<id>`. */
  id: string;
  /** Single-metric mode: the metric to sum per bucket. Ignored when
   *  `metrics` + `derive` are set. */
  metric?: string;
  /** Multi-metric mode: the metric names to collect per bucket before
   *  feeding them to `derive`. Required when `derive` is set. */
  metrics?: string[];
  dimensionKey?: string;
  unit: StatsUnit;
  /** i18n key under `pages:thingStats.trend.<id>`. */
  labelKey: string;
  /** When set, the per-bucket value is computed from a record of
   *  `metricName -> per-bucket sum` for every name in `metrics`.
   *  Returning null hides that bucket (no point plotted). */
  derive?: (sumByMetric: Record<string, number>) => number | null;
}

export interface ThingStatsCatalogEntry {
  kpis: KpiSpec[];
  breakdowns: BreakdownSpec[];
  trends: TrendSpec[];
}

// Hub rollup_5m emits status counts with `_count` suffix
// (status_2xx_count / status_4xx_count / status_5xx_count) — see
// packages/shared/metrics/types.go. Earlier code read the unsuffixed
// names and silently returned null on every Thing, leaving the Stats
// tab Success rate KPI empty across prod.
const SUCCESS_RATE = (sums: Record<string, number>): number | null => {
  const ok = sums['status_2xx_count'] ?? 0;
  const total =
    (sums['status_2xx_count'] ?? 0) +
    (sums['status_4xx_count'] ?? 0) +
    (sums['status_5xx_count'] ?? 0);
  if (total <= 0) return null;
  return (ok / total) * 100;
};

const LATENCY_AVG_MS = (sums: Record<string, number>): number | null => {
  const sum = sums['latency_sum'] ?? 0;
  const count = sums['latency_count'] ?? 0;
  if (count <= 0) return null;
  return sum / count;
};

// Average derive helpers for the four phase metrics. Each uses the matching
// {phase}_sum / {phase}_count pair written by the agent local rollup (and
// the Hub rollup once the writer is extended to cover server-side traffic).
const LATENCY_PHASE_AVG = (sumKey: string, countKey: string) =>
  (sums: Record<string, number>): number | null => {
    const sum = sums[sumKey] ?? 0;
    const count = sums[countKey] ?? 0;
    if (count <= 0) return null;
    return sum / count;
  };

/** Shared KPIs every data-plane Thing type carries. */
const SHARED_KPIS: KpiSpec[] = [
  {
    id: 'requests',
    metric: 'request_count',
    dimensionKey: '',
    unit: 'count',
    labelKey: 'pages:thingStats.kpi.requests',
  },
  {
    id: 'successRate',
    unit: 'percent',
    labelKey: 'pages:thingStats.kpi.successRate',
    derive: SUCCESS_RATE,
  },
  {
    id: 'avgLatency',
    unit: 'ms',
    labelKey: 'pages:thingStats.kpi.avgLatency',
    derive: LATENCY_AVG_MS,
  },
  // bytesIn / bytesOut intentionally live in the agent KPI block,
  // not here — only the agent local rollup writes bytes_in_sum /
  // bytes_out_sum (see packages/agent/internal/localrollup). Hub rollup_5m
  // never emits them, so the tiles were dead-on-arrival for ai-gateway
  // and compliance-proxy.
  {
    id: 'errors5xx',
    // Hub rollup emits status_5xx_count, not status_5xx. Tile is empty
    // until at least one 5xx lands in the window.
    metric: 'status_5xx_count',
    dimensionKey: '',
    unit: 'count',
    labelKey: 'pages:thingStats.kpi.errors5xx',
  },
  // Phase metrics: each derives an average from the matching
  // {phase}_sum / {phase}_count pair so the KPI tile renders "avg ms"
  // alongside the existing avgLatency.
  {
    id: 'avgUsOverhead',
    unit: 'ms',
    labelKey: 'pages:thingStats.kpi.avgUsOverhead',
    derive: LATENCY_PHASE_AVG('latency_us_sum', 'latency_us_count'),
  },
  {
    id: 'avgUpstreamTtfb',
    unit: 'ms',
    labelKey: 'pages:thingStats.kpi.avgUpstreamTtfb',
    derive: LATENCY_PHASE_AVG('latency_upstream_ttfb_sum', 'latency_upstream_ttfb_count'),
  },
  {
    id: 'avgUpstreamTotal',
    unit: 'ms',
    labelKey: 'pages:thingStats.kpi.avgUpstreamTotal',
    derive: LATENCY_PHASE_AVG('latency_upstream_total_sum', 'latency_upstream_total_count'),
  },
  {
    id: 'avgHooks',
    unit: 'ms',
    labelKey: 'pages:thingStats.kpi.avgHooks',
    derive: LATENCY_PHASE_AVG('latency_hooks_sum', 'latency_hooks_count'),
  },
];

const SHARED_HOST_BREAKDOWN: BreakdownSpec = {
  id: 'topHosts',
  dimensionKey: 'target_host',
  metric: 'request_count',
  unit: 'count',
  titleKey: 'pages:thingStats.breakdown.topHosts',
  valueColumnKey: 'pages:thingStats.breakdown.requestsColumn',
};

const SHARED_TREND_REQUESTS: TrendSpec = {
  id: 'requestsTrend',
  metric: 'request_count',
  dimensionKey: '',
  unit: 'count',
  labelKey: 'pages:thingStats.trend.requests',
};

const SHARED_TREND_LATENCY_SUM: TrendSpec = {
  id: 'latencySumTrend',
  metric: 'latency_sum',
  dimensionKey: '',
  unit: 'ms',
  labelKey: 'pages:thingStats.trend.latencySum',
};

// Phase trends — avg-per-bucket via the {phase}_sum / {phase}_count pairs.
// Volume-independent: a spike on these lines means real per-request slowdown,
// not just a traffic surge. Preferred over plotting the raw `_sum` line which
// conflates request volume with per-request cost.
//
// Rollups currently carry us / upstream_ttfb / upstream_total but NOT
// hooks (the rollup writer doesn't emit latency_hooks_* yet). The hooks trend
// is wired so it lights up automatically once that producer ships; until then
// it renders an empty card like any other "data not yet available" trend.
const phaseAvgDerive = (sumKey: string, countKey: string) =>
  (sums: Record<string, number>): number | null => {
    const sum = sums[sumKey] ?? 0;
    const count = sums[countKey] ?? 0;
    if (count <= 0) return null;
    return sum / count;
  };

const SHARED_TREND_LATENCY_US: TrendSpec = {
  id: 'latencyUsTrend',
  metrics: ['latency_us_sum', 'latency_us_count'],
  dimensionKey: '',
  unit: 'ms',
  labelKey: 'pages:thingStats.trend.latencyUs',
  derive: phaseAvgDerive('latency_us_sum', 'latency_us_count'),
};
const SHARED_TREND_LATENCY_UPSTREAM_TTFB: TrendSpec = {
  id: 'latencyUpstreamTtfbTrend',
  metrics: ['latency_upstream_ttfb_sum', 'latency_upstream_ttfb_count'],
  dimensionKey: '',
  unit: 'ms',
  labelKey: 'pages:thingStats.trend.latencyUpstreamTtfb',
  derive: phaseAvgDerive('latency_upstream_ttfb_sum', 'latency_upstream_ttfb_count'),
};
const SHARED_TREND_LATENCY_UPSTREAM_TOTAL: TrendSpec = {
  id: 'latencyUpstreamTotalTrend',
  metrics: ['latency_upstream_total_sum', 'latency_upstream_total_count'],
  dimensionKey: '',
  unit: 'ms',
  labelKey: 'pages:thingStats.trend.latencyUpstreamTotal',
  derive: phaseAvgDerive('latency_upstream_total_sum', 'latency_upstream_total_count'),
};
const SHARED_TREND_LATENCY_HOOKS: TrendSpec = {
  id: 'latencyHooksTrend',
  metrics: ['latency_hooks_sum', 'latency_hooks_count'],
  dimensionKey: '',
  unit: 'ms',
  labelKey: 'pages:thingStats.trend.latencyHooks',
  derive: phaseAvgDerive('latency_hooks_sum', 'latency_hooks_count'),
};
const SHARED_PHASE_TRENDS: TrendSpec[] = [
  SHARED_TREND_LATENCY_US,
  SHARED_TREND_LATENCY_UPSTREAM_TTFB,
  SHARED_TREND_LATENCY_UPSTREAM_TOTAL,
  SHARED_TREND_LATENCY_HOOKS,
];

export const THING_STATS_CATALOG: Record<ThingTypeKey, ThingStatsCatalogEntry> = {
  // Agent — emits action decisions (passthrough / inspect / deny) and per-
  // source-process attribution. No tokens / cost / bump.
  agent: {
    kpis: [
      ...SHARED_KPIS,
      // Agent-only: localrollup emits per-bucket byte counters on
      // intercepted traffic. Hub rollup_5m never writes these, so
      // ai-gateway / compliance-proxy don't render them.
      {
        id: 'bytesIn',
        metric: 'bytes_in_sum',
        dimensionKey: '',
        unit: 'bytes',
        labelKey: 'pages:thingStats.kpi.bytesIn',
      },
      {
        id: 'bytesOut',
        metric: 'bytes_out_sum',
        dimensionKey: '',
        unit: 'bytes',
        labelKey: 'pages:thingStats.kpi.bytesOut',
      },
      {
        id: 'distinctProcesses',
        metric: 'distinct_source_processes',
        dimensionKey: '',
        unit: 'count',
        labelKey: 'pages:thingStats.kpi.distinctProcesses',
      },
      {
        id: 'distinctHosts',
        metric: 'distinct_target_hosts',
        dimensionKey: '',
        unit: 'count',
        labelKey: 'pages:thingStats.kpi.distinctHosts',
      },
      {
        id: 'actionsDenied',
        metric: 'action_deny',
        dimensionKey: '',
        unit: 'count',
        labelKey: 'pages:thingStats.kpi.actionsDenied',
      },
    ],
    breakdowns: [
      SHARED_HOST_BREAKDOWN,
      {
        id: 'topProcesses',
        dimensionKey: 'source_process',
        metric: 'request_count',
        unit: 'count',
        titleKey: 'pages:thingStats.breakdown.topProcesses',
        valueColumnKey: 'pages:thingStats.breakdown.requestsColumn',
      },
      {
        id: 'actionBreakdown',
        dimensionKey: 'action',
        metric: 'request_count',
        unit: 'count',
        titleKey: 'pages:thingStats.breakdown.actions',
        valueColumnKey: 'pages:thingStats.breakdown.requestsColumn',
      },
    ],
    trends: [SHARED_TREND_REQUESTS, SHARED_TREND_LATENCY_SUM, ...SHARED_PHASE_TRENDS],
  },

  // ai-gateway — emits tokens (prompt / completion / total) + provider / model
  // attribution. No bump / action / source_process.
  'ai-gateway': {
    kpis: [
      ...SHARED_KPIS,
      {
        id: 'promptTokens',
        metric: 'prompt_tokens',
        dimensionKey: '',
        unit: 'tokens',
        labelKey: 'pages:thingStats.kpi.promptTokens',
      },
      {
        id: 'completionTokens',
        metric: 'completion_tokens',
        dimensionKey: '',
        unit: 'tokens',
        labelKey: 'pages:thingStats.kpi.completionTokens',
      },
      {
        id: 'totalTokens',
        metric: 'total_tokens',
        dimensionKey: '',
        unit: 'tokens',
        labelKey: 'pages:thingStats.kpi.totalTokens',
      },
    ],
    breakdowns: [
      {
        id: 'topModels',
        dimensionKey: 'model',
        metric: 'total_tokens',
        unit: 'tokens',
        titleKey: 'pages:thingStats.breakdown.topModels',
        valueColumnKey: 'pages:thingStats.breakdown.tokensColumn',
      },
      {
        // `routed_provider`, not `provider`. The rollup pipeline used to emit
        // `provider=<requested>` but OpenAI-style requests carry no requested
        // provider, so the dim was always empty and was retired in favour of
        // `routed_provider` (the provider that actually handled the call).
        // See packages/nexus-hub/internal/jobs/rollup_5m.go::buildEventDims.
        id: 'topProviders',
        dimensionKey: 'routed_provider',
        metric: 'request_count',
        unit: 'count',
        titleKey: 'pages:thingStats.breakdown.topProviders',
        valueColumnKey: 'pages:thingStats.breakdown.requestsColumn',
      },
      SHARED_HOST_BREAKDOWN,
    ],
    trends: [
      SHARED_TREND_REQUESTS,
      {
        id: 'totalTokensTrend',
        metric: 'total_tokens',
        dimensionKey: '',
        unit: 'tokens',
        labelKey: 'pages:thingStats.trend.totalTokens',
      },
      SHARED_TREND_LATENCY_SUM,
      ...SHARED_PHASE_TRENDS,
    ],
  },

  // compliance-proxy — emits TLS bump counters + hook decisions + target_host
  // attribution. No tokens / cost / action / source_process.
  'compliance-proxy': {
    kpis: [
      ...SHARED_KPIS,
      {
        id: 'bumpSuccess',
        metric: 'bump_success',
        dimensionKey: '',
        unit: 'count',
        labelKey: 'pages:thingStats.kpi.bumpSuccess',
      },
      {
        id: 'bumpFailed',
        metric: 'bump_failed',
        dimensionKey: '',
        unit: 'count',
        labelKey: 'pages:thingStats.kpi.bumpFailed',
      },
      {
        id: 'bumpExempt',
        metric: 'bump_exempt',
        dimensionKey: '',
        unit: 'count',
        labelKey: 'pages:thingStats.kpi.bumpExempt',
      },
      {
        id: 'hookDeny',
        metric: 'hook_deny',
        dimensionKey: '',
        unit: 'count',
        labelKey: 'pages:thingStats.kpi.hookDeny',
      },
    ],
    breakdowns: [
      SHARED_HOST_BREAKDOWN,
      {
        id: 'hookBreakdown',
        dimensionKey: 'hook_decision',
        metric: 'request_count',
        unit: 'count',
        titleKey: 'pages:thingStats.breakdown.hookDecisions',
        valueColumnKey: 'pages:thingStats.breakdown.requestsColumn',
      },
    ],
    trends: [SHARED_TREND_REQUESTS, SHARED_TREND_LATENCY_SUM, ...SHARED_PHASE_TRENDS],
  },
};

/**
 * Return the catalog entry for a Thing type. Unknown / non-data-plane types
 * (control-plane, nexus-hub) return null — the UI hides the Stats tab for
 * those.
 */
export function catalogFor(thingType: string): ThingStatsCatalogEntry | null {
  if (thingType in THING_STATS_CATALOG) {
    return THING_STATS_CATALOG[thingType as ThingTypeKey];
  }
  return null;
}

/** Stable list of Thing types the Stats tab supports. */
export function isThingStatsType(thingType: string): thingType is ThingTypeKey {
  return thingType in THING_STATS_CATALOG;
}

/** Format a numeric value against a StatsUnit for KPI / breakdown rendering. */
export function formatStatsValue(v: number | null | undefined, unit: StatsUnit): string {
  if (v == null || !Number.isFinite(v)) return '—';
  switch (unit) {
    case 'percent':
      return `${v.toFixed(1)}%`;
    case 'ms':
      if (Math.abs(v) >= 1000) return `${(v / 1000).toFixed(2)} s`;
      return `${v.toFixed(v < 10 ? 2 : 0)} ms`;
    case 'bytes': {
      const KiB = 1024, MiB = KiB * 1024, GiB = MiB * 1024;
      const abs = Math.abs(v);
      if (abs >= GiB) return `${(v / GiB).toFixed(2)} GiB`;
      if (abs >= MiB) return `${(v / MiB).toFixed(2)} MiB`;
      if (abs >= KiB) return `${(v / KiB).toFixed(1)} KiB`;
      return `${v.toFixed(0)} B`;
    }
    case 'tokens':
    case 'count':
      if (Math.abs(v) >= 1_000_000) return `${(v / 1_000_000).toFixed(2)}M`;
      if (Math.abs(v) >= 1_000) return `${(v / 1_000).toFixed(1)}k`;
      return v.toFixed(v < 10 && v !== Math.floor(v) ? 2 : 0);
    case 'usd':
      return `$${v.toFixed(v < 1 ? 4 : 2)}`;
    default:
      return String(v);
  }
}

/**
 * Stats page — local rollup dashboard.
 *
 * Reads pre-aggregated metrics from the agent's SQLite
 * thing_metric_rollup_local_* tables via the QUERY_STATS IPC command.
 * Renders KPI cards + a single inline SVG trend chart + a per-
 * dimension top-N breakdown table. Recharts is intentionally skipped
 * (see MiniLineChart for the why).
 *
 * Catalog scope: agent-only. Unlike the CP UI's polymorphic catalog
 * that covers ai-gateway / compliance-proxy / agent, this page only
 * needs the agent metric set (request_count, status_*x, latency_*,
 * bytes_*, action_*, distinct_*). Hardcoding it here keeps the page
 * focused and the bundle small.
 */

import { useEffect, useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { agentApi } from '@/api/agent';
import type { StatsResponse } from '@/api/agent';
import { MiniLineChart } from './MiniLineChart';
import { isAIHost } from '@/lib/aiHosts';
import styles from './Stats.module.css';
import page from '../_shared/pageStyles.module.css';

type RangePreset = '1h' | '6h' | '24h' | '7d' | '30d';

const RANGE_MS: Record<RangePreset, number> = {
  '1h': 60 * 60 * 1000,
  '6h': 6 * 60 * 60 * 1000,
  '24h': 24 * 60 * 60 * 1000,
  '7d': 7 * 24 * 60 * 60 * 1000,
  '30d': 30 * 24 * 60 * 60 * 1000,
};

// Breakdown tabs. Agent is a transparent proxy, not the AI gateway,
// so provider_name and model_name are populated only for the handful of
// adapters that decode the wire (openai-compat path). For consumer-
// surface traffic (chatgpt-web / claude-web / cursor) those fields are
// always empty — a Provider tab here would render mostly "no data" rows
// and mislead the operator. Host + Process cover every flow; the two
// breakdown tables are rendered side-by-side instead of behind a
// dropdown so both questions ("which sites" + "which apps") are
// answered without a click.

interface KpiDef {
  id: string;
  /** When set, the value is the SUM of this metric over the response window. */
  metric?: string;
  /** Computed KPIs (e.g. success rate) get a derive callback. */
  derive?: (sums: Record<string, number>) => number | null;
  unit: 'count' | 'percent' | 'ms' | 'bytes';
  labelKey: string;
}

const KPIS: KpiDef[] = [
  { id: 'requests', metric: 'request_count', unit: 'count', labelKey: 'stats.kpi.requests' },
  {
    id: 'successRate',
    unit: 'percent',
    labelKey: 'stats.kpi.successRate',
    derive: (s) => {
      // localrollup emits status_*_count keys (e.g. status_2xx_count).
      // Earlier code read status_2xx (missing the _count suffix) which
      // caused successRate to stay n/a even when counters were populated.
      const ok = s['status_2xx_count'] ?? 0;
      const total = ok + (s['status_4xx_count'] ?? 0) + (s['status_5xx_count'] ?? 0);
      return total > 0 ? (ok / total) * 100 : null;
    },
  },
  {
    id: 'avgLatency',
    unit: 'ms',
    labelKey: 'stats.kpi.avgLatency',
    derive: (s) => {
      const sum = s['latency_sum'] ?? 0;
      const count = s['latency_count'] ?? 0;
      return count > 0 ? sum / count : null;
    },
  },
  // Avg Us = latency_us_sum / count (agent local overhead).
  // Avg Upstream = latency_upstream_total_sum / count (upstream wall time).
  {
    id: 'avgUs',
    unit: 'ms',
    labelKey: 'stats.kpi.avgUs',
    derive: (s) => {
      const sum = s['latency_us_sum'] ?? 0;
      const count = s['latency_us_count'] ?? 0;
      return count > 0 ? sum / count : null;
    },
  },
  {
    id: 'avgUpstream',
    unit: 'ms',
    labelKey: 'stats.kpi.avgUpstream',
    derive: (s) => {
      const sum = s['latency_upstream_total_sum'] ?? 0;
      const count = s['latency_upstream_total_count'] ?? 0;
      return count > 0 ? sum / count : null;
    },
  },
  { id: 'bytesIn', metric: 'bytes_in_sum', unit: 'bytes', labelKey: 'stats.kpi.bytesIn' },
  { id: 'bytesOut', metric: 'bytes_out_sum', unit: 'bytes', labelKey: 'stats.kpi.bytesOut' },
  // Health-ratio KPIs derived from localrollup action / hook / bump
  // counters. Tell the operator at a glance: how much traffic NE
  // actually intercepts (vs lets through), whether policy is blocking,
  // and whether the TLS bump is healthy.
  {
    id: 'inspectRate',
    unit: 'percent',
    labelKey: 'stats.kpi.inspectRate',
    derive: (s) => {
      const inspect = s['action_inspect_count'] ?? 0;
      const total = inspect + (s['action_passthrough_count'] ?? 0) + (s['action_deny_count'] ?? 0);
      return total > 0 ? (inspect / total) * 100 : null;
    },
  },
  {
    id: 'hookAllowRate',
    unit: 'percent',
    labelKey: 'stats.kpi.hookAllowRate',
    derive: (s) => {
      const allow = s['hook_allow_count'] ?? 0;
      const total = allow + (s['hook_deny_count'] ?? 0) + (s['hook_error_count'] ?? 0);
      return total > 0 ? (allow / total) * 100 : null;
    },
  },
  {
    id: 'bumpSuccessRate',
    unit: 'percent',
    labelKey: 'stats.kpi.bumpSuccessRate',
    derive: (s) => {
      const ok = s['bump_success_count'] ?? 0;
      const total = ok + (s['bump_failed_count'] ?? 0) + (s['bump_exempt_count'] ?? 0);
      return total > 0 ? (ok / total) * 100 : null;
    },
  },
];

function formatValue(v: number | null, unit: KpiDef['unit']): string {
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
    case 'count':
    default:
      if (Math.abs(v) >= 1_000_000) return `${(v / 1_000_000).toFixed(2)}M`;
      if (Math.abs(v) >= 1_000) return `${(v / 1_000).toFixed(1)}k`;
      return v.toFixed(v < 10 && v !== Math.floor(v) ? 2 : 0);
  }
}

/** Sum a metric across all global-dimension rows. */
function sumGlobal(rows: StatsResponse['rows'], metric: string): number {
  let total = 0;
  for (const r of rows) {
    if (r.metricName === metric && !r.dimensionKey) total += r.value;
  }
  return total;
}

/**
 * Extract the typed-dimension value from a localrollup
 * dimension_key. localrollup writes keys as `"<dim>=<value>"`
 * (localrollup.go:282), e.g. "target_host=chatgpt.com". Returns the
 * value when dim matches, null otherwise. Caller can filter rows for
 * the relevant breakdown bucket and pull the subdimension cleanly.
 */
function extractDimValue(dimensionKey: string | undefined, dim: string): string | null {
  if (!dimensionKey) return null;
  const prefix = dim + '=';
  if (!dimensionKey.startsWith(prefix)) return null;
  return dimensionKey.slice(prefix.length);
}

interface BreakdownRow {
  subDimension: string;
  value: number;
  avgUs: number | null;
  avgUpstream: number | null;
}

/**
 * Aggregate breakdown rows for a single typed dimension. Filters by
 * dimension_key prefix (LIKE "<dim>=%" matched server-side, then
 * extractDimValue picks the value); accumulates request_count plus the
 * phase aggregates (us/upstream sum+count) so the render layer can
 * pivot to Avg columns without recomputing.
 */
function aggregateBreakdown(
  rows: StatsResponse['rows'],
  dim: string,
  cohortIsAI: boolean,
): BreakdownRow[] {
  type Acc = {
    requests: number;
    usSum: number; usCount: number;
    upstreamSum: number; upstreamCount: number;
  };
  const m = new Map<string, Acc>();
  const get = (k: string) => {
    let v = m.get(k);
    if (!v) {
      v = { requests: 0, usSum: 0, usCount: 0, upstreamSum: 0, upstreamCount: 0 };
      m.set(k, v);
    }
    return v;
  };
  for (const r of rows) {
    const value = extractDimValue(r.dimensionKey, dim);
    if (value === null || value === '') continue;
    // AI cohort filter applies only to the host breakdown — there's no
    // way to know if a process is "AI" without a registry.
    if (cohortIsAI && dim === 'target_host' && !isAIHost(value)) continue;
    const a = get(value);
    switch (r.metricName) {
      case 'request_count': a.requests += r.value; break;
      case 'latency_us_sum': a.usSum += r.value; break;
      case 'latency_us_count': a.usCount += r.value; break;
      case 'latency_upstream_total_sum': a.upstreamSum += r.value; break;
      case 'latency_upstream_total_count': a.upstreamCount += r.value; break;
    }
  }
  return Array.from(m.entries())
    .map(([subDimension, v]) => ({
      subDimension,
      value: v.requests,
      avgUs: v.usCount > 0 ? v.usSum / v.usCount : null,
      avgUpstream: v.upstreamCount > 0 ? v.upstreamSum / v.upstreamCount : null,
    }))
    .sort((a, b) => b.value - a.value)
    .slice(0, 10);
}

/** Per-bucket sum series for one metric across global-dimension rows. */
function trendSeries(rows: StatsResponse['rows'], metric: string) {
  const m = new Map<string, number>();
  for (const r of rows) {
    if (r.metricName !== metric || r.dimensionKey) continue;
    m.set(r.bucketStart, (m.get(r.bucketStart) ?? 0) + r.value);
  }
  return Array.from(m.entries())
    .map(([bucket, value]) => ({ bucket, value }))
    .sort((a, b) => a.bucket.localeCompare(b.bucket));
}

type CohortFilter = 'all' | 'ai';
const COHORT_STORAGE_KEY = 'nexus.agent.stats.cohort';

export function Stats() {
  const { t } = useTranslation();
  const [range, setRange] = useState<RangePreset>('24h');
  // Trend chart metric switcher. Defaults to request_count so the
  // chart behaves identically for users who don't switch it.
  const [trendMetric, setTrendMetric] = useState<string>('request_count');
  // Cohort filter: "All flows" (default) vs "AI only". AI cohort is
  // derived client-side from per-target_host rows filtered by isAIHost.
  // Persisted in localStorage so the user's choice survives reloads.
  const [cohort, setCohort] = useState<CohortFilter>(() => {
    try {
      const raw = localStorage.getItem(COHORT_STORAGE_KEY);
      return raw === 'ai' ? 'ai' : 'all';
    } catch {
      return 'all';
    }
  });
  useEffect(() => {
    try { localStorage.setItem(COHORT_STORAGE_KEY, cohort); } catch { /* best-effort */ }
  }, [cohort]);

  // Pin the window per range change so re-renders don't shift bucket
  // boundaries underneath in-flight queries.
  const { startISO, endISO } = useMemo(() => {
    const end = new Date();
    const start = new Date(end.getTime() - RANGE_MS[range]);
    return { startISO: start.toISOString(), endISO: end.toISOString() };
  }, [range]);

  // Global query — KPIs + trend chart. Polls every 30s so a long-lived
  // dashboard reflects new buckets without manual refresh.
  const { data, isLoading, error } = useQuery({
    queryKey: ['agent', 'stats', 'global', range],
    queryFn: () => agentApi.queryStats({ start: startISO, end: endISO }),
    refetchInterval: 30_000,
    staleTime: 10_000,
  });

  // Breakdown queries — one per typed dimension shown as side-by-side
  // tabs. Separate queries so the cache key is dim-scoped (avoid
  // re-fetching when only the time range changes) and so each table
  // renders independently when one returns empty.
  // We request the phase metrics too so the host breakdown can show
  // Avg Us / Avg Upstream columns; the process breakdown stays simple
  // (process-level latency is less actionable than host-level).
  const breakdownHost = useQuery({
    queryKey: ['agent', 'stats', 'breakdown', range, 'target_host'],
    queryFn: () =>
      agentApi.queryStats({
        start: startISO, end: endISO,
        metrics: ['request_count', 'latency_us_sum', 'latency_us_count', 'latency_upstream_total_sum', 'latency_upstream_total_count'],
        dimension: 'target_host',
      }),
    refetchInterval: 30_000,
    staleTime: 10_000,
  });
  const breakdownProcess = useQuery({
    queryKey: ['agent', 'stats', 'breakdown', range, 'source_process'],
    queryFn: () =>
      agentApi.queryStats({
        start: startISO, end: endISO,
        metrics: ['request_count'],
        dimension: 'source_process',
      }),
    refetchInterval: 30_000,
    staleTime: 10_000,
  });

  if (isLoading || !data) {
    return (
      <div className={styles.root}>
        <h1 className={styles.title}>{t('stats.title')}</h1>
        <p className={styles.subtitle}>{t('stats.loading')}</p>
      </div>
    );
  }

  if (data.error) {
    return (
      <div className={styles.root}>
        <h1 className={styles.title}>{t('stats.title')}</h1>
        <p className={styles.empty}>{data.error}</p>
      </div>
    );
  }

  if (error) {
    return (
      <div className={styles.root}>
        <h1 className={styles.title}>{t('stats.title')}</h1>
        <p className={styles.empty}>{error.message}</p>
      </div>
    );
  }

  // Pre-compute the SUM-of-metric map once so each KPI's derive() reads
  // O(1). When AI cohort is selected, re-aggregate from per-target_host
  // rows filtered by isAIHost; otherwise sum global (dimensionKey="") rows.
  const sums: Record<string, number> = {};
  if (cohort === 'ai') {
    for (const r of data.rows) {
      const host = extractDimValue(r.dimensionKey, 'target_host');
      if (host === null || host === '' || !isAIHost(host)) continue;
      sums[r.metricName] = (sums[r.metricName] ?? 0) + r.value;
    }
  } else {
    for (const r of data.rows) {
      if (r.dimensionKey) continue;
      sums[r.metricName] = (sums[r.metricName] ?? 0) + r.value;
    }
  }

  // Side-by-side breakdowns. Host shows Avg Us / Avg Upstream;
  // Process keeps to request counts (process-level latency rarely
  // matches user intent — they want "what host did this process hit"
  // which is the Host table anyway).
  const cohortIsAI = cohort === 'ai';
  const hostRows = breakdownHost.data ? aggregateBreakdown(breakdownHost.data.rows, 'target_host', cohortIsAI) : [];
  const processRows = breakdownProcess.data ? aggregateBreakdown(breakdownProcess.data.rows, 'source_process', cohortIsAI) : [];

  return (
    <div className={styles.root}>
      <header>
        <h1 className={styles.title}>{t('stats.title')}</h1>
        <p className={styles.subtitle}>{t('stats.subtitle')}</p>
      </header>

      <div className={styles.headerRow}>
        <label className={page.mutedSmall} htmlFor="range">
          {t('stats.range')}
        </label>
        <select
          id="range"
          className={page.formSelect}
          value={range}
          onChange={(e) => setRange(e.target.value as RangePreset)}
        >
          <option value="1h">{t('stats.range1h')}</option>
          <option value="6h">{t('stats.range6h')}</option>
          <option value="24h">{t('stats.range24h')}</option>
          <option value="7d">{t('stats.range7d')}</option>
          <option value="30d">{t('stats.range30d')}</option>
        </select>
        <div
          role="tablist"
          aria-label={t('stats.cohort.label', 'Cohort')}
          style={{
            display: 'inline-flex',
            border: '1px solid var(--color-border)',
            borderRadius: 'var(--g-radius-md)',
            overflow: 'hidden',
            marginLeft: 'var(--g-space-3)',
          }}
        >
          {(['all', 'ai'] as const).map((value) => (
            <button
              key={value}
              type="button"
              role="tab"
              aria-selected={cohort === value}
              onClick={() => setCohort(value)}
              style={{
                padding: 'var(--g-space-1) var(--g-space-3)',
                background: cohort === value ? 'var(--color-primary)' : 'var(--color-surface)',
                color: cohort === value ? 'var(--color-primary-foreground)' : 'var(--color-text)',
                fontSize: 'var(--g-font-size-sm)',
                fontWeight: cohort === value ? 'var(--g-font-weight-semibold)' : 'var(--g-font-weight-normal)',
                cursor: 'pointer',
                border: 'none',
              }}
            >
              {value === 'all' ? t('stats.cohort.all', 'All flows') : t('stats.cohort.ai', 'AI only')}
            </button>
          ))}
        </div>
        <span className={styles.granuleBadge}>
          {t('stats.granulePrefix')}: {data.granule}
        </span>
      </div>

      {/* KPI cards */}
      <div className={styles.kpiGrid}>
        {KPIS.map((kpi) => {
          const value = kpi.derive
            ? kpi.derive(sums)
            : kpi.metric
              ? sumGlobal(data.rows, kpi.metric)
              : null;
          // emptyHint surfaces WHY a KPI shows n/a so the user
          // doesn't mistake a quiet window for a broken metric.
          const isEmpty = value == null || !Number.isFinite(value);
          const emptyHint = isEmpty
            ? (
                kpi.id === 'successRate'     ? t('stats.kpi.emptyHint.successRate',     'No flows in this window — successRate becomes available once any request lands.')
              : kpi.id === 'avgUs'           ? t('stats.kpi.emptyHint.avgUs',           'No agent-overhead samples yet — populates once any flow completes the local pipeline.')
              : kpi.id === 'avgUpstream'     ? t('stats.kpi.emptyHint.avgUpstream',     'No upstream-total samples yet — populates from inspect flows that complete the upstream relay.')
              : kpi.id === 'inspectRate'     ? t('stats.kpi.emptyHint.inspectRate',     'No flows yet — inspect/passthrough/deny mix needs at least one decision.')
              : kpi.id === 'hookAllowRate'   ? t('stats.kpi.emptyHint.hookAllowRate',   'No hook decisions yet — needs at least one flow that ran the compliance pipeline.')
              : kpi.id === 'bumpSuccessRate' ? t('stats.kpi.emptyHint.bumpSuccessRate', 'No bump events yet — needs at least one inspect flow with a TLS handshake.')
              : ''
            )
            : '';
          return (
            <div key={kpi.id} className={styles.kpiCard} title={emptyHint || undefined}>
              <span className={styles.kpiLabel}>{t(kpi.labelKey)}</span>
              <span className={styles.kpiValue}>{isEmpty ? t('stats.kpi.naLabel', 'n/a') : formatValue(value, kpi.unit)}</span>
              {isEmpty && emptyHint && (
                <span style={{ color: 'var(--color-text-muted)', fontSize: 'var(--g-font-size-xs)', marginTop: 'var(--g-space-1)' }}>
                  {t('stats.kpi.naWhy', 'Why?')}
                </span>
              )}
            </div>
          );
        })}
      </div>

      {/* Trend chart — switchable metric. */}
      <div className={styles.headerRow}>
        <h3 className={styles.sectionTitle} style={{ margin: 'var(--g-space-0)' }}>{t('stats.trendTitle')}</h3>
        <select
          className={page.formSelect}
          value={trendMetric}
          onChange={(e) => setTrendMetric(e.target.value)}
          aria-label={t('stats.trendMetric', 'Trend metric')}
        >
          <option value="request_count">{t('stats.metric.requests', 'Requests')}</option>
          <option value="latency_us_sum">{t('stats.metric.our', 'Our Overhead')}</option>
          <option value="latency_upstream_total_sum">{t('stats.metric.upstream', 'Upstream')}</option>
        </select>
      </div>
      <div className={styles.chartCard}>
        <MiniLineChart
          data={trendSeries(data.rows, trendMetric)}
          ariaLabel={t('stats.trendTitle')}
        />
      </div>

      {/* Breakdowns: Host + Process side-by-side (stacked when
          viewport is narrow). No dropdown — both dimensions answer
          different questions ("which sites" vs "which apps") and a
          selector would hide one answer at a time. */}
      <div className={styles.headerRow}>
        <h3 className={styles.sectionTitle} style={{ margin: 'var(--g-space-0)' }}>
          {t('stats.breakdownTitle')}
        </h3>
      </div>
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(360px, 1fr))', gap: 'var(--g-space-3)' }}>
        <div className={styles.chartCard}>
          <div className={page.mutedSmall} style={{ marginBottom: 'var(--g-space-2)' }}>
            {t('stats.dim.target_host', 'Top Hosts')}
          </div>
          {hostRows.length === 0 ? (
            <p className={styles.empty}>{t('stats.breakdownEmpty')}</p>
          ) : (
            <table className={styles.breakdownTable}>
              <thead>
                <tr>
                  <th>{t('stats.dim.target_host', 'Host')}</th>
                  <th>{t('stats.requestsColumn')}</th>
                  <th>{t('stats.col.avgUs', 'Avg Us')}</th>
                  <th>{t('stats.col.avgUpstream', 'Avg Upstream')}</th>
                </tr>
              </thead>
              <tbody>
                {hostRows.map((r) => (
                  <tr key={r.subDimension}>
                    <td>{r.subDimension}</td>
                    <td>{formatValue(r.value, 'count')}</td>
                    <td>{formatValue(r.avgUs, 'ms')}</td>
                    <td>{formatValue(r.avgUpstream, 'ms')}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
        <div className={styles.chartCard}>
          <div className={page.mutedSmall} style={{ marginBottom: 'var(--g-space-2)' }}>
            {t('stats.dim.source_process', 'Top Processes')}
          </div>
          {processRows.length === 0 ? (
            <p className={styles.empty}>{t('stats.breakdownEmpty')}</p>
          ) : (
            <table className={styles.breakdownTable}>
              <thead>
                <tr>
                  <th>{t('stats.dim.source_process', 'Process')}</th>
                  <th>{t('stats.requestsColumn')}</th>
                </tr>
              </thead>
              <tbody>
                {processRows.map((r) => (
                  <tr key={r.subDimension}>
                    <td>{r.subDimension}</td>
                    <td>{formatValue(r.value, 'count')}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </div>
    </div>
  );
}

export default Stats;

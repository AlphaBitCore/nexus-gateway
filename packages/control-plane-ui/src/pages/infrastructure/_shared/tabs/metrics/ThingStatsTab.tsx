/**
 * ThingStatsTab — per-Thing stats dashboard.
 *
 * Reused by:
 *   - InfraNodeDetailPage (Stats tab for agent / ai-gateway / compliance-proxy)
 *   - FleetDeviceDetailPage (Stats tab for the agent device)
 *
 * Backed by `GET /api/admin/things/:id/stats`. Renders three sections:
 *
 *   1. KPI cards — quick health glance (request_count, success rate, latency,
 *      bytes, token totals, agent action breakdowns, etc.).
 *   2. Trends — small-multiple line charts (one Recharts <LineChart> per metric).
 *   3. Breakdown table — top-N rows for a chosen dimension
 *      (model / provider / target_host / source_process / hook decision).
 *
 * The metric catalog (`thingStatsMetricCatalog.ts`) drives which sections
 * appear per Thing type. When the backend returns `enabled: false` (agent
 * with Hub rollup OFF) the disabled banner replaces the chart sections so
 * the dashboard does not silently look broken.
 */

import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { useApi } from '@/hooks/useApi';
import { thingStatsApi } from '@/api/services';
import type { ThingStatsResponse, ThingStatsRow } from '@/api/services';
import {
  Stack, Select, Skeleton, ErrorBanner, Tabs, TabsList, TabsTrigger, TabsContent,
} from '@/components/ui';
import { TimeSeriesChart } from '@/components/charts/TimeSeriesChart';
import type { TimeSeriesUnit } from '@/components/charts/TimeSeriesChart';

import {
  catalogFor, formatStatsValue,
} from './thingStatsMetricCatalog';
import type {
  BreakdownSpec, KpiSpec, TrendSpec, StatsUnit,
} from './thingStatsMetricCatalog';
import styles from './ThingStatsTab.module.css';

interface Props {
  thingId: string;
  thingType: string;
}

type RangePreset = '1h' | '6h' | '24h' | '7d' | '30d';

const RANGE_MS: Record<RangePreset, number> = {
  '1h': 60 * 60 * 1000,
  '6h': 6 * 60 * 60 * 1000,
  '24h': 24 * 60 * 60 * 1000,
  '7d': 7 * 24 * 60 * 60 * 1000,
  '30d': 30 * 24 * 60 * 60 * 1000,
};

/** Sum a metric across all rows whose dimension matches. */
function sumGlobal(rows: ThingStatsRow[], metric: string, dimensionKey = ''): number {
  let total = 0;
  for (const r of rows) {
    if (r.metricName === metric && (r.dimensionKey ?? '') === dimensionKey) {
      total += r.value;
    }
  }
  return total;
}

/** Build a `{metricName: SUM}` map over the global-dimension rows for derived KPIs. */
function buildGlobalSums(rows: ThingStatsRow[]): Record<string, number> {
  const out: Record<string, number> = {};
  for (const r of rows) {
    if ((r.dimensionKey ?? '') !== '') continue;
    out[r.metricName] = (out[r.metricName] ?? 0) + r.value;
  }
  return out;
}

/**
 * Map a `StatsUnit` from the catalog to the `TimeSeriesUnit` understood by
 * `<TimeSeriesChart>`. The catalog has a few units the chart doesn't carry
 * (e.g. `tokens`, `usd`); render those as `count` and rely on the chart
 * formatter — the chart legend label already disambiguates the meaning.
 */
function statsUnitToChartUnit(u: StatsUnit): TimeSeriesUnit {
  if (u === 'tokens' || u === 'usd') return 'count';
  return u;
}

/** Time-series points for a single trend, summed across (dimensionKey, subDim).
 *
 * Two paths, mirroring the TrendSpec shape:
 *   - When `spec.derive` is set, collect per-bucket sums for every name
 *     in `spec.metrics`, then run derive() per bucket. Used by phase trends
 *     to render avg = sum/count instead of raw sum.
 *   - Otherwise, the legacy single-metric sum path: per-bucket sum of
 *     `spec.metric` across subDimensions.
 */
function trendPoints(rows: ThingStatsRow[], spec: TrendSpec) {
  // Multi-metric derive path (phase trends).
  if (spec.derive && spec.metrics && spec.metrics.length > 0) {
    const wanted = new Set(spec.metrics);
    const byBucket = new Map<string, Record<string, number>>();
    for (const r of rows) {
      if (!wanted.has(r.metricName)) continue;
      if ((r.dimensionKey ?? '') !== (spec.dimensionKey ?? '')) continue;
      const sums = byBucket.get(r.bucketStart) ?? {};
      sums[r.metricName] = (sums[r.metricName] ?? 0) + r.value;
      byBucket.set(r.bucketStart, sums);
    }
    const points: Array<{ bucket: string; value: number }> = [];
    for (const [bucket, sums] of byBucket) {
      const v = spec.derive(sums);
      if (v != null && Number.isFinite(v)) {
        points.push({ bucket, value: v });
      }
    }
    return points.sort((a, b) => a.bucket.localeCompare(b.bucket));
  }
  // Legacy single-metric sum path.
  const byBucket = new Map<string, number>();
  for (const r of rows) {
    if (r.metricName !== spec.metric) continue;
    if ((r.dimensionKey ?? '') !== (spec.dimensionKey ?? '')) continue;
    byBucket.set(r.bucketStart, (byBucket.get(r.bucketStart) ?? 0) + r.value);
  }
  return Array.from(byBucket.entries())
    .map(([bucket, value]) => ({ bucket, value }))
    .sort((a, b) => a.bucket.localeCompare(b.bucket));
}

/**
 * Top-N breakdown rows for a dimension, sorted by value desc.
 *
 * The rollup pipeline stores `dimensionKey` as a single string of the form
 * `<name>=<value>` (e.g. `routed_provider=dee77e2b-…`), produced by
 * `metrics.BuildDimensionKey`. The catalog spec's `dimensionKey` carries
 * only the bare name (`routed_provider`). Matching with strict equality
 * therefore never lined up — every breakdown returned zero rows even
 * though the data was present. Fixed here by matching on the `<name>=`
 * prefix and extracting the value as the per-row label.
 *
 * `subDimension` (the second axis the SQL stores) is the source-domain
 * filter (`vk`/`proxy`/`agent`) and is intentionally NOT the grouping key
 * for the table — we want one row per dim value, not one per source.
 */
function breakdownRows(
  rows: ThingStatsRow[],
  spec: BreakdownSpec,
  topN = 10,
): Array<{ subDimension: string; value: number }> {
  const byDimValue = new Map<string, number>();
  const prefix = spec.dimensionKey + '=';
  for (const r of rows) {
    if (r.metricName !== spec.metric) continue;
    const dk = r.dimensionKey ?? '';
    if (!dk.startsWith(prefix)) continue;
    const dimValue = dk.slice(prefix.length);
    if (!dimValue) continue;
    byDimValue.set(dimValue, (byDimValue.get(dimValue) ?? 0) + r.value);
  }
  return Array.from(byDimValue.entries())
    // Keep the `subDimension` field name in the returned shape because the
    // table renderer reads it — it's the row label, not the SQL subDim.
    .map(([subDimension, value]) => ({ subDimension, value }))
    .sort((a, b) => b.value - a.value)
    .slice(0, topN);
}

export function ThingStatsTab({ thingId, thingType }: Props) {
  const { t } = useTranslation();
  const [range, setRange] = useState<RangePreset>('24h');

  // We pin the time window at mount + every range change so re-renders don't
  // shift the bucket boundary mid-render (which would make Recharts re-animate).
  const { startISO, endISO } = useMemo(() => {
    const end = new Date();
    const start = new Date(end.getTime() - RANGE_MS[range]);
    return { startISO: start.toISOString(), endISO: end.toISOString() };
  }, [range]);

  // The catalog tells us which dimensions to pre-fetch. We request a single
  // combined window of metric names by issuing one call per dimension we care
  // about (server filters by `?dimension=`); the global call uses no dimension.
  //
  // Pragma: a smaller surface = one call without a dimension param, which
  // returns all dimensionless rows. That's enough for KPIs + trends. Breakdown
  // tables make separate calls to keep the global call light.

  const { data, loading, error, refetch } = useApi<ThingStatsResponse>(
    () => thingStatsApi.get(thingId, { start: startISO, end: endISO }),
    ['admin', 'things', 'stats', thingId, range],
  );

  const catalog = catalogFor(thingType);

  if (loading && !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner error={error} onRetry={refetch} />;
  if (!data) return null;

  // Some Thing types aren't covered by per-Thing rollup at all (control-plane,
  // nexus-hub). Surface this rather than render an empty dashboard.
  if (!catalog) {
    return (
      <div className={styles.empty}>
        {t('pages:thingStats.notSupportedType', { type: thingType })}
      </div>
    );
  }

  // Agent with Hub rollup OFF: short-circuit to the banner. Fleet operators
  // should view detailed metrics on the agent's local UI instead. The Hub
  // also returns a `rollupDisabledMessage` string, but it is English-only;
  // we render the t()-localized banner instead so the page works in all UI
  // languages.
  if (!data.enabled) {
    return (
      <Stack gap="md">
        <div className={styles.disabledBanner}>
          {t('pages:thingStats.disabledFallback', {
            defaultValue: 'Per-agent rollup is not enabled on the Hub. View detailed metrics on the agent local UI.',
          })}
        </div>
      </Stack>
    );
  }

  const globalSums = buildGlobalSums(data.rows);

  return (
    <Stack gap="md">
      <div className={styles.header}>
        <div className={styles.rangeSelect}>
          <Select
            value={range}
            onValueChange={(v) => setRange(v as RangePreset)}
            options={[
              { value: '1h', label: t('pages:thingStats.range1h', { defaultValue: 'Last 1 hour' }) },
              { value: '6h', label: t('pages:thingStats.range6h', { defaultValue: 'Last 6 hours' }) },
              { value: '24h', label: t('pages:thingStats.range24h', { defaultValue: 'Last 24 hours' }) },
              { value: '7d', label: t('pages:thingStats.range7d', { defaultValue: 'Last 7 days' }) },
              { value: '30d', label: t('pages:thingStats.range30d', { defaultValue: 'Last 30 days' }) },
            ]}
          />
        </div>
        <span className={styles.granuleBadge}>
          {t('pages:thingStats.granulePrefix', { defaultValue: 'Granule' })}: {data.granule}
        </span>
      </div>

      {/* KPI cards */}
      <section className={styles.kpiSection}>
        <h3 className={styles.kpiTitle}>
          {t('pages:thingStats.summaryHeading', { defaultValue: 'Summary' })}
        </h3>
        <div className={styles.kpiGrid}>
          {catalog.kpis.map((kpi: KpiSpec) => {
            const value = kpi.derive
              ? kpi.derive(globalSums)
              : kpi.metric
                ? sumGlobal(data.rows, kpi.metric, kpi.dimensionKey ?? '')
                : null;
            return (
              <div key={kpi.id} className={styles.kpiCard}>
                <span className={styles.kpiLabel}>
                  {t(kpi.labelKey, { defaultValue: kpi.id })}
                </span>
                <span className={styles.kpiValue}>
                  {formatStatsValue(value, kpi.unit)}
                </span>
              </div>
            );
          })}
        </div>
      </section>

      {/* Trends — small multiples */}
      <section className={styles.trendsSection}>
        <h3 className={styles.trendsTitle}>
          {t('pages:thingStats.trendsHeading', { defaultValue: 'Trends' })}
        </h3>
        <div className={styles.trendGrid}>
          {catalog.trends.map((trend: TrendSpec) => (
            <div key={trend.id} className={styles.trendCard}>
              <div className={styles.trendLabel}>
                {t(trend.labelKey, { defaultValue: trend.id })}
              </div>
              <TimeSeriesChart
                data={trendPoints(data.rows, trend)}
                label={t(trend.labelKey, { defaultValue: trend.id })}
                unit={statsUnitToChartUnit(trend.unit)}
                height={140}
              />
            </div>
          ))}
        </div>
      </section>

      {/* Breakdowns — one tab per dimension */}
      {catalog.breakdowns.length > 0 && (
        <BreakdownSection
          thingId={thingId}
          startISO={startISO}
          endISO={endISO}
          breakdowns={catalog.breakdowns}
        />
      )}
    </Stack>
  );
}

/* ---------- Breakdown section ---------- */

interface BreakdownSectionProps {
  thingId: string;
  startISO: string;
  endISO: string;
  breakdowns: BreakdownSpec[];
}

function BreakdownSection({ thingId, startISO, endISO, breakdowns }: BreakdownSectionProps) {
  const { t } = useTranslation();
  const [active, setActive] = useState<string>(breakdowns[0]?.id ?? '');

  return (
    <section className={styles.breakdownsSection}>
      <h3 className={styles.breakdownsTitle}>
        {t('pages:thingStats.breakdownsHeading', { defaultValue: 'Breakdowns' })}
      </h3>
      <Tabs value={active} onValueChange={setActive}>
        <TabsList>
          {breakdowns.map((b) => (
            <TabsTrigger key={b.id} value={b.id}>
              {t(b.titleKey, { defaultValue: b.id })}
            </TabsTrigger>
          ))}
        </TabsList>
        {breakdowns.map((b) => (
          <TabsContent key={b.id} value={b.id}>
            <BreakdownPane
              thingId={thingId}
              startISO={startISO}
              endISO={endISO}
              spec={b}
            />
          </TabsContent>
        ))}
      </Tabs>
    </section>
  );
}

interface BreakdownPaneProps {
  thingId: string;
  startISO: string;
  endISO: string;
  spec: BreakdownSpec;
}

function BreakdownPane({ thingId, startISO, endISO, spec }: BreakdownPaneProps) {
  const { t } = useTranslation();

  const { data, loading, error, refetch } = useApi<ThingStatsResponse>(
    () =>
      thingStatsApi.get(thingId, {
        start: startISO,
        end: endISO,
        metric: spec.metric,
        dimension: spec.dimensionKey,
      }),
    ['admin', 'things', 'stats', 'breakdown', thingId, spec.id, startISO, endISO],
  );

  if (loading && !data) return <Skeleton.Table rows={5} cols={2} />;
  if (error) return <ErrorBanner error={error} onRetry={refetch} />;
  if (!data) return null;

  const rows = breakdownRows(data.rows, spec, 20);
  if (rows.length === 0) {
    return (
      <div className={styles.empty}>
        {t('pages:thingStats.breakdownEmpty', { defaultValue: 'No data for this breakdown in the selected window.' })}
      </div>
    );
  }

  // Server-stamped lookup: dim value (UUID/slug) → display name. The renderer
  // prefers the name; missing entries fall back to the raw value (raw values
  // are already human-readable for target_host / hook_decision, so no name
  // resolution is needed for those dims).
  const displayNames = data.displayNames ?? {};

  return (
    <table style={{ width: '100%', borderCollapse: 'collapse' }}>
      <thead>
        <tr>
          <th style={{ textAlign: 'left', padding: 'var(--g-space-2)', borderBottom: '1px solid var(--color-border)' }}>
            {t(`pages:thingStats.breakdown.dim.${spec.dimensionKey}`, {
              defaultValue: spec.dimensionKey,
            })}
          </th>
          <th style={{ textAlign: 'right', padding: 'var(--g-space-2)', borderBottom: '1px solid var(--color-border)' }}>
            {t(spec.valueColumnKey, { defaultValue: spec.valueColumnKey })}
          </th>
        </tr>
      </thead>
      <tbody>
        {rows.map((r) => (
          <tr key={r.subDimension}>
            <td style={{ padding: 'var(--g-space-2)', borderBottom: '1px solid var(--color-border)' }}>
              {displayNames[r.subDimension] ?? r.subDimension}
            </td>
            <td style={{ padding: 'var(--g-space-2)', borderBottom: '1px solid var(--color-border)', textAlign: 'right' }}>
              {formatStatsValue(r.value, spec.unit)}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

export default ThingStatsTab;

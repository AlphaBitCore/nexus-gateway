import { formatDateTime } from '@/lib/format';
import type { MetricAggregatePoint } from '../../api/types';

/**
 * Rollups keyed by `routed_provider:*` are preferred to avoid double-counting
 * with other series in the same bucket. The dimension is named "routed
 * provider" because OpenAI-style clients can't pin a provider — there is
 * only one provider per request, the one that actually handled it. The
 * legacy `provider:*` prefix was retired with the rollup writer change;
 * any historical rows with that prefix are pre-fix duplicates (the
 * migration deletes them).
 */
export function isProviderRollupRow(r: MetricAggregatePoint): boolean {
  return typeof r.dimensionKey === 'string' && r.dimensionKey.startsWith('routed_provider:');
}

/**
 * Prefer provider-keyed rows for a metric when present; otherwise use all rows for that metric
 * (e.g. only non-provider breakdowns exist).
 */
export function rowsForMetricChart(rows: MetricAggregatePoint[], metricName: string): MetricAggregatePoint[] {
  const forMetric = rows.filter((r) => r.metricName === metricName);
  const providerRows = forMetric.filter(isProviderRollupRow);
  return providerRows.length > 0 ? providerRows : forMetric;
}

export function formatBucketAxis(iso: string, rangeHours: number): string {
  if (rangeHours <= 48) return iso.slice(11, 16);
  const d = new Date(iso);
  return formatDateTime(d);
}

/** Avg per bucket = sumMetricByBucket(sumMetric) / sumMetricByBucket(countMetric),
 *  emitting 0 when count is 0 so chart axes don't divide by zero. Used by the
 *  Phase-breakdown over-time chart on Metrics Explorer where the rollup
 *  writer ships separate `_sum` + `_count` metric names for each phase. */
export function avgMetricByBucket(
  rows: MetricAggregatePoint[],
  sumMetric: string,
  countMetric: string,
  options?: { rangeHours?: number },
): Array<{ bucketStart: string; bucketAt: string; avg: number }> {
  const sums = sumMetricByBucket(rows, sumMetric, options);
  const counts = sumMetricByBucket(rows, countMetric, options);
  const byBucket = new Map(counts.map(c => [c.bucketAt, c.total]));
  return sums.map(s => {
    const c = byBucket.get(s.bucketAt) ?? 0;
    return {
      bucketStart: s.bucketStart,
      bucketAt: s.bucketAt,
      avg: c > 0 ? s.total / c : 0,
    };
  });
}

/** Sum rollup `value` strings per bucket for one metric (dimensions combined per chart rules). */
export function sumMetricByBucket(
  rows: MetricAggregatePoint[],
  metricName: string,
  options?: { rangeHours?: number },
): Array<{ bucketStart: string; bucketAt: string; total: number }> {
  const rangeHours = options?.rangeHours ?? 24;
  const subset = rowsForMetricChart(rows, metricName);
  const map = new Map<string, number>();
  for (const r of subset) {
    const v = parseFloat(r.value);
    if (!Number.isFinite(v)) continue;
    map.set(r.bucketStart, (map.get(r.bucketStart) ?? 0) + v);
  }
  return [...map.entries()]
    .sort((a, b) => a[0].localeCompare(b[0]))
    .map(([bucketAt, total]) => ({
      bucketStart: formatBucketAxis(bucketAt, rangeHours),
      bucketAt,
      total,
    }));
}

/** Sum all numeric values for the given rows (already filtered). */
export function sumPointValues(rows: MetricAggregatePoint[]): number {
  let s = 0;
  for (const r of rows) {
    const v = parseFloat(r.value);
    if (Number.isFinite(v)) s += v;
  }
  return s;
}

export interface PivotProviderSeriesResult {
  data: Array<Record<string, string | number>>;
  providers: string[];
}

/** One row per bucket; one numeric column per provider (from `dimensions.provider`). */
export function pivotProviderMetricByBucket(
  rows: MetricAggregatePoint[],
  metricName: string,
  options?: { rangeHours?: number },
): PivotProviderSeriesResult {
  const rangeHours = options?.rangeHours ?? 24;
  const subset = rowsForMetricChart(rows, metricName);
  const buckets = new Map<string, Map<string, number>>();
  const providersSet = new Set<string>();

  for (const r of subset) {
    // Provider rollup dim values are stable UUIDs; the handler also ships
    // a `providerLabel` (display name) so chart legends read "OpenAI"
    // instead of a UUID. Prefer the label and fall back to the ID when
    // it's missing (e.g. legacy buckets emitted before the rename).
    const labelRaw = r.dimensions?.providerLabel;
    const idRaw = r.dimensions?.provider;
    const p = typeof labelRaw === 'string' && labelRaw.length > 0
      ? labelRaw
      : (typeof idRaw === 'string' ? idRaw : '');
    if (!p) continue;
    providersSet.add(p);
    const b = r.bucketStart;
    if (!buckets.has(b)) buckets.set(b, new Map());
    const v = parseFloat(r.value);
    if (!Number.isFinite(v)) continue;
    const m = buckets.get(b)!;
    m.set(p, (m.get(p) ?? 0) + v);
  }

  const sortedBuckets = [...buckets.keys()].sort();
  const providers = [...providersSet].sort();
  const data = sortedBuckets.map((bucketAt) => {
    const row: Record<string, string | number> = {
      bucketStart: formatBucketAxis(bucketAt, rangeHours),
      bucketAt,
    };
    const pm = buckets.get(bucketAt)!;
    for (const pr of providers) {
      row[pr] = pm.get(pr) ?? 0;
    }
    return row;
  });
  return { data, providers };
}

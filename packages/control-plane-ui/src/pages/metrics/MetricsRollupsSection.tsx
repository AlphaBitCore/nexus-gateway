import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '../../hooks/useApi';
import { analyticsApi } from '@/api/services';
import { LoadingSpinner, ErrorBanner, Stack } from '@/components/ui';
import { useTheme } from '../../theme/useTheme';
import { getSeriesColors, getPhaseColors, getAxisTickStyle, getGridStroke } from '@nexus-gateway/ui-shared';
import type { MetricAggregatesResponse } from '../../api/types';
import {
  pivotProviderMetricByBucket,
  rowsForMetricChart,
  sumMetricByBucket,
  sumPointValues,
  avgMetricByBucket,
} from './metrics-aggregates-helpers';
import styles from './MetricsRollupsSection.module.css';
import { rangeToIso } from './metrics-rollups-helpers';
import { KpiCards } from './KpiCards';
import { SystemOverviewCharts } from './SystemOverviewCharts';
import { ByProviderGrid } from './ByProviderGrid';

interface MetricsRollupsSectionProps {
  embedded?: boolean;
  externalStart?: string; // ISO string — overrides internal time selector
  externalEnd?: string;
  source?: string;
}

export function MetricsRollupsSection({ embedded, externalStart, externalEnd, source }: MetricsRollupsSectionProps) {
  const { t } = useTranslation();
  const { resolvedMode } = useTheme();
  const seriesColors = getSeriesColors(resolvedMode);
  const tickStyle = getAxisTickStyle(resolvedMode);
  const gridStroke = getGridStroke(resolvedMode);
  const phase = getPhaseColors(resolvedMode);
  const hasExternal = !!(externalStart && externalEnd);
  const [hours, setHours] = useState(24);
  const internal = useMemo(() => rangeToIso(hours), [hours]);
  const start = hasExternal ? externalStart! : internal.start;
  const end = hasExternal ? externalEnd! : internal.end;

  // Compute effective hours for series bucketing
  const effectiveHours = useMemo(() => {
    const ms = new Date(end).getTime() - new Date(start).getTime();
    return Math.max(1, Math.round(ms / 3600_000));
  }, [start, end]);

  const metricsParams = useMemo(() => {
    const p: Record<string, string> = { start, end };
    if (source) p.source = source;
    return p;
  }, [start, end, source]);
  const { data, loading, error, refetch } = useApi<MetricAggregatesResponse>(
    () => analyticsApi.metricsAggregates(metricsParams),
    ['admin', 'metrics', 'aggregates', start, end, source ?? ''],
  );

  const requestSeries = useMemo(
    () => (data ? sumMetricByBucket(data.data, 'request_count', { rangeHours: effectiveHours }) : []),
    [data, effectiveHours],
  );
  const tokenSeries = useMemo(
    () => (data ? sumMetricByBucket(data.data, 'token_usage', { rangeHours: effectiveHours }) : []),
    [data, effectiveHours],
  );
  const costSeries = useMemo(
    () => (data ? sumMetricByBucket(data.data, 'estimated_cost', { rangeHours: effectiveHours }) : []),
    [data, effectiveHours],
  );
  const errorSeries = useMemo(
    () => (data ? sumMetricByBucket(data.data, 'error_count', { rangeHours: effectiveHours }) : []),
    [data, effectiveHours],
  );
  const cacheHitSeries = useMemo(
    () => (data ? sumMetricByBucket(data.data, 'cache_hits', { rangeHours: effectiveHours }) : []),
    [data, effectiveHours],
  );
  const cacheSavedCostSeries = useMemo(
    () => (data ? sumMetricByBucket(data.data, 'cache_saved_cost', { rangeHours: effectiveHours }) : []),
    [data, effectiveHours],
  );

  const reqByProvider = useMemo(
    () => (data ? pivotProviderMetricByBucket(data.data, 'request_count', { rangeHours: effectiveHours }) : { data: [], providers: [] }),
    [data, effectiveHours],
  );
  const tokenByProvider = useMemo(
    () => (data ? pivotProviderMetricByBucket(data.data, 'token_usage', { rangeHours: effectiveHours }) : { data: [], providers: [] }),
    [data, effectiveHours],
  );
  const costByProvider = useMemo(
    () => (data ? pivotProviderMetricByBucket(data.data, 'estimated_cost', { rangeHours: effectiveHours }) : { data: [], providers: [] }),
    [data, effectiveHours],
  );
  const latencyByProvider = useMemo(
    () => (data ? pivotProviderMetricByBucket(data.data, 'latency_p50', { rangeHours: effectiveHours }) : { data: [], providers: [] }),
    [data, effectiveHours],
  );

  // Phase breakdown over time — 4 average lines per bucket assembled from the
  // rollup writer's separate `_sum` + `_count` metric pairs. Combined into one
  // stacked-area dataset so a single chart shows where wall-clock time was
  // spent (same picture as LatencyPhasesPanel on Analytics → Latency). Body
  // is derived from upstream_total − upstream_ttfb so the segments stack
  // cleanly without double-counting body inside the upstream-total line.
  const phaseOverTimeData = useMemo(() => {
    if (!data) return [];
    const us = avgMetricByBucket(data.data, 'latency_us_sum', 'latency_us_count', { rangeHours: effectiveHours });
    const ttfb = avgMetricByBucket(data.data, 'latency_upstream_ttfb_sum', 'latency_upstream_ttfb_count', { rangeHours: effectiveHours });
    const upTotal = avgMetricByBucket(data.data, 'latency_upstream_total_sum', 'latency_upstream_total_count', { rangeHours: effectiveHours });
    const hooks = avgMetricByBucket(data.data, 'latency_hooks_sum', 'latency_hooks_count', { rangeHours: effectiveHours });
    const usByBucket = new Map(us.map(p => [p.bucketAt, p.avg]));
    const ttfbByBucket = new Map(ttfb.map(p => [p.bucketAt, p.avg]));
    const upByBucket = new Map(upTotal.map(p => [p.bucketAt, p.avg]));
    const hooksByBucket = new Map(hooks.map(p => [p.bucketAt, p.avg]));
    // Union of bucket keys — any phase that has at least one sample in a
    // bucket should produce a point so the area shows a non-zero phase
    // even when the sibling phase was idle.
    const allBuckets = new Set<string>([
      ...usByBucket.keys(),
      ...ttfbByBucket.keys(),
      ...upByBucket.keys(),
      ...hooksByBucket.keys(),
    ]);
    return [...allBuckets]
      .sort()
      .map(bucketAt => {
        const ttfbAvg = ttfbByBucket.get(bucketAt) ?? 0;
        const upAvg = upByBucket.get(bucketAt) ?? 0;
        return {
          bucketStart: us.find(p => p.bucketAt === bucketAt)?.bucketStart
            ?? ttfb.find(p => p.bucketAt === bucketAt)?.bucketStart
            ?? upTotal.find(p => p.bucketAt === bucketAt)?.bucketStart
            ?? hooks.find(p => p.bucketAt === bucketAt)?.bucketStart
            ?? bucketAt,
          us: Math.round(usByBucket.get(bucketAt) ?? 0),
          hooks: Math.round(hooksByBucket.get(bucketAt) ?? 0),
          ttfb: Math.round(ttfbAvg),
          // body stacks on top of ttfb — it's the streaming-body residual
          // (upstream_total − upstream_ttfb) so the stack height equals
          // upstream_total + us + hooks.
          body: Math.max(0, Math.round(upAvg - ttfbAvg)),
        };
      });
  }, [data, effectiveHours]);

  const kpis = useMemo(() => {
    if (!data) return null;
    const totalRequests = sumPointValues(rowsForMetricChart(data.data, 'request_count'));
    const totalTokens = sumPointValues(rowsForMetricChart(data.data, 'token_usage'));
    const totalCost = sumPointValues(rowsForMetricChart(data.data, 'estimated_cost'));
    const totalErrors = sumPointValues(rowsForMetricChart(data.data, 'error_count'));
    const totalCacheHits = sumPointValues(rowsForMetricChart(data.data, 'cache_hits'));
    const totalCacheSaved = sumPointValues(rowsForMetricChart(data.data, 'cache_saved_cost'));
    return { totalRequests, totalTokens, totalCost, totalErrors, totalCacheHits, totalCacheSaved };
  }, [data]);

  const adminAuthRows = useMemo(() => (data ? data.data.filter((r) => r.metricName === 'admin_auth') : []), [data]);
  const showVKMetrics = !source || source === 'vk';

  if (loading) return <LoadingSpinner />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!data) return null;

  const totalPoints = data.data.length;

  return (
    <Stack gap="lg">
      <KpiCards
        hasExternal={hasExternal}
        hours={hours}
        setHours={setHours}
        kpis={kpis}
        totalPoints={totalPoints}
        adminAuthRows={adminAuthRows}
      />

      <SystemOverviewCharts
        resolvedMode={resolvedMode}
        tickStyle={tickStyle}
        gridStroke={gridStroke}
        showVKMetrics={showVKMetrics}
        requestSeries={requestSeries}
        tokenSeries={tokenSeries}
        costSeries={costSeries}
        errorSeries={errorSeries}
        cacheHitSeries={cacheHitSeries}
        cacheSavedCostSeries={cacheSavedCostSeries}
      />

      <ByProviderGrid
        seriesColors={seriesColors}
        tickStyle={tickStyle}
        gridStroke={gridStroke}
        phase={phase}
        reqByProvider={reqByProvider}
        tokenByProvider={tokenByProvider}
        costByProvider={costByProvider}
        latencyByProvider={latencyByProvider}
        phaseOverTimeData={phaseOverTimeData}
      />
    </Stack>
  );
}

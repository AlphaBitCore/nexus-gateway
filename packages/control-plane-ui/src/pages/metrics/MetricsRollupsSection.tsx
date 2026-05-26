import { useMemo, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '../../hooks/useApi';
import { analyticsApi } from '@/api/services';
import {
  Card, LoadingSpinner, ErrorBanner, ExpandableWrapper, Stack, AnimatedNumber,
} from '@/components/ui';
import { useTheme } from '../../theme/useTheme';
import { getSeriesColors, getSemanticColor, getPhaseColors, getAxisTickStyle, getGridStroke } from '@nexus-gateway/ui-shared';
import type { MetricAggregatesResponse } from '../../api/types';
import {
  pivotProviderMetricByBucket,
  rowsForMetricChart,
  sumMetricByBucket,
  sumPointValues,
  avgMetricByBucket,
} from './metrics-aggregates-helpers';
import { LineChart, Line, XAxis, YAxis, Tooltip, ResponsiveContainer, Legend, CartesianGrid, AreaChart, Area } from 'recharts';
import { formatDateTime, formatUsd, formatCompact, formatTokens } from '@/lib/format';
import styles from './MetricsRollupsSection.module.css';

function rangeToIso(hours: number): { start: string; end: string } {
  const end = new Date();
  const start = new Date(end.getTime() - hours * 3600_000);
  return { start: start.toISOString(), end: end.toISOString() };
}

type RollupTooltipRow = { bucketAt?: string };

function tooltipBucketLabel(_label: unknown, payload: readonly unknown[]): string {
  const row = (payload?.[0] as { payload?: RollupTooltipRow } | undefined)?.payload;
  const at = row?.bucketAt;
  if (typeof at === 'string') return formatDateTime(at);
  return '';
}

function formatLatencyTooltip(value: unknown, name: unknown): [string, string] {
  const n = typeof value === 'number' ? value : Number(value);
  return [`${Number.isFinite(n) ? n : 0} ms`, String(name ?? 'p50')];
}

function formatUsdBySeriesTooltip(value: unknown, name: unknown): [string, string] {
  const n = typeof value === 'number' ? value : Number(value);
  return [formatUsd(Number.isFinite(n) ? n : 0), String(name ?? '')];
}

function StatCard({ label, value, subtitle }: { label: string; value: ReactNode; subtitle?: string }) {
  return (
    <Card className={styles.statCard}>
      <div className={styles.statLabel}>{label}</div>
      <div className={styles.statValue}>{value}</div>
      {subtitle && <div className={styles.statSubtitle}>{subtitle}</div>}
    </Card>
  );
}

function ChartPanel({
  title,
  subtitle,
  empty,
  emptyText,
  children,
}: {
  title: string;
  subtitle?: string;
  empty?: boolean;
  emptyText?: string;
  children: ReactNode;
}) {
  return (
    <ExpandableWrapper>
      <Card className={styles.chartPanel}>
        <h2
          className={styles.chartPanelTitle}
          style={{ marginBottom: subtitle ? 'var(--g-space-xs)' : 'var(--g-space-md)' }}
        >
          {title}
        </h2>
        {subtitle ? (
          <p className={styles.chartPanelSubtitle}>{subtitle}</p>
        ) : null}
        {empty ? <p className={styles.chartPanelEmpty}>{emptyText}</p> : children}
      </Card>
    </ExpandableWrapper>
  );
}

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
      {embedded ? (
        <p className={styles.mutedInfo}>
          {t('pages:metrics.rollupsDescription')}
        </p>
      ) : null}

      {!hasExternal && (
        <div className={styles.rangeBar}>
          <label className={styles.rangeLabel}>
            <span>{t('pages:metrics.range')}</span>
            <select
              value={hours}
              onChange={(e) => setHours(Number(e.target.value))}
              className={styles.rangeSelect}
            >
              <option value={6}>{t('pages:metrics.last6Hours')}</option>
              <option value={24}>{t('pages:metrics.last24Hours')}</option>
              <option value={168}>{t('pages:metrics.last7Days')}</option>
            </select>
          </label>
        </div>
      )}

      {kpis ? (
        <div className={styles.kpiGrid}>
          <StatCard label={t('pages:metrics.totalRequests')} value={<AnimatedNumber value={kpis.totalRequests} format={formatCompact} />} subtitle={t('pages:metrics.totalRequestsSubtitle')} />
          <StatCard label={t('pages:metrics.totalTokens')} value={<AnimatedNumber value={kpis.totalTokens} format={formatTokens} />} subtitle={t('pages:metrics.totalTokensSubtitle')} />
          <StatCard label={t('pages:metrics.estCost')} value={<AnimatedNumber value={kpis.totalCost} precision={2} format={formatUsd} />} subtitle={t('pages:metrics.estCostSubtitle')} />
          <StatCard label={t('pages:metrics.errors')} value={<AnimatedNumber value={kpis.totalErrors} format={formatCompact} />} subtitle={t('pages:metrics.errorsSubtitle')} />
          <StatCard label={t('pages:metrics.cacheHits')} value={<AnimatedNumber value={kpis.totalCacheHits} format={formatCompact} />} subtitle={t('pages:metrics.cacheHitsSubtitle')} />
          {kpis.totalCacheSaved > 0 && (
            <StatCard label={t('pages:metrics.cacheSavings')} value={<AnimatedNumber value={kpis.totalCacheSaved} precision={2} format={formatUsd} />} subtitle={t('pages:metrics.cacheSavingsSubtitle')} />
          )}
          <StatCard label={t('pages:metrics.rollupRows')} value={<AnimatedNumber value={totalPoints} />} subtitle={t('pages:metrics.rollupRowsSubtitle')} />
        </div>
      ) : null}

      {adminAuthRows.length > 0 ? (
        <p className={styles.muted}>
          {t('pages:metrics.adminAuthNote', { count: adminAuthRows.length })}
        </p>
      ) : null}

      <div className={styles.sectionHeading}>{t('pages:metrics.systemOverview', 'System Overview')}</div>
      <p className={styles.sectionSubtitle}>{t('pages:metrics.systemOverviewSubtitle', 'Overall traffic patterns across all providers')}</p>

      {/* Requests — full width, hero chart */}
      <div className={styles.chartFull}>
        <ChartPanel title={t('pages:metrics.chartRequestsTotal')} subtitle={t('pages:metrics.chartSummedSubtitle')} empty={requestSeries.length === 0} emptyText={t('pages:metrics.noDataInWindow')}>
          <ResponsiveContainer width="100%" height={300}>
            <LineChart data={requestSeries}>
              <CartesianGrid strokeDasharray="3 3" stroke={gridStroke} opacity={0.6} />
              <XAxis dataKey="bucketStart" tick={tickStyle} />
              <YAxis tick={tickStyle} />
              <Tooltip labelFormatter={tooltipBucketLabel} />
              <Legend />
              <Line type="monotone" dataKey="total" name="Requests" stroke={getSemanticColor(resolvedMode, 'requests')} dot={false} strokeWidth={2} />
            </LineChart>
          </ResponsiveContainer>
        </ChartPanel>
      </div>

      {/* Tokens + Cost — side by side (VK only) */}
      {showVKMetrics && (
        <div className={styles.chartPair}>
          <ChartPanel title={t('pages:metrics.chartTokensTotal')} subtitle={t('pages:metrics.chartSummedSubtitle')} empty={tokenSeries.length === 0} emptyText={t('pages:metrics.noDataInWindow')}>
            <ResponsiveContainer width="100%" height={280}>
              <LineChart data={tokenSeries}>
                <CartesianGrid strokeDasharray="3 3" stroke={gridStroke} opacity={0.6} />
                <XAxis dataKey="bucketStart" tick={tickStyle} />
                <YAxis tick={tickStyle} tickFormatter={(v) => formatTokens(Number(v))} />
                <Tooltip
                  labelFormatter={tooltipBucketLabel}
                  formatter={(value, name) => [formatTokens(Number(value)), name as string]}
                />
                <Legend />
                <Line type="monotone" dataKey="total" name="Tokens" stroke={getSemanticColor(resolvedMode, 'tokens')} dot={false} strokeWidth={2} />
              </LineChart>
            </ResponsiveContainer>
          </ChartPanel>

          <ChartPanel title={t('pages:metrics.chartCostTotal')} subtitle={t('pages:metrics.chartCostSubtitle')} empty={costSeries.length === 0} emptyText={t('pages:metrics.noDataInWindow')}>
            <ResponsiveContainer width="100%" height={280}>
              <LineChart
                data={(() => {
                  const bySaved = new Map(cacheSavedCostSeries.map(r => [r.bucketAt, r.total]));
                  return costSeries.map(r => ({ ...r, savedCost: bySaved.get(r.bucketAt) ?? 0 }));
                })()}
              >
                <CartesianGrid strokeDasharray="3 3" stroke={gridStroke} opacity={0.6} />
                <XAxis dataKey="bucketStart" tick={tickStyle} />
                <YAxis tick={tickStyle} />
                <Tooltip labelFormatter={tooltipBucketLabel} formatter={formatUsdBySeriesTooltip} />
                <Legend />
                <Line type="monotone" dataKey="total" name={t('pages:metrics.costGross')} stroke={getSemanticColor(resolvedMode, 'cost')} dot={false} strokeWidth={2} />
                <Line type="monotone" dataKey="savedCost" name={t('pages:metrics.costCacheSaved')} stroke={getSemanticColor(resolvedMode, 'cacheHits')} dot={{ r: 2 }} strokeWidth={2} />
              </LineChart>
            </ResponsiveContainer>
          </ChartPanel>
        </div>
      )}

      {/* Errors + Cache — full width */}
      <div className={styles.chartFull}>
        <ChartPanel title={t('pages:metrics.chartErrorsCacheHits')} subtitle={t('pages:metrics.chartErrorsCacheSubtitle')} empty={errorSeries.length === 0 && cacheHitSeries.length === 0} emptyText={t('pages:metrics.noDataInWindow')}>
          <ResponsiveContainer width="100%" height={280}>
            <LineChart
              data={(() => {
                const byBucket = new Map<string, { bucketStart: string; bucketAt: string; errors: number; cache: number }>();
                for (const r of errorSeries) {
                  byBucket.set(r.bucketAt, {
                    bucketStart: r.bucketStart,
                    bucketAt: r.bucketAt,
                    errors: r.total,
                    cache: byBucket.get(r.bucketAt)?.cache ?? 0,
                  });
                }
                for (const r of cacheHitSeries) {
                  const prev = byBucket.get(r.bucketAt);
                  if (prev) {
                    prev.cache = r.total;
                  } else {
                    byBucket.set(r.bucketAt, {
                      bucketStart: r.bucketStart,
                      bucketAt: r.bucketAt,
                      errors: 0,
                      cache: r.total,
                    });
                  }
                }
                return [...byBucket.values()].sort((a, b) => a.bucketAt.localeCompare(b.bucketAt));
              })()}
            >
              <CartesianGrid strokeDasharray="3 3" stroke={gridStroke} opacity={0.6} />
              <XAxis dataKey="bucketStart" tick={tickStyle} />
              <YAxis tick={tickStyle} />
              <Tooltip labelFormatter={tooltipBucketLabel} />
              <Legend />
              <Line type="monotone" dataKey="errors" name="Errors" stroke={getSemanticColor(resolvedMode, 'errors')} dot={false} strokeWidth={2} />
              <Line type="monotone" dataKey="cache" name="Cache hits" stroke={getSemanticColor(resolvedMode, 'cacheHits')} dot={false} strokeWidth={2} />
            </LineChart>
          </ResponsiveContainer>
        </ChartPanel>
      </div>

      <div className={styles.sectionHeading}>{t('pages:metrics.byProvider')}</div>
      <p className={styles.sectionSubtitle}>{t('pages:metrics.byProviderSubtitle', 'Performance breakdown per AI provider')}</p>

      <div className={styles.byProviderGrid}>
        <ChartPanel
          title={t('pages:metrics.requestsByProvider')}
          empty={reqByProvider.data.length === 0 || reqByProvider.providers.length === 0}
          emptyText={t('pages:metrics.noDataInWindow')}
        >
          <ResponsiveContainer width="100%" height={300}>
            <LineChart data={reqByProvider.data}>
              <CartesianGrid strokeDasharray="3 3" stroke={gridStroke} opacity={0.6} />
              <XAxis dataKey="bucketStart" tick={tickStyle} />
              <YAxis tick={tickStyle} />
              <Tooltip labelFormatter={tooltipBucketLabel} />
              <Legend />
              {reqByProvider.providers.map((p, i) => (
                <Line
                  key={p}
                  type="monotone"
                  dataKey={p}
                  name={p}
                  stroke={seriesColors[i % seriesColors.length]}
                  dot={false}
                  strokeWidth={2}
                />
              ))}
            </LineChart>
          </ResponsiveContainer>
        </ChartPanel>

        <ChartPanel
          title={t('pages:metrics.tokensByProvider')}
          empty={tokenByProvider.data.length === 0 || tokenByProvider.providers.length === 0}
          emptyText={t('pages:metrics.noDataInWindow')}
        >
          <ResponsiveContainer width="100%" height={300}>
            <LineChart data={tokenByProvider.data}>
              <CartesianGrid strokeDasharray="3 3" stroke={gridStroke} opacity={0.6} />
              <XAxis dataKey="bucketStart" tick={tickStyle} />
              <YAxis tick={tickStyle} tickFormatter={(v) => formatTokens(Number(v))} />
              <Tooltip
                labelFormatter={tooltipBucketLabel}
                formatter={(value, name) => [formatTokens(Number(value)), name as string]}
              />
              <Legend />
              {tokenByProvider.providers.map((p, i) => (
                <Line
                  key={p}
                  type="monotone"
                  dataKey={p}
                  name={p}
                  stroke={seriesColors[i % seriesColors.length]}
                  dot={false}
                  strokeWidth={2}
                />
              ))}
            </LineChart>
          </ResponsiveContainer>
        </ChartPanel>

        <ChartPanel
          title={t('pages:metrics.costByProvider')}
          empty={costByProvider.data.length === 0 || costByProvider.providers.length === 0}
          emptyText={t('pages:metrics.noDataInWindow')}
        >
          <ResponsiveContainer width="100%" height={300}>
            <LineChart data={costByProvider.data}>
              <CartesianGrid strokeDasharray="3 3" stroke={gridStroke} opacity={0.6} />
              <XAxis dataKey="bucketStart" tick={tickStyle} />
              <YAxis tick={tickStyle} />
              <Tooltip labelFormatter={tooltipBucketLabel} formatter={formatUsdBySeriesTooltip} />
              <Legend />
              {costByProvider.providers.map((p, i) => (
                <Line
                  key={p}
                  type="monotone"
                  dataKey={p}
                  name={p}
                  stroke={seriesColors[i % seriesColors.length]}
                  dot={false}
                  strokeWidth={2}
                />
              ))}
            </LineChart>
          </ResponsiveContainer>
        </ChartPanel>

        <ChartPanel
          title={t('pages:metrics.latencyByProvider')}
          subtitle={t('pages:metrics.latencyByProviderSubtitle')}
          empty={latencyByProvider.data.length === 0 || latencyByProvider.providers.length === 0}
          emptyText={t('pages:metrics.noDataInWindow')}
        >
          <ResponsiveContainer width="100%" height={300}>
            <LineChart data={latencyByProvider.data}>
              <CartesianGrid strokeDasharray="3 3" stroke={gridStroke} opacity={0.6} />
              <XAxis dataKey="bucketStart" tick={tickStyle} />
              <YAxis tick={tickStyle} />
              <Tooltip labelFormatter={tooltipBucketLabel} formatter={formatLatencyTooltip} />
              <Legend />
              {latencyByProvider.providers.map((p, i) => (
                <Line
                  key={p}
                  type="monotone"
                  dataKey={p}
                  name={p}
                  stroke={seriesColors[i % seriesColors.length]}
                  dot={false}
                  strokeWidth={2}
                />
              ))}
            </LineChart>
          </ResponsiveContainer>
        </ChartPanel>

        {/* Phase breakdown over time — same colour grammar as
            LatencyWaterfall + LatencyPhasesPanel + LatencyMini so an
            operator who learns the palette on the Traffic Detail
            waterfall reads this chart immediately. */}
        <ChartPanel
          title={t('pages:metrics.latencyPhaseBreakdown', 'Latency Phase Breakdown Over Time')}
          subtitle={t('pages:metrics.latencyPhaseBreakdownSubtitle', 'Average ms per phase per bucket. Hooks + Our Overhead + Upstream TTFB + Upstream Body stacked = end-to-end average. Empty until the rollup writer has flushed phase metrics for the window.')}
          empty={phaseOverTimeData.length === 0}
          emptyText={t('pages:metrics.noDataInWindow')}
        >
          <ResponsiveContainer width="100%" height={300}>
            <AreaChart data={phaseOverTimeData}>
              <CartesianGrid strokeDasharray="3 3" stroke={gridStroke} opacity={0.6} />
              <XAxis dataKey="bucketStart" tick={tickStyle} />
              <YAxis tick={tickStyle} />
              <Tooltip labelFormatter={tooltipBucketLabel} formatter={formatLatencyTooltip} />
              <Legend />
              <Area type="monotone" dataKey="hooks" stackId="latency" stroke={phase.reqHooks} fill={phase.reqHooks} name={t('pages:traffic.detail.waterfall.reqHooks', 'Hooks')} />
              <Area type="monotone" dataKey="us"    stackId="latency" stroke={phase.our} fill={phase.our} name={t('pages:traffic.detail.waterfall.ourOther', 'Our Overhead')} />
              <Area type="monotone" dataKey="ttfb"  stackId="latency" stroke={phase.ttfb} fill={phase.ttfb} name={t('pages:traffic.detail.waterfall.upstreamTtfb', 'Upstream TTFB')} />
              <Area type="monotone" dataKey="body"  stackId="latency" stroke={phase.body} fill={phase.body} name={t('pages:traffic.detail.waterfall.upstreamBody', 'Upstream Body')} />
            </AreaChart>
          </ResponsiveContainer>
        </ChartPanel>
      </div>
    </Stack>
  );
}

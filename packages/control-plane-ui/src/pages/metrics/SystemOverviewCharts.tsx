import { useTranslation } from 'react-i18next';
import { getSemanticColor, type ChartColorMode } from '@nexus-gateway/ui-shared';
import { LineChart, Line, XAxis, YAxis, Tooltip, ResponsiveContainer, Legend, CartesianGrid } from 'recharts';
import { formatTokens } from '@/lib/format';
import styles from './MetricsRollupsSection.module.css';
import { ChartPanel, tooltipBucketLabel, formatUsdBySeriesTooltip } from './metrics-rollups-helpers';

type MetricSeriesPoint = { bucketAt: string; bucketStart: string; total: number };

interface SystemOverviewChartsProps {
  resolvedMode: ChartColorMode;
  tickStyle: Record<string, string | number>;
  gridStroke: string;
  showVKMetrics: boolean;
  requestSeries: MetricSeriesPoint[];
  tokenSeries: MetricSeriesPoint[];
  costSeries: MetricSeriesPoint[];
  errorSeries: MetricSeriesPoint[];
  cacheHitSeries: MetricSeriesPoint[];
  cacheSavedCostSeries: MetricSeriesPoint[];
}

export function SystemOverviewCharts({
  resolvedMode,
  tickStyle,
  gridStroke,
  showVKMetrics,
  requestSeries,
  tokenSeries,
  costSeries,
  errorSeries,
  cacheHitSeries,
  cacheSavedCostSeries,
}: SystemOverviewChartsProps) {
  const { t } = useTranslation();

  return (
    <>
      <div className={styles.sectionHeaderBlock}>
        <div className={styles.sectionHeading}>{t('pages:metrics.systemOverview', 'System Overview')}</div>
        <p className={styles.sectionSubtitle}>{t('pages:metrics.systemOverviewSubtitle', 'Overall traffic patterns across all providers')}</p>
      </div>

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
    </>
  );
}

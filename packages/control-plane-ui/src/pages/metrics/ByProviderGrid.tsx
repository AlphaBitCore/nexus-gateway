import { useTranslation } from 'react-i18next';
import { getPhaseColors } from '@nexus-gateway/ui-shared';
import { LineChart, Line, XAxis, YAxis, Tooltip, ResponsiveContainer, Legend, CartesianGrid, AreaChart, Area } from 'recharts';
import { formatTokens } from '@/lib/format';
import type { PivotProviderSeriesResult } from './metrics-aggregates-helpers';
import styles from './MetricsRollupsSection.module.css';
import { ChartPanel, tooltipBucketLabel, formatLatencyTooltip, formatUsdBySeriesTooltip } from './metrics-rollups-helpers';

type PhaseColors = ReturnType<typeof getPhaseColors>;

type PhaseOverTimePoint = {
  bucketStart: string;
  us: number;
  hooks: number;
  ttfb: number;
  body: number;
};

interface ByProviderGridProps {
  seriesColors: readonly string[];
  tickStyle: Record<string, string | number>;
  gridStroke: string;
  phase: PhaseColors;
  reqByProvider: PivotProviderSeriesResult;
  tokenByProvider: PivotProviderSeriesResult;
  costByProvider: PivotProviderSeriesResult;
  latencyByProvider: PivotProviderSeriesResult;
  phaseOverTimeData: PhaseOverTimePoint[];
}

export function ByProviderGrid({
  seriesColors,
  tickStyle,
  gridStroke,
  phase,
  reqByProvider,
  tokenByProvider,
  costByProvider,
  latencyByProvider,
  phaseOverTimeData,
}: ByProviderGridProps) {
  const { t } = useTranslation();

  return (
    <>
      <div className={styles.sectionHeaderBlock}>
        <div className={styles.sectionHeading}>{t('pages:metrics.byProvider')}</div>
        <p className={styles.sectionSubtitle}>{t('pages:metrics.byProviderSubtitle', 'Performance breakdown per AI provider')}</p>
      </div>

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
    </>
  );
}

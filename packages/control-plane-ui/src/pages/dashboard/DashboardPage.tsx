import { useState, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import clsx from 'clsx';
import { useApi } from '../../hooks/useApi';
import { useCountUp } from '../../hooks/useCountUp';
import { Stack, Grid, Card, PageHeader, Tooltip, ErrorBanner, ExpandableWrapper, Sparkline, Button, AnimatedNumber } from '@/components/ui';
import type { AnalyticsSummary, ProviderBreakdown, Provider, SparklineResponse } from '../../api/types';
import { ADMIN_LIST_FULL_PAGE_PARAMS } from '../../constants/admin-api';
import { analyticsApi, providerApi } from '@/api/services';
import type { CacheROISummary } from '@/api/services/overview/analytics';
import { proxyApi } from '../../api/services/infrastructure/misc/proxy';
import styles from './DashboardPage.module.css';
import { formatTokens } from '@/lib/format';

/* ── Time window ────────────────────────────────────────────────────────── */

type TimeWindow = '1h' | '1d' | '7d' | '30d';

const WINDOW_MS: Record<TimeWindow, number> = {
  '1h':  60 * 60_000,
  '1d':  24 * 60 * 60_000,
  '7d':  7  * 24 * 60 * 60_000,
  '30d': 30 * 24 * 60 * 60_000,
};

const WINDOW_OPTIONS: TimeWindow[] = ['1h', '1d', '7d', '30d'];

/* ── Info icon ──────────────────────────────────────────────────────────── */

function InfoIcon({ description }: { description: string }) {
  return (
    <Tooltip content={description} side="bottom">
      <span className={styles.infoIcon}>i</span>
    </Tooltip>
  );
}

/* ── Main Component ─────────────────────────────────────────────────────── */

export function DashboardPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();

  /* ── Time window state ──────────────────────────────────────────────── */

  const [timeWindow, setTimeWindow] = useState<TimeWindow>('30d');

  const { startTime, endTime } = useMemo(() => {
    const end = new Date();
    const start = new Date(end.getTime() - WINDOW_MS[timeWindow]);
    return { startTime: start.toISOString(), endTime: end.toISOString() };
  }, [timeWindow]);

  /* ── Data fetching ──────────────────────────────────────────────────── */

  const { data: summary, loading, error, refetch } = useApi<AnalyticsSummary>(
    () => analyticsApi.summary({ startTime, endTime }),
    ['admin', 'analytics', 'summary', timeWindow],
  );
  const { data: providers } = useApi<{ data: ProviderBreakdown[] }>(
    () => analyticsApi.byProvider({ startTime, endTime }),
    ['admin', 'analytics', 'by-provider', timeWindow],
  );
  const { data: providerList } = useApi<{ data: Provider[] }>(
    () => providerApi.list({ ...ADMIN_LIST_FULL_PAGE_PARAMS }),
    ['admin', 'providers', 'list', 'dashboard'],
  );
  const { data: sparklineData } = useApi<SparklineResponse>(
    () => analyticsApi.sparkline({ startTime, endTime }),
    ['admin', 'analytics', 'sparkline', timeWindow],
  );
  const { data: cacheROI } = useApi<CacheROISummary>(
    () => analyticsApi.cacheROI({ start: startTime, end: endTime }),
    ['admin', 'analytics', 'cache-roi', 'dashboard', timeWindow],
  );

  // Per-provider latency phase percentiles for the Latency Health row.
  // Aggregating by `provider` lets us surface the slowest provider in a
  // standalone callout without a second round-trip.
  const { data: latencyPhases } = useApi(
    () => analyticsApi.latencyPhases({ groupBy: 'provider', start: startTime, end: endTime }),
    ['admin', 'analytics', 'latency-phases', 'dashboard', timeWindow],
  );

  /* ── Compliance Proxy ───────────────────────────────────────────────── */

  const { data: proxyCoverage, error: proxyCoverageError } = useApi(
    () => proxyApi.getComplianceCoverage(startTime, endTime),
    ['proxy', 'compliance', 'dashboard', timeWindow],
  );
  const { data: rejectStats, error: rejectStatsError } = useApi(
    () => proxyApi.getRejectStats(startTime, endTime),
    ['proxy', 'reject-stats', 'dashboard', timeWindow],
  );

  const proxyReachable = useMemo(() => {
    if (proxyCoverage || rejectStats) return true;
    if (proxyCoverageError && rejectStatsError) return false;
    return null;
  }, [proxyCoverage, rejectStats, proxyCoverageError, rejectStatsError]);

  const proxyTotalRequests = useMemo(() => {
    if (!proxyCoverage?.breakdown) return 0;
    return Object.values(proxyCoverage.breakdown).reduce((sum, v) => sum + v, 0);
  }, [proxyCoverage]);

  const proxyRejectCount = rejectStats?.totalRejects ?? 0;
  const proxyCoveragePercent = proxyCoverage?.coveragePercent ?? 0;

  /* ── Derived metrics ────────────────────────────────────────────────── */

  const p95Latency = summary?.p95LatencyMs ?? 0;

  const sparkData = useMemo(() => {
    const series = sparklineData?.series ?? [];
    if (series.length === 0) return { requests: [], errors: [], latency: [], cost: [], tokens: [], cacheHitRate: [], cacheSavings: [] };
    return {
      requests: series.map(b => b.values?.request_count ?? 0),
      errors: series.map(b =>
        (b.values?.status_4xx_count ?? 0) + (b.values?.status_5xx_count ?? 0),
      ),
      latency: series.map(b => {
        const sum = b.values?.latency_sum ?? 0;
        const count = b.values?.latency_count ?? 0;
        return count > 0 ? Math.round(sum / count) : 0;
      }),
      cost: series.map(b => Math.round((b.values?.estimated_cost_usd ?? 0) * 10000)),
      tokens: series.map(b => b.values?.total_tokens ?? 0),
      cacheHitRate: series.map(b => {
        const hits = b.values?.cache_hit_count ?? 0;
        const total = b.values?.request_count ?? 0;
        return total > 0 ? Math.round((hits / total) * 1000) : 0;
      }),
      cacheSavings: series.map(b =>
        Math.round(((b.values?.cache_saved_cost_usd ?? 0) + (b.values?.cache_net_savings_usd ?? 0)) * 10000),
      ),
    };
  }, [sparklineData]);

  /* ── Count-up animations (must be before early returns) ─────────────── */

  const vkRequests = summary?.totalRequests ?? 0;
  const vkErrors = summary?.errorCount ?? 0;
  const combinedRequests = vkRequests + proxyTotalRequests;

  const animRequests = useCountUp(combinedRequests);
  const animErrorRate10x = useCountUp(Math.round((summary?.errorRate ?? 0) * 1000));
  const animErrors = useCountUp(vkErrors + proxyRejectCount);
  const animP95 = useCountUp(Math.round(p95Latency));
  const animAvg = useCountUp(Math.round(summary?.avgLatencyMs ?? 0));
  const animCost100x = useCountUp(Math.round((summary?.totalEstimatedCostUsd ?? 0) * 100));
  const animTokens = useCountUp(summary?.totalTokens ?? 0);

  const topProviders = useMemo(() => {
    const list = [...(providers?.data ?? [])];
    list.sort((a, b) => b.requestCount - a.requestCount);
    return list.slice(0, 5);
  }, [providers]);

  /* ── Loading / error states ─────────────────────────────────────────── */

  if (loading) return (
    <Stack gap="lg">
      <PageHeader title={t('pages:dashboard.title')} />
      <Grid columns={4} gap="md">
        {[0, 1, 2, 3].map((i) => (
          <div key={i} className={styles.skeletonCard}>
            <div className={styles.skeletonBarLabel} />
            <div className={styles.skeletonBarValue} />
          </div>
        ))}
      </Grid>
    </Stack>
  );
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!summary) return null;

  const activeProviders = (providerList?.data ?? []).filter(p => p.enabled).length;
  const totalProviders = (providerList?.data ?? []).length;
  const cacheHitRate = summary?.cacheHitRate ?? 0;

  const pointCount = Math.max(sparkData.requests.length, 2);
  const activeProvidersSpark = Array<number>(pointCount).fill(activeProviders);
  const coverageSpark = proxyReachable !== false
    ? Array<number>(pointCount).fill(Math.round(proxyCoveragePercent))
    : [0, 0];

  const vkRequestsPct = combinedRequests > 0 ? (vkRequests / combinedRequests) * 100 : 100;
  const proxyRequestsPct = combinedRequests > 0 ? (proxyTotalRequests / combinedRequests) * 100 : 0;
  const errorRateClass = (summary.errorRate ?? 0) > 0.05 ? styles.metricValueDanger : styles.metricValueSuccess;

  const windowLabel = t(`pages:dashboard.win${timeWindow}` as never);

  /* ── Render ─────────────────────────────────────────────────────────── */

  return (
    <Stack gap="lg">
      <section className={styles.hero}>
        <div className={styles.heroRow}>
          <div className={styles.heroText}>
            <h1 className={styles.heroTitle}>{t('pages:dashboard.title')}</h1>
            <p className={styles.heroSubtitle}>{t('pages:dashboard.heroSubtitle')}</p>
          </div>
          <div className={styles.windowPicker}>
            {WINDOW_OPTIONS.map((w) => (
              <button
                key={w}
                type="button"
                className={clsx(styles.windowBtn, w === timeWindow && styles.windowBtnActive)}
                onClick={() => setTimeWindow(w)}
              >
                {t(`pages:dashboard.win${w}` as never)}
              </button>
            ))}
          </div>
        </div>
        <div className={styles.heroStats}>
          <div className={styles.heroStat}>
            <span className={styles.heroStatLabel}>{t('pages:dashboard.combinedRequests')}</span>
            <span className={styles.heroStatValue}>{animRequests.toLocaleString()}</span>
            <span className={styles.heroStatSub}>
              {t('pages:dashboard.splitBarSubtitle', { vk: vkRequests.toLocaleString(), proxy: proxyTotalRequests.toLocaleString() })}
            </span>
          </div>
          <div className={styles.heroStat}>
            <span className={styles.heroStatLabel}>{t('pages:dashboard.totalCostLabel')}</span>
            <span className={styles.heroStatValue}>${(animCost100x / 100).toFixed(2)}</span>
            <span className={styles.heroStatSub}>{t('pages:dashboard.estimated')}</span>
          </div>
          <div className={styles.heroStat}>
            <span className={styles.heroStatLabel}>{t('pages:dashboard.totalTokensLabel')}</span>
            <span className={styles.heroStatValue}>{formatTokens(animTokens)}</span>
            <span className={styles.heroStatSub}>{t('pages:dashboard.promptPlusCompletion')}</span>
          </div>
          <div className={styles.heroStat}>
            <span className={styles.heroStatLabel}>{t('pages:dashboard.complianceCoverage')}</span>
            <span className={styles.heroStatValue}>
              {proxyReachable === false ? '—' : <AnimatedNumber value={proxyCoveragePercent} precision={1} format={(n) => `${n.toFixed(1)}%`} />}
            </span>
            <span className={styles.heroStatSub}>
              {proxyReachable === false ? t('pages:dashboard.proxyNotReachable') : windowLabel}
            </span>
          </div>
        </div>
      </section>

      {/* ── Section 1: System Health ────────────────────────────────────── */}
      <div className={styles.sectionHeader}>
        <h2 className={styles.sectionTitle}>{t('pages:dashboard.sectionSystemHealth')}</h2>
        <p className={styles.sectionSubtitle}>{t('pages:dashboard.sectionSystemHealthSubtitle')}</p>
      </div>

      <Grid columns={4} gap="md">
        {/* Combined Requests */}
        <ExpandableWrapper>
          <Card padding="lg" className={styles.metricCard}>
            <div className={styles.metricLabelRow}>
              {t('pages:dashboard.combinedRequests')}
              <InfoIcon description={t('pages:dashboard.combinedRequestsDesc')} />
            </div>
            <div className={styles.metricValueRow}>
              <span className={styles.metricValue}>{animRequests.toLocaleString()}</span>
              <Sparkline data={sparkData.requests} color="var(--color-primary)" />
            </div>
            <div className={styles.splitBar}>
              <div title={t('pages:dashboard.splitBarVk', { count: vkRequests.toLocaleString() })} className={styles.splitBarVk} style={{ width: `${vkRequestsPct}%` }} />
              <div title={t('pages:dashboard.splitBarProxy', { count: proxyTotalRequests.toLocaleString() })} className={styles.splitBarProxy} style={{ width: `${proxyRequestsPct}%` }} />
            </div>
            <span className={styles.metricSubtitle}>
              {t('pages:dashboard.splitBarSubtitle', { vk: vkRequests.toLocaleString(), proxy: proxyTotalRequests.toLocaleString() })}
            </span>
          </Card>
        </ExpandableWrapper>

        {/* Error Rate */}
        <ExpandableWrapper>
          <Card padding="lg" className={styles.metricCard}>
            <div className={styles.metricLabelRow}>
              {t('pages:dashboard.errorRateLabel')}
              <InfoIcon description={t('pages:dashboard.errorRateDesc')} />
            </div>
            <div className={styles.metricValueRow}>
              <span className={errorRateClass}>{(animErrorRate10x / 10).toFixed(1)}%</span>
              <Sparkline data={sparkData.errors} color="var(--color-danger)" />
            </div>
            <span className={styles.metricSubtitle}>{t('pages:dashboard.errorsAndRejects', { count: animErrors.toLocaleString() })}</span>
          </Card>
        </ExpandableWrapper>

        {/* P95 Latency — keeps the sparkline that makes this card
            unique, but the subtitle now exposes the phase breakdown
            (Us / TTFB / Upstream) using the busiest provider's P95
            from latencyPhases when available. Lets the eye scan
            "1573ms · Us 3 · TTFB 1.2s · Upstream 1.6s" at a glance —
            same direction as Provider Usage / Detail cards. Falls
            back to the old avg subtitle when latencyPhases is empty. */}
        <ExpandableWrapper>
          <Card padding="lg" className={styles.metricCard}>
            <div className={styles.metricLabelRow}>
              {t('pages:dashboard.p95LatencyLabel')}
              <InfoIcon description={t('pages:dashboard.p95LatencyDesc')} />
            </div>
            <div className={styles.metricValueRow}>
              <span className={styles.metricValue}>{animP95}ms</span>
              <Sparkline data={sparkData.latency} color="var(--color-warning)" />
            </div>
            {(() => {
              const topPhase = latencyPhases?.rows
                ? [...latencyPhases.rows].sort((a, b) => b.requestCount - a.requestCount)[0]
                : null;
              const fmt = (v: number | null | undefined): string =>
                v == null ? '—' : v < 1000 ? `${v}ms` : `${(v / 1000).toFixed(2)}s`;
              if (topPhase && (topPhase.usOverheadP95Ms != null || topPhase.upstreamTtfbP95Ms != null || topPhase.upstreamTotalP95Ms != null)) {
                const parts: string[] = [];
                if (topPhase.usOverheadP95Ms != null) parts.push(`Us ${fmt(topPhase.usOverheadP95Ms)}`);
                if (topPhase.upstreamTtfbP95Ms != null) parts.push(`TTFB ${fmt(topPhase.upstreamTtfbP95Ms)}`);
                if (topPhase.upstreamTotalP95Ms != null) parts.push(`Upstream ${fmt(topPhase.upstreamTotalP95Ms)}`);
                return (
                  <span className={styles.metricSubtitle} title={t('pages:dashboard.latencyPhaseHint', 'P95 split by busiest provider')}>
                    {parts.join(' · ')}
                  </span>
                );
              }
              return <span className={styles.metricSubtitle}>{t('pages:dashboard.avgLatencyLabel', { value: animAvg })}</span>;
            })()}
          </Card>
        </ExpandableWrapper>

        {/* Compliance Coverage */}
        <ExpandableWrapper>
          <Card padding="lg" className={styles.metricCard}>
            <div className={styles.metricLabelRow}>
              {t('pages:dashboard.complianceCoverage')}
              <InfoIcon description={t('pages:dashboard.complianceCoverageDesc')} />
            </div>
            <div className={styles.metricValueRow}>
              <span
                className={clsx(
                  styles.metricValue,
                  proxyReachable === false ? styles.coverageMuted
                    : proxyCoveragePercent >= 95 ? styles.coverageSuccess
                    : proxyCoveragePercent >= 80 ? styles.coverageWarning
                    : styles.coverageDanger,
                )}
              >
                {proxyReachable === false ? '—' : <AnimatedNumber value={proxyCoveragePercent} precision={1} format={(n) => `${n.toFixed(1)}%`} />}
              </span>
              <Sparkline
                data={coverageSpark}
                color={
                  proxyReachable === false ? 'var(--color-text-muted)'
                    : proxyCoveragePercent >= 95 ? 'var(--color-success)'
                    : proxyCoveragePercent >= 80 ? 'var(--color-warning)'
                    : 'var(--color-danger)'
                }
              />
            </div>
            <span className={styles.metricSubtitle}>
              {proxyReachable === false ? t('pages:dashboard.proxyNotReachable') : windowLabel}
            </span>
          </Card>
        </ExpandableWrapper>
      </Grid>

      {/* ── Section 1.5: Latency Health ─────────────────────────────── */}
      {latencyPhases?.rows && latencyPhases.rows.length > 0 && (() => {
        const sorted = [...latencyPhases.rows].sort((a, b) => b.requestCount - a.requestCount);
        const top = sorted[0];
        const slowest = [...latencyPhases.rows]
          .filter((r) => r.upstreamTotalP95Ms != null)
          .sort((a, b) => (b.upstreamTotalP95Ms ?? 0) - (a.upstreamTotalP95Ms ?? 0))[0];
        const fmtMs = (v: number | null | undefined) =>
          v == null ? '—' : v < 1000 ? `${v}ms` : `${(v / 1000).toFixed(2)}s`;
        return (
          <>
            <div className={styles.sectionHeader}>
              <h2 className={styles.sectionTitle}>{t('pages:dashboard.sectionLatencyHealth', 'Latency Health')}</h2>
              <p className={styles.sectionSubtitle}>
                {t('pages:dashboard.sectionLatencyHealthSubtitle', 'Our overhead versus the upstream provider — by the busiest provider in the window.')}
              </p>
            </div>
            <Grid columns={3} gap="md">
              <Card padding="lg" className={styles.metricCard}>
                <div className={styles.metricLabelRow}>{t('pages:dashboard.latencyOurOverheadP95', 'Our Overhead P95')}</div>
                <div className={styles.metricValueRow}>
                  <span className={styles.metricValue}>{fmtMs(top.usOverheadP95Ms)}</span>
                </div>
                <span className={styles.metricSubtitle}>{top.groupLabel}</span>
              </Card>
              <Card padding="lg" className={styles.metricCard}>
                <div className={styles.metricLabelRow}>{t('pages:dashboard.latencyUpstreamTotalP95', 'Upstream P95')}</div>
                <div className={styles.metricValueRow}>
                  <span className={styles.metricValue}>{fmtMs(top.upstreamTotalP95Ms)}</span>
                </div>
                <span className={styles.metricSubtitle}>{top.groupLabel}</span>
              </Card>
              <Card padding="lg" className={clsx(styles.metricCard, styles.clickable)} onClick={() => navigate('/analytics')}>
                <div className={styles.metricLabelRow}>{t('pages:dashboard.latencySlowestProvider', 'Slowest Upstream Provider')}</div>
                <div className={styles.metricValueRow}>
                  <span className={styles.metricValue}>{slowest ? fmtMs(slowest.upstreamTotalP95Ms) : '—'}</span>
                </div>
                <span className={styles.metricSubtitle}>{slowest?.groupLabel ?? '—'}</span>
              </Card>
            </Grid>
          </>
        );
      })()}

      {/* ── Section 2: Business Snapshot ─────────────────────────────────── */}
      <div className={styles.sectionHeader}>
        <h2 className={styles.sectionTitle}>{t('pages:dashboard.sectionBusinessSnapshot')}</h2>
        <p className={styles.sectionSubtitle}>{t('pages:dashboard.sectionBusinessSnapshotSubtitle')}</p>
      </div>

      <Grid columns={4} gap="md">
        <ExpandableWrapper>
          <Card padding="lg" className={styles.metricCard}>
            <div className={styles.metricLabelRow}>
              {t('pages:dashboard.totalCostLabel')}
              <InfoIcon description={t('pages:dashboard.totalCostDesc')} />
            </div>
            <div className={styles.metricValueRow}>
              <span className={styles.metricValue}>${(animCost100x / 100).toFixed(2)}</span>
              <Sparkline data={sparkData.cost} color="var(--color-success)" />
            </div>
            <span className={styles.metricSubtitle}>{t('pages:dashboard.estimated')}</span>
          </Card>
        </ExpandableWrapper>

        <ExpandableWrapper>
          <Card padding="lg" className={styles.metricCard}>
            <div className={styles.metricLabelRow}>
              {t('pages:dashboard.totalTokensLabel')}
            </div>
            <div className={styles.metricValueRow}>
              <span className={styles.metricValue}>{formatTokens(animTokens)}</span>
              <Sparkline data={sparkData.tokens} color="var(--color-primary)" />
            </div>
            <span className={styles.metricSubtitle}>{t('pages:dashboard.promptPlusCompletion')}</span>
          </Card>
        </ExpandableWrapper>

        <ExpandableWrapper>
          <Card padding="lg" className={styles.metricCard}>
            <div className={styles.metricLabelRow}>
              {t('pages:dashboard.activeProvidersLabel')}
              <InfoIcon description={t('pages:dashboard.activeProvidersDesc')} />
            </div>
            <div className={styles.metricValueRow}>
              <span className={styles.metricValue}>
                <AnimatedNumber value={activeProviders} />/<AnimatedNumber value={totalProviders} />
              </span>
              <Sparkline data={activeProvidersSpark} color="var(--color-primary)" />
            </div>
          </Card>
        </ExpandableWrapper>

        <ExpandableWrapper>
          <Card padding="lg" className={styles.metricCard}>
            <div className={styles.metricLabelRow}>
              {t('pages:dashboard.cacheHitRateLabel')}
              <InfoIcon description={t('pages:dashboard.cacheHitRateDesc')} />
            </div>
            <div className={styles.metricValueRow}>
              <span className={styles.metricValue}><AnimatedNumber value={cacheHitRate * 100} precision={1} format={(n) => `${n.toFixed(1)}%`} /></span>
              <Sparkline data={sparkData.cacheHitRate} color="var(--color-success)" />
            </div>
            <span className={styles.metricSubtitle}>{windowLabel}</span>
          </Card>
        </ExpandableWrapper>

        {cacheROI && (() => {
          const combinedSavings = (cacheROI.totalGatewayCacheSavingsUsd ?? 0) + cacheROI.totalCacheNetSavingsUsd;
          if (combinedSavings === 0) return null;
          return (
            <ExpandableWrapper>
              <Card padding="lg" className={styles.metricCard}>
                <div className={styles.metricLabelRow}>
                  {t('pages:dashboard.cacheSavingsLabel')}
                  <InfoIcon description={t('pages:dashboard.cacheSavingsDesc')} />
                </div>
                <div className={styles.metricValueRow}>
                  <span
                    className={clsx(styles.metricValue, combinedSavings >= 0 ? styles.savingsPositive : styles.savingsNegative)}
                  >
                    <AnimatedNumber
                      value={combinedSavings}
                      precision={2}
                      format={(n) => n >= 0 ? `$${n.toFixed(2)}` : `-$${Math.abs(n).toFixed(2)}`}
                    />
                  </span>
                  <Sparkline data={sparkData.cacheSavings} color={combinedSavings >= 0 ? 'var(--color-success)' : 'var(--color-danger)'} />
                </div>
                <span className={styles.metricSubtitle}>{t('pages:dashboard.cacheSavingsSubtitleShort')}</span>
              </Card>
            </ExpandableWrapper>
          );
        })()}
      </Grid>

      {/* Top Providers table */}
      {topProviders.length > 0 && (
        <Card data-testid="dashboard-top-providers" padding="lg">
          <div className={styles.tableHeaderRow}>
            <h2 className={styles.sectionTitle}>{t('pages:dashboard.topProviders')}</h2>
            <Button variant="ghost" size="sm" onClick={() => navigate('/analytics')}>
              {t('pages:dashboard.viewAll')}
            </Button>
          </div>
          <div className={styles.tableWrapper}>
            <table className={styles.table}>
              <thead>
                <tr>
                  <th className={styles.th}>{t('pages:dashboard.colProvider')}</th>
                  <th className={styles.th}>{t('pages:dashboard.colRequests')}</th>
                  <th className={styles.th}>{t('pages:dashboard.colAvgLatency')}</th>
                  <th className={styles.th}>{t('pages:dashboard.colTokens')}</th>
                  <th className={styles.th}>{t('pages:dashboard.colCost')}</th>
                </tr>
              </thead>
              <tbody>
                {topProviders.map((p, i) => (
                  <tr key={p.provider ?? i} className={styles.tableRow}>
                    <td className={styles.tdBold}>{p.providerLabel || p.provider}</td>
                    <td className={styles.td}>{p.requestCount.toLocaleString()}</td>
                    <td className={styles.td}>{Math.round(p.avgLatencyMs)}ms</td>
                    <td className={styles.td}>{formatTokens(p.totalTokens)}</td>
                    <td className={styles.tdMono}>${Number(p.totalEstimatedCostUsd).toFixed(2)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Card>
      )}
    </Stack>
  );
}

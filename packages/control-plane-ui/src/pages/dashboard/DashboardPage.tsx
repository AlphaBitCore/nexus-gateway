import { useState, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { useApi } from '../../hooks/useApi';
import { useCountUp } from '../../hooks/useCountUp';
import { Stack, Grid, PageHeader, ErrorBanner } from '@/components/ui';
import type { AnalyticsSummary, ProviderBreakdown, Provider, SparklineResponse } from '../../api/types';
import { ADMIN_LIST_FULL_PAGE_PARAMS } from '../../constants/admin-api';
import { analyticsApi, providerApi } from '@/api/services';
import type { CacheROISummary } from '@/api/services/overview/analytics';
import { proxyApi } from '../../api/services/infrastructure/misc/proxy';
import styles from './DashboardPage.module.css';
import { WINDOW_MS, type TimeWindow, type TrafficSource, sourceToRollupParam, sourceToLatencyParam } from './dashboardShared';
import { HeroSection } from './HeroSection';
import { HealthCards } from './HealthCards';
import { LatencyHealth } from './LatencyHealth';
import { BusinessSnapshot } from './BusinessSnapshot';
import { ProvidersTable } from './ProvidersTable';

/* ── Main Component ─────────────────────────────────────────────────────── */

export function DashboardPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();

  /* ── Time window state ──────────────────────────────────────────────── */

  const [timeWindow, setTimeWindow] = useState<TimeWindow>('30d');
  const [source, setSource] = useState<TrafficSource>('all');

  const { startTime, endTime } = useMemo(() => {
    const end = new Date();
    const start = new Date(end.getTime() - WINDOW_MS[timeWindow]);
    return { startTime: start.toISOString(), endTime: end.toISOString() };
  }, [timeWindow]);

  // The source selector maps to two different backend vocabularies: the
  // rollup endpoints (summary, by-provider) take vk|proxy|agent; latency-phases
  // takes ai-gateway|compliance-proxy|agent. Sparkline/cacheROI have no source
  // dimension, so trend charts stay cross-source (noted in the UI copy).
  const rollupSourceParam = sourceToRollupParam(source);
  const latencySource = sourceToLatencyParam(source);

  /* ── Data fetching ──────────────────────────────────────────────────── */

  const { data: summary, loading, error, refetch } = useApi<AnalyticsSummary>(
    () => analyticsApi.summary({ startTime, endTime, ...rollupSourceParam }),
    ['admin', 'analytics', 'summary', timeWindow, source],
  );
  const { data: providers } = useApi<{ data: ProviderBreakdown[] }>(
    () => analyticsApi.byProvider({ startTime, endTime, ...rollupSourceParam }),
    ['admin', 'analytics', 'by-provider', timeWindow, source],
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
    () => analyticsApi.latencyPhases({ groupBy: 'provider', start: startTime, end: endTime, source: latencySource }),
    ['admin', 'analytics', 'latency-phases', 'dashboard', timeWindow, source],
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

  // Surface a warning when the proxy is reachable but nothing in the window
  // was covered by compliance hooks — a 0% figure otherwise reads as a metric,
  // not the governance gap it actually is.
  const showComplianceWarning = proxyReachable === true && proxyTotalRequests > 0 && proxyCoveragePercent === 0;

  /* ── Render ─────────────────────────────────────────────────────────── */

  return (
    <Stack gap="lg">
      <HeroSection
        timeWindow={timeWindow}
        setTimeWindow={setTimeWindow}
        source={source}
        setSource={setSource}
        animRequests={animRequests}
        vkRequests={vkRequests}
        proxyTotalRequests={proxyTotalRequests}
        animCost100x={animCost100x}
        animTokens={animTokens}
        proxyReachable={proxyReachable}
        proxyCoveragePercent={proxyCoveragePercent}
        windowLabel={windowLabel}
      />

      {showComplianceWarning && (
        <div className={styles.complianceWarning} role="alert">
          <span className={styles.complianceWarningIcon} aria-hidden="true">⚠</span>
          <span className={styles.complianceWarningText}>
            <span className={styles.complianceWarningTitle}>{t('pages:dashboard.complianceWarningTitle')}</span>
            <span className={styles.complianceWarningBody}>{t('pages:dashboard.complianceWarningBody')}</span>
          </span>
        </div>
      )}

      <HealthCards
        sparkData={sparkData}
        animRequests={animRequests}
        vkRequests={vkRequests}
        proxyTotalRequests={proxyTotalRequests}
        vkRequestsPct={vkRequestsPct}
        proxyRequestsPct={proxyRequestsPct}
        errorRateClass={errorRateClass}
        animErrorRate10x={animErrorRate10x}
        animErrors={animErrors}
        animP95={animP95}
        animAvg={animAvg}
        latencyPhases={latencyPhases}
        proxyReachable={proxyReachable}
        proxyCoveragePercent={proxyCoveragePercent}
        coverageSpark={coverageSpark}
        windowLabel={windowLabel}
      />

      <LatencyHealth latencyPhases={latencyPhases} navigate={navigate} />

      <BusinessSnapshot
        animCost100x={animCost100x}
        animTokens={animTokens}
        sparkData={sparkData}
        activeProviders={activeProviders}
        totalProviders={totalProviders}
        activeProvidersSpark={activeProvidersSpark}
        cacheHitRate={cacheHitRate}
        windowLabel={windowLabel}
        cacheROI={cacheROI}
      />

      <ProvidersTable topProviders={topProviders} navigate={navigate} />
    </Stack>
  );
}

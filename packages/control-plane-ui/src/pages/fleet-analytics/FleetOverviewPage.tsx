import { useState, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { fleetAnalyticsApi, type FleetSummary, type FleetTrendsResponse, type TopDestinationsResponse } from '@/api/services';
import { useApi } from '@/hooks/useApi';
import { PageHeader, Card, Stack, LoadingSpinner, ErrorBanner, Button } from '@/components/ui';
import { Link } from 'react-router-dom';
import { useTheme } from '@/theme/useTheme';
import { getPieColors, getSemanticColor } from '@nexus-gateway/ui-shared';
import {
  PieChart, Pie, Cell, BarChart, Bar, XAxis, YAxis, Tooltip, ResponsiveContainer,
  LineChart, Line, CartesianGrid, Legend,
} from 'recharts';
import styles from './FleetOverviewPage.module.css';

type TimeRange = '24h' | '7d' | '30d';

function computeRange(range: TimeRange): { startTime: string; endTime: string } {
  const now = new Date();
  const ms = range === '24h' ? 24 * 3600_000 : range === '7d' ? 7 * 24 * 3600_000 : 30 * 24 * 3600_000;
  return {
    startTime: new Date(now.getTime() - ms).toISOString(),
    endTime: now.toISOString(),
  };
}

export function FleetOverviewPage() {
  const { t } = useTranslation();
  const { resolvedMode } = useTheme();
  const [range, setRange] = useState<TimeRange>('24h');

  const summaryQuery = useApi<FleetSummary>(
    () => fleetAnalyticsApi.summary(),
    ['admin', 'fleet-analytics', 'summary'],
  );

  const trendParams = useMemo(() => {
    const r = computeRange(range);
    return { metric: 'device_fleet_status', startTime: r.startTime, endTime: r.endTime };
  }, [range]);

  const trendsQuery = useApi<FleetTrendsResponse>(
    () => fleetAnalyticsApi.trends(trendParams),
    ['admin', 'fleet-analytics', 'trends', trendParams.metric, trendParams.startTime, trendParams.endTime],
  );

  const topDestQuery = useApi<TopDestinationsResponse>(
    () => fleetAnalyticsApi.topDestinations({ limit: '10', windowHours: '24' }),
    ['admin', 'fleet-analytics', 'top-destinations'],
  );

  if (summaryQuery.loading && !summaryQuery.data) return <LoadingSpinner />;
  if (summaryQuery.error) return <ErrorBanner message={summaryQuery.error.message} onRetry={summaryQuery.refetch} />;
  if (!summaryQuery.data) return null;

  const summary = summaryQuery.data;
  const pieColors = getPieColors(resolvedMode);

  const statusPieData = [
    { name: t('pages:fleetOverview.kpiActive'), value: summary.active },
    { name: t('pages:fleetOverview.kpiStale'), value: summary.stale },
    { name: t('pages:fleetOverview.kpiCritical'), value: summary.critical },
    { name: t('pages:fleetOverview.kpiRevoked'), value: summary.revoked },
  ].filter((d) => d.value > 0);

  // Reshape trend buckets: pivot dimension `status` into separate series
  const trendChartData = (() => {
    if (!trendsQuery.data) return [];
    const grouped = new Map<string, Record<string, number | string>>();
    for (const b of trendsQuery.data.buckets) {
      const ts = new Date(b.bucketStart).toLocaleString();
      const status = b.dimensions.status ?? 'unknown';
      const existing = grouped.get(ts) ?? { time: ts };
      existing[status] = b.value;
      grouped.set(ts, existing);
    }
    return Array.from(grouped.values());
  })();

  return (
    <Stack gap="lg">
      <PageHeader title={t('pages:fleetOverview.title')} subtitle={t('pages:fleetOverview.subtitle')} />

      <Card padding="md">
        <Stack direction="horizontal" gap="md" align="center">
          <p style={{ margin: 'var(--g-space-0)', flex: 1, color: 'var(--color-text-secondary)' }}>
            {t('pages:fleetOverview.movedBanner')}
          </p>
          <Link to="/infrastructure/nodes">
            <Button variant="primary" size="sm">{t('pages:fleetOverview.movedBannerLink')}</Button>
          </Link>
        </Stack>
      </Card>

      {/* KPI cards */}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, 1fr)', gap: 'var(--g-space-4)' }}>
        <KpiCard label={t('pages:fleetOverview.kpiTotal')} value={summary.total} />
        <KpiCard label={t('pages:fleetOverview.kpiActive')} value={summary.active} />
        <KpiCard label={t('pages:fleetOverview.kpiStale')} value={summary.stale} subtitle={`${summary.stalePct}%`} />
        <KpiCard label={t('pages:fleetOverview.kpiCritical')} value={summary.critical} subtitle={`${summary.criticalPct}%`} />
      </div>

      {/* Charts row */}
      <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 'var(--g-space-4)' }}>
        <Card padding="lg">
          <h3>{t('pages:fleetOverview.statusBreakdown')}</h3>
          <ResponsiveContainer width="100%" height={240}>
            <PieChart>
              <Pie data={statusPieData} dataKey="value" nameKey="name" outerRadius={80} label>
                {statusPieData.map((_, i) => (
                  <Cell key={i} fill={pieColors[i % pieColors.length]} />
                ))}
              </Pie>
              <Tooltip />
            </PieChart>
          </ResponsiveContainer>
        </Card>

        <Card padding="lg">
          <h3>{t('pages:fleetOverview.fleetHealth')}</h3>
          <ResponsiveContainer width="100%" height={240}>
            <BarChart data={[
              { name: t('pages:fleetOverview.kpiActive'), count: summary.active },
              { name: t('pages:fleetOverview.kpiStale'), count: summary.stale },
              { name: t('pages:fleetOverview.kpiCritical'), count: summary.critical },
              { name: t('pages:fleetOverview.kpiRevoked'), count: summary.revoked },
            ]}>
              <XAxis dataKey="name" />
              <YAxis allowDecimals={false} />
              <Tooltip />
              <Bar dataKey="count" fill={getSemanticColor(resolvedMode, 'requests')} />
            </BarChart>
          </ResponsiveContainer>
        </Card>
      </div>

      {/* Trend chart */}
      <Card padding="lg">
        <Stack direction="horizontal" gap="md" align="center">
          <h3 style={{ flex: 1, margin: 'var(--g-space-0)' }}>{t('pages:fleetOverview.statusTrend')}</h3>
          <select value={range} onChange={(e) => setRange(e.target.value as TimeRange)} className={styles.filterSelect}>
            <option value="24h">{t('pages:fleetOverview.range24h')}</option>
            <option value="7d">{t('pages:fleetOverview.range7d')}</option>
            <option value="30d">{t('pages:fleetOverview.range30d')}</option>
          </select>
        </Stack>
        {trendsQuery.loading && !trendsQuery.data ? (
          <LoadingSpinner />
        ) : trendChartData.length === 0 ? (
          <p className={styles.emptyText}>
            {t('pages:fleetOverview.noTrendData')}
          </p>
        ) : (
          <ResponsiveContainer width="100%" height={300}>
            <LineChart data={trendChartData}>
              <CartesianGrid strokeDasharray="3 3" />
              <XAxis dataKey="time" />
              <YAxis allowDecimals={false} />
              <Tooltip />
              <Legend />
              {['ACTIVE', 'ENROLLED', 'OFFLINE', 'REVOKED'].map((status, i) => (
                <Line key={status} dataKey={status} stroke={pieColors[i % pieColors.length]} dot={false} />
              ))}
            </LineChart>
          </ResponsiveContainer>
        )}
      </Card>

      {/* Top destinations */}
      <Card padding="lg">
        <h3>{t('pages:fleetOverview.topDestinations')}</h3>
        {topDestQuery.loading && !topDestQuery.data ? (
          <LoadingSpinner />
        ) : (
          <table style={{ width: '100%', borderCollapse: 'collapse' }}>
            <thead>
              <tr>
                <th style={{ textAlign: 'left', padding: 'var(--g-space-2)' }}>{t('pages:fleetOverview.destHost')}</th>
                <th style={{ textAlign: 'right', padding: 'var(--g-space-2)' }}>{t('pages:fleetOverview.eventCount')}</th>
                <th style={{ textAlign: 'right', padding: 'var(--g-space-2)' }}>{t('pages:fleetOverview.deviceCount')}</th>
              </tr>
            </thead>
            <tbody>
              {(topDestQuery.data?.data ?? []).map((row) => (
                <tr key={row.destHost}>
                  <td style={{ padding: 'var(--g-space-2)', fontFamily: 'monospace' }}>{row.destHost}</td>
                  <td style={{ padding: 'var(--g-space-2)', textAlign: 'right' }}>{row.eventCount}</td>
                  <td style={{ padding: 'var(--g-space-2)', textAlign: 'right' }}>{row.deviceCount}</td>
                </tr>
              ))}
              {(topDestQuery.data?.data ?? []).length === 0 && (
                <tr>
                  <td colSpan={3} style={{ padding: 'var(--g-space-2)', textAlign: 'center', color: 'var(--color-text-secondary)' }}>
                    {t('pages:fleetOverview.noDestinations')}
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        )}
      </Card>
    </Stack>
  );
}

function KpiCard({ label, value, subtitle }: { label: string; value: number; subtitle?: string }) {
  return (
    <Card padding="md">
      <div style={{ fontSize: 'var(--g-font-size-base)', color: 'var(--color-text-secondary)' }}>{label}</div>
      <div style={{ fontSize: 'var(--g-font-size-3xl)', fontWeight: 'var(--g-font-weight-bold)', marginTop: 'var(--g-space-1)' }}>{value}</div>
      {subtitle && <div style={{ fontSize: 'var(--g-font-size-xs)', color: 'var(--color-text-secondary)' }}>{subtitle}</div>}
    </Card>
  );
}

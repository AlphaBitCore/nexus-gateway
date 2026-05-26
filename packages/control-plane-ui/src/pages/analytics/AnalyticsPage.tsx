import { useState, useMemo, useCallback, useRef, useEffect, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { analyticsApi } from '@/api/services';
import { useApi } from '../../hooks/useApi';
import {
  PageHeader, LoadingSpinner, ErrorBanner, Card, Stack,
  Tabs, TabsList, TabsTrigger, TabsContent,
  DataTable, Button, AnimatedNumber,
  type DataTableColumn,
} from '@/components/ui';
import { MetricsRollupsSection } from '../metrics/MetricsRollupsSection';
import { LatencyPhasesPanel } from './LatencyPhasesPanel';
import { useTheme } from '../../theme/useTheme';
import { getPieColors, getSemanticColor, getTooltipStyle } from '@nexus-gateway/ui-shared';
import type { AnalyticsSummary, CostData, UsageData } from '../../api/types';
import type { CacheROISummary } from '@/api/services/overview/analytics';
import { BarChart, Bar, XAxis, YAxis, Tooltip, ResponsiveContainer, PieChart, Pie, Cell } from 'recharts';
import styles from './AnalyticsPage.module.css';
import { formatTokens } from '@/lib/format';
import { topNWithOther, PIE_SLICE_CAP } from '@/lib/chartData';

type TimeRange = '24h' | '7d' | '30d' | 'custom';
type GroupBy = 'provider' | 'model' | 'project' | 'organization' | 'user' | 'virtual_key' | 'host' | 'device';

function computeTimeRange(range: TimeRange, customStart: string, customEnd: string): { startTime: string; endTime: string } {
  const now = new Date();
  if (range === 'custom') {
    return {
      startTime: customStart || new Date(now.getTime() - 24 * 3600_000).toISOString(),
      endTime: customEnd || now.toISOString(),
    };
  }
  const hours = range === '24h' ? 24 : range === '7d' ? 168 : 720;
  return {
    startTime: new Date(now.getTime() - hours * 3600_000).toISOString(),
    endTime: now.toISOString(),
  };
}

/* ── Stat card ───────────────────────────────────────────────────────────── */

function StatCard({ label, value, subtitle }: { label: string; value: ReactNode; subtitle?: string }) {
  return (
    <Card className={styles.statCard}>
      <div className={styles.statLabel}>{label}</div>
      <div className={styles.statValue}>{value}</div>
      {subtitle && <div className={styles.statSubtitle}>{subtitle}</div>}
    </Card>
  );
}

/* ── CSV export helper ───────────────────────────────────────────────────── */

function downloadCsv(rows: BreakdownRow[], columns: DataTableColumn<BreakdownRow>[], filename: string) {
  const header = columns.map((c) => c.label).join(',');
  const body = rows.map((r) =>
    columns.map((c) => {
      const val = r[c.key as keyof BreakdownRow];
      const str = String(val ?? '');
      return str.includes(',') || str.includes('"') ? `"${str.replace(/"/g, '""')}"` : str;
    }).join(','),
  ).join('\n');
  const blob = new Blob([header + '\n' + body], { type: 'text/csv' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  a.click();
  URL.revokeObjectURL(url);
}

/* ── Breakdown table ─────────────────────────────────────────────────────── */

interface BreakdownRow {
  name: string;
  extra: string;
  requestCount: number;
  totalTokens: number;
  totalCostUsd: number;
  cacheHitRate?: number;
  cacheSavingsUsd?: number;
}

function makeBaseColumns(t: (key: string) => string): DataTableColumn<BreakdownRow>[] {
  return [
    { key: 'requestCount', label: t('pages:traffic.colRequests'), sortable: true, render: (r) => (r.requestCount ?? 0).toLocaleString() },
    { key: 'totalTokens', label: t('pages:traffic.colTokens'), sortable: true, render: (r) => formatTokens(r.totalTokens ?? 0) },
    { key: 'totalCostUsd', label: t('pages:traffic.colCostUsd'), sortable: true, render: (r) => `$${(r.totalCostUsd ?? 0).toFixed(4)}` },
    {
      key: 'cacheHitRate', label: t('pages:analytics.colCacheHitRate'), sortable: true,
      render: (r) => r.cacheHitRate != null ? `${(r.cacheHitRate * 100).toFixed(1)}%` : '—',
    },
    {
      key: 'cacheSavingsUsd', label: t('pages:analytics.colCacheSavings'), sortable: true,
      render: (r) => r.cacheSavingsUsd != null && r.cacheSavingsUsd > 0
        ? <span className={styles.savingsValue}>${r.cacheSavingsUsd.toFixed(4)}</span>
        : '—',
    },
  ];
}

function makeColumns(nameLabel: string, t: (key: string) => string, extraLabel?: string): DataTableColumn<BreakdownRow>[] {
  const cols: DataTableColumn<BreakdownRow>[] = [
    { key: 'name', label: nameLabel, sortable: true },
  ];
  if (extraLabel) {
    cols.push({ key: 'extra', label: extraLabel, sortable: true });
  }
  return [...cols, ...makeBaseColumns(t)];
}

function mergeRows(costData?: CostData[], usageData?: UsageData[]): BreakdownRow[] {
  const map = new Map<string, BreakdownRow>();
  for (const c of costData ?? []) {
    const hits = c.cacheHitCount ?? 0;
    const reqs = c.requestCount ?? 0;
    const gatewaySavings = c.gatewayCacheSavingsUsd ?? 0;
    const l4Savings = c.cacheNetSavingsUsd ?? 0;
    const totalSavings = gatewaySavings + l4Savings;
    map.set(c.group, {
      name: c.groupLabel || c.group,
      extra: c.groupExtra ?? '',
      requestCount: reqs,
      totalTokens: c.totalTokens ?? 0,
      totalCostUsd: c.totalCostUsd ?? 0,
      cacheHitRate: reqs > 0 && hits > 0 ? hits / reqs : undefined,
      cacheSavingsUsd: totalSavings > 0 ? totalSavings : undefined,
    });
  }
  for (const u of usageData ?? []) {
    const existing = map.get(u.group);
    if (existing) {
      existing.totalTokens = u.totalTokens ?? 0;
      existing.requestCount = u.requestCount ?? 0;
      if (!existing.extra && u.groupExtra) existing.extra = u.groupExtra;
    } else {
      map.set(u.group, {
        name: u.groupLabel || u.group,
        extra: u.groupExtra ?? '',
        requestCount: u.requestCount ?? 0,
        totalTokens: u.totalTokens ?? 0,
        totalCostUsd: 0,
      });
    }
  }
  return Array.from(map.values());
}

function BreakdownTable({ title, nameLabel, extraLabel, groupByKey, timeParams, searchPlaceholder }: {
  title: string;
  nameLabel: string;
  extraLabel?: string;
  groupByKey: string;
  timeParams: Record<string, string>;
  searchPlaceholder?: string;
}) {
  const { t } = useTranslation();
  const [search, setSearch] = useState('');
  const [debouncedSearch, setDebouncedSearch] = useState('');
  const debounceRef = useRef<ReturnType<typeof setTimeout>>(undefined);

  useEffect(() => {
    clearTimeout(debounceRef.current);
    debounceRef.current = setTimeout(() => setDebouncedSearch(search), 300);
    return () => clearTimeout(debounceRef.current);
  }, [search]);

  const queryParams = useMemo(() => ({
    groupBy: groupByKey,
    ...timeParams,
    ...(debouncedSearch && { q: debouncedSearch }),
    limit: '50',
  }), [groupByKey, timeParams, debouncedSearch]);

  const { data: costData } = useApi<{ data: CostData[] }>(
    () => analyticsApi.cost(queryParams),
    ['admin', 'analytics', 'cost', groupByKey, timeParams.startTime, timeParams.endTime, debouncedSearch],
  );
  const { data: usageData } = useApi<{ data: UsageData[] }>(
    () => analyticsApi.usage(queryParams),
    ['admin', 'analytics', 'usage', groupByKey, timeParams.startTime, timeParams.endTime, debouncedSearch],
  );

  const rows = useMemo(() => mergeRows(costData?.data, usageData?.data), [costData, usageData]);
  const columns = useMemo(() => makeColumns(nameLabel, t, extraLabel), [nameLabel, t, extraLabel]);

  const handleExport = useCallback(async () => {
    // Fetch all data (no limit) for export
    const [allCost, allUsage] = await Promise.all([
      analyticsApi.cost({ groupBy: groupByKey, ...timeParams }) as Promise<{ data: CostData[] }>,
      analyticsApi.usage({ groupBy: groupByKey, ...timeParams }) as Promise<{ data: UsageData[] }>,
    ]);
    const allRows = mergeRows(allCost.data, allUsage.data);
    downloadCsv(allRows, columns, `${title.replace(/\s+/g, '-').toLowerCase()}.csv`);
  }, [groupByKey, timeParams, columns, title]);

  return (
    <Card className={styles.tableCard}>
      <div className={styles.tableHeader}>
        <h3 className={styles.tableTitle}>{title}</h3>
        <div className={styles.tableActions}>
          <input
            type="text"
            placeholder={searchPlaceholder ?? t('pages:traffic.searchPlaceholder', { name: nameLabel.toLowerCase() })}
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            className={styles.searchInput}
          />
          <Button variant="secondary" size="sm" onClick={handleExport}>
            {t('pages:traffic.exportCsv')}
          </Button>
        </div>
      </div>
      <DataTable columns={columns} data={rows} pageSize={10} hideSearch emptyMessage={t('pages:traffic.noBreakdownData', { title: title.toLowerCase() })} />
    </Card>
  );
}

/* ── Main page ───────────────────────────────────────────────────────────── */

export function AnalyticsPage() {
  const { t } = useTranslation();
  const { resolvedMode } = useTheme();

  const TIME_RANGE_OPTIONS = useMemo(() => [
    { value: '24h' as TimeRange, label: t('pages:traffic.timeRangeLast24h') },
    { value: '7d' as TimeRange, label: t('pages:traffic.timeRangeLast7d') },
    { value: '30d' as TimeRange, label: t('pages:traffic.timeRangeLast30d') },
    { value: 'custom' as TimeRange, label: t('pages:traffic.timeRangeCustom') },
  ], [t]);

  const pieColors = getPieColors(resolvedMode);
  const tooltipStyle = getTooltipStyle(resolvedMode);
  const [activeTab, setActiveTab] = useState('analytics');
  const [timeRange, setTimeRange] = useState<TimeRange>('7d');
  const [groupBy, setGroupBy] = useState<GroupBy>('provider');
  const [customStart, setCustomStart] = useState('');
  const [customEnd, setCustomEnd] = useState('');
  const [source, setSource] = useState<'' | 'vk' | 'proxy' | 'agent'>('');

  const GROUP_BY_OPTIONS = useMemo(() => {
    const all: { value: GroupBy; label: string }[] = [
      { value: 'provider', label: t('pages:analytics.groupBy_provider') },
      { value: 'model', label: t('pages:analytics.groupBy_model') },
      { value: 'user', label: t('pages:analytics.groupBy_user') },
      { value: 'organization', label: t('pages:analytics.groupBy_organization') },
      { value: 'virtual_key', label: t('pages:analytics.groupBy_virtual_key') },
      { value: 'host', label: t('pages:analytics.groupBy_host') },
      { value: 'device', label: t('pages:analytics.groupBy_device') },
      { value: 'project', label: t('pages:analytics.groupBy_project') },
    ];
    switch (source) {
      case 'vk': return all.filter(o => ['provider', 'model', 'user', 'virtual_key', 'organization', 'project'].includes(o.value));
      case 'proxy': return all.filter(o => ['host', 'user', 'organization'].includes(o.value));
      case 'agent': return all.filter(o => ['device', 'host', 'user'].includes(o.value));
      default: return all;
    }
  }, [t, source]);

  const { startTime, endTime } = useMemo(
    () => computeTimeRange(timeRange, customStart, customEnd),
    [timeRange, customStart, customEnd],
  );

  const apiParams = useMemo(() => {
    const p: Record<string, string> = { startTime, endTime };
    if (source) p.source = source;
    return p;
  }, [startTime, endTime, source]);

  /* Reset groupBy when source changes and current option is unavailable */
  const groupByValues = useMemo(() => GROUP_BY_OPTIONS.map(o => o.value), [GROUP_BY_OPTIONS]);
  useEffect(() => {
    if (!groupByValues.includes(groupBy)) {
      setGroupBy(groupByValues[0] ?? 'provider');
    }
  }, [source, groupByValues, groupBy]);

  const showVKMetrics = source === '' || source === 'vk';

  const { data: summary, loading, error, refetch } = useApi<AnalyticsSummary>(
    () => analyticsApi.summary(apiParams),
    ['admin', 'analytics', 'summary', startTime, endTime, source],
  );
  const { data: costData } = useApi<{ data: CostData[] }>(
    () => analyticsApi.cost({ groupBy, ...apiParams }),
    ['admin', 'analytics', 'cost', groupBy, startTime, endTime, source],
  );
  const { data: usageData } = useApi<{ data: UsageData[] }>(
    () => analyticsApi.usage({ groupBy, ...apiParams }),
    ['admin', 'analytics', 'usage', groupBy, startTime, endTime, source],
  );
  const { data: cacheROI } = useApi<CacheROISummary>(
    () => analyticsApi.cacheROI({ start: startTime, end: endTime }),
    ['admin', 'analytics', 'cache-roi', startTime, endTime],
  );

  if (loading) return <LoadingSpinner />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!summary) return null;

  return (
    <Stack gap="lg">
      <PageHeader title={t('pages:traffic.analytics')} />

      {/* ── Filter bar (page-level, affects both tabs) ───────────── */}
      <Card className={styles.filterCard}>
        <label className={styles.filterGroup}>
          <span className={styles.filterLabel}>{t('pages:traffic.labelTimeRange')}</span>
          <select value={timeRange} onChange={(e) => setTimeRange(e.target.value as TimeRange)} className={styles.filterSelect}>
            {TIME_RANGE_OPTIONS.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
          </select>
        </label>
        {timeRange === 'custom' && (
          <div className={styles.filterGroup}>
            <input
              type="datetime-local"
              value={customStart}
              onChange={(e) => setCustomStart(e.target.value ? new Date(e.target.value).toISOString() : '')}
              className={styles.filterInput}
            />
            <span className={styles.filterLabel}>to</span>
            <input
              type="datetime-local"
              value={customEnd}
              onChange={(e) => setCustomEnd(e.target.value ? new Date(e.target.value).toISOString() : '')}
              className={styles.filterInput}
            />
          </div>
        )}
        <label className={styles.filterGroup}>
          <span className={styles.filterLabel}>{t('pages:traffic.labelGroupBy')}</span>
          <select value={groupBy} onChange={(e) => setGroupBy(e.target.value as GroupBy)} className={styles.filterSelect}>
            {GROUP_BY_OPTIONS.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
          </select>
        </label>
        <div className={styles.filterGroup}>
          <span className={styles.filterLabel}>{t('pages:analytics.sourceFilter')}</span>
          <div className={styles.buttonGroup}>
            {([
              { value: '' as const, label: t('pages:analytics.sourceAll') },
              { value: 'vk' as const, label: t('pages:analytics.sourceVK') },
              { value: 'proxy' as const, label: t('pages:analytics.sourceProxy') },
              { value: 'agent' as const, label: t('pages:analytics.sourceAgent') },
            ]).map(opt => (
              <button
                key={opt.value}
                type="button"
                className={`${styles.filterBtn} ${source === opt.value ? styles.filterBtnActive : ''}`}
                onClick={() => setSource(opt.value)}
              >
                {opt.label}
              </button>
            ))}
          </div>
        </div>
      </Card>

      <Tabs value={activeTab} onValueChange={setActiveTab}>
        <TabsList>
          <TabsTrigger value="analytics">{t('pages:traffic.analytics')}</TabsTrigger>
          <TabsTrigger value="latency">{t('pages:analytics.latencyTab', 'Latency')}</TabsTrigger>
          <TabsTrigger value="metrics">{t('pages:traffic.metrics')}</TabsTrigger>
        </TabsList>

        <TabsContent value="metrics">
          <MetricsRollupsSection embedded source={source} externalStart={startTime} externalEnd={endTime} />
        </TabsContent>

        <TabsContent value="latency">
          <LatencyPhasesPanel start={startTime} end={endTime} source={source} />
        </TabsContent>

        <TabsContent value="analytics">
          <Stack gap="lg">

          {/* Chips */}
          {(timeRange !== '7d' || groupBy !== 'provider') && (
            <div className={styles.chipBar}>
              {timeRange !== '7d' && (
                <span className={styles.chip}>
                  {timeRange === 'custom' ? t('pages:traffic.customRange') : TIME_RANGE_OPTIONS.find((o) => o.value === timeRange)?.label}
                  <button onClick={() => setTimeRange('7d')} className={styles.chipClose} aria-label={t('pages:traffic.resetTimeRange')}>&times;</button>
                </span>
              )}
              {groupBy !== 'provider' && (
                <span className={styles.chip}>
                  {t('pages:traffic.byGroupBy', { groupBy })}
                  <button onClick={() => setGroupBy('provider')} className={styles.chipClose} aria-label={t('pages:traffic.resetGroupBy')}>&times;</button>
                </span>
              )}
            </div>
          )}

          {/* ── KPI stats ─────────────────────────────────────────── */}
          <div className={styles.statsGrid}>
            <StatCard
              label={t('pages:traffic.statTotalRequests')}
              value={<AnimatedNumber value={summary.totalRequests} />}
              subtitle={t('pages:traffic.subtitleInSelectedPeriod')}
            />
            {showVKMetrics && (
              <StatCard
                label={t('pages:traffic.statTotalCost')}
                value={<AnimatedNumber value={Number(summary.totalEstimatedCostUsd)} precision={2} format={(n) => `$${n.toFixed(2)}`} />}
                subtitle={t('pages:traffic.subtitleEstimated')}
              />
            )}
            {showVKMetrics && (
              <StatCard
                label={t('pages:traffic.statTotalTokens')}
                value={<AnimatedNumber value={summary.totalTokens} format={formatTokens} />}
                subtitle={t('pages:traffic.subtitlePromptCompletion')}
              />
            )}
            <StatCard
              label={t('pages:traffic.statAvgLatency')}
              value={<AnimatedNumber value={summary.avgLatencyMs} format={(n) => `${Math.round(n)}ms`} />}
              subtitle={t('pages:traffic.subtitleAvgResponseTime')}
            />
            {showVKMetrics && summary.cacheHitRate > 0 && (
              <StatCard
                label={t('pages:traffic.statCacheHitRate')}
                value={<AnimatedNumber value={summary.cacheHitRate * 100} precision={1} format={(n) => `${n.toFixed(1)}%`} />}
                subtitle={t('pages:traffic.subtitleCacheHitRate')}
              />
            )}
            {showVKMetrics && cacheROI && (() => {
              const combinedSavings = (cacheROI.totalGatewayCacheSavingsUsd ?? 0) + cacheROI.totalCacheNetSavingsUsd;
              return combinedSavings !== 0 ? (
                <StatCard
                  label={t('pages:analytics.statCacheNetSavings')}
                  value={<AnimatedNumber value={combinedSavings} precision={2} format={(n) => `$${n.toFixed(2)}`} />}
                  subtitle={t('pages:analytics.subtitleCacheNetSavings')}
                />
              ) : null;
            })()}
          </div>

          {/* ── Distribution charts (VK only) ──────────────────────── */}
          {showVKMetrics && (
            <>
              <div className={styles.sectionHeader}>
                <h2 className={styles.sectionTitle}>{t('pages:traffic.sectionCostUsage')}</h2>
                <p className={styles.sectionSubtitle}>{t('pages:traffic.sectionCostUsageSubtitle', { groupBy })}</p>
              </div>

              <div className={styles.chartsGrid}>
                <Card className={styles.chartCard}>
                  <h3 className={styles.chartTitle}>{t('pages:traffic.chartCostBy', { axis: groupBy })}</h3>
                  {costData?.data && costData.data.length > 0 ? (() => {
                    // Pre-compute percentages from the totalCostUsd column so
                    // every slice carries its own pre-rounded share. We round
                    // each slice down to 1 decimal then assign the residual to
                    // the largest slice — guarantees the rendered numbers sum
                    // to exactly 100.0 even in the face of rounding.
                    const enrichedAll = costData.data.map((d) => ({
                      ...d,
                      displayGroup: d.groupLabel || d.group,
                      cost: d.totalCostUsd ?? 0,
                    }));
                    // Cap the long-tail at top-N + "Other" so pies stay
                    // readable when groupBy = model / user / project blows
                    // out to 20+ categories.
                    const enriched = topNWithOther(
                      enrichedAll,
                      PIE_SLICE_CAP,
                      (r) => r.cost,
                      (totalCost, droppedCount) => ({
                        ...enrichedAll[0],
                        group: '__other__',
                        displayGroup: `${t('common:other')} (${droppedCount})`,
                        groupLabel: t('common:other'),
                        cost: totalCost,
                        totalCostUsd: totalCost,
                      }),
                    );
                    const total = enriched.reduce((s, d) => s + d.cost, 0);
                    type PieRow = (typeof enriched)[number] & { percent: number };
                    const withPct: PieRow[] = total > 0
                      ? enriched.map((d) => ({ ...d, percent: Math.floor((d.cost / total) * 1000) / 10 }))
                      : enriched.map((d) => ({ ...d, percent: 0 }));
                    if (total > 0 && withPct.length > 0) {
                      const drift = +(100 - withPct.reduce((s, d) => s + d.percent, 0)).toFixed(1);
                      if (drift !== 0) {
                        let largestIdx = 0;
                        for (let i = 1; i < withPct.length; i++) {
                          if (withPct[i].cost > withPct[largestIdx].cost) largestIdx = i;
                        }
                        withPct[largestIdx].percent = +(withPct[largestIdx].percent + drift).toFixed(1);
                      }
                    }
                    return (
                      <ResponsiveContainer width="100%" height={300}>
                        <PieChart>
                          <Pie
                            data={withPct}
                            dataKey="cost"
                            nameKey="displayGroup"
                            cx="50%" cy="50%" outerRadius={90}
                            label={(props) => {
                              const p = props as { displayGroup?: string; percent?: number };
                              const pct = typeof p.percent === 'number' ? p.percent.toFixed(1) : '0.0';
                              return `${p.displayGroup ?? ''} ${pct}%`;
                            }}
                          >
                            {withPct.map((_, i) => <Cell key={i} fill={pieColors[i % pieColors.length]} />)}
                          </Pie>
                          <Tooltip
                            contentStyle={tooltipStyle}
                            cursor={{ fill: 'transparent' }}
                            formatter={(value, _name, item) => {
                              const num = typeof value === 'number' ? value : Number(value ?? 0);
                              const pct = (item?.payload as { percent?: number } | undefined)?.percent;
                              return [`$${num.toFixed(4)} (${(pct ?? 0).toFixed(1)}%)`, _name as string];
                            }}
                          />
                        </PieChart>
                      </ResponsiveContainer>
                    );
                  })() : (
                    <div className={styles.emptyChart}>{t('pages:traffic.noDataForPeriod')}</div>
                  )}
                </Card>

                <Card className={styles.chartCard}>
                  <h3 className={styles.chartTitle}>{t('pages:traffic.chartTokenUsageBy', { axis: groupBy })}</h3>
                  {usageData?.data && usageData.data.length > 0 ? (
                    <ResponsiveContainer width="100%" height={300}>
                      <BarChart data={usageData.data.map((d) => ({ ...d, displayGroup: d.groupLabel || d.group }))}>
                        <XAxis dataKey="displayGroup" tick={{ fontSize: 12 }} />
                        <YAxis tick={{ fontSize: 12 }} tickFormatter={(v) => formatTokens(Number(v))} />
                        <Tooltip
                          contentStyle={tooltipStyle}
                          cursor={{ fill: 'transparent' }}
                          formatter={(value, name) => [formatTokens(Number(value)), name as string]}
                        />
                        <Bar dataKey="totalPromptTokens" name={t('pages:traffic.chartPrompt')} fill={getSemanticColor(resolvedMode, 'prompt')} stackId="tokens" />
                        <Bar dataKey="totalCompletionTokens" name={t('pages:traffic.chartCompletion')} fill={getSemanticColor(resolvedMode, 'completion')} stackId="tokens" />
                      </BarChart>
                    </ResponsiveContainer>
                  ) : (
                    <div className={styles.emptyChart}>{t('pages:traffic.noDataForPeriod')}</div>
                  )}
                </Card>
              </div>
            </>
          )}

          {/* ── Breakdown table (controlled by page-level GroupBy) ──── */}
          <BreakdownTable
            title={t('pages:analytics.breakdownBy') + ' ' + t(`pages:analytics.groupBy_${groupBy}`)}
            nameLabel={t(`pages:analytics.groupBy_${groupBy}`)}
            groupByKey={groupBy}
            timeParams={apiParams}
          />

          </Stack>
        </TabsContent>
      </Tabs>
    </Stack>
  );
}

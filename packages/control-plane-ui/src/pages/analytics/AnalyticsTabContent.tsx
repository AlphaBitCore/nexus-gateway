// Tab bodies for the Analytics page: the main analytics tab (KPIs, charts,
// breakdown table), the latency tab, and the metrics tab. Each tab owns its
// own filter state via the shared AnalyticsFilterBar; AnalyticsPage.tsx
// composes them under the tab strip.

import { useMemo, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { analyticsApi } from '@/api/services';
import { useApi } from '../../hooks/useApi';
import { LoadingSpinner, ErrorBanner, Stack } from '@/components/ui';
import { MetricsRollupsSection } from '../metrics/MetricsRollupsSection';
import { LatencyPhasesPanel } from './LatencyPhasesPanel';
import { AnalyticsKpiSection } from './AnalyticsKpiSection';
import { AnalyticsChartsSection } from './AnalyticsChartsSection';
import { BreakdownTable } from './AnalyticsBreakdownSection';
import type { useTheme } from '../../theme/useTheme';
import type { getPieColors, getTooltipStyle } from '@nexus-gateway/ui-shared';
import type { AnalyticsSummary, CostData, UsageData } from '../../api/types';
import type { CacheROISummary } from '@/api/services/overview/analytics';
import {
  AnalyticsFilterBar, buildGroupByOptions, computeTimeRange,
  type GroupBy, type SourceFilter, type TabFilters, type TimeRange,
} from './AnalyticsFilterBar';
import styles from './AnalyticsPage.module.css';

export function AnalyticsTabContent({
  filters,
  onFiltersChange,
  groupBy,
  onGroupByChange,
  timeRangeOptions,
  sourceOptions,
  pieColors,
  tooltipStyle,
  resolvedMode,
}: {
  filters: TabFilters;
  onFiltersChange: (next: TabFilters) => void;
  groupBy: GroupBy;
  onGroupByChange: (next: GroupBy) => void;
  timeRangeOptions: { value: TimeRange; label: string }[];
  sourceOptions: { value: SourceFilter; label: string }[];
  pieColors: ReturnType<typeof getPieColors>;
  tooltipStyle: ReturnType<typeof getTooltipStyle>;
  resolvedMode: ReturnType<typeof useTheme>['resolvedMode'];
}) {
  const { t } = useTranslation();
  const groupByOptions = useMemo(() => buildGroupByOptions(t, filters.source), [t, filters.source]);
  const groupByValues = useMemo(() => groupByOptions.map(o => o.value), [groupByOptions]);

  useEffect(() => {
    if (!groupByValues.includes(groupBy)) {
      onGroupByChange(groupByValues[0] ?? 'provider');
    }
  }, [groupByValues, groupBy, onGroupByChange]);

  const { startTime, endTime } = useMemo(
    () => computeTimeRange(filters.timeRange, filters.customStart, filters.customEnd),
    [filters.timeRange, filters.customStart, filters.customEnd],
  );
  const apiParams = useMemo(() => {
    const p: Record<string, string> = { startTime, endTime };
    if (filters.source) p.source = filters.source;
    return p;
  }, [startTime, endTime, filters.source]);
  const showVKMetrics = filters.source === '' || filters.source === 'vk';

  const { data: summary, loading, error, refetch } = useApi<AnalyticsSummary>(
    () => analyticsApi.summary(apiParams),
    ['admin', 'analytics', 'summary', startTime, endTime, filters.source],
  );
  const { data: costData } = useApi<{ data: CostData[] }>(
    () => analyticsApi.cost({ groupBy, ...apiParams }),
    ['admin', 'analytics', 'cost', groupBy, startTime, endTime, filters.source],
  );
  const { data: usageData } = useApi<{ data: UsageData[] }>(
    () => analyticsApi.usage({ groupBy, ...apiParams }),
    ['admin', 'analytics', 'usage', groupBy, startTime, endTime, filters.source],
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
      <AnalyticsFilterBar
        filters={filters}
        onFiltersChange={onFiltersChange}
        timeRangeOptions={timeRangeOptions}
        sourceOptions={sourceOptions}
        groupBy={groupBy}
        onGroupByChange={onGroupByChange}
        groupByOptions={groupByOptions}
      />

      {(filters.timeRange !== '7d' || groupBy !== 'provider') && (
        <div className={styles.chipBar}>
          {filters.timeRange !== '7d' && (
            <span className={styles.chip}>
              {filters.timeRange === 'custom' ? t('pages:traffic.customRange') : timeRangeOptions.find((o) => o.value === filters.timeRange)?.label}
              <button onClick={() => onFiltersChange({ ...filters, timeRange: '7d' })} className={styles.chipClose} aria-label={t('pages:traffic.resetTimeRange')}>&times;</button>
            </span>
          )}
          {groupBy !== 'provider' && (
            <span className={styles.chip}>
              {t('pages:traffic.byGroupBy', { groupBy })}
              <button onClick={() => onGroupByChange('provider')} className={styles.chipClose} aria-label={t('pages:traffic.resetGroupBy')}>&times;</button>
            </span>
          )}
        </div>
      )}

      <AnalyticsKpiSection summary={summary} showVKMetrics={showVKMetrics} cacheROI={cacheROI} />

      {showVKMetrics && (
        <AnalyticsChartsSection
          groupBy={groupBy}
          costData={costData}
          usageData={usageData}
          pieColors={pieColors}
          tooltipStyle={tooltipStyle}
          resolvedMode={resolvedMode}
        />
      )}

      <BreakdownTable
        title={t('pages:analytics.breakdownBy') + ' ' + t(`pages:analytics.groupBy_${groupBy}`)}
        nameLabel={t(`pages:analytics.groupBy_${groupBy}`)}
        groupByKey={groupBy}
        timeParams={apiParams}
      />
    </Stack>
  );
}

export function LatencyTabContent({
  filters,
  onFiltersChange,
  timeRangeOptions,
  sourceOptions,
}: {
  filters: TabFilters;
  onFiltersChange: (next: TabFilters) => void;
  timeRangeOptions: { value: TimeRange; label: string }[];
  sourceOptions: { value: SourceFilter; label: string }[];
}) {
  const { startTime, endTime } = useMemo(
    () => computeTimeRange(filters.timeRange, filters.customStart, filters.customEnd),
    [filters.timeRange, filters.customStart, filters.customEnd],
  );

  return (
    <Stack gap="lg">
      <AnalyticsFilterBar
        filters={filters}
        onFiltersChange={onFiltersChange}
        timeRangeOptions={timeRangeOptions}
        sourceOptions={sourceOptions}
      />
      <LatencyPhasesPanel start={startTime} end={endTime} source={filters.source} />
    </Stack>
  );
}

export function MetricsTabContent({
  filters,
  onFiltersChange,
  timeRangeOptions,
  sourceOptions,
}: {
  filters: TabFilters;
  onFiltersChange: (next: TabFilters) => void;
  timeRangeOptions: { value: TimeRange; label: string }[];
  sourceOptions: { value: SourceFilter; label: string }[];
}) {
  const { t } = useTranslation();
  const { startTime, endTime } = useMemo(
    () => computeTimeRange(filters.timeRange, filters.customStart, filters.customEnd),
    [filters.timeRange, filters.customStart, filters.customEnd],
  );

  return (
    <Stack gap="lg">
      <AnalyticsFilterBar
        filters={filters}
        onFiltersChange={onFiltersChange}
        timeRangeOptions={timeRangeOptions}
        sourceOptions={sourceOptions}
        trailingText={t('pages:metrics.rollupsDescription')}
      />
      <MetricsRollupsSection embedded source={filters.source} externalStart={startTime} externalEnd={endTime} />
    </Stack>
  );
}

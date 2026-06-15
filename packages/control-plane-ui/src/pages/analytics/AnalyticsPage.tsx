import { useState, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import {
  PageHeader, Stack,
  Tabs, TabsList, TabsTrigger, TabsContent,
} from '@/components/ui';
import { useTheme } from '../../theme/useTheme';
import { getPieColors, getTooltipStyle } from '@nexus-gateway/ui-shared';
import {
  DEFAULT_TAB_FILTERS,
  type GroupBy, type SourceFilter, type TabFilters, type TimeRange,
} from './AnalyticsFilterBar';
import { AnalyticsTabContent, LatencyTabContent, MetricsTabContent } from './AnalyticsTabContent';
import styles from './AnalyticsPage.module.css';
import trafficTabStyles from '../traffic/analytics/TrafficAnalyticsPage.module.css';

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
  const [analyticsFilters, setAnalyticsFilters] = useState<TabFilters>(DEFAULT_TAB_FILTERS);
  const [latencyFilters, setLatencyFilters] = useState<TabFilters>(DEFAULT_TAB_FILTERS);
  const [metricsFilters, setMetricsFilters] = useState<TabFilters>(DEFAULT_TAB_FILTERS);
  const [analyticsGroupBy, setAnalyticsGroupBy] = useState<GroupBy>('provider');
  const SOURCE_OPTIONS = useMemo(() => [
    { value: '' as SourceFilter, label: t('pages:analytics.sourceAll') },
    { value: 'vk' as SourceFilter, label: t('pages:analytics.sourceVK') },
    { value: 'proxy' as SourceFilter, label: t('pages:analytics.sourceProxy') },
    { value: 'agent' as SourceFilter, label: t('pages:analytics.sourceAgent') },
  ], [t]);

  return (
    <Stack gap="lg">
      <div className={styles.analyticsHeader}>
        <PageHeader title={t('pages:traffic.analytics')} subtitle={t('pages:analytics.subtitle')} />
      </div>

      <Tabs value={activeTab} onValueChange={setActiveTab}>
        <TabsList className={trafficTabStyles.sourceTabsList}>
          <TabsTrigger value="analytics" className={trafficTabStyles.sourceTabsTrigger}>{t('pages:traffic.analytics')}</TabsTrigger>
          <TabsTrigger value="latency" className={trafficTabStyles.sourceTabsTrigger}>{t('pages:analytics.latencyTab', 'Latency')}</TabsTrigger>
          <TabsTrigger value="metrics" className={trafficTabStyles.sourceTabsTrigger}>{t('pages:traffic.metrics')}</TabsTrigger>
        </TabsList>

        <TabsContent value="metrics">
          <MetricsTabContent
            filters={metricsFilters}
            onFiltersChange={setMetricsFilters}
            timeRangeOptions={TIME_RANGE_OPTIONS}
            sourceOptions={SOURCE_OPTIONS}
          />
        </TabsContent>

        <TabsContent value="latency">
          <LatencyTabContent
            filters={latencyFilters}
            onFiltersChange={setLatencyFilters}
            timeRangeOptions={TIME_RANGE_OPTIONS}
            sourceOptions={SOURCE_OPTIONS}
          />
        </TabsContent>

        <TabsContent value="analytics">
          <AnalyticsTabContent
            filters={analyticsFilters}
            onFiltersChange={setAnalyticsFilters}
            groupBy={analyticsGroupBy}
            onGroupByChange={setAnalyticsGroupBy}
            timeRangeOptions={TIME_RANGE_OPTIONS}
            sourceOptions={SOURCE_OPTIONS}
            pieColors={pieColors}
            tooltipStyle={tooltipStyle}
            resolvedMode={resolvedMode}
          />
        </TabsContent>
      </Tabs>
    </Stack>
  );
}

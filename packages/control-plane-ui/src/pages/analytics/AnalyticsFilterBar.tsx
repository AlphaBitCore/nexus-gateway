// Shared filter model for the Analytics page tabs (time range, source,
// group-by) plus the filter bar component every tab renders. Lives beside
// AnalyticsPage.tsx and reuses its CSS module so the rendered markup is
// identical wherever the bar appears.

import { useTranslation } from 'react-i18next';
import styles from './AnalyticsPage.module.css';

export type TimeRange = '24h' | '7d' | '30d' | 'custom';
export type GroupBy = 'provider' | 'model' | 'project' | 'organization' | 'user' | 'virtual_key' | 'host' | 'device';
export type SourceFilter = '' | 'vk' | 'proxy' | 'agent';

export interface TabFilters {
  timeRange: TimeRange;
  customStart: string;
  customEnd: string;
  source: SourceFilter;
}

export const DEFAULT_TAB_FILTERS: TabFilters = {
  timeRange: '7d',
  customStart: '',
  customEnd: '',
  source: '',
};

export function computeTimeRange(range: TimeRange, customStart: string, customEnd: string): { startTime: string; endTime: string } {
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

export function buildGroupByOptions(t: (key: string) => string, source: SourceFilter) {
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
}

export function AnalyticsFilterBar({
  filters,
  onFiltersChange,
  timeRangeOptions,
  sourceOptions,
  groupBy,
  onGroupByChange,
  groupByOptions,
  trailingText,
}: {
  filters: TabFilters;
  onFiltersChange: (next: TabFilters) => void;
  timeRangeOptions: { value: TimeRange; label: string }[];
  sourceOptions: { value: SourceFilter; label: string }[];
  groupBy?: GroupBy;
  onGroupByChange?: (next: GroupBy) => void;
  groupByOptions?: { value: GroupBy; label: string }[];
  trailingText?: string;
}) {
  const { t } = useTranslation();
  const patchFilters = (patch: Partial<TabFilters>) => onFiltersChange({ ...filters, ...patch });

  return (
    <div className={styles.filterCard}>
      <label className={styles.filterGroup}>
        <select
          aria-label={t('pages:analytics.labelTimeRange')}
          value={filters.timeRange}
          onChange={(e) => patchFilters({ timeRange: e.target.value as TimeRange })}
          className={styles.filterSelect}
        >
          {timeRangeOptions.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
        </select>
      </label>
      {filters.timeRange === 'custom' && (
        <div className={styles.filterGroup}>
          <input
            type="datetime-local"
            aria-label={t('pages:analytics.customRangeStart')}
            value={filters.customStart}
            onChange={(e) => patchFilters({ customStart: e.target.value ? new Date(e.target.value).toISOString() : '' })}
            className={styles.filterInput}
          />
          <span className={styles.rangeSeparator}>to</span>
          <input
            type="datetime-local"
            aria-label={t('pages:analytics.customRangeEnd')}
            value={filters.customEnd}
            onChange={(e) => patchFilters({ customEnd: e.target.value ? new Date(e.target.value).toISOString() : '' })}
            className={styles.filterInput}
          />
        </div>
      )}
      {groupBy && onGroupByChange && groupByOptions ? (
        <label className={styles.filterGroup}>
          <select
            aria-label={t('pages:analytics.labelGroupBy')}
            value={groupBy}
            onChange={(e) => onGroupByChange(e.target.value as GroupBy)}
            className={styles.filterSelect}
          >
            {groupByOptions.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
          </select>
        </label>
      ) : null}
      <label className={styles.filterGroup}>
        <select
          aria-label={t('pages:analytics.sourceFilter')}
          value={filters.source}
          onChange={(e) => patchFilters({ source: e.target.value as SourceFilter })}
          className={styles.filterSelect}
        >
          {sourceOptions.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
        </select>
      </label>
      {trailingText ? <p className={styles.filterMeta}>{trailingText}</p> : null}
    </div>
  );
}

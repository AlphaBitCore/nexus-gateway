import { useState, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { quotaAnalyticsApi } from '@/api/services/ai-gateway/quotaAnalytics';
import type { QuotaUsageRow, QuotaTopConsumer } from '@/api/services/ai-gateway/quotaAnalytics';
import {
  PageHeader, DataTable, Badge,
  Skeleton, ErrorBanner, Stack, Card,
} from '@/components/ui';
import type { DataTableColumn } from '@/components/ui';
import styles from './QuotaUsageDashboard.module.css';

/* -- Alert level badge ----------------------------------------------------- */

function alertVariant(level: string): 'success' | 'warning' | 'danger' | 'default' {
  switch (level) {
    case 'critical': return 'danger';
    case 'warning': return 'warning';
    case 'normal': return 'success';
    default: return 'default';
  }
}

/* -- Component ------------------------------------------------------------- */

export function QuotaUsageDashboard() {
  const { t } = useTranslation();
  const [period, setPeriod] = useState('monthly');
  const [scope, setScope] = useState('user');

  const { data: overviewData, loading: overviewLoading, error: overviewError } = useApi<{ data: QuotaUsageRow[] }>(
    () => quotaAnalyticsApi.overview({ period, scope }),
    ['admin', 'quota-analytics', 'overview', period, scope],
  );

  const { data: topData, loading: topLoading, error: topError } = useApi<{ data: QuotaTopConsumer[] }>(
    () => quotaAnalyticsApi.top({ period, scope, limit: '10' }),
    ['admin', 'quota-analytics', 'top', period, scope],
  );

  const onPeriodChange = useCallback((e: React.ChangeEvent<HTMLSelectElement>) => {
    setPeriod(e.target.value);
  }, []);

  const onScopeChange = useCallback((e: React.ChangeEvent<HTMLSelectElement>) => {
    setScope(e.target.value);
  }, []);

  const overviewRows = overviewData?.data ?? [];
  const topRows = topData?.data ?? [];

  const overviewColumns: DataTableColumn<QuotaUsageRow>[] = [
    { key: 'entityName', label: t('pages:quotaUsage.entityName') },
    { key: 'entityType', label: t('pages:quotaUsage.entityType') },
    {
      key: 'costLimitUsd',
      label: t('pages:quotaUsage.costLimit'),
      render: (r) => `$${Number(r.costLimitUsd ?? 0).toFixed(2)}`,
    },
    {
      key: 'currentCostUsd',
      label: t('pages:quotaUsage.currentCost'),
      render: (r) => `$${Number(r.currentCostUsd ?? 0).toFixed(2)}`,
    },
    {
      key: 'usagePercent',
      label: t('pages:quotaUsage.usage'),
      render: (r) => (
        <div className={styles.progressContainer}>
          <div
            className={styles.progressBar}
            style={{ width: `${Math.min(r.usagePercent ?? 0, 100)}%` }}
            data-level={r.alertLevel}
          />
          <span className={styles.progressLabel}>{(r.usagePercent ?? 0).toFixed(1)}%</span>
        </div>
      ),
    },
    {
      key: 'alertLevel',
      label: t('pages:quotaUsage.alertLevel'),
      render: (r) => <Badge variant={alertVariant(r.alertLevel)}>{r.alertLevel}</Badge>,
    },
  ];

  const topColumns: DataTableColumn<QuotaTopConsumer>[] = [
    { key: 'entityName', label: t('pages:quotaUsage.entityName') },
    { key: 'entityType', label: t('pages:quotaUsage.entityType') },
    {
      key: 'totalCostUsd',
      label: t('pages:quotaUsage.totalCost'),
      render: (r) => `$${Number(r.totalCostUsd ?? 0).toFixed(2)}`,
    },
  ];

  const isLoading = overviewLoading || topLoading;
  const firstError = overviewError || topError;

  if (isLoading) return <Skeleton.ListPageSkeleton />;
  if (firstError) return <ErrorBanner message={firstError.message} />;

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('pages:quotaUsage.title')}
        subtitle={t('pages:quotaUsage.subtitle')}
      />

      {/* Filters */}
      <Stack direction="horizontal" gap="sm">
        <select
          aria-label={t('pages:quotaUsage.periodLabel')}
          value={period}
          onChange={onPeriodChange}
          className={styles.filterSelect}
        >
          <option value="monthly">{t('pages:quotaUsage.monthly')}</option>
          <option value="weekly">{t('pages:quotaUsage.weekly')}</option>
        </select>
        <select
          aria-label={t('pages:quotaUsage.scopeLabel')}
          value={scope}
          onChange={onScopeChange}
          className={styles.filterSelect}
        >
          <option value="user">{t('pages:quotaUsage.scopeUser')}</option>
          <option value="project">{t('pages:quotaUsage.scopeProject')}</option>
          <option value="vk">{t('pages:quotaUsage.scopeVk')}</option>
        </select>
      </Stack>

      {/* Overview table */}
      <Card padding="none">
        <DataTable
          hideSearch
          frameless
          columns={overviewColumns}
          data={overviewRows}
          emptyMessage={t('pages:quotaUsage.noData')}
        />
      </Card>

      {/* Top consumers */}
      <Card>
        <h3 className={styles.sectionTitle}>{t('pages:quotaUsage.topConsumers')}</h3>
        <DataTable
          hideSearch
          frameless
          columns={topColumns}
          data={topRows}
          emptyMessage={t('pages:quotaUsage.noData')}
        />
      </Card>
    </Stack>
  );
}

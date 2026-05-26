import { useState, useEffect, useRef, useCallback } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useDebouncedValue } from '@/hooks/useDebouncedValue';
import { useMutation } from '@/hooks/useMutation';
import { hubApi } from '@/api/services/infrastructure/nodes/hub';
import type { ScheduledJob } from '@/api/services/infrastructure/nodes/hub';
import {
  PageHeader, Stack, Button, Badge, statusToVariant, Tooltip,
  DataTable, LoadingSpinner, ErrorBanner, ListFilterToolbar,
  ListPagination, DEFAULT_ADMIN_LIST_PAGE_SIZE,
} from '@/components/ui';
import type { AdminListPageSize, DataTableColumn } from '@/components/ui';
import styles from './InfraJobsPage.module.css';

function jobStatusVariant(status: string | undefined | null) {
  switch ((status ?? '').toLowerCase()) {
    case 'ok':
    case 'success':
      return 'success';
    case 'running':
      return 'warning';
    case 'failed':
    case 'error':
      return 'danger';
    case 'interrupted':
      return 'default';
    default:
      return statusToVariant(status ?? '');
  }
}

// Go time.Duration marshals as nanoseconds (int64). Render as a short human string.
function formatNsDuration(ns: number | null | undefined): string {
  if (ns == null || !Number.isFinite(ns) || ns <= 0) return '\u2014';
  const ms = ns / 1e6;
  if (ms < 1) return `${Math.max(1, Math.round(ns / 1e3))}\u00b5s`;
  if (ms < 1000) return `${ms < 10 ? ms.toFixed(1) : Math.round(ms)}ms`;
  const totalSec = Math.round(ms / 1000);
  if (totalSec < 60) return `${totalSec}s`;
  const totalMin = Math.floor(totalSec / 60);
  const secRem = totalSec % 60;
  if (totalMin < 60) return secRem ? `${totalMin}m${secRem}s` : `${totalMin}m`;
  const hours = Math.floor(totalMin / 60);
  const minRem = totalMin % 60;
  return minRem ? `${hours}h${minRem}m` : `${hours}h`;
}

export default function InfraJobsPage() {
  const { t } = useTranslation('pages');
  const navigate = useNavigate();
  const [busyJob, setBusyJob] = useState<string | null>(null);
  const [offset, setOffset] = useState(0);
  const [pageLimit, setPageLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);
  const [searchInput, setSearchInput] = useState('');
  const debouncedSearch = useDebouncedValue(searchInput, 300);
  const [enabledFilter, setEnabledFilter] = useState('');

  const resetPage = useCallback(() => setOffset(0), []);

  const {
    data,
    loading,
    error,
    refetch,
  } = useApi(
    () => hubApi.listJobs({
      limit: pageLimit,
      offset,
      search: debouncedSearch || undefined,
      enabled: enabledFilter || undefined,
    }),
    ['admin', 'jobs', 'list', offset, pageLimit, debouncedSearch, enabledFilter],
  );

  const trigger = useMutation(
    (id: string) => hubApi.triggerJob(id),
    {
      successMessage: t('infrastructure.jobTriggered', 'Job triggered successfully'),
      invalidateQueries: [['admin', 'jobs', 'list']],
      onSuccess: () => setBusyJob(null),
    },
  );

  const toggle = useMutation(
    ({ id, enabled }: { id: string; enabled: boolean }) => hubApi.updateJob(id, { enabled }),
    {
      successMessage: t('infrastructure.jobUpdated', 'Job updated'),
      invalidateQueries: [['admin', 'jobs', 'list']],
      onSuccess: () => setBusyJob(null),
    },
  );

  // Auto-refresh every 30s
  const refetchRef = useRef(refetch);
  refetchRef.current = refetch;
  useEffect(() => {
    const interval = setInterval(() => { refetchRef.current(); }, 30_000);
    return () => clearInterval(interval);
  }, []);

  const columns: DataTableColumn<ScheduledJob>[] = [
    {
      key: 'name',
      label: t('infrastructure.jobName', 'Job Name'),
      sortable: true,
      render: (row) => (
        <div className={styles.nameCell}>
          <div className={styles.nameText}>{row.name}</div>
          {row.description && (
            <Tooltip content={row.description} side="bottom">
              <div className={styles.descText}>{row.description}</div>
            </Tooltip>
          )}
        </div>
      ),
    },
    {
      key: 'interval',
      label: t('infrastructure.interval', 'Interval'),
      render: (row) => formatNsDuration(row.interval),
      sortable: true,
    },
    {
      key: 'lastRun',
      label: t('infrastructure.lastRun', 'Last Run'),
      render: (row) => row.lastRun ? new Date(row.lastRun).toLocaleString() : '\u2014',
      sortable: true,
    },
    {
      key: 'nextRun',
      label: t('infrastructure.nextRun', 'Next Run'),
      render: (row) => row.nextRun ? new Date(row.nextRun).toLocaleString() : '\u2014',
      sortable: true,
    },
    {
      key: 'lastStatus',
      label: t('infrastructure.status', 'Status'),
      render: (row) => (
        <Badge variant={jobStatusVariant(row.lastStatus)}>{row.lastStatus ?? '\u2014'}</Badge>
      ),
      sortable: true,
    },
    {
      key: 'lastDuration',
      label: t('infrastructure.lastDuration', 'Last Duration'),
      render: (row) => formatNsDuration(row.lastDuration),
      sortable: true,
    },
    {
      key: 'runCount',
      label: t('infrastructure.runCount', 'Runs'),
      render: (row) => row.runCount.toLocaleString(),
      sortable: true,
    },
    {
      key: 'errorCount',
      label: t('infrastructure.errorCount', 'Errors'),
      render: (row) => (
        <span className={row.errorCount > 0 ? styles.errorCount : undefined}>
          {row.errorCount.toLocaleString()}
        </span>
      ),
      sortable: true,
    },
    {
      key: 'enabled',
      label: t('infrastructure.enabled', 'Enabled'),
      sortable: true,
      render: (row) => (
        <Badge variant={row.enabled ? 'success' : 'default'}>
          {row.enabled ? t('infrastructure.enabled', 'Enabled') : t('infrastructure.disabled', 'Disabled')}
        </Badge>
      ),
    },
    {
      key: '_actions',
      label: t('infrastructure.actions', 'Actions'),
      sortable: false,
      render: (row) => (
        <Stack direction="horizontal" gap="xs">
          <Button
            variant="secondary"
            size="sm"
            loading={busyJob === `trigger:${row.id}`}
            onClick={(e) => {
              e.stopPropagation();
              setBusyJob(`trigger:${row.id}`);
              trigger.mutate(row.id).catch(() => setBusyJob(null));
            }}
          >
            {t('infrastructure.triggerJob')}
          </Button>
          <Button
            variant="ghost"
            size="sm"
            loading={busyJob === `toggle:${row.id}`}
            onClick={(e) => {
              e.stopPropagation();
              setBusyJob(`toggle:${row.id}`);
              toggle.mutate({ id: row.id, enabled: !row.enabled }).catch(() => setBusyJob(null));
            }}
          >
            {row.enabled ? t('infrastructure.disable', 'Disable') : t('infrastructure.enable', 'Enable')}
          </Button>
        </Stack>
      ),
    },
  ];

  if (loading && !data) return <LoadingSpinner />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('infrastructure.jobsTitle')}
        subtitle={t('infrastructure.jobsDescription')}
        action={<span className={styles.refreshNote}>{t('infrastructure.autoRefresh', 'Auto-refreshes every 30s')}</span>}
      />

      <ListFilterToolbar
        searchPlaceholder={t('infrastructure.searchJobsPlaceholder', 'Search by name or description…')}
        searchValue={searchInput}
        onSearchChange={(v) => { setSearchInput(v); resetPage(); }}
        meta={
          (data?.total ?? 0) === 0
            ? undefined
            : t('infrastructure.showingJobs', 'Showing {{count}} job(s) on this page · {{total}} total matching', { count: (data?.jobs ?? []).length, total: data?.total ?? 0 })
        }
      >
        <select
          aria-label={t('infrastructure.enabled', 'Enabled')}
          value={enabledFilter}
          onChange={(e) => { setEnabledFilter(e.target.value); resetPage(); }}
          className={styles.filterSelect}
        >
          <option value="">{t('infrastructure.filterAll', 'All')}</option>
          <option value="true">{t('infrastructure.enabled', 'Enabled')}</option>
          <option value="false">{t('infrastructure.disabled', 'Disabled')}</option>
        </select>
      </ListFilterToolbar>

      <DataTable<ScheduledJob>
        columns={columns}
        data={data?.jobs ?? []}
        hideSearch
        emptyMessage={t('infrastructure.noJobs')}
        loading={loading}
        onRowClick={(row) => navigate(`/infrastructure/jobs/${row.id}`)}
      />
      <ListPagination
        offset={offset}
        limit={pageLimit}
        total={data?.total ?? 0}
        onOffsetChange={(v) => setOffset(v)}
        onLimitChange={(v) => { setPageLimit(v); setOffset(0); }}
      />
    </Stack>
  );
}

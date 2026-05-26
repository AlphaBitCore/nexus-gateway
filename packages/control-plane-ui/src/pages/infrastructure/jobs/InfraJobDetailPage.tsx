import { useState } from 'react';
import { useParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { hubApi } from '@/api/services/infrastructure/nodes/hub';
import type { ScheduledJob, JobRun } from '@/api/services/infrastructure/nodes/hub';
import {
  Stack, Card, Badge, Button, Skeleton, ErrorBanner, Breadcrumb, statusToVariant,
  DataTable, ListPagination, DEFAULT_ADMIN_LIST_PAGE_SIZE,
} from '@/components/ui';
import type { AdminListPageSize, DataTableColumn } from '@/components/ui';
import styles from './InfraJobDetailPage.module.css';

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
    case 'skipped':
      return 'default';
    default:
      return statusToVariant(status ?? '');
  }
}

function formatNsDuration(ns: number | null | undefined): string {
  if (ns == null || !Number.isFinite(ns) || ns <= 0) return '—';
  const ms = ns / 1e6;
  if (ms < 1) return `${Math.max(1, Math.round(ns / 1e3))}µs`;
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

function formatMs(ms: number | null | undefined): string {
  if (ms == null || !Number.isFinite(ms) || ms < 0) return '—';
  if (ms < 1000) return `${ms}ms`;
  const totalSec = Math.round(ms / 1000);
  if (totalSec < 60) return `${totalSec}s`;
  const totalMin = Math.floor(totalSec / 60);
  const secRem = totalSec % 60;
  return secRem ? `${totalMin}m${secRem}s` : `${totalMin}m`;
}

function InfoRow({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className={styles.infoRow}>
      <dt className={styles.infoLabel}>{label}</dt>
      <dd className={styles.infoValue}>{value ?? '—'}</dd>
    </div>
  );
}

export default function InfraJobDetailPage() {
  const { t } = useTranslation('pages');
  const { id } = useParams<{ id: string }>();
  const [busy, setBusy] = useState<string | null>(null);
  const [offset, setOffset] = useState(0);
  const [pageLimit, setPageLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);

  const {
    data: job,
    loading: jobLoading,
    error: jobError,
    refetch: refetchJob,
  } = useApi<ScheduledJob>(
    () => hubApi.getJob(id ?? ''),
    ['admin', 'jobs', 'detail', id],
    { skip: !id },
  );

  const {
    data: runsData,
    loading: runsLoading,
    error: runsError,
    refetch: refetchRuns,
  } = useApi<{ runs: JobRun[]; total: number; limit: number; offset: number }>(
    () => hubApi.listJobRuns(id ?? '', { limit: pageLimit, offset }),
    ['admin', 'jobs', 'runs', id, offset, pageLimit],
    { skip: !id },
  );

  const trigger = useMutation(
    (jobId: string) => hubApi.triggerJob(jobId),
    {
      successMessage: t('infrastructure.jobTriggered', 'Job triggered successfully'),
      invalidateQueries: [['admin', 'jobs', 'runs', id], ['admin', 'jobs', 'detail', id]],
      onSuccess: () => setBusy(null),
    },
  );

  const toggle = useMutation(
    ({ jobId, enabled }: { jobId: string; enabled: boolean }) => hubApi.updateJob(jobId, { enabled }),
    {
      successMessage: t('infrastructure.jobUpdated', 'Job updated'),
      invalidateQueries: [['admin', 'jobs', 'detail', id]],
      onSuccess: () => setBusy(null),
    },
  );

  const runColumns: DataTableColumn<JobRun>[] = [
    {
      key: 'startedAt',
      label: t('infrastructure.startedAt', 'Started At'),
      render: (run) => new Date(run.startedAt).toLocaleString(),
    },
    {
      key: 'durationMs',
      label: t('infrastructure.durationMs', 'Duration'),
      render: (run) => formatMs(run.durationMs),
    },
    {
      key: 'status',
      label: t('infrastructure.status', 'Status'),
      render: (run) => <Badge variant={jobStatusVariant(run.status)}>{run.status}</Badge>,
    },
    {
      key: 'replicaId',
      label: t('infrastructure.replicaId', 'Replica'),
      render: (run) => (run.replicaId ? <code className={styles.mono}>{run.replicaId}</code> : '—'),
    },
    {
      key: 'error',
      label: t('infrastructure.error', 'Error'),
      render: (run) => (run.error ? <code className={styles.errorText}>{run.error}</code> : '—'),
    },
  ];

  if (jobLoading && !job) return <Skeleton.DetailPageSkeleton />;
  if (jobError) return <ErrorBanner message={jobError.message} onRetry={refetchJob} />;
  if (!job) return null;

  return (
    <Stack gap="lg">
      <Breadcrumb items={[
        { label: t('infrastructure.jobsTitle'), to: '/infrastructure/jobs' },
        { label: job.name },
      ]} />

      <div className={styles.headerRow}>
        <h1 className={styles.headerName}>{job.name}</h1>
        <Badge variant={jobStatusVariant(job.lastStatus)}>{job.lastStatus ?? '—'}</Badge>
        <Badge variant={job.enabled ? 'success' : 'default'}>
          {job.enabled ? t('infrastructure.enabled', 'Enabled') : t('infrastructure.disabled', 'Disabled')}
        </Badge>
        <div className={styles.headerActions}>
          <Button
            variant="secondary"
            size="sm"
            loading={busy === 'trigger'}
            onClick={() => {
              setBusy('trigger');
              trigger.mutate(job.id).catch(() => setBusy(null));
            }}
          >
            {t('infrastructure.triggerJob')}
          </Button>
          <Button
            variant="ghost"
            size="sm"
            loading={busy === 'toggle'}
            onClick={() => {
              setBusy('toggle');
              toggle.mutate({ jobId: job.id, enabled: !job.enabled }).catch(() => setBusy(null));
            }}
          >
            {job.enabled ? t('infrastructure.disable', 'Disable') : t('infrastructure.enable', 'Enable')}
          </Button>
        </div>
      </div>

      <Card>
        <h2 className={styles.sectionTitle}>{t('infrastructure.jobInfo', 'Job Information')}</h2>
        <dl className={styles.infoGrid}>
          <InfoRow label={t('infrastructure.jobId', 'Job ID')} value={<code className={styles.mono}>{job.id}</code>} />
          <InfoRow label={t('infrastructure.jobName', 'Job Name')} value={job.name} />
          <InfoRow label={t('infrastructure.description', 'Description')} value={job.description} />
          <InfoRow label={t('infrastructure.interval', 'Interval')} value={formatNsDuration(job.interval)} />
          <InfoRow label={t('infrastructure.status', 'Status')} value={<Badge variant={jobStatusVariant(job.lastStatus)}>{job.lastStatus ?? '—'}</Badge>} />
          <InfoRow label={t('infrastructure.lastRun', 'Last Run')} value={job.lastRun ? new Date(job.lastRun).toLocaleString() : null} />
          <InfoRow label={t('infrastructure.nextRun', 'Next Run')} value={job.nextRun ? new Date(job.nextRun).toLocaleString() : null} />
          <InfoRow label={t('infrastructure.lastDuration', 'Last Duration')} value={formatNsDuration(job.lastDuration)} />
          <InfoRow label={t('infrastructure.runCount', 'Runs')} value={job.runCount.toLocaleString()} />
          <InfoRow label={t('infrastructure.errorCount', 'Errors')} value={
            <span className={job.errorCount > 0 ? styles.errorCount : undefined}>
              {job.errorCount.toLocaleString()}
            </span>
          } />
          {job.lastError && (
            <InfoRow label={t('infrastructure.lastError', 'Last Error')} value={
              <code className={styles.errorText}>{job.lastError}</code>
            } />
          )}
        </dl>
      </Card>

      <Card>
        <div className={styles.runsHeader}>
          <h2 className={styles.sectionTitle}>{t('infrastructure.runHistory', 'Run History')}</h2>
          <Button variant="ghost" size="sm" onClick={() => refetchRuns()}>
            {t('infrastructure.refresh', 'Refresh')}
          </Button>
        </div>

        {runsError && <ErrorBanner message={runsError.message} onRetry={refetchRuns} />}

        <DataTable<JobRun>
          columns={runColumns}
          data={runsData?.runs ?? []}
          hideSearch
          serverPaginated
          loading={runsLoading}
          emptyMessage={t('infrastructure.noJobRuns', 'No runs recorded yet')}
        />
        <ListPagination
          offset={offset}
          limit={pageLimit}
          total={runsData?.total ?? 0}
          onOffsetChange={(v) => setOffset(v)}
          onLimitChange={(v) => { setPageLimit(v); setOffset(0); }}
        />
      </Card>
    </Stack>
  );
}

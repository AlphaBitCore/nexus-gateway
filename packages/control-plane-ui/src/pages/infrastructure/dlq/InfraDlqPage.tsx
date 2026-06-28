/**
 * InfraDlqPage — Dead-letter queue admin view.
 *
 * Reads traffic_event_dlq rows from /api/admin/observability/dlq, lets
 * operators inspect why each row landed there (lastError + delivery count),
 * and exposes a single per-row retry button that POSTs back to
 * /api/admin/observability/dlq/:id/retry. Retry republishes the original
 * payload to its MQ subject + deletes the DLQ row on success.
 *
 * Pagination: offset-based via the shared ListPagination (rows-per-page,
 * page numbers, row range + total, First/Prev/Next/Last) — identical to
 * every other admin list page (jobs, nodes, audit). The Hub list endpoint
 * returns {rows,total} for the current subject filter; DataTable renders
 * the page slice with serverPaginated so it neither re-slices nor draws
 * its own footer.
 *
 * IAM: page-load requires admin:observability-dlq.read; retry button is
 * hidden when the user lacks admin:observability-dlq.manage (the
 * shellRouteConfig allowedActions also gates the route mount).
 */
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { useDebouncedValue } from '@/hooks/useDebouncedValue';
import { dlqApi } from '@/api/services/infrastructure/dlq/dlq';
import type { DlqRow } from '@/api/services/infrastructure/dlq/dlq';
import { usePermission } from '@/hooks/usePermission';
import {
  PageHeader, Stack, Button, Badge, DataTable, ListFilterToolbar,
  LoadingSpinner, ErrorBanner,
  ListPagination, DEFAULT_ADMIN_LIST_PAGE_SIZE,
} from '@/components/ui';
import type { AdminListPageSize, DataTableColumn } from '@/components/ui';
import styles from './InfraDlqPage.module.css';

function fmtTime(iso: string): string {
  try { return new Date(iso).toLocaleString(); } catch { return iso; }
}

function fmtSize(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

export default function InfraDlqPage() {
  const { t } = useTranslation('pages');
  const canRetry = usePermission('observability-dlq:manage');

  const [subject, setSubject] = useState('');
  const debouncedSubject = useDebouncedValue(subject, 300);
  const [offset, setOffset] = useState(0);
  const [pageLimit, setPageLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);
  const [retryingId, setRetryingId] = useState<string | null>(null);

  const { data, loading, error, refetch } = useApi(
    () => dlqApi.list({
      subject: debouncedSubject.trim() || undefined,
      limit: pageLimit,
      offset,
    }),
    ['admin', 'dlq', 'list', offset, pageLimit, debouncedSubject],
  );

  // Retry republishes raw bytes to MQ + deletes the row; a success toast
  // plus list invalidation matches the rest of the admin mutation surface.
  const retry = useMutation(
    (id: string) => dlqApi.retry(id),
    {
      successMessage: t('infrastructure.dlq.retried'),
      invalidateQueries: [['admin', 'dlq', 'list']],
      onSuccess: () => setRetryingId(null),
    },
  );

  const onRetry = (row: DlqRow) => {
    if (!confirm(t('infrastructure.dlq.retryConfirm', { msgId: row.msgId }))) return;
    setRetryingId(row.id);
    retry.mutate(row.id).catch(() => setRetryingId(null));
  };

  const columns: DataTableColumn<DlqRow>[] = [
    { key: 'dlqInsertedAt', label: t('infrastructure.dlq.colInsertedAt'), render: (r) => fmtTime(r.dlqInsertedAt) },
    { key: 'subject', label: t('infrastructure.dlq.colSubject'), render: (r) => <code>{r.subject}</code> },
    { key: 'msgId', label: t('infrastructure.dlq.colMsgId'), render: (r) => <code>{r.msgId}</code> },
    { key: 'deliveryCount', label: t('infrastructure.dlq.colDeliveries'), render: (r) => <Badge variant="warning">{r.deliveryCount}</Badge> },
    { key: 'payloadSize', label: t('infrastructure.dlq.colSize'), render: (r) => fmtSize(r.payloadSize) },
    {
      key: 'lastError',
      label: t('infrastructure.dlq.colLastError'),
      sortable: false,
      cellStyle: { maxWidth: 360, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' },
      render: (r) => <span title={r.lastError ?? ''}>{r.lastError ?? '—'}</span>,
    },
    ...(canRetry
      ? [{
          key: 'actions',
          label: t('infrastructure.dlq.colActions'),
          sortable: false,
          render: (r: DlqRow) => (
            <Button
              size="sm"
              variant="danger"
              loading={retryingId === r.id}
              onClick={() => onRetry(r)}
            >
              {t('infrastructure.dlq.retry')}
            </Button>
          ),
        } as DataTableColumn<DlqRow>]
      : []),
  ];

  if (loading && !data) return <LoadingSpinner />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('infrastructure.dlq.title')}
        subtitle={t('infrastructure.dlq.description')}
      />

      <ListFilterToolbar
        variant="boxed"
        className={styles.filterToolbar}
        searchWidth={420}
        hideClearButton
        searchPlaceholder={t('infrastructure.dlq.subjectPlaceholder')}
        searchValue={subject}
        onSearchChange={(v) => { setSubject(v); setOffset(0); }}
      />

      <DataTable
        columns={columns}
        data={data?.rows ?? []}
        emptyMessage={t('infrastructure.dlq.empty')}
        loading={loading}
        serverPaginated
        hideSearch
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

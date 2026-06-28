/**
 * ExemptionsPage — single unified list of compliance exemption grants and
 * PENDING exemption_request rows. The Status filter (default: All) drives the
 * server-side `tab` param. Each row carries a `kind` discriminator so the
 * actions column branches between grant lifecycle controls (Enable/Disable +
 * Delete pre-activation) and pending approve/reject.
 */
import { useCallback, useMemo, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { useApi } from '@/hooks/useApi';
import { complianceApi } from '@/api/services/compliance/compliance';
import type {
  ExemptionListTab,
  UnifiedExemptionRow,
} from '@/api/services/compliance/compliance';
import {
  Badge,
  Button,
  DataTable,
  ErrorBanner,
  ListPagination,
  DEFAULT_ADMIN_LIST_PAGE_SIZE,
  LoadingSpinner,
  PageHeader,
  RevokeActionIcon,
  RowActionIconButton,
  RowActions,
  Select,
  Stack,
} from '@/components/ui';
import type { AdminListPageSize, DataTableColumn } from '@/components/ui';
import { useToast } from '@/context/ToastContext';
import { ExemptionCreateDialog } from './ExemptionCreateDialog';
import { ExemptionDeleteDialog } from './ExemptionDeleteDialog';
import { ExemptionApproveDialog } from './ExemptionApproveDialog';
import { ExemptionRejectDialog } from './ExemptionRejectDialog';
import { useExemptionDialogs } from './useExemptionDialogs';
import styles from './ExemptionsPage.module.css';

const EM_DASH = '—';

const STATUS_BADGE_VARIANT: Record<UnifiedExemptionRow['status'], 'success' | 'info' | 'warning' | 'outline'> = {
  effective: 'success',
  oncoming: 'info',
  pending: 'warning',
  expired: 'outline',
};

function EnableActionIcon() {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" aria-hidden>
      <circle cx="12" cy="12" r="10" />
      <path d="m8 12 2.5 2.5L16 9" />
    </svg>
  );
}

export function ExemptionsPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { addToast } = useToast();

  const [statusFilter, setStatusFilter] = useState<ExemptionListTab>('all');
  const [offset, setOffset] = useState(0);
  const [pageLimit, setPageLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);

  const { data: page, loading, error, refetch } = useApi(
    () =>
      complianceApi.listExemptions({
        tab: statusFilter,
        limit: pageLimit,
        offset,
      }),
    ['admin', 'compliance-exemptions', 'list', statusFilter, offset, pageLimit],
  );

  const rows = page?.rows ?? [];
  const total = page?.total ?? 0;

  const onStatusChange = useCallback((v: string) => {
    setStatusFilter(v as ExemptionListTab);
    setOffset(0);
    setPageLimit(DEFAULT_ADMIN_LIST_PAGE_SIZE);
  }, []);

  const statusOptions = useMemo(
    () => [
      { value: 'all', label: t('pages:compliance.exemptions.statusAll') },
      { value: 'effective', label: t('pages:compliance.exemptions.statusEffective') },
      { value: 'oncoming', label: t('pages:compliance.exemptions.statusOncoming') },
      { value: 'pending', label: t('pages:compliance.exemptions.statusPending') },
      { value: 'expired', label: t('pages:compliance.exemptions.statusExpired') },
    ],
    [t],
  );

  const formatTs = useCallback((iso: string | null): string => {
    if (!iso) return EM_DASH;
    return new Date(iso).toLocaleString();
  }, []);

  const relativeTime = useCallback(
    (iso: string | null): string => {
      if (!iso) return EM_DASH;
      const now = Date.now();
      const target = new Date(iso).getTime();
      const diffMs = target - now;
      if (diffMs <= 0) {
        return t('pages:compliance.exemptions.relative.expired', 'expired');
      }
      const minutes = Math.floor(diffMs / 60000);
      if (minutes < 60) {
        return t('pages:compliance.exemptions.relative.minutes', { n: minutes });
      }
      const hours = Math.floor(minutes / 60);
      if (hours < 24) {
        return t('pages:compliance.exemptions.relative.hours', { n: hours });
      }
      const days = Math.floor(hours / 24);
      return t('pages:compliance.exemptions.relative.days', { n: days });
    },
    [t],
  );

  const [showCreate, setShowCreate] = useState(false);
  const [patchBusyId, setPatchBusyId] = useState<string | null>(null);

  const {
    deleteTarget,
    setDeleteTarget,
    approveTarget,
    setApproveTarget,
    rejectTarget,
    setRejectTarget,
    rejectNote,
    setRejectNote,
    approveBusy,
    rejectBusy,
    handleDelete,
    handleApprove,
    handleReject,
  } = useExemptionDialogs(refetch);

  const handleToggleInactive = useCallback(
    async (row: UnifiedExemptionRow) => {
      if (row.kind !== 'grant' || row.inactive === null) return;
      setPatchBusyId(row.id);
      try {
        await complianceApi.patchExemptionGrant(row.id, { inactive: !row.inactive });
        addToast(t('pages:compliance.exemptions.toggleSuccess'), 'success');
        void refetch();
      } catch (err) {
        const msg = err instanceof Error ? err.message : 'unknown error';
        addToast(t('pages:compliance.exemptions.toggleError', { error: msg }), 'error');
      } finally {
        setPatchBusyId(null);
      }
    },
    [addToast, t, refetch],
  );

  const columns: DataTableColumn<UnifiedExemptionRow>[] = useMemo(
    () => [
      {
        key: 'id',
        label: t('common:id'),
        render: (row) => <code style={{ fontSize: 'var(--g-font-size-xs)' }}>{row.id.slice(0, 8)}</code>,
      },
      {
        key: 'status',
        label: t('pages:compliance.exemptions.colStatus'),
        render: (row) => (
          <Stack direction="horizontal" gap="sm" align="center">
            <Badge variant={STATUS_BADGE_VARIANT[row.status]}>
              {t(`pages:compliance.exemptions.status${row.status.charAt(0).toUpperCase()}${row.status.slice(1)}`)}
            </Badge>
            {row.kind === 'grant' && row.inactive ? (
              <Badge variant="warning">{t('pages:compliance.exemptions.statusDisabled')}</Badge>
            ) : null}
          </Stack>
        ),
      },
      {
        key: 'sourceIp',
        label: t('pages:compliance.exemptions.sourceIpLabel', 'Source IP'),
        render: (row) => <code>{row.sourceIp}</code>,
      },
      {
        key: 'targetHost',
        label: t('pages:compliance.exemptions.targetHostLabel', 'Target host'),
        render: (row) => <code>{row.targetHost}</code>,
      },
      {
        key: 'effectiveFrom',
        label: t('pages:compliance.exemptions.colEffectiveFrom'),
        render: (row) => <span title={row.effectiveFrom ?? ''}>{formatTs(row.effectiveFrom)}</span>,
      },
      {
        key: 'expiresAt',
        label: t('pages:compliance.exemptions.expiresLabel', 'Expires'),
        render: (row) => (
          <span title={row.expiresAt ? new Date(row.expiresAt).toLocaleString() : ''}>
            {relativeTime(row.expiresAt)}
          </span>
        ),
      },
      {
        key: 'reason',
        label: t('pages:compliance.exemptions.reasonLabel', 'Reason'),
        render: (row) => row.reason || EM_DASH,
      },
      {
        key: 'requestedBy',
        label: t('pages:compliance.exemptions.colRequestedBy'),
        render: (row) => row.requestedBy ?? EM_DASH,
      },
      {
        key: 'approvedBy',
        label: t('pages:compliance.exemptions.approvedByLabel', 'Approved by'),
        render: (row) => row.approvedBy ?? EM_DASH,
      },
      {
        key: 'createdAt',
        label: t('pages:compliance.exemptions.colCreatedAt'),
        render: (row) => formatTs(row.createdAt),
      },
      {
        key: 'actions',
        label: t('common:actions', 'Actions'),
        sortable: false,
        render: (row) => {
          if (row.kind === 'pending') {
            return (
              <Stack direction="horizontal" gap="sm" onClick={(e) => e.stopPropagation()}>
                <Button size="sm" variant="secondary" onClick={() => setApproveTarget(row)}>
                  {t('pages:compliance.exemptions.approveBtn')}
                </Button>
                <Button
                  size="sm"
                  variant="danger"
                  onClick={() => {
                    setRejectNote('');
                    setRejectTarget(row);
                  }}
                >
                  {t('pages:compliance.exemptions.rejectBtn')}
                </Button>
              </Stack>
            );
          }
          // Grant row
          return (
            <Stack direction="horizontal" gap="sm" onClick={(e) => e.stopPropagation()}>
              <RowActions>
                {row.inactive ? (
                  <RowActionIconButton
                    label={t('pages:compliance.exemptions.enableBtn')}
                    disabled={patchBusyId === row.id}
                    onAction={() => {
                      void handleToggleInactive(row);
                    }}
                  >
                    <EnableActionIcon />
                  </RowActionIconButton>
                ) : (
                  <RowActionIconButton
                    label={t('pages:compliance.exemptions.disableBtn')}
                    tone="danger"
                    disabled={patchBusyId === row.id}
                    onAction={() => {
                      void handleToggleInactive(row);
                    }}
                  >
                    <RevokeActionIcon />
                  </RowActionIconButton>
                )}
              </RowActions>
              {row.activatedAt === null ? (
                <Button
                  size="sm"
                  variant="danger"
                  onClick={(e: React.MouseEvent) => {
                    e.stopPropagation();
                    setDeleteTarget(row);
                  }}
                >
                  {t('common:delete', 'Delete')}
                </Button>
              ) : null}
            </Stack>
          );
        },
      },
    ],
    [t, formatTs, relativeTime, patchBusyId, handleToggleInactive, setApproveTarget, setRejectNote, setRejectTarget, setDeleteTarget],
  );

  // Initial-load fallback — show a full-page spinner only on the very first
  // mount before any data has arrived. Subsequent loadings (e.g., filter
  // changes that point useApi at a fresh queryKey with no cache entry yet)
  // keep the success tree mounted so DataTable, Dialog, and the virtualizer
  // don't unmount/remount, which trips React 19's hooks check inside the
  // virtualizer when the tree comes back.
  const hasLoadedRef = useRef(false);
  if (page) hasLoadedRef.current = true;

  if (!hasLoadedRef.current) {
    if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
    if (loading) return <LoadingSpinner />;
  }

  return (
    <div className={styles.page}>
      <PageHeader
          title={t('pages:compliance.exemptions.title', 'Temporary Exemptions')}
          subtitle={t(
            'pages:compliance.exemptions.subtitle',
            'Source/target pairs that bypass compliance hooks for a limited window',
          )}
          action={
            <Button variant="primary" onClick={() => setShowCreate(true)}>
              {t('pages:compliance.exemptions.create', 'Create exemption')}
            </Button>
          }
        />

      <div className={styles.statusFilter}>
        <Select
          value={statusFilter}
          onValueChange={onStatusChange}
          options={statusOptions}
        />
      </div>

      <DataTable
        className={styles.exemptionsTable}
        columns={columns}
        data={rows}
        hideSearch
        onRowClick={(r) => navigate(`/compliance/exemptions/${r.id}`)}
        emptyMessage={t('pages:compliance.exemptions.noRows')}
      />
      <ListPagination
        offset={offset}
        limit={pageLimit}
        total={total}
        onOffsetChange={setOffset}
        onLimitChange={(v) => { setPageLimit(v); setOffset(0); }}
      />

      <ExemptionCreateDialog
        open={showCreate}
        onOpenChange={setShowCreate}
        refetch={refetch}
      />

      <ExemptionDeleteDialog
        deleteTarget={deleteTarget}
        setDeleteTarget={setDeleteTarget}
        onConfirm={handleDelete}
      />

      <ExemptionApproveDialog
        approveTarget={approveTarget}
        setApproveTarget={setApproveTarget}
        approveBusy={approveBusy}
        onConfirm={handleApprove}
      />

      <ExemptionRejectDialog
        rejectTarget={rejectTarget}
        setRejectTarget={setRejectTarget}
        rejectNote={rejectNote}
        setRejectNote={setRejectNote}
        rejectBusy={rejectBusy}
        onConfirm={handleReject}
      />
    </div>
  );
}

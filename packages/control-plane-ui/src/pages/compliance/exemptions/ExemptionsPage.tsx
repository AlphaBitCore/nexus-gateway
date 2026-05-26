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
import { useAuth } from '@/auth/context/AuthContext';
import { useApi } from '@/hooks/useApi';
import { complianceApi } from '@/api/services/compliance/compliance';
import type {
  CreateExemptionRequest,
  ExemptionListTab,
  UnifiedExemptionRow,
} from '@/api/services/compliance/compliance';
import {
  AlertDialog,
  Badge,
  Button,
  Card,
  Checkbox,
  DataTable,
  Dialog,
  ErrorBanner,
  FormField,
  Input,
  ListPagination,
  DEFAULT_ADMIN_LIST_PAGE_SIZE,
  LoadingSpinner,
  PageHeader,
  Select,
  Stack,
  Textarea,
} from '@/components/ui';
import type { AdminListPageSize, DataTableColumn } from '@/components/ui';
import { useToast } from '@/context/ToastContext';

const EM_DASH = '—';

const STATUS_BADGE_VARIANT: Record<UnifiedExemptionRow['status'], 'success' | 'info' | 'warning' | 'outline'> = {
  effective: 'success',
  oncoming: 'info',
  pending: 'warning',
  expired: 'outline',
};

export function ExemptionsPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { addToast } = useToast();
  const { email, keyName } = useAuth();

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
  const [creating, setCreating] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<UnifiedExemptionRow | null>(null);
  const [approveTarget, setApproveTarget] = useState<UnifiedExemptionRow | null>(null);
  const [rejectTarget, setRejectTarget] = useState<UnifiedExemptionRow | null>(null);
  const [rejectNote, setRejectNote] = useState('');
  const [approveBusy, setApproveBusy] = useState(false);
  const [rejectBusy, setRejectBusy] = useState(false);
  const [patchBusyId, setPatchBusyId] = useState<string | null>(null);

  const [formSourceIp, setFormSourceIp] = useState('');
  const [formTargetHost, setFormTargetHost] = useState('');
  const [formDuration, setFormDuration] = useState('1440');
  const [formReason, setFormReason] = useState('');
  const [submitAsPending, setSubmitAsPending] = useState(false);

  const durationOptions = useMemo(
    () =>
      [
        { value: '60', labelKey: 'duration.1h' },
        { value: '240', labelKey: 'duration.4h' },
        { value: '720', labelKey: 'duration.12h' },
        { value: '1440', labelKey: 'duration.24h' },
        { value: '2880', labelKey: 'duration.48h' },
        { value: '10080', labelKey: 'duration.7d' },
      ].map((o) => ({
        value: o.value,
        label: t(`pages:compliance.exemptions.${o.labelKey}`),
      })),
    [t],
  );

  const resetForm = useCallback(() => {
    setFormSourceIp('');
    setFormTargetHost('');
    setFormDuration('1440');
    setFormReason('');
    setSubmitAsPending(false);
  }, []);

  const handleCreate = useCallback(async () => {
    if (!formSourceIp.trim() || !formTargetHost.trim()) {
      addToast(t('pages:compliance.exemptions.validation.sourceTargetRequired'), 'error');
      return;
    }
    const durationMinutes = parseInt(formDuration, 10);
    if (Number.isNaN(durationMinutes) || durationMinutes <= 0) {
      addToast(t('pages:compliance.exemptions.validation.durationPositive'), 'error');
      return;
    }
    const reasonTrim = formReason.trim();
    if (reasonTrim.length < 4 || reasonTrim.length > 500) {
      addToast(t('pages:compliance.exemptions.validation.reasonLengthAdmin'), 'error');
      return;
    }
    setCreating(true);
    try {
      if (submitAsPending) {
        await complianceApi.createPendingExemptionRequest({
          transactionId: crypto.randomUUID(),
          sourceIp: formSourceIp.trim(),
          targetHost: formTargetHost.trim(),
          reason: reasonTrim,
          durationMinutes,
          requestedBy: (email && email.trim()) || keyName || 'admin-ui',
        });
        addToast(t('pages:compliance.exemptions.createPendingSuccess'), 'success');
      } else {
        const req: CreateExemptionRequest = {
          sourceIp: formSourceIp.trim(),
          targetHost: formTargetHost.trim(),
          durationMinutes,
          reason: reasonTrim,
        };
        await complianceApi.createExemptionGrant(req);
        addToast(t('pages:compliance.exemptions.createSuccess', 'Exemption created'), 'success');
      }
      setShowCreate(false);
      resetForm();
      void refetch();
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'unknown error';
      addToast(
        submitAsPending
          ? t('pages:compliance.exemptions.createPendingError', { error: msg })
          : t('pages:compliance.exemptions.createError', { error: msg }),
        'error',
      );
    } finally {
      setCreating(false);
    }
  }, [formSourceIp, formTargetHost, formDuration, formReason, submitAsPending, email, keyName, addToast, t, resetForm, refetch]);

  const handleDelete = useCallback(async () => {
    if (!deleteTarget) return;
    try {
      await complianceApi.deleteExemptionGrant(deleteTarget.id);
      addToast(t('pages:compliance.exemptions.deleteSuccess', 'Exemption deleted'), 'success');
      setDeleteTarget(null);
      void refetch();
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'unknown error';
      addToast(t('pages:compliance.exemptions.deleteError', { error: msg }), 'error');
    }
  }, [deleteTarget, addToast, t, refetch]);

  const handleApprove = useCallback(async () => {
    if (!approveTarget) return;
    setApproveBusy(true);
    try {
      await complianceApi.approveExemption(approveTarget.id);
      addToast(t('pages:compliance.exemptions.approveSuccess'), 'success');
      setApproveTarget(null);
      void refetch();
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'unknown error';
      addToast(t('pages:compliance.exemptions.approveError', { error: msg }), 'error');
    } finally {
      setApproveBusy(false);
    }
  }, [approveTarget, addToast, t, refetch]);

  const handleReject = useCallback(async () => {
    if (!rejectTarget) return;
    const reason = rejectNote.trim();
    if (reason.length < 2) {
      addToast(t('pages:compliance.exemptions.validation.rejectReasonRequired'), 'error');
      return;
    }
    setRejectBusy(true);
    try {
      await complianceApi.rejectExemption(rejectTarget.id, reason);
      addToast(t('pages:compliance.exemptions.rejectSuccess'), 'success');
      setRejectTarget(null);
      setRejectNote('');
      void refetch();
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'unknown error';
      addToast(t('pages:compliance.exemptions.rejectError', { error: msg }), 'error');
    } finally {
      setRejectBusy(false);
    }
  }, [rejectTarget, rejectNote, addToast, t, refetch]);

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
        label: 'ID',
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
        label: '',
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
              <Button
                size="sm"
                variant="secondary"
                disabled={patchBusyId === row.id}
                onClick={(e: React.MouseEvent) => {
                  e.stopPropagation();
                  void handleToggleInactive(row);
                }}
              >
                {row.inactive
                  ? t('pages:compliance.exemptions.enableBtn')
                  : t('pages:compliance.exemptions.disableBtn')}
              </Button>
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
    [t, formatTs, relativeTime, patchBusyId, handleToggleInactive],
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
    <>
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

      <Card>
        <Stack gap="md">
          <Stack direction="horizontal" gap="sm" align="center">
            <label
              htmlFor="exemption-status-filter"
              style={{ fontSize: 'var(--g-font-size-sm)', color: 'var(--color-text-secondary)' }}
            >
              {t('pages:compliance.exemptions.statusFilterLabel')}
            </label>
            <div style={{ minWidth: '180px' }}>
              <Select
                value={statusFilter}
                onValueChange={onStatusChange}
                options={statusOptions}
              />
            </div>
          </Stack>

          <DataTable
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
        </Stack>
      </Card>

      <Dialog
        open={showCreate}
        onOpenChange={(open) => {
          setShowCreate(open);
          if (!open) resetForm();
        }}
        title={t('pages:compliance.exemptions.createTitle', 'Create temporary exemption')}
        description={t(
          'pages:compliance.exemptions.createDesc',
          'Exempted traffic will still be TLS-bumped but will skip compliance hooks.',
        )}
      >
        <Stack gap="md">
          <FormField label={t('pages:compliance.exemptions.sourceIpLabel', 'Source IP or CIDR')}>
            <Input
              value={formSourceIp}
              onChange={(e) => setFormSourceIp(e.target.value)}
              placeholder={t(
                'pages:compliance.exemptions.placeholder.sourceIp',
                'e.g. 10.0.0.0/24 or 10.0.0.5',
              )}
            />
          </FormField>

          <FormField label={t('pages:compliance.exemptions.targetHostLabel', 'Target host')}>
            <Input
              value={formTargetHost}
              onChange={(e) => setFormTargetHost(e.target.value)}
              placeholder={t(
                'pages:compliance.exemptions.placeholder.targetHost',
                'e.g. api.openai.com or *.openai.com',
              )}
            />
          </FormField>

          <Stack direction="horizontal" gap="sm" align="center">
            <Checkbox
              id="exemption-submit-pending"
              checked={submitAsPending}
              onCheckedChange={(v) => setSubmitAsPending(v === true)}
            />
            <label
              htmlFor="exemption-submit-pending"
              style={{ cursor: 'pointer', fontSize: 'var(--g-font-size-sm)' }}
            >
              {t('pages:compliance.exemptions.submitAsPendingLabel')}
            </label>
          </Stack>
          <p style={{ margin: 'var(--g-space-0)', fontSize: 'var(--g-font-size-xs)', color: 'var(--color-text-secondary)' }}>
            {t('pages:compliance.exemptions.submitAsPendingHint')}
          </p>

          <FormField label={t('pages:compliance.exemptions.durationLabel', 'Duration')}>
            <Select value={formDuration} onValueChange={setFormDuration} options={durationOptions} />
          </FormField>

          <FormField label={t('pages:compliance.exemptions.reasonLabel', 'Reason')}>
            <Textarea
              value={formReason}
              onChange={(e) => setFormReason(e.target.value)}
              placeholder={t(
                'pages:compliance.exemptions.placeholder.reason',
                'e.g. false positive investigation',
              )}
              rows={3}
            />
          </FormField>

          <Stack direction="horizontal" gap="sm" justify="end">
            <Button
              variant="ghost"
              onClick={() => {
                setShowCreate(false);
                resetForm();
              }}
              disabled={creating}
            >
              {t('common:cancel', 'Cancel')}
            </Button>
            <Button
              variant="primary"
              onClick={handleCreate}
              disabled={creating || !formSourceIp.trim() || !formTargetHost.trim()}
            >
              {creating
                ? t('pages:compliance.exemptions.creating', 'Creating…')
                : t('pages:compliance.exemptions.createBtn', 'Create')}
            </Button>
          </Stack>
        </Stack>
      </Dialog>

      <AlertDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => {
          if (!open) setDeleteTarget(null);
        }}
        title={t('pages:compliance.exemptions.deleteTitle', 'Delete exemption?')}
        description={
          deleteTarget
            ? t('pages:compliance.exemptions.deleteDesc', {
                sourceIp: deleteTarget.sourceIp,
                targetHost: deleteTarget.targetHost,
              })
            : ''
        }
        confirmLabel={t('common:delete', 'Delete')}
        cancelLabel={t('common:cancel', 'Cancel')}
        onConfirm={handleDelete}
        variant="danger"
      />

      <AlertDialog
        open={approveTarget !== null}
        onOpenChange={(open) => {
          if (!open) setApproveTarget(null);
        }}
        title={t('pages:compliance.exemptions.approveTitle')}
        description={
          approveTarget
            ? t('pages:compliance.exemptions.approveDesc', {
                sourceIp: approveTarget.sourceIp,
                targetHost: approveTarget.targetHost,
              })
            : ''
        }
        confirmLabel={t('pages:compliance.exemptions.approveConfirm')}
        cancelLabel={t('common:cancel', 'Cancel')}
        onConfirm={() => {
          void handleApprove();
        }}
        loading={approveBusy}
        variant="default"
      />

      <Dialog
        open={rejectTarget !== null}
        onOpenChange={(open) => {
          if (!open) {
            setRejectTarget(null);
            setRejectNote('');
          }
        }}
        title={t('pages:compliance.exemptions.rejectTitle')}
        description={t('pages:compliance.exemptions.rejectDesc')}
      >
        <Stack gap="md">
          <FormField label={t('pages:compliance.exemptions.rejectReasonLabel')}>
            <Textarea
              value={rejectNote}
              onChange={(e) => setRejectNote(e.target.value)}
              placeholder={t('pages:compliance.exemptions.rejectReasonPlaceholder')}
              rows={3}
            />
          </FormField>
          <Stack direction="horizontal" gap="sm" justify="end">
            <Button
              variant="ghost"
              onClick={() => {
                setRejectTarget(null);
                setRejectNote('');
              }}
              disabled={rejectBusy}
            >
              {t('common:cancel', 'Cancel')}
            </Button>
            <Button
              variant="danger"
              onClick={() => void handleReject()}
              disabled={rejectBusy || rejectNote.trim().length < 2}
            >
              {rejectBusy
                ? t('pages:compliance.exemptions.rejectSubmitting')
                : t('pages:compliance.exemptions.rejectSubmit')}
            </Button>
          </Stack>
        </Stack>
      </Dialog>
    </>
  );
}

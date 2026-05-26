import { useState, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import clsx from 'clsx';
import { useApi } from '../../hooks/useApi';
import { dsarApi } from '../../api/services/compliance/dsar';
import { iamApi } from '../../api/services';
import type {
  DSARRequest,
  DSARRequestStatus,
  DSARRequestType,
  DSARFulfillResponse,
} from '../../api/services/compliance/dsar';
import {
  PageHeader, LoadingSpinner, ErrorBanner, Card, Stack, Button, Input,
  Dialog, AlertDialog, FormField, Textarea, RowActions,
  RowActionTextLinkButton, RowActionTerminal,
  ListPagination, DEFAULT_ADMIN_LIST_PAGE_SIZE,
} from '@/components/ui';
import type { AdminListPageSize } from '@/components/ui';
import { useToast } from '@/context/ToastContext';
import styles from '../compliance/dashboard/ComplianceDashboardPage.module.css';

/**
 * S42 — DSAR (Data Subject Access Request) management page.
 *
 * Compliance officers file, triage, and fulfill GDPR/CCPA requests
 * through this page. ACCESS exports the subject's audit rows; ERASURE
 * anonymises them across both audit tables. Both actions are audited
 * via AdminAuditLog so the compliance trail is itself reviewable.
 */

function useStatusOptions() {
  const { t } = useTranslation();
  return [
    { value: '' as const, label: t('pages:security.dsar.allStatuses') },
    { value: 'PENDING' as const, label: t('pages:security.dsar.pending') },
    { value: 'IN_PROGRESS' as const, label: t('pages:security.dsar.inProgress') },
    { value: 'COMPLETED' as const, label: t('pages:security.dsar.completed') },
    { value: 'REJECTED' as const, label: t('pages:security.dsar.rejected') },
  ];
}

function useTypeOptions() {
  const { t } = useTranslation();
  return [
    { value: 'ACCESS' as const, label: t('pages:security.dsar.typeAccess') },
    { value: 'ERASURE' as const, label: t('pages:security.dsar.typeErasure') },
  ];
}

function statusClass(s: DSARRequestStatus, css: Record<string, string>): string {
  switch (s) {
    case 'PENDING':     return css.dsarStatusPending;
    case 'IN_PROGRESS': return css.dsarStatusInProgress;
    case 'COMPLETED':   return css.dsarStatusCompleted;
    case 'REJECTED':    return css.dsarStatusRejected;
  }
}

export function DSARPage() {
  const { t } = useTranslation();
  const { addToast } = useToast();
  const STATUS_OPTIONS = useStatusOptions();
  const TYPE_OPTIONS = useTypeOptions();

  const [statusFilter, setStatusFilter] = useState<DSARRequestStatus | ''>('');
  const [offset, setOffset] = useState(0);
  const [pageLimit, setPageLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);

  const { data, loading, error, refetch } = useApi(
    () => dsarApi.list({
      status: statusFilter || undefined,
      limit: pageLimit,
      offset,
    }),
    ['admin', 'dsar', 'list', statusFilter, offset, pageLimit],
  );
  const { data: usersData } = useApi<{ data: Array<{ id: string; displayName: string; email?: string }> }>(
    () => iamApi.listUsers() as Promise<unknown> as Promise<{ data: Array<{ id: string; displayName: string; email?: string }> }>,
    ['admin', 'users', 'list'],
  );

  const [creating, setCreating] = useState(false);
  const [draft, setDraft] = useState({
    subjectId: '',
    contact: '',
    type: 'ACCESS' as DSARRequestType,
    notes: '',
  });
  const [saving, setSaving] = useState(false);

  const [fulfilling, setFulfilling] = useState<DSARRequest | null>(null);
  const [fulfillResult, setFulfillResult] = useState<DSARFulfillResponse | null>(null);

  const openCreate = useCallback(() => {
    setDraft({ subjectId: '', contact: '', type: 'ACCESS', notes: '' });
    setCreating(true);
  }, []);

  const handleCreate = useCallback(async () => {
    if (!draft.subjectId.trim()) {
      addToast(t('pages:security.dsar.subjectIdRequired'), 'error');
      return;
    }
    setSaving(true);
    try {
      await dsarApi.create({
        subjectId: draft.subjectId.trim(),
        type: draft.type,
        contact: draft.contact.trim() || null,
        notes: draft.notes.trim() || null,
      });
      addToast(t('pages:security.dsar.requestCreated'), 'success');
      setCreating(false);
      refetch();
    } catch (err) {
      addToast(t('pages:security.dsar.createFailed', { error: err instanceof Error ? err.message : 'unknown error' }), 'error');
    } finally {
      setSaving(false);
    }
  }, [draft, addToast, refetch, t]);

  const handleAdvance = useCallback(async (req: DSARRequest, nextStatus: DSARRequestStatus) => {
    try {
      await dsarApi.update(req.id, { status: nextStatus });
      addToast(t('pages:security.dsar.statusAdvanced', { status: nextStatus }), 'success');
      refetch();
    } catch (err) {
      addToast(t('pages:security.dsar.updateFailed', { error: err instanceof Error ? err.message : 'unknown error' }), 'error');
    }
  }, [addToast, refetch, t]);

  const handleFulfill = useCallback(async () => {
    if (!fulfilling) return;
    try {
      const result = await dsarApi.fulfill(fulfilling.id);
      setFulfillResult(result);
      addToast(
        fulfilling.type === 'ACCESS'
          ? t('pages:security.dsar.exportedRows', { vk: result.export?.vk.length ?? 0, proxy: result.export?.proxy.length ?? 0 })
          : t('pages:security.dsar.anonymisedRows', { vk: result.outcome?.vkRowsAnonymised ?? 0, proxy: result.outcome?.proxyRowsAnonymised ?? 0 }),
        'success',
      );
      refetch();
    } catch (err) {
      addToast(t('pages:security.dsar.fulfillFailed', { error: err instanceof Error ? err.message : 'unknown error' }), 'error');
    }
  }, [fulfilling, addToast, refetch, t]);

  const handleDownloadExport = useCallback(() => {
    if (!fulfillResult?.export) return;
    const json = JSON.stringify(fulfillResult.export, null, 2);
    const blob = new Blob([json], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `dsar-export-${fulfilling?.subjectId ?? 'unknown'}-${Date.now()}.json`;
    a.click();
    URL.revokeObjectURL(url);
  }, [fulfillResult, fulfilling]);

  if (loading && !data) return <LoadingSpinner />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const requests = data?.requests ?? [];
  const total = data?.total ?? 0;

  return (
    <>
      <PageHeader
        title={t('pages:security.dsar.title', 'Data Subject Requests')}
        subtitle={t(
          'pages:security.dsar.subtitle',
          'Manage GDPR/CCPA data subject access and erasure requests. Fulfilment runs against both Virtual Key gateway and compliance-proxy audit tables and writes its outcome to the AdminAuditLog.',
        )}
      />

      <Stack gap="md">
        <Card>
          <Stack gap="sm">
            <div className={styles.sectionTitle}>
              {t('pages:security.dsar.queueTitle', 'Request Queue')} ({total})
            </div>
            <div className={styles.filterBar}>
              <label className={styles.filterField}>
                {t('pages:security.dsar.filterStatus', 'Status filter')}
                <select
                  value={statusFilter}
                  onChange={(e) => {
                    setStatusFilter(e.target.value as DSARRequestStatus | '');
                    setOffset(0);
                  }}
                  style={{ marginLeft: 'var(--g-space-2)', padding: 'var(--g-space-1) var(--g-space-2)' }}
                >
                  {STATUS_OPTIONS.map((o) => (
                    <option key={o.value} value={o.value}>{o.label}</option>
                  ))}
                </select>
              </label>
              <Button variant="ghost" onClick={refetch}>
                {t('common:refresh', 'Refresh')}
              </Button>
              <Button variant="primary" onClick={openCreate}>
                + {t('pages:security.dsar.fileRequest', 'File request')}
              </Button>
            </div>
            {requests.length === 0 ? (
              <div className={styles.noData}>
                {t('pages:security.dsar.empty', 'No DSAR requests in this view.')}
              </div>
            ) : (
              <div className={styles.tableWrapper}>
                <table className={styles.table}>
                  <thead>
                    <tr>
                      <th>{t('pages:security.dsar.colSubject')}</th>
                      <th>{t('pages:security.dsar.colType')}</th>
                      <th>{t('pages:security.dsar.colStatus')}</th>
                      <th>{t('pages:security.dsar.colFiled')}</th>
                      <th>{t('pages:security.dsar.colCompleted')}</th>
                      <th>{t('pages:security.dsar.colNotes')}</th>
                      <th></th>
                    </tr>
                  </thead>
                  <tbody>
                    {requests.map((r) => (
                      <tr key={r.id}>
                        <td>
                          <code>{(() => {
                            const user = (usersData?.data ?? []).find((u) => u.id === r.subjectId);
                            return user ? `${user.displayName}${user.email ? ` (${user.email})` : ''}` : r.subjectId;
                          })()}</code>
                          {r.contact && (
                            <div className={styles.metaText}>
                              {r.contact}
                            </div>
                          )}
                        </td>
                        <td>{r.type}</td>
                        <td className={clsx(statusClass(r.status, styles))}>
                          {r.status}
                        </td>
                        <td>{new Date(r.createdAt).toLocaleString()}</td>
                        <td>{r.completedAt ? new Date(r.completedAt).toLocaleString() : '—'}</td>
                        <td style={{ fontSize: 'var(--g-font-size-xs)', maxWidth: '20rem' }}>
                          {r.notes ?? '—'}
                        </td>
                        <td>
                          {r.status === 'PENDING' && (
                            <RowActions variant="text">
                              <RowActionTextLinkButton label={t('pages:security.dsar.start', 'Start')} onAction={() => handleAdvance(r, 'IN_PROGRESS')} />
                              <RowActionTextLinkButton label={t('pages:security.dsar.reject', 'Reject')} tone="danger" onAction={() => handleAdvance(r, 'REJECTED')} />
                            </RowActions>
                          )}
                          {r.status === 'IN_PROGRESS' && (
                            <Button
                              size="sm"
                              variant="primary"
                              onClick={() => { setFulfilling(r); setFulfillResult(null); }}
                            >
                              {t('pages:security.dsar.fulfill', 'Fulfill')}
                            </Button>
                          )}
                          {(r.status === 'COMPLETED' || r.status === 'REJECTED') && (
                            <RowActions variant="text">
                              <RowActionTerminal>{t('pages:security.dsar.terminal')}</RowActionTerminal>
                            </RowActions>
                          )}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
            <ListPagination
              offset={offset}
              limit={pageLimit}
              total={total}
              onOffsetChange={(v) => setOffset(v)}
              onLimitChange={(v) => { setPageLimit(v); setOffset(0); }}
            />
          </Stack>
        </Card>
      </Stack>

      <Dialog
        open={creating}
        onOpenChange={(open) => { if (!open) setCreating(false); }}
        title={t('pages:security.dsar.fileRequest', 'File DSAR Request')}
      >
        <Stack gap="md">
          <FormField label={t('pages:security.dsar.subjectId', 'Data Subject (User)')}>
            <select
              value={draft.subjectId}
              onChange={(e) => setDraft({ ...draft, subjectId: e.target.value })}
              style={{ width: '100%', padding: 'var(--g-space-1-5) var(--g-space-2)' }}
            >
              <option value="">{t('pages:security.dsar.selectUser')}</option>
              {(usersData?.data ?? []).map((u) => (
                <option key={u.id} value={u.id}>
                  {u.displayName}{u.email ? ` (${u.email})` : ''}
                </option>
              ))}
            </select>
          </FormField>
          <FormField label={t('pages:security.dsar.contact', 'Contact (optional)')}>
            <Input
              value={draft.contact}
              onChange={(e) => setDraft({ ...draft, contact: e.target.value })}
              placeholder={t('pages:security.dsar.contactPlaceholder')}
            />
          </FormField>
          <FormField label={t('pages:security.dsar.requestType', 'Request type')}>
            <select
              value={draft.type}
              onChange={(e) => setDraft({ ...draft, type: e.target.value as DSARRequestType })}
              style={{ width: '100%', padding: 'var(--g-space-1-5) var(--g-space-2)' }}
            >
              {TYPE_OPTIONS.map((o) => (
                <option key={o.value} value={o.value}>{o.label}</option>
              ))}
            </select>
          </FormField>
          <FormField label={t('pages:security.dsar.requestNotes', 'Notes')}>
            <Textarea
              value={draft.notes}
              onChange={(e) => setDraft({ ...draft, notes: e.target.value })}
              rows={3}
            />
          </FormField>
          <Stack direction="horizontal" gap="sm" justify="end">
            <Button variant="ghost" onClick={() => setCreating(false)} disabled={saving}>
              {t('common:cancel', 'Cancel')}
            </Button>
            <Button variant="primary" onClick={handleCreate} disabled={saving}>
              {saving ? t('common:saving', 'Saving…') : t('common:save', 'Save')}
            </Button>
          </Stack>
        </Stack>
      </Dialog>

      <AlertDialog
        open={fulfilling !== null && !fulfillResult}
        onOpenChange={(open) => { if (!open) setFulfilling(null); }}
        title={fulfilling?.type === 'ERASURE'
          ? t('pages:security.dsar.confirmErasure', 'Confirm Erasure')
          : t('pages:security.dsar.confirmAccess', 'Run Access Export')}
        description={
          fulfilling
            ? fulfilling.type === 'ERASURE'
              ? t('pages:security.dsar.anonymiseDescription', { subject: fulfilling.subjectId })
              : t('pages:security.dsar.exportDescription', { subject: fulfilling.subjectId })
            : ''
        }
        confirmLabel={fulfilling?.type === 'ERASURE'
          ? t('pages:security.dsar.confirmAnonymise', 'Anonymise')
          : t('pages:security.dsar.confirmExport', 'Export')}
        cancelLabel={t('common:cancel', 'Cancel')}
        onConfirm={handleFulfill}
        variant={fulfilling?.type === 'ERASURE' ? 'danger' : 'default'}
      />

      {fulfillResult && (
        <Dialog
          open
          onOpenChange={(open) => {
            if (!open) {
              setFulfilling(null);
              setFulfillResult(null);
            }
          }}
          title={fulfilling?.type === 'ERASURE'
            ? t('pages:security.dsar.erasureComplete', 'Erasure Complete')
            : t('pages:security.dsar.exportReady', 'Export Ready')}
        >
          <Stack gap="md">
            {fulfilling?.type === 'ACCESS' && fulfillResult.export && (
              <>
                <div style={{ fontSize: 'var(--g-font-size-base)' }}>
                  {t('pages:security.dsar.exportedSummary', { vk: fulfillResult.export.vk.length, proxy: fulfillResult.export.proxy.length })}
                </div>
                <Button variant="primary" onClick={handleDownloadExport}>
                  {t('pages:security.dsar.downloadJson', 'Download JSON')}
                </Button>
              </>
            )}
            {fulfilling?.type === 'ERASURE' && fulfillResult.outcome && (
              <div style={{ fontSize: 'var(--g-font-size-base)' }}>
                {t('pages:security.dsar.anonymisedSummary', { vk: fulfillResult.outcome.vkRowsAnonymised, proxy: fulfillResult.outcome.proxyRowsAnonymised })}
              </div>
            )}
            <Stack direction="horizontal" gap="sm" justify="end">
              <Button
                variant="ghost"
                onClick={() => {
                  setFulfilling(null);
                  setFulfillResult(null);
                }}
              >
                {t('common:close', 'Close')}
              </Button>
            </Stack>
          </Stack>
        </Dialog>
      )}
    </>
  );
}

/**
 * Read-only detail + lifecycle actions for a unified compliance exemption row.
 * Backed by GET /api/admin/compliance/exemptions/:id.
 */
import { useCallback, useState } from 'react';
import { useParams, useNavigate, Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { complianceApi } from '@/api/services/compliance/compliance';
import type { UnifiedExemptionRow } from '@/api/services/compliance/compliance';
import {
  AlertDialog,
  Badge,
  Button,
  Card,
  Dialog,
  ErrorBanner,
  FormField,
  Skeleton,
  Stack,
  Textarea,
} from '@/components/ui';
import { useToast } from '@/context/ToastContext';
import { useQueryClient } from '@tanstack/react-query';
import iamStyles from '@/pages/iam/_shared/Iam.module.css';
import styles from './ComplianceExemptionDetailPage.module.css';

const EM_DASH = '\u2014';

const STATUS_BADGE_VARIANT: Record<UnifiedExemptionRow['status'], 'success' | 'info' | 'warning' | 'outline'> = {
  effective: 'success',
  oncoming: 'info',
  pending: 'warning',
  expired: 'outline',
};

export function ComplianceExemptionDetailPage() {
  const { t } = useTranslation();
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const { addToast } = useToast();
  const queryClient = useQueryClient();

  const { data: row, loading, error, refetch } = useApi<UnifiedExemptionRow>(
    () => complianceApi.getExemption(id!),
    ['admin', 'compliance-exemptions', 'detail', id],
  );

  const [deleteOpen, setDeleteOpen] = useState(false);
  const [approveOpen, setApproveOpen] = useState(false);
  const [rejectOpen, setRejectOpen] = useState(false);
  const [rejectNote, setRejectNote] = useState('');
  const [approveBusy, setApproveBusy] = useState(false);
  const [rejectBusy, setRejectBusy] = useState(false);
  const [patchBusy, setPatchBusy] = useState(false);

  const invalidateAll = useCallback(async () => {
    await queryClient.invalidateQueries({ queryKey: ['api', 'admin', 'compliance-exemptions'] });
  }, [queryClient]);

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

  const handleDelete = useCallback(async () => {
    if (!row) return;
    try {
      await complianceApi.deleteExemptionGrant(row.id);
      addToast(t('pages:compliance.exemptions.deleteSuccess', 'Exemption deleted'), 'success');
      setDeleteOpen(false);
      await invalidateAll();
      navigate('/compliance/exemptions');
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'unknown error';
      addToast(t('pages:compliance.exemptions.deleteError', { error: msg }), 'error');
    }
  }, [row, addToast, t, invalidateAll, navigate]);

  const handleApprove = useCallback(async () => {
    if (!row) return;
    setApproveBusy(true);
    try {
      await complianceApi.approveExemption(row.id);
      addToast(t('pages:compliance.exemptions.approveSuccess'), 'success');
      setApproveOpen(false);
      await invalidateAll();
      navigate('/compliance/exemptions');
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'unknown error';
      addToast(t('pages:compliance.exemptions.approveError', { error: msg }), 'error');
    } finally {
      setApproveBusy(false);
    }
  }, [row, addToast, t, invalidateAll, navigate]);

  const handleReject = useCallback(async () => {
    if (!row) return;
    const reason = rejectNote.trim();
    if (reason.length < 2) {
      addToast(t('pages:compliance.exemptions.validation.rejectReasonRequired'), 'error');
      return;
    }
    setRejectBusy(true);
    try {
      await complianceApi.rejectExemption(row.id, reason);
      addToast(t('pages:compliance.exemptions.rejectSuccess'), 'success');
      setRejectOpen(false);
      setRejectNote('');
      await invalidateAll();
      navigate('/compliance/exemptions');
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'unknown error';
      addToast(t('pages:compliance.exemptions.rejectError', { error: msg }), 'error');
    } finally {
      setRejectBusy(false);
    }
  }, [row, rejectNote, addToast, t, invalidateAll, navigate]);

  const handleToggleInactive = useCallback(async () => {
    if (!row || row.kind !== 'grant' || row.inactive === null) return;
    setPatchBusy(true);
    try {
      await complianceApi.patchExemptionGrant(row.id, { inactive: !row.inactive });
      addToast(t('pages:compliance.exemptions.toggleSuccess'), 'success');
      await invalidateAll();
      void refetch();
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'unknown error';
      addToast(t('pages:compliance.exemptions.toggleError', { error: msg }), 'error');
    } finally {
      setPatchBusy(false);
    }
  }, [row, addToast, t, invalidateAll, refetch]);

  if (loading) return <Skeleton.DetailPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!row) return null;

  const title = `${row.sourceIp} \u2192 ${row.targetHost}`;

  return (
    <Stack gap="lg">
      <section className={styles.detailHeader}>
        <div className={styles.headerTitleRow}>
          <Link to="/compliance/exemptions" className={styles.backLink} aria-label={t('common:back', 'Back')}>
            <svg className={styles.backIcon} width="20" height="20" viewBox="0 0 20 20" fill="none" aria-hidden="true">
              <path d="M8.33333 5L3.33333 10L8.33333 15" stroke="currentColor" strokeWidth="1.66667" strokeLinecap="round" strokeLinejoin="round" />
              <path d="M4.16667 10H13.3333C15.1743 10 16.6667 11.4924 16.6667 13.3333V15" stroke="currentColor" strokeWidth="1.66667" strokeLinecap="round" strokeLinejoin="round" />
            </svg>
          </Link>
          <div className={styles.headerTextBlock}>
            <h1 className={styles.detailTitle}>{title}</h1>
            <p className={styles.detailSubtitle}>{t('pages:compliance.exemptions.detailSubtitle')}</p>
          </div>
          <div className={styles.detailToolbar}>
            {row.kind === 'pending' ? (
              <>
                <Button variant="primary" onClick={() => setApproveOpen(true)}>
                  {t('pages:compliance.exemptions.approveBtn')}
                </Button>
                <Button
                  variant="primary"
                  onClick={() => {
                    setRejectNote('');
                    setRejectOpen(true);
                  }}
                >
                  {t('pages:compliance.exemptions.rejectBtn')}
                </Button>
              </>
            ) : (
              <>
                <Button variant="primary" disabled={patchBusy} onClick={() => void handleToggleInactive()}>
                  {row.inactive
                    ? t('pages:compliance.exemptions.enableBtn')
                    : t('pages:compliance.exemptions.disableBtn')}
                </Button>
                {row.activatedAt === null ? (
                  <Button variant="primary" onClick={() => setDeleteOpen(true)}>
                    {t('common:delete', 'Delete')}
                  </Button>
                ) : null}
              </>
            )}
          </div>
        </div>
      </section>

      <Card>
        <div className={styles.detailGrid}>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:compliance.exemptions.colId')}</div>
            <div className={iamStyles.kvValue}>
              <code>{row.id}</code>
            </div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:compliance.exemptions.colStatus')}</div>
            <div className={iamStyles.kvValue}>
              <Stack direction="horizontal" gap="sm" align="center">
                <Badge variant={STATUS_BADGE_VARIANT[row.status]}>
                  {t(`pages:compliance.exemptions.status${row.status.charAt(0).toUpperCase()}${row.status.slice(1)}`)}
                </Badge>
                {row.kind === 'grant' && row.inactive ? (
                  <Badge variant="warning">{t('pages:compliance.exemptions.statusDisabled')}</Badge>
                ) : null}
              </Stack>
            </div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:compliance.exemptions.sourceIpLabel', 'Source IP')}</div>
            <div className={iamStyles.kvValue}>
              <code>{row.sourceIp}</code>
            </div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:compliance.exemptions.targetHostLabel', 'Target host')}</div>
            <div className={iamStyles.kvValue}>
              <code>{row.targetHost}</code>
            </div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:compliance.exemptions.colEffectiveFrom')}</div>
            <div className={iamStyles.kvValue}>{formatTs(row.effectiveFrom)}</div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:compliance.exemptions.expiresLabel', 'Expires')}</div>
            <div className={iamStyles.kvValue}>
              <span title={row.expiresAt ? new Date(row.expiresAt).toLocaleString() : ''}>
                {relativeTime(row.expiresAt)}
              </span>
            </div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:compliance.exemptions.colDurationMinutes')}</div>
            <div className={iamStyles.kvValue}>{row.durationMinutes}</div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:compliance.exemptions.reasonLabel', 'Reason')}</div>
            <div className={iamStyles.kvValue}>{row.reason || EM_DASH}</div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:compliance.exemptions.colRequestedBy')}</div>
            <div className={iamStyles.kvValue}>{row.requestedBy ?? EM_DASH}</div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:compliance.exemptions.approvedByLabel', 'Approved by')}</div>
            <div className={iamStyles.kvValue}>{row.approvedBy ?? EM_DASH}</div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:compliance.exemptions.colCreatedAt')}</div>
            <div className={iamStyles.kvValue}>{formatTs(row.createdAt)}</div>
          </div>
          {row.transactionId ? (
            <div>
              <div className={iamStyles.kvLabel}>{t('pages:compliance.exemptions.transactionIdLabel')}</div>
              <div className={iamStyles.kvValue}>
                <code>{row.transactionId}</code>
              </div>
            </div>
          ) : null}
        </div>
      </Card>

      <AlertDialog
        open={deleteOpen}
        onOpenChange={setDeleteOpen}
        title={t('pages:compliance.exemptions.deleteTitle', 'Delete exemption?')}
        description={
          t('pages:compliance.exemptions.deleteDesc', {
            defaultValue:
              'Remove exemption for {{sourceIp}} \u2192 {{targetHost}}? Compliance hooks will apply to matching traffic immediately.',
            sourceIp: row.sourceIp,
            targetHost: row.targetHost,
          })
        }
        confirmLabel={t('common:delete', 'Delete')}
        cancelLabel={t('common:cancel', 'Cancel')}
        onConfirm={() => void handleDelete()}
        variant="danger"
      />

      <AlertDialog
        open={approveOpen}
        onOpenChange={setApproveOpen}
        title={t('pages:compliance.exemptions.approveTitle')}
        description={t('pages:compliance.exemptions.approveDesc', {
          sourceIp: row.sourceIp,
          targetHost: row.targetHost,
        })}
        confirmLabel={t('pages:compliance.exemptions.approveConfirm')}
        cancelLabel={t('common:cancel', 'Cancel')}
        onConfirm={() => void handleApprove()}
        loading={approveBusy}
        variant="default"
      />

      <Dialog
        open={rejectOpen}
        onOpenChange={(open) => {
          setRejectOpen(open);
          if (!open) setRejectNote('');
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
                setRejectOpen(false);
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
    </Stack>
  );
}

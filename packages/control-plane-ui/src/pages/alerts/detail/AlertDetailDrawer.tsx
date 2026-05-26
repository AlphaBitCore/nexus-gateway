/**
 * AlertDetailDrawer — side-sheet that opens from AlertListPage when the user
 * clicks a row. Mirrors the DeviceDetailDrawer pattern: overlay + fixed-right
 * drawer, Escape closes, focus lands on the close button.
 *
 * Renders: header (rule + severity + state chips), timestamp metadata,
 * a per-rule details block (dispatched via the `detailRenderers/` registry
 * so each rule gets a tailored view — usage bar for quota.threshold,
 * expiring-keys row for quota.vk_expiring, failure/sample counts for the
 * proxy rate rules, and a generic JSON fallback for anything else), a
 * dispatch history table, and Ack / Resolve action buttons.
 *
 * Ack/Resolve call `refetch()` on the drawer's own detail query and also
 * notify the parent via `onMutated` so the inbox list re-queries.
 */
import { useEffect, useRef } from 'react';
import { useTranslation } from 'react-i18next';
import clsx from 'clsx';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { alertsApi } from '@/api/services';
import type {
  Alert,
  AlertDetailResponse,
  AlertSeverity,
  AlertState,
} from '@/api/services';
import { Badge, Button, Stack, Skeleton } from '@/components/ui';
import type { BadgeProps } from '@/components/ui';
import { getDetailRenderer } from '../detailRenderers';
import styles from './AlertDetailDrawer.module.css';

export const DRAWER_MS = 240;

function severityVariant(s: AlertSeverity): BadgeProps['variant'] {
  switch (s) {
    case 'critical':
    case 'high':
      return 'danger';
    case 'medium':
      return 'warning';
    case 'low':
      return 'info';
    default:
      return 'default';
  }
}

function stateVariant(s: AlertState): BadgeProps['variant'] {
  switch (s) {
    case 'firing':
      return 'danger';
    case 'acknowledged':
      return 'warning';
    case 'resolved':
      return 'success';
    default:
      return 'default';
  }
}

function fmt(ts?: string | null): string {
  if (!ts) return '—';
  return new Date(ts).toLocaleString();
}

export interface AlertDetailDrawerProps {
  alertId: string | null;
  visible: boolean;
  onClose: () => void;
  /** Called after Ack/Resolve succeeds so the parent list can refetch. */
  onMutated?: () => void;
}

export function AlertDetailDrawer({
  alertId,
  visible,
  onClose,
  onMutated,
}: AlertDetailDrawerProps) {
  const { t } = useTranslation();
  const closeBtnRef = useRef<HTMLButtonElement>(null);

  const { data, loading, error, refetch } = useApi<AlertDetailResponse>(
    () => alertsApi.detail(alertId!),
    ['admin', 'alerts', 'detail', alertId ?? ''],
    { skip: !alertId || !visible },
  );

  const { mutate: ackAlert, loading: ackLoading } = useMutation<
    string,
    Alert
  >((id) => alertsApi.ack(id), {
    onSuccess: () => {
      refetch();
      onMutated?.();
    },
    successMessage: t('pages:alerts.drawer.ackSuccess'),
  });

  const { mutate: resolveAlert, loading: resolveLoading } = useMutation<
    string,
    Alert
  >((id) => alertsApi.resolve(id), {
    onSuccess: () => {
      refetch();
      onMutated?.();
    },
    successMessage: t('pages:alerts.drawer.resolveSuccess'),
  });

  // Escape to close + autofocus close button when drawer opens.
  useEffect(() => {
    if (!visible) return;
    const handler = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose();
    };
    document.addEventListener('keydown', handler);
    closeBtnRef.current?.focus();
    return () => document.removeEventListener('keydown', handler);
  }, [visible, onClose]);

  if (!alertId) return null;

  const title = data?.ruleId ?? t('pages:alerts.drawer.loading');

  return (
    <>
      <div
        className={clsx(styles.overlay, visible && styles.overlayVisible)}
        onClick={onClose}
        aria-hidden
      />
      <div
        className={clsx(styles.drawer, visible && styles.drawerVisible)}
        role="dialog"
        aria-modal="true"
        aria-label={title}
      >
        <div className={styles.header}>
          <h2>{title}</h2>
          <button
            ref={closeBtnRef}
            className={styles.closeBtn}
            onClick={onClose}
            aria-label={t('pages:alerts.drawer.close')}
          >
            &times;
          </button>
        </div>
        <div className={styles.content}>
          {loading && !data ? (
            <Skeleton.Card lines={6} />
          ) : error ? (
            <div className={styles.errorText}>{error.message}</div>
          ) : !data ? null : (
            <Stack gap="md">
              <div className={styles.chipRow}>
                <Badge variant={severityVariant(data.severity)}>
                  {t(`pages:alerts.inbox.severities.${data.severity}`)}
                </Badge>
                <Badge variant={stateVariant(data.state)}>
                  {t(`pages:alerts.inbox.states.${data.state}`)}
                </Badge>
                <Badge variant="outline">{data.sourceType}</Badge>
              </div>

              {data.message && <p className={styles.message}>{data.message}</p>}

              <div className={styles.metaRow}>
                <span className={styles.metaLabel}>
                  {t('pages:alerts.drawer.target')}
                </span>
                <span className={styles.metaValue}>
                  {data.targetLabel || data.targetKey}
                </span>
              </div>
              <div className={styles.metaRow}>
                <span className={styles.metaLabel}>
                  {t('pages:alerts.drawer.firedAt')}
                </span>
                <span className={styles.metaValue}>{fmt(data.firedAt)}</span>
              </div>
              <div className={styles.metaRow}>
                <span className={styles.metaLabel}>
                  {t('pages:alerts.drawer.lastSeenAt')}
                </span>
                <span className={styles.metaValue}>{fmt(data.lastSeenAt)}</span>
              </div>
              <div className={styles.metaRow}>
                <span className={styles.metaLabel}>
                  {t('pages:alerts.drawer.duplicateCount')}
                </span>
                <span className={styles.metaValue}>{data.duplicateCount}</span>
              </div>
              {data.acknowledgedAt && (
                <div className={styles.metaRow}>
                  <span className={styles.metaLabel}>
                    {t('pages:alerts.drawer.acknowledgedAt')}
                  </span>
                  <span className={styles.metaValue}>
                    {fmt(data.acknowledgedAt)}
                    {data.acknowledgedBy ? ` — ${data.acknowledgedBy}` : ''}
                  </span>
                </div>
              )}
              {data.resolvedAt && (
                <div className={styles.metaRow}>
                  <span className={styles.metaLabel}>
                    {t('pages:alerts.drawer.resolvedAt')}
                  </span>
                  <span className={styles.metaValue}>
                    {fmt(data.resolvedAt)}
                    {data.resolvedBy ? ` — ${data.resolvedBy}` : ''}
                    {data.resolvedReason ? ` (${data.resolvedReason})` : ''}
                  </span>
                </div>
              )}

              {(() => {
                const Renderer = getDetailRenderer(data.ruleId);
                return (
                  <>
                    <h3 className={styles.sectionHeading}>
                      {t('pages:alerts.drawer.details')}
                    </h3>
                    <Renderer alert={data} />
                  </>
                );
              })()}

              <h3 className={styles.sectionHeading}>
                {t('pages:alerts.drawer.dispatchHistory')}
              </h3>
              <table className={styles.dispatchTable}>
                <thead>
                  <tr>
                    <th>{t('pages:alerts.drawer.dispatchTime')}</th>
                    <th>{t('pages:alerts.drawer.dispatchChannel')}</th>
                    <th>{t('pages:alerts.drawer.dispatchStatus')}</th>
                    <th>{t('pages:alerts.drawer.dispatchError')}</th>
                  </tr>
                </thead>
                <tbody>
                  {data.dispatches.length === 0 ? (
                    <tr>
                      <td colSpan={4} className={styles.noData}>
                        {t('pages:alerts.drawer.noDispatches')}
                      </td>
                    </tr>
                  ) : (
                    data.dispatches.map((d) => (
                      <tr key={d.id}>
                        <td>{fmt(d.attemptedAt)}</td>
                        <td>{d.channelName}</td>
                        <td>
                          <Badge variant={d.success ? 'success' : 'danger'}>
                            {d.success
                              ? t('pages:alerts.drawer.dispatchOk')
                              : t('pages:alerts.drawer.dispatchFailed')}
                            {d.statusCode != null ? ` (${d.statusCode})` : ''}
                          </Badge>
                        </td>
                        <td className={styles.errorCell} title={d.errorMsg ?? ''}>
                          {d.errorMsg ?? '—'}
                        </td>
                      </tr>
                    ))
                  )}
                </tbody>
              </table>

              <Stack direction="horizontal" gap="sm">
                {data.state === 'firing' && (
                  <Button
                    variant="secondary"
                    loading={ackLoading}
                    onClick={() => ackAlert(data.id)}
                  >
                    {t('pages:alerts.drawer.ack')}
                  </Button>
                )}
                {data.state !== 'resolved' && (
                  <Button
                    variant="primary"
                    loading={resolveLoading}
                    onClick={() => resolveAlert(data.id)}
                  >
                    {t('pages:alerts.drawer.resolve')}
                  </Button>
                )}
              </Stack>
            </Stack>
          )}
        </div>
      </div>
    </>
  );
}

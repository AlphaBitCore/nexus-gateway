import type { useTranslation } from 'react-i18next';
import type { AgentAuditEvent, FleetAuditEvent, DeviceAssignmentDetail } from '@/api/types';
import type { DataTableColumn } from '@/components/ui';
import { Badge } from '@/components/ui';
import styles from '../FleetDeviceDetailPage.module.css';

type TFunc = ReturnType<typeof useTranslation>['t'];

export const hookDecisionVariant = (d?: string | null) => {
  if (!d) return 'default' as const;
  const v = d.toLowerCase();
  if (v === 'allow') return 'success' as const;
  if (v === 'deny' || v === 'block' || v === 'block-hard') return 'danger' as const;
  if (v === 'redact' || v === 'modify') return 'warning' as const;
  return 'default' as const;
};

export const buildTrafficColumns = (t: TFunc): DataTableColumn<AgentAuditEvent>[] => [
  { key: 'timestamp', label: t('pages:fleet.timestamp'), render: (r) => new Date(r.timestamp).toLocaleString() },
  { key: 'sourceProcess', label: t('pages:fleet.source'), render: (r) => r.sourceProcess || '—' },
  { key: 'destHost', label: t('pages:fleet.targetHost'), render: (r) => r.destHost ?? '—' },
  { key: 'action', label: t('common:action'), render: (r) => <Badge variant="outline">{r.action || '—'}</Badge> },
  {
    key: 'requestHookDecision',
    label: t('pages:fleet.hookDecision'),
    render: (r) => r.requestHookDecision
      ? <Badge variant={hookDecisionVariant(r.requestHookDecision)}>{r.requestHookDecision}</Badge>
      : <span className={styles.dim}>—</span>,
  },
  {
    key: 'duration',
    label: t('common:duration'),
    render: (r) => r.duration != null ? `${r.duration}ms` : <span className={styles.dim}>—</span>,
  },
];

export const buildTimelineColumns = (t: TFunc): DataTableColumn<DeviceAssignmentDetail>[] => [
  { key: 'userDisplayName', label: t('pages:fleet.displayName'), render: (r) => r.userDisplayName ?? r.userId },
  { key: 'userOsUsername', label: t('pages:fleet.osUsername'), render: (r) => r.userOsUsername ?? '—' },
  { key: 'source', label: t('pages:fleet.source') },
  { key: 'assignedAt', label: t('pages:fleet.assignedAt'), render: (r) => new Date(r.assignedAt).toLocaleString() },
  {
    key: 'releasedAt',
    label: t('pages:fleet.releasedAt'),
    render: (r) => r.releasedAt
      ? new Date(r.releasedAt).toLocaleString()
      : <Badge variant="success">{t('pages:fleet.currentAssignment')}</Badge>,
  },
];

export const buildAdminAuditColumns = (t: TFunc): DataTableColumn<FleetAuditEvent>[] => [
  { key: 'timestamp', label: t('pages:fleet.timestamp'), render: (r) => new Date(r.timestamp).toLocaleString() },
  { key: 'source', label: t('pages:fleet.source') },
  { key: 'targetHost', label: t('pages:fleet.targetHost'), render: (r) => r.targetHost ?? '—' },
  { key: 'requestHookDecision', label: t('pages:fleet.requestHookDecision'), render: (r) => r.requestHookDecision ?? '—' },
];

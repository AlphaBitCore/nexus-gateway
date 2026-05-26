/**
 * NodeLoginHistoryTab — device assignment login history for a Thing/Node.
 * Calls GET /api/admin/nodes/:id/device-assignments.
 *
 * Wire shape comes from `store.DeviceAssignmentDetail` — same struct the
 * /devices Activity tab consumes, so field names here mirror that page
 * (assignedAt / releasedAt / userDisplayName / userOsUsername) for cross-
 * page consistency. Earlier this file declared loginAt / logoutAt /
 * userEmail which don't exist on the wire — every column rendered "—"
 * regardless of the underlying data.
 */
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { api } from '@/api/client';
import { DataTable, Badge, ErrorBanner, Skeleton } from '@/components/ui';
import type { DataTableColumn } from '@/components/ui';
import { formatDateTime } from '@/lib/format';

interface NodeAssignmentRow {
  id: string;
  userId: string;
  userDisplayName: string | null;
  userOsUsername: string | null;
  assignedAt: string;
  releasedAt: string | null;
  source: string;
  loginMethod: string | null;
  ipAddress: string | null;
}

interface Props {
  nodeId: string;
}

export function NodeLoginHistoryTab({ nodeId }: Props) {
  const { t } = useTranslation();

  const { data, loading, error, refetch } = useApi<{ data: NodeAssignmentRow[]; total: number }>(
    () =>
      api
        .get<{ data: NodeAssignmentRow[]; total: number }>(
          `/api/admin/nodes/${nodeId}/device-assignments`,
        )
        .catch(() => ({ data: [], total: 0 })),
    ['admin', 'nodes', 'login-history', nodeId],
  );

  const columns: DataTableColumn<NodeAssignmentRow>[] = [
    {
      key: 'user',
      label: t('pages:nodeDetail.loginHistory.user'),
      render: (r) => r.userDisplayName ?? r.userOsUsername ?? r.userId,
    },
    {
      key: 'loginAt',
      label: t('pages:nodeDetail.loginHistory.loginAt'),
      render: (r) => formatDateTime(r.assignedAt),
    },
    {
      key: 'logoutAt',
      label: t('pages:nodeDetail.loginHistory.logoutAt'),
      render: (r) =>
        r.releasedAt ? (
          formatDateTime(r.releasedAt)
        ) : (
          <Badge variant="success">{t('pages:nodeDetail.loginHistory.active')}</Badge>
        ),
    },
    {
      key: 'loginMethod',
      // Falls back to the assignment source ("login" / "heartbeat" /
      // "enrollment" / "manual") when no explicit loginMethod is set —
      // the manual + heartbeat paths don't write loginMethod but the
      // source is still informative enough to render instead of "—".
      label: t('pages:nodeDetail.loginHistory.loginMethod'),
      render: (r) => r.loginMethod ?? r.source ?? '—',
    },
    {
      key: 'ipAddress',
      label: t('pages:nodeDetail.loginHistory.ipAddress'),
      render: (r) => r.ipAddress ?? '—',
    },
  ];

  if (loading) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  return (
    <DataTable
      hideSearch
      columns={columns}
      data={data?.data ?? []}
      emptyMessage={t('pages:nodeDetail.loginHistory.noHistory')}
    />
  );
}

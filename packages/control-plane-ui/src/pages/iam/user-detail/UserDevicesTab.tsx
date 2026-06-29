/**
 * UserDevicesTab — shows device assignment history for a Nexus user.
 * Calls GET /api/admin/users/:id/device-assignments.
 * Backend not yet shipped → falls back to an empty-state with a note.
 */
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { api } from '@/api/client';
import { DataTable, Badge, ErrorBanner, Skeleton } from '@/components/ui';
import type { DataTableColumn } from '@/components/ui';
import { formatDateTime } from '@/lib/format';

interface DeviceAssignmentRow {
  id: string;
  deviceId: string;
  deviceName: string | null;
  hostname: string | null;
  assignedAt: string;
  releasedAt: string | null;
  loginMethod: string | null;
  ipAddress: string | null;
  isCurrent: boolean;
}

interface Props {
  userId: string;
}

export function UserDevicesTab({ userId }: Props) {
  const { t } = useTranslation();

  const { data, loading, error, refetch } = useApi<{ data: DeviceAssignmentRow[]; total: number }>(
    () =>
      api
        .get<{ data: DeviceAssignmentRow[]; total: number }>(
          `/api/admin/users/${userId}/device-assignments`,
        )
        .catch(() => ({ data: [], total: 0 })),
    ['admin', 'iam', 'users', 'device-assignments', userId],
  );

  const columns: DataTableColumn<DeviceAssignmentRow>[] = [
    {
      key: 'hostname',
      label: t('pages:userDetail.devices.hostname'),
      render: (r) => r.hostname ?? r.deviceName ?? r.deviceId,
    },
    {
      key: 'assignedAt',
      label: t('pages:userDetail.devices.loginAt'),
      render: (r) => formatDateTime(r.assignedAt),
    },
    {
      key: 'releasedAt',
      label: t('pages:userDetail.devices.logoutAt'),
      render: (r) =>
        r.releasedAt ? (
          formatDateTime(r.releasedAt)
        ) : (
          <Badge variant="success">{t('pages:userDetail.devices.active')}</Badge>
        ),
    },
    {
      key: 'loginMethod',
      label: t('pages:userDetail.devices.loginMethod'),
      render: (r) => r.loginMethod ?? '—',
    },
    {
      key: 'ipAddress',
      label: t('pages:userDetail.devices.ipAddress'),
      render: (r) => r.ipAddress ?? '—',
    },
  ];

  if (loading) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const rows = data?.data ?? [];

  return (
    <DataTable
      hideSearch
      columns={columns}
      data={rows}
      emptyMessage={t('pages:userDetail.devices.noDevices')}
    />
  );
}

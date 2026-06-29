import { useTranslation } from 'react-i18next';
import type { DeviceAssignmentDetail, FleetAuditEvent } from '@/api/types';
import type { AdminListPageSize } from '@/components/ui';
import { Stack, DataTable, ListPagination } from '@/components/ui';
import { buildTimelineColumns, buildAdminAuditColumns } from './columns';
import styles from '../FleetDeviceDetailPage.module.css';

interface ActivityTabProps {
  timelineRows: DeviceAssignmentDetail[];
  auditRows: FleetAuditEvent[];
  auditTotal: number;
  auditOffset: number;
  auditLimit: AdminListPageSize;
  onAuditOffsetChange: (offset: number) => void;
  onAuditLimitChange: (limit: AdminListPageSize) => void;
}

export function ActivityTab({
  timelineRows,
  auditRows,
  auditTotal,
  auditOffset,
  auditLimit,
  onAuditOffsetChange,
  onAuditLimitChange,
}: ActivityTabProps) {
  const { t } = useTranslation();
  const timelineColumns = buildTimelineColumns(t);
  const adminAuditColumns = buildAdminAuditColumns(t);
  return (
    <Stack gap="md">
      <Stack gap="sm">
        <h4 className={styles.sectionTitle}>{t('pages:fleet.activityAssignments')}</h4>
        <DataTable columns={timelineColumns} data={timelineRows} hideSearch />
      </Stack>
      <Stack gap="sm">
        <h4 className={styles.sectionTitle}>{t('pages:fleet.activityAdminActions')}</h4>
        <DataTable columns={adminAuditColumns} data={auditRows} hideSearch />
        <ListPagination
          total={auditTotal}
          offset={auditOffset}
          limit={auditLimit}
          onOffsetChange={onAuditOffsetChange}
          onLimitChange={onAuditLimitChange}
        />
      </Stack>
    </Stack>
  );
}

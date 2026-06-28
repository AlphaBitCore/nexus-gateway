import { useTranslation } from 'react-i18next';
import type { AgentAuditEvent } from '@/api/types';
import type { AdminListPageSize } from '@/components/ui';
import { Stack, DataTable, ListPagination } from '@/components/ui';
import { ThingStatsTab } from '@/pages/infrastructure/_shared/tabs/metrics/ThingStatsTab';
import { buildTrafficColumns } from './columns';
import styles from '../FleetDeviceDetailPage.module.css';

interface TrafficTabProps {
  id: string;
  canViewStats: boolean;
  allEvents: AgentAuditEvent[];
  eventsTotal: number;
  eventsOffset: number;
  eventsLimit: AdminListPageSize;
  onOffsetChange: (offset: number) => void;
  onLimitChange: (limit: AdminListPageSize) => void;
}

export function TrafficTab({
  id,
  canViewStats,
  allEvents,
  eventsTotal,
  eventsOffset,
  eventsLimit,
  onOffsetChange,
  onLimitChange,
}: TrafficTabProps) {
  const { t } = useTranslation();
  const trafficColumns = buildTrafficColumns(t);
  return (
    <Stack gap="md">
      {canViewStats && (
        <div className={styles.statsSection}>
          <ThingStatsTab thingId={id} thingType="agent" />
        </div>
      )}
      <Stack gap="sm" className={styles.trafficEventsSection}>
        <h4 className={styles.sectionTitle}>{t('pages:fleet.trafficEventsTitle')}</h4>
        <p className={styles.trafficEventsSubtitle}>{t('pages:fleet.trafficEventsSubtitle')}</p>
        <DataTable columns={trafficColumns} data={allEvents} hideSearch />
        <ListPagination
          total={eventsTotal}
          offset={eventsOffset}
          limit={eventsLimit}
          onOffsetChange={onOffsetChange}
          onLimitChange={onLimitChange}
        />
      </Stack>
    </Stack>
  );
}

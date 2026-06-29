import { useTranslation } from 'react-i18next';
import type { AgentAuditEvent } from '@/api/types';
import { Stack, DataTable } from '@/components/ui';
import { buildTrafficColumns } from './columns';
import styles from '../FleetDeviceDetailPage.module.css';

interface ComplianceTabProps {
  complianceEvents: AgentAuditEvent[];
}

export function ComplianceTab({ complianceEvents }: ComplianceTabProps) {
  const { t } = useTranslation();
  const trafficColumns = buildTrafficColumns(t);
  return (
    <Stack gap="sm">
      <h4 className={styles.sectionTitle}>{t('pages:fleet.complianceEventsTitle')}</h4>
      {complianceEvents.length === 0 ? (
        <p className={styles.empty}>{t('pages:fleet.complianceEventsEmpty')}</p>
      ) : (
        <DataTable columns={trafficColumns} data={complianceEvents} hideSearch />
      )}
    </Stack>
  );
}

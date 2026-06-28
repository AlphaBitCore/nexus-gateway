import { useTranslation } from 'react-i18next';
import { DataTable, Card, Stack } from '@/components/ui';
import { LatencyMini } from '@/components/charts/LatencyMini';
import type { TrafficEvent } from '@/api/types';
import { formatDateTime } from '@/lib/format';
import styles from '../VirtualKeyDetail.module.css';

export interface VirtualKeyAccessLogTabProps {
  auditLogs: TrafficEvent[];
}

export function VirtualKeyAccessLogTab({ auditLogs }: VirtualKeyAccessLogTabProps) {
  const { t } = useTranslation();

  return (
    <Stack gap="md">
      <h2 className={styles.widgetTitle}>{t('pages:virtualKeys.recentAccessLogs')}</h2>
      <Card padding="none">
        <DataTable hideSearch
          frameless
          columns={[
            { key: 'timestamp', label: t('pages:virtualKeys.colTime'), render: r => formatDateTime(r.timestamp) },
            { key: 'method', label: t('pages:virtualKeys.colMethod') },
            { key: 'path', label: t('pages:virtualKeys.colPath') },
            { key: 'statusCode', label: t('pages:virtualKeys.colStatus'), render: r => r.statusCode != null ? String(r.statusCode) : '--' },
            {
              key: 'latencyMs',
              label: t('pages:virtualKeys.colLatency'),
              render: r => (
                <LatencyMini
                  size="row"
                  latencyMs={r.latencyMs}
                  upstreamTtfbMs={r.upstreamTtfbMs}
                  upstreamTotalMs={r.upstreamTotalMs}
                  requestHooksMs={r.requestHooksMs}
                  responseHooksMs={r.responseHooksMs}
                />
              ),
            },
            { key: 'modelName', label: t('pages:virtualKeys.colModel'), render: r => r.routedModelName ?? r.modelName ?? '--' },
          ]}
          data={auditLogs}
          emptyMessage={t('pages:virtualKeys.noRecentAccessLogs')}
        />
      </Card>
    </Stack>
  );
}

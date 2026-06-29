import { useTranslation } from 'react-i18next';
import { Button, Select } from '@/components/ui';
import { ALL, NODE_TYPE_OPTIONS } from './recentErrorsHelpers';
import styles from './InfraRecentErrorsPage.module.css';

interface FilterPanelProps {
  timeRange: string;
  setTimeRange: (v: string) => void;
  nodeType: string;
  setNodeType: (v: string) => void;
  eventType: string;
  setEventType: (v: string) => void;
  onRefresh: () => void;
}

export function FilterPanel({
  timeRange,
  setTimeRange,
  nodeType,
  setNodeType,
  eventType,
  setEventType,
  onRefresh,
}: FilterPanelProps) {
  const { t } = useTranslation('pages');

  return (
    <div className={styles.filterBar}>
      <div className={styles.filterField}>
        <Select
          value={timeRange}
          onValueChange={setTimeRange}
          options={[
            { value: '1h', label: t('infrastructure.recentErrors.range1h') },
            { value: '24h', label: t('infrastructure.recentErrors.range24h') },
            { value: '7d', label: t('infrastructure.recentErrors.range7d') },
            { value: '30d', label: t('infrastructure.recentErrors.range30d') },
          ]}
        />
      </div>
      <div className={styles.filterField}>
        <Select
          value={nodeType || ALL}
          onValueChange={(v) => setNodeType(v === ALL ? '' : v)}
          options={[
            { value: ALL, label: t('infrastructure.recentErrors.allNodeType', 'All Node type') },
            ...NODE_TYPE_OPTIONS.map((s) => ({ value: s, label: s })),
          ]}
        />
      </div>
      <div className={styles.filterField}>
        <Select
          value={eventType || ALL}
          onValueChange={(v) => setEventType(v === ALL ? '' : v)}
          options={[
            { value: ALL, label: t('infrastructure.recentErrors.allEventType', 'All Event type') },
            { value: 'error', label: t('infrastructure.recentErrors.eventTypeError') },
            { value: 'crash', label: t('infrastructure.recentErrors.eventTypeCrash') },
            { value: 'lifecycle', label: t('infrastructure.recentErrors.eventTypeLifecycle') },
            { value: 'watchdog', label: t('infrastructure.recentErrors.eventTypeWatchdog') },
          ]}
        />
      </div>
      <Button type="button" variant="secondary" size="sm" onClick={onRefresh}>
        {t('common:refresh')}
      </Button>
    </div>
  );
}

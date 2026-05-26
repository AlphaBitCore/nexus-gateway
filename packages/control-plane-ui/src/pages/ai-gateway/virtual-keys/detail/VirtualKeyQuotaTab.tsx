import { useTranslation } from 'react-i18next';
import { Tooltip, Stack, Card } from '@/components/ui';
import type { VirtualKey } from '@/api/types';
import styles from '../VirtualKeyDetail.module.css';

export interface VirtualKeyQuotaTabProps {
  vk: VirtualKey;
}

export function VirtualKeyQuotaTab({ vk }: VirtualKeyQuotaTabProps) {
  const { t } = useTranslation();

  return (
    <Stack gap="md">
      <Card>
        <Stack direction="horizontal" gap="xs" align="center" className={styles.sectionTitleRow}>
          <h2 className={`${styles.widgetTitle} ${styles.sectionTitleNoMargin}`}>{t('pages:virtualKeys.rateLimits')}</h2>
          <Tooltip content={t('pages:virtualKeys.rateLimitsTooltip')}>
            <span className={styles.helpIcon}>?</span>
          </Tooltip>
        </Stack>
        <div className={styles.kvGrid}>
          <div>
            <Stack direction="horizontal" gap="xs" align="center" className={styles.kvLabelRow}>
              <span className={styles.kvLabel}>{t('pages:virtualKeys.rateLimitRpm')}</span>
              <Tooltip content={t('pages:virtualKeys.rateLimitRpmTooltip')}>
                <span className={styles.helpIcon}>?</span>
              </Tooltip>
            </Stack>
            <div className={styles.statValue}>
              {vk.rateLimitRpm != null ? vk.rateLimitRpm.toLocaleString() : '--'}
            </div>
          </div>
        </div>
      </Card>
    </Stack>
  );
}

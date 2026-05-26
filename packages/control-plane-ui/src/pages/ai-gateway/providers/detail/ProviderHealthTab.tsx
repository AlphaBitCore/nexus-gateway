import clsx from 'clsx';
import { useTranslation } from 'react-i18next';
import { Badge, statusToVariant, Card } from '@/components/ui';
import { LatencyMini } from '@/components/charts/LatencyMini';
import type { ProviderDetailState } from './useProviderDetail';
import styles from './ProviderDetail.module.css';

interface ProviderHealthTabProps {
  detail: ProviderDetailState;
}

export function ProviderHealthTab({ detail }: ProviderHealthTabProps) {
  const { t } = useTranslation();
  const { healthData } = detail;

  return (
    <Card>
      {healthData ? (
        <>
        <div style={{ marginBottom: 'var(--g-space-3)', fontSize: 'var(--g-font-size-xs)', color: 'var(--color-text-muted)' }}>
          {t('pages:providers.cacheHealthWindowNote')}
        </div>
        <div className={styles.healthGrid}>
          <div className={styles.healthMetric}>
            <div className={styles.mb8}><Badge variant={statusToVariant(healthData.status)}>{healthData.status}</Badge></div>
            <div className={styles.healthLabel}>{t('pages:providers.healthStatus')}</div>
          </div>
          <div className={styles.healthMetric}>
            <div className={clsx(styles.healthValue, (healthData.errorRate ?? 0) > 0.05 ? styles.colorDanger : styles.colorSuccess)}>
              {((healthData.errorRate ?? 0) * 100).toFixed(1)}%
            </div>
            <div className={styles.healthLabel}>{t('pages:providers.healthErrorRate')}</div>
          </div>
          <div className={styles.healthMetric}>
            {/* Phase-aware. healthData carries avgUpstreamTotalMs +
                avgUpstreamTtfbMs on the new API path; LatencyMini falls
                back to a single-bar render when either is null. */}
            <LatencyMini
              size="card"
              title={t('pages:providers.healthAvgLatency')}
              latencyMs={healthData.avgLatencyMs}
              upstreamTotalMs={(healthData as { avgUpstreamTotalMs?: number | null }).avgUpstreamTotalMs ?? null}
              upstreamTtfbMs={(healthData as { avgUpstreamTtfbMs?: number | null }).avgUpstreamTtfbMs ?? null}
            />
          </div>
          {healthData.sampleCount != null && (
            <div className={styles.healthMetric}>
              <div className={styles.healthValue}>{healthData.sampleCount.toLocaleString()}</div>
              <div className={styles.healthLabel}>{t('pages:providers.healthSamples')}</div>
            </div>
          )}
        </div>
        </>
      ) : (
        <div className={styles.emptyState}>{t('pages:providers.healthNotAvailable')}</div>
      )}
    </Card>
  );
}

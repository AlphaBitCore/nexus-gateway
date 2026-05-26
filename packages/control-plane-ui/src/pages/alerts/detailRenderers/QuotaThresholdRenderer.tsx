/**
 * QuotaThresholdRenderer — presents a `quota.threshold` alert's evidence.
 *
 * Expected `alert.details` shape (from
 * `packages/nexus-hub/internal/jobs/quota_alert_check.go`):
 *
 *   {
 *     pct:            number, // observed usage percent (e.g. 87.3)
 *     threshold:      number, // crossed threshold percent (e.g. 80)
 *     costLimitUsd:   number, // configured spend cap in USD
 *     currentCostUsd: number, // observed spend in USD
 *     targetType:     string, // e.g. "user" | "vk" | "organization"
 *     targetId:       string,
 *     period:         string, // "YYYY-MM"
 *     overrideId?:    string,
 *     policyId?:      string,
 *     organizationId?:string,
 *   }
 *
 * Renders a usage bar filled to `pct`, with the `threshold` marked, plus
 * `currentCostUsd` / `costLimitUsd` and the target + period metadata. Missing
 * or wrong-typed fields render as an em dash or are skipped.
 */
import { useTranslation } from 'react-i18next';
import styles from './renderer.module.css';
import type { DetailRendererProps } from './types';

const DASH = '—';

function numOrUndef(v: unknown): number | undefined {
  return typeof v === 'number' && Number.isFinite(v) ? v : undefined;
}

function strOrUndef(v: unknown): string | undefined {
  return typeof v === 'string' && v.length > 0 ? v : undefined;
}

function clampPct(n: number): number {
  if (n < 0) return 0;
  if (n > 100) return 100;
  return n;
}

function fmtPct(n: number | undefined): string {
  if (n == null) return DASH;
  return `${n.toFixed(1)}%`;
}

function fmtUsd(n: number | undefined): string {
  if (n == null) return DASH;
  return `$${n.toFixed(2)}`;
}

export function QuotaThresholdRenderer({ alert }: DetailRendererProps) {
  const { t } = useTranslation();
  const d = alert.details ?? {};

  const pct = numOrUndef(d.pct);
  const threshold = numOrUndef(d.threshold);
  const costLimit = numOrUndef(d.costLimitUsd);
  const currentCost = numOrUndef(d.currentCostUsd);
  const targetType = strOrUndef(d.targetType);
  const targetId = strOrUndef(d.targetId);
  const period = strOrUndef(d.period);

  const barWidth = pct != null ? clampPct(pct) : 0;
  const barSeverity =
    pct != null && threshold != null
      ? pct >= 95
        ? styles.barFillDanger
        : pct >= threshold
          ? styles.barFillWarning
          : ''
      : '';

  return (
    <div className={styles.statGrid} style={{ gridTemplateColumns: '1fr' }}>
      {pct != null && (
        <div>
          <div className={styles.emphasis}>
            {t('pages:alerts.detailRenderers.quotaThreshold.usage')}{' '}
            <span>{fmtPct(pct)}</span>
          </div>
          <div className={styles.bar} aria-hidden>
            <div
              className={`${styles.barFill} ${barSeverity}`}
              style={{ width: `${barWidth}%` }}
            />
            {threshold != null && (
              <div
                className={styles.barThreshold}
                style={{ left: `${clampPct(threshold)}%` }}
                title={t(
                  'pages:alerts.detailRenderers.quotaThreshold.thresholdLabel',
                  { pct: threshold },
                )}
              />
            )}
          </div>
          <div className={styles.barCaption}>
            <span>0%</span>
            {threshold != null && (
              <span>
                {t('pages:alerts.detailRenderers.quotaThreshold.thresholdLabel', {
                  pct: threshold,
                })}
              </span>
            )}
            <span>100%</span>
          </div>
        </div>
      )}

      <div className={styles.statGrid}>
        <span className={styles.statLabel}>
          {t('pages:alerts.detailRenderers.quotaThreshold.spend')}
        </span>
        <span className={styles.statValue}>
          {currentCost != null || costLimit != null
            ? t('pages:alerts.detailRenderers.quotaThreshold.spendValue', {
                current: fmtUsd(currentCost),
                limit: fmtUsd(costLimit),
              })
            : DASH}
        </span>

        <span className={styles.statLabel}>
          {t('pages:alerts.detailRenderers.quotaThreshold.target')}
        </span>
        <span className={styles.statValue}>
          {targetType && targetId
            ? `${targetType}:${targetId}`
            : (targetType ?? targetId ?? DASH)}
        </span>

        <span className={styles.statLabel}>
          {t('pages:alerts.detailRenderers.quotaThreshold.period')}
        </span>
        <span className={styles.statValue}>{period ?? DASH}</span>
      </div>
    </div>
  );
}

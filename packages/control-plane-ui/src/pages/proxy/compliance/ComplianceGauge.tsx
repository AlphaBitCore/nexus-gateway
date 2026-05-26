import styles from './ComplianceGauge.module.css';
import clsx from 'clsx';
import { useTranslation } from 'react-i18next';

interface ComplianceGaugeProps {
  percent: number;
  size?: 'sm' | 'md' | 'lg';
}

function gaugeColor(percent: number): string {
  if (percent < 70) return 'var(--color-danger)';
  if (percent < 90) return 'var(--color-warning)';
  return 'var(--color-success)';
}

export function ComplianceGauge({ percent, size = 'md' }: ComplianceGaugeProps) {
  const { t } = useTranslation();
  const clamped = Math.min(100, Math.max(0, percent));
  const color = gaugeColor(clamped);
  const pctStr = `${clamped.toFixed(1)}%`;

  return (
    <div className={clsx(styles.gauge, styles[size])}>
      <div className={styles.label}>{t('pages:proxy.status.complianceCoverageLabel')}</div>
      <div className={styles.percentText} style={{ color }}>
        {pctStr}
      </div>
      <div className={styles.barTrack}>
        <div
          className={styles.barFill}
          style={{ width: `${clamped}%`, backgroundColor: color }}
          role="progressbar"
          aria-valuenow={clamped}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-label={t('pages:proxy.status.complianceCoverageAria', { percent: clamped.toFixed(1) })}
        />
      </div>
    </div>
  );
}

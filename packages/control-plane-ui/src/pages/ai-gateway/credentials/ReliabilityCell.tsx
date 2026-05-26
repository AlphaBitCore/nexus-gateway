import { useTranslation } from 'react-i18next';
import clsx from 'clsx';
import type { Credential } from '@/api/types';
import styles from './ReliabilityCell.module.css';

// ReliabilityCell renders a single compact badge summarising a credential's
// circuit + health state, with a rich hover card. One cell, one badge, one
// click to drill down to the Reliability tab.

type Tone = 'good' | 'warn' | 'bad' | 'idle';

function statusTone(c: Credential): Tone {
  // Circuit OPEN always wins (red).
  if (c.circuitState === 'open') return 'bad';
  // Health unavailable also red.
  if (c.healthStatus === 'unavailable') return 'bad';
  // half_open or degraded → amber.
  if (c.circuitState === 'half_open' || c.healthStatus === 'degraded') return 'warn';
  // collecting / unknown → muted.
  if (!c.healthStatus || c.healthStatus === 'unknown' || c.healthStatus === 'collecting') return 'idle';
  // healthy + closed → green.
  return 'good';
}

function toneClass(tone: Tone): string {
  switch (tone) {
    case 'good': return styles.toneGood;
    case 'warn': return styles.toneWarn;
    case 'bad':  return styles.toneBad;
    default:     return styles.toneIdle;
  }
}

function trendGlyph(trend: string | null | undefined): string {
  switch (trend) {
    case 'improving': return '↗';
    case 'degrading': return '↘';
    case 'stable':    return '→';
    default:          return '';
  }
}

function relTimeSince(iso: string | null | undefined, locale: string): string {
  if (!iso) return '';
  const diff = Date.now() - new Date(iso).getTime();
  if (diff < 0) return new Date(iso).toLocaleString(locale);
  const sec = Math.round(diff / 1000);
  if (sec < 60) return `${sec}s`;
  const min = Math.round(sec / 60);
  if (min < 60) return `${min}m`;
  const hr = Math.round(min / 60);
  if (hr < 24) return `${hr}h`;
  return `${Math.round(hr / 24)}d`;
}

function relTimeUntil(iso: string | null | undefined): string {
  if (!iso) return '';
  const diff = new Date(iso).getTime() - Date.now();
  if (diff <= 0) return 'now';
  const sec = Math.round(diff / 1000);
  if (sec < 60) return `${sec}s`;
  const min = Math.round(sec / 60);
  return `${min}m`;
}

export function ReliabilityCell({ cred }: { cred: Credential }) {
  const { t, i18n } = useTranslation();
  const tone = statusTone(cred);

  // Label: prefer the worst-of view. "Open" / "Half-open" trumps health when set.
  let label: string;
  if (cred.circuitState === 'open') {
    label = t('pages:credentials.reliability_open');
  } else if (cred.circuitState === 'half_open') {
    label = t('pages:credentials.reliability_half_open');
  } else if (cred.healthStatus) {
    label = t(`pages:credentials.health_${cred.healthStatus}`, { defaultValue: cred.healthStatus });
  } else {
    label = t('pages:credentials.health_unknown');
  }

  const trend = trendGlyph(cred.healthTrend);
  const liveFails = cred.liveCircuit?.authFailsCurrent ?? 0;

  const rate5m = cred.healthSuccessRate5m ?? null;
  const rate1h = cred.healthSuccessRate1h ?? null;
  const samples = cred.healthSamplesObserved ?? 0;
  const dom = cred.healthDominantError ?? null;

  return (
    <span className={styles.cell}>
      <span className={clsx(styles.badge, toneClass(tone))}>
        {label}
        {trend && <span className={styles.trend} aria-hidden>{trend}</span>}
      </span>
      <span className={styles.popover} role="tooltip">
        <span className={styles.popoverGrid}>
          <span className={styles.popoverLabel}>{t('pages:credentials.health')}</span>
          <span>
            {t(`pages:credentials.health_${cred.healthStatus ?? 'unknown'}`, { defaultValue: cred.healthStatus ?? 'unknown' })}
            {rate5m != null && <> · {(rate5m * 100).toFixed(1)}%</>}
          </span>

          {samples < 5 && samples > 0 && (
            <>
              <span className={styles.popoverLabel}>{t('pages:credentials.collecting')}</span>
              <span>{t('pages:credentials.collectingProgress', { observed: samples, target: 5 })}</span>
            </>
          )}

          {dom && dom !== 'none' && (
            <>
              <span className={styles.popoverLabel}>{t('pages:credentials.dominantError')}</span>
              <span>{t(`pages:credentials.dominantError_${dom}`, { defaultValue: dom })}</span>
            </>
          )}

          {rate1h != null && (
            <>
              <span className={styles.popoverLabel}>{t('pages:credentials.rate1h')}</span>
              <span>{(rate1h * 100).toFixed(1)}%</span>
            </>
          )}

          <span className={styles.popoverLabel}>{t('pages:credentials.circuit')}</span>
          <span>
            {t(`pages:credentials.circuit_${cred.circuitState ?? 'closed'}`, { defaultValue: cred.circuitState ?? 'closed' })}
            {cred.circuitReason && <> · {t(`pages:credentials.circuitReason_${cred.circuitReason}`, { defaultValue: cred.circuitReason })}</>}
          </span>

          {cred.circuitOpenedAt && (
            <>
              <span className={styles.popoverLabel}>{t('pages:credentials.openedAgo')}</span>
              <span>{relTimeSince(cred.circuitOpenedAt, i18n.language)}</span>
            </>
          )}

          {cred.circuitNextProbeAt && cred.circuitReason === 'rate_limit' && (
            <>
              <span className={styles.popoverLabel}>{t('pages:credentials.nextProbeIn')}</span>
              <span>{relTimeUntil(cred.circuitNextProbeAt)}</span>
            </>
          )}

          {liveFails > 0 && (
            <>
              <span className={styles.popoverLabel}>{t('pages:credentials.liveAuthFails')}</span>
              <span>{liveFails}</span>
            </>
          )}

          <span className={styles.popoverLabel}>{t('pages:credentials.checkedAgo')}</span>
          <span>{cred.healthCheckedAt ? relTimeSince(cred.healthCheckedAt, i18n.language) : '—'}</span>
        </span>
      </span>
    </span>
  );
}

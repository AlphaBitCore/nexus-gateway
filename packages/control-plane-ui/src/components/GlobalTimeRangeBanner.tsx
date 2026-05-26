import { useTranslation } from 'react-i18next';
import clsx from 'clsx';
import { useTimeRange } from '../context/TimeRangeContext';
import type { TimeRangePreset } from '../context/TimeRangeContext';
import styles from './GlobalTimeRangeBanner.module.css';

/**
 * Slim global time-range picker (S45, M5-12).
 *
 * Mounted in the Shell content area above the page outlet. Every page
 * that opts into the `useTimeRange` hook stays in sync with this picker.
 * Pages with their own per-page overrides (compliance dashboard,
 * discovery, unified audit) read the context as their initial range and
 * still allow per-page custom edits — the global picker is the default
 * not a hard mandate.
 *
 * Hidden on routes where time range is meaningless (e.g. /setup) by
 * checking the current location prefix.
 */

const HIDDEN_ROUTE_PREFIXES = [
  '/setup',
  '/iam/',
'/security/dsar',
  '/config/',
  '/projects',
  '/organizations',
  '/credentials',
];

const PRESET_LABELS: Array<{ value: Exclude<TimeRangePreset, 'custom'>; label: string }> = [
  { value: '24h', label: '24h' },
  { value: '7d', label: '7d' },
  { value: '30d', label: '30d' },
  { value: '90d', label: '90d' },
];

export function GlobalTimeRangeBanner() {
  const { t } = useTranslation();
  const { range, setPreset } = useTimeRange();

  // Read the location once on render. We deliberately do NOT subscribe
  // via useLocation because the banner re-renders on context change
  // anyway, and a route change is also a context change in practice
  // (the next page reads the same context).
  const path = typeof window !== 'undefined' ? window.location.pathname : '';
  if (HIDDEN_ROUTE_PREFIXES.some((p) => path.startsWith(p))) {
    return null;
  }

  return (
    <div role="status" className={styles.banner}>
      <span>{t('common:timeRange', 'Time range')}:</span>
      <strong className={styles.currentRange}>
        {new Date(range.start).toLocaleString()} → {new Date(range.end).toLocaleString()}
      </strong>
      <span className={styles.spacer} />
      <span className={styles.presetButtons}>
        {PRESET_LABELS.map((p) => (
          <button
            key={p.value}
            type="button"
            onClick={() => setPreset(p.value)}
            className={clsx(styles.presetBtn, range.preset === p.value && styles.presetBtnActive)}
          >
            {p.label}
          </button>
        ))}
      </span>
    </div>
  );
}

import { useTranslation } from 'react-i18next';
import clsx from 'clsx';
import { AnimatedNumber } from '@/components/ui';
import { formatTokens, formatUsd } from '@/lib/format';
import styles from './DashboardPage.module.css';
import { WINDOW_OPTIONS, SOURCE_OPTIONS, type TimeWindow, type TrafficSource } from './dashboardShared';

interface HeroSectionProps {
  timeWindow: TimeWindow;
  setTimeWindow: (w: TimeWindow) => void;
  source: TrafficSource;
  setSource: (s: TrafficSource) => void;
  animRequests: number;
  vkRequests: number;
  proxyTotalRequests: number;
  animCost100x: number;
  animTokens: number;
  proxyReachable: boolean | null;
  proxyCoveragePercent: number;
  windowLabel: string;
}

export function HeroSection({
  timeWindow,
  setTimeWindow,
  source,
  setSource,
  animRequests,
  vkRequests,
  proxyTotalRequests,
  animCost100x,
  animTokens,
  proxyReachable,
  proxyCoveragePercent,
  windowLabel,
}: HeroSectionProps) {
  const { t } = useTranslation();
  const renderStatMeta = (label: string, detail: string) => (
    <span className={styles.heroStatMeta}>
      <span>{label}</span>
      <span className={styles.heroStatDivider}>|</span>
      <span>{detail}</span>
    </span>
  );

  return (
    <section className={styles.hero}>
      <div className={styles.heroRow}>
        <div className={styles.heroText}>
          <h1 className={styles.heroTitle}>{t('pages:dashboard.title')}</h1>
          <p className={styles.heroSubtitle}>{t('pages:dashboard.heroSubtitle')}</p>
        </div>
        <div className={styles.heroControls}>
          <div className={styles.windowPicker} role="group" aria-label={t('pages:dashboard.sourcePickerLabel', 'Traffic source')}>
            {SOURCE_OPTIONS.map((s) => (
              <button
                key={s}
                type="button"
                className={clsx(styles.windowBtn, s === source && styles.windowBtnActive)}
                onClick={() => setSource(s)}
              >
                {t(`pages:dashboard.source_${s}` as never)}
              </button>
            ))}
          </div>
          <div className={styles.windowPicker}>
            {WINDOW_OPTIONS.map((w) => (
              <button
                key={w}
                type="button"
                className={clsx(styles.windowBtn, w === timeWindow && styles.windowBtnActive)}
                onClick={() => setTimeWindow(w)}
              >
                {t(`pages:dashboard.win${w}` as never)}
              </button>
            ))}
          </div>
        </div>
      </div>
      <div className={styles.heroStats}>
        <div className={styles.heroStat}>
          <span className={styles.heroStatValue}>{animRequests.toLocaleString()}</span>
          {renderStatMeta(
            t('pages:dashboard.combinedRequests'),
            t('pages:dashboard.splitBarSubtitle', { vk: vkRequests.toLocaleString(), proxy: proxyTotalRequests.toLocaleString() }),
          )}
        </div>
        <div className={styles.heroStat}>
          <span className={styles.heroStatValue}>{formatUsd(animCost100x / 100)}</span>
          {renderStatMeta(t('pages:dashboard.totalCostLabel'), t('pages:dashboard.estimated'))}
        </div>
        <div className={styles.heroStat}>
          <span className={styles.heroStatValue}>{formatTokens(animTokens)}</span>
          {renderStatMeta(t('pages:dashboard.totalTokensLabel'), t('pages:dashboard.promptPlusCompletion'))}
        </div>
        <div className={styles.heroStat}>
          <span className={styles.heroStatValue}>
            {proxyReachable === false ? '—' : <AnimatedNumber value={proxyCoveragePercent} precision={1} format={(n) => `${n.toFixed(1)}%`} />}
          </span>
          {renderStatMeta(t('pages:dashboard.complianceCoverage'), proxyReachable === false ? t('pages:dashboard.proxyNotReachable') : windowLabel)}
        </div>
      </div>
    </section>
  );
}

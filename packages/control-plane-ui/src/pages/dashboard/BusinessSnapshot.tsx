import { useTranslation } from 'react-i18next';
import clsx from 'clsx';
import { Grid, Card, ExpandableWrapper, Sparkline, AnimatedNumber } from '@/components/ui';
import type { CacheROISummary } from '@/api/services/overview/analytics';
import { formatTokens, formatUsd } from '@/lib/format';
import styles from './DashboardPage.module.css';
import { InfoIcon } from './dashboardShared';

interface SparkData {
  requests: number[];
  errors: number[];
  latency: number[];
  cost: number[];
  tokens: number[];
  cacheHitRate: number[];
  cacheSavings: number[];
}

interface BusinessSnapshotProps {
  animCost100x: number;
  animTokens: number;
  sparkData: SparkData;
  activeProviders: number;
  totalProviders: number;
  activeProvidersSpark: number[];
  cacheHitRate: number;
  windowLabel: string;
  cacheROI: CacheROISummary | null | undefined;
}

export function BusinessSnapshot({
  animCost100x,
  animTokens,
  sparkData,
  activeProviders,
  totalProviders,
  activeProvidersSpark,
  cacheHitRate,
  windowLabel,
  cacheROI,
}: BusinessSnapshotProps) {
  const { t } = useTranslation();

  return (
    <>
      {/* ── Section 2: Business Snapshot ─────────────────────────────────── */}
      <div className={styles.sectionHeader}>
        <h2 className={styles.sectionTitle}>{t('pages:dashboard.sectionBusinessSnapshot')}</h2>
        <p className={styles.sectionSubtitle}>{t('pages:dashboard.sectionBusinessSnapshotSubtitle')}</p>
      </div>

      <Grid columns={4} gap="md">
        <ExpandableWrapper>
          <Card padding="lg" className={styles.metricCard}>
            <div className={styles.metricLabelRow}>
              {t('pages:dashboard.totalCostLabel')}
              <InfoIcon description={t('pages:dashboard.totalCostDesc')} />
            </div>
            <div className={styles.metricValueRow}>
              <span className={styles.metricValue}>{formatUsd(animCost100x / 100)}</span>
              <Sparkline data={sparkData.cost} color="var(--color-success)" />
            </div>
            <span className={styles.metricSubtitle}>{t('pages:dashboard.estimated')}</span>
          </Card>
        </ExpandableWrapper>

        <ExpandableWrapper>
          <Card padding="lg" className={styles.metricCard}>
            <div className={styles.metricLabelRow}>
              {t('pages:dashboard.totalTokensLabel')}
            </div>
            <div className={styles.metricValueRow}>
              <span className={styles.metricValue}>{formatTokens(animTokens)}</span>
              <Sparkline data={sparkData.tokens} color="var(--color-primary)" />
            </div>
            <span className={styles.metricSubtitle}>{t('pages:dashboard.promptPlusCompletion')}</span>
          </Card>
        </ExpandableWrapper>

        <ExpandableWrapper>
          <Card padding="lg" className={styles.metricCard}>
            <div className={styles.metricLabelRow}>
              {t('pages:dashboard.activeProvidersLabel')}
              <InfoIcon description={t('pages:dashboard.activeProvidersDesc')} />
            </div>
            <div className={styles.metricValueRow}>
              <span className={styles.metricValue}>
                <AnimatedNumber value={activeProviders} />/<AnimatedNumber value={totalProviders} />
              </span>
              <Sparkline data={activeProvidersSpark} color="var(--color-primary)" />
            </div>
          </Card>
        </ExpandableWrapper>

        <ExpandableWrapper>
          <Card padding="lg" className={styles.metricCard}>
            <div className={styles.metricLabelRow}>
              {t('pages:dashboard.cacheHitRateLabel')}
              <InfoIcon description={t('pages:dashboard.cacheHitRateDesc')} />
            </div>
            <div className={styles.metricValueRow}>
              <span className={styles.metricValue}><AnimatedNumber value={cacheHitRate * 100} precision={1} format={(n) => `${n.toFixed(1)}%`} /></span>
              <Sparkline data={sparkData.cacheHitRate} color="var(--color-success)" />
            </div>
            <span className={styles.metricSubtitle}>{windowLabel}</span>
          </Card>
        </ExpandableWrapper>

        {cacheROI && (() => {
          const combinedSavings = (cacheROI.totalGatewayCacheSavingsUsd ?? 0) + cacheROI.totalCacheNetSavingsUsd;
          if (combinedSavings === 0) return null;
          return (
            <ExpandableWrapper>
              <Card padding="lg" className={styles.metricCard}>
                <div className={styles.metricLabelRow}>
                  {t('pages:dashboard.cacheSavingsLabel')}
                  <InfoIcon description={t('pages:dashboard.cacheSavingsDesc')} />
                </div>
                <div className={styles.metricValueRow}>
                  <span
                    className={clsx(styles.metricValue, combinedSavings >= 0 ? styles.savingsPositive : styles.savingsNegative)}
                  >
                    <AnimatedNumber
                      value={combinedSavings}
                      precision={2}
                      format={(n) => formatUsd(n)}
                    />
                  </span>
                  <Sparkline data={sparkData.cacheSavings} color={combinedSavings >= 0 ? 'var(--color-success)' : 'var(--color-danger)'} />
                </div>
                <span className={styles.metricSubtitle}>{t('pages:dashboard.cacheSavingsSubtitleShort')}</span>
              </Card>
            </ExpandableWrapper>
          );
        })()}
      </Grid>
    </>
  );
}

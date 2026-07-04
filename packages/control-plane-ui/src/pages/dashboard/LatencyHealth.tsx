import { useTranslation } from 'react-i18next';
import clsx from 'clsx';
import { Grid, Card } from '@/components/ui';
import type { LatencyPhaseRow } from '@/api/services/overview/analytics';
import styles from './DashboardPage.module.css';

interface LatencyHealthProps {
  latencyPhases: { rows?: LatencyPhaseRow[] } | null | undefined;
  navigate: (path: string) => void;
}

export function LatencyHealth({ latencyPhases, navigate }: LatencyHealthProps) {
  const { t } = useTranslation();

  /* ── Section 1.5: Latency Health ─────────────────────────────── */
  if (!(latencyPhases?.rows && latencyPhases.rows.length > 0)) return null;

  const sorted = [...latencyPhases.rows].sort((a, b) => b.requestCount - a.requestCount);
  const top = sorted[0];
  const slowest = [...latencyPhases.rows]
    .filter((r) => r.upstreamTotalP95Ms != null)
    .sort((a, b) => (b.upstreamTotalP95Ms ?? 0) - (a.upstreamTotalP95Ms ?? 0))[0];
  const fmtMs = (v: number | null | undefined) =>
    v == null ? '—' : v < 1000 ? `${v}ms` : `${(v / 1000).toFixed(2)}s`;
  return (
    <>
      <div className={styles.sectionHeader}>
        <h2 className={styles.sectionTitle}>{t('pages:dashboard.sectionLatencyHealth', 'Latency Health')}</h2>
        <p className={styles.sectionSubtitle}>
          {t('pages:dashboard.sectionLatencyHealthSubtitle', 'Our overhead versus the upstream provider — by the busiest provider in the window.')}
        </p>
      </div>
      <Grid columns={4} gap="md">
        <Card padding="lg" className={styles.metricCard}>
          <div className={styles.metricLabelRow}>{t('pages:dashboard.latencyOurOverheadP95', 'Our Overhead P95')}</div>
          <div className={styles.metricValueRow}>
            <span className={styles.metricValue}>{fmtMs(top.usOverheadP95Ms)}</span>
          </div>
          <span className={styles.metricSubtitle}>{top.groupLabel}</span>
        </Card>
        <Card padding="lg" className={styles.metricCard}>
          <div className={styles.metricLabelRow}>{t('pages:dashboard.latencyUpstreamTotalP95', 'Upstream P95')}</div>
          <div className={styles.metricValueRow}>
            <span className={styles.metricValue}>{fmtMs(top.upstreamTotalP95Ms)}</span>
          </div>
          <span className={styles.metricSubtitle}>{top.groupLabel}</span>
        </Card>
        <Card padding="lg" className={styles.metricCard}>
          <div className={styles.metricLabelRow}>{t('pages:dashboard.latencyEndToEndP95', 'End-to-End P95')}</div>
          <div className={styles.metricValueRow}>
            <span className={styles.metricValue}>{fmtMs(top.totalP95Ms)}</span>
          </div>
          <span className={styles.metricSubtitle}>{top.groupLabel}</span>
        </Card>
        <Card padding="lg" className={clsx(styles.metricCard, styles.clickable)} onClick={() => navigate('/analytics')}>
          <div className={styles.metricLabelRow}>{t('pages:dashboard.latencySlowestProvider', 'Slowest Upstream Provider')}</div>
          <div className={styles.metricValueRow}>
            <span className={styles.metricValue}>{slowest ? fmtMs(slowest.upstreamTotalP95Ms) : '—'}</span>
          </div>
          <span className={styles.metricSubtitle}>{slowest?.groupLabel ?? '—'}</span>
        </Card>
      </Grid>
    </>
  );
}

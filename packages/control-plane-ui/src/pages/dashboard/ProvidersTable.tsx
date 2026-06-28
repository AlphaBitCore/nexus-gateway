import { useTranslation } from 'react-i18next';
import { Card, Button } from '@/components/ui';
import type { ProviderBreakdown } from '../../api/types';
import { formatTokens, formatUsd } from '@/lib/format';
import styles from './DashboardPage.module.css';

interface ProvidersTableProps {
  topProviders: ProviderBreakdown[];
  navigate: (path: string) => void;
}

export function ProvidersTable({ topProviders, navigate }: ProvidersTableProps) {
  const { t } = useTranslation();

  /* Top Providers table */
  if (topProviders.length === 0) return null;

  return (
    <Card data-testid="dashboard-top-providers" padding="lg">
      <div className={styles.tableHeaderRow}>
        <h2 className={styles.sectionTitle}>{t('pages:dashboard.topProviders')}</h2>
        <Button variant="ghost" size="sm" onClick={() => navigate('/analytics')}>
          {t('pages:dashboard.viewAll')}
        </Button>
      </div>
      <div className={styles.tableWrapper}>
        <table className={styles.table}>
          <thead>
            <tr>
              <th className={styles.th}>{t('pages:dashboard.colProvider')}</th>
              <th className={styles.th}>{t('pages:dashboard.colRequests')}</th>
              <th className={styles.th}>{t('pages:dashboard.colAvgLatency')}</th>
              <th className={styles.th}>{t('pages:dashboard.colTokens')}</th>
              <th className={styles.th}>{t('pages:dashboard.colCost')}</th>
            </tr>
          </thead>
          <tbody>
            {topProviders.map((p, i) => (
              <tr key={p.provider ?? i} className={styles.tableRow}>
                <td className={styles.tdBold}>{p.providerLabel || p.provider}</td>
                <td className={styles.td}>{p.requestCount.toLocaleString()}</td>
                <td className={styles.td}>{Math.round(p.avgLatencyMs)}ms</td>
                <td className={styles.td}>{formatTokens(p.totalTokens)}</td>
                <td className={styles.tdMono}>{formatUsd(Number(p.totalEstimatedCostUsd))}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </Card>
  );
}

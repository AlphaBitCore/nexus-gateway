import { useTranslation } from 'react-i18next';
import { useApi } from '../../../hooks/useApi';
import { systemApi, providerApi } from '@/api/services';
import {
  PageHeader, LoadingSpinner, ErrorBanner, Card, Button, Stack, Grid,
} from '@/components/ui';
import type { Provider, ProviderHealth } from '../../../api/types';
import { LatencyMini } from '@/components/charts/LatencyMini';
import { ADMIN_LIST_FULL_PAGE_PARAMS } from '../../../constants/admin-api';
import styles from './ProviderHealthPage.module.css';

function statusDotColor(status: string): string {
  if (status === 'healthy' || status === 'enabled') return 'var(--color-success)';
  if (status === 'degraded') return 'var(--color-warning)';
  if (status === 'unhealthy' || status === 'down' || status === 'disabled') return 'var(--color-danger)';
  return 'var(--color-text-muted)';
}

function statusLabel(status: string): string {
  return status.charAt(0).toUpperCase() + status.slice(1);
}

function statusPillBg(status: string): string {
  if (status === 'healthy' || status === 'enabled') return 'var(--color-success-light)';
  if (status === 'degraded') return 'var(--color-warning-light)';
  if (status === 'unhealthy' || status === 'down' || status === 'disabled') return 'var(--color-danger-light)';
  return 'var(--color-border-light)';
}

function statusPillColor(status: string): string {
  if (status === 'healthy' || status === 'enabled') return 'var(--color-success-dark)';
  if (status === 'degraded') return 'var(--color-warning-dark)';
  if (status === 'unhealthy' || status === 'down' || status === 'disabled') return 'var(--color-danger-dark)';
  return 'var(--color-text-muted)';
}

export function ProviderHealthPage() {
  const { t } = useTranslation();
  const { data: providersData, loading: providersLoading, error: providersError, refetch } = useApi<{ data: Provider[] }>(
    () => providerApi.list({ ...ADMIN_LIST_FULL_PAGE_PARAMS }),
    ['admin', 'providers', 'list', 'health-page'],
  );
  const { data: healthData, loading: healthLoading } = useApi<{ data: ProviderHealth[] }>(
    () => systemApi.listProviderHealth(),
    ['admin', 'providers', 'health', 'list'],
  );

  const loading = providersLoading || healthLoading;
  if (loading) return <LoadingSpinner />;
  if (providersError) return <ErrorBanner message={providersError.message} onRetry={refetch} />;

  const providers = providersData?.data ?? [];
  const healthMap = new Map(
    (healthData?.data ?? []).map((h) => [h.providerId, h]),
  );

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('pages:status.providerHealth.title')}
        subtitle={t('pages:status.providerHealthSubtitle')}
        action={
          <Button variant="secondary" onClick={refetch}>{t('pages:traffic.refresh')}</Button>
        }
      />

      {providers.length === 0 ? (
        <Card>
          <div className={styles.emptyState}>{t('pages:status.providerHealth.noProviders')}</div>
        </Card>
      ) : (
        <div className={styles.cardGrid}>
          {providers.map((provider) => {
            const health = healthMap.get(provider.id);
            const status = health?.status ?? (provider.enabled ? 'unknown' : 'disabled');

            return (
              <Card key={provider.id} padding="lg">
                <div className={styles.cardHeader}>
                  <h3 className={styles.providerName}>
                    {provider.displayName || provider.name}
                  </h3>
                  <span
                    className={styles.statusPill}
                    style={{ background: statusPillBg(status), color: statusPillColor(status) }}
                  >
                    <span className={styles.dot} style={{ background: statusDotColor(status) }} />
                    {statusLabel(status)}
                  </span>
                </div>

                <Grid columns={2} gap="md">
                  <div>
                    <div className={styles.metricLabel}>{t('pages:status.providerHealth.errorRate')}</div>
                    <div className={styles.metricValue}>
                      {health ? `${(health.errorRate * 100).toFixed(1)}%` : '-'}
                    </div>
                  </div>
                  <div>
                    {/* LatencyMini renders the avgUpstreamTotalMs / TtfbMs
                        split when the backend returns them; falls back to
                        a single neutral bar otherwise so the row stays
                        visually anchored even on legacy aggregates. */}
                    <LatencyMini
                      size="card"
                      title={t('pages:status.providerHealth.avgLatency')}
                      latencyMs={health?.avgLatencyMs ?? null}
                      upstreamTotalMs={(health as { avgUpstreamTotalMs?: number | null } | undefined)?.avgUpstreamTotalMs ?? null}
                      upstreamTtfbMs={(health as { avgUpstreamTtfbMs?: number | null } | undefined)?.avgUpstreamTtfbMs ?? null}
                    />
                  </div>
                  <div>
                    <div className={styles.metricLabel}>{t('pages:status.providerHealth.sampleCount')}</div>
                    <div className={styles.metricValue}>
                      {health?.sampleCount?.toLocaleString() ?? '-'}
                    </div>
                  </div>
                  <div>
                    <div className={styles.metricLabel}>{t('pages:providers.adapter', 'Adapter')}</div>
                    <div className={styles.metricValue}>
                      {t(`pages:providers.adapterOption_${provider.adapterType}`, provider.adapterType)}
                    </div>
                  </div>
                </Grid>
              </Card>
            );
          })}
        </div>
      )}
    </Stack>
  );
}

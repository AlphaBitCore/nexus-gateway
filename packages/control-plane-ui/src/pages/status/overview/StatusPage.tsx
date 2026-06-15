import { useTranslation } from 'react-i18next';
import { useApi } from '../../../hooks/useApi';
import { providerApi, systemApi, opsMetricsApi } from '@/api/services';
import type { ServiceMetricsResponse } from '@/api/services/infrastructure/misc/system';
import type { OpsMetricSample } from '@/api/services/infrastructure/ops/opsmetrics';
import {
  PageHeader, LoadingSpinner, ErrorBanner, Card, Button, Stack,
  Tooltip,
  Tabs, TabsList, TabsTrigger, TabsContent,
} from '@/components/ui';
import type { SystemSettings, Provider, ProviderHealth } from '../../../api/types';
import { ADMIN_LIST_FULL_PAGE_PARAMS } from '../../../constants/admin-api';
import { usePermission } from '../../../hooks/usePermission';
import { useState, useEffect, useMemo } from 'react';
import { Link } from 'react-router-dom';
import { ServiceCard } from '../services/ServiceCard';
import { RecentErrorsWidget } from '../services/RecentErrorsWidget';
import { groupOpsSamples } from '../services/groupOpsSamples';
import styles from './StatusPage.module.css';

interface ReadinessResponse {
  status: string;
  checks?: Record<string, string>;
}

interface InstancesResponse {
  instances: any[];
  count: number;
  services: Record<string, any>;
}

function dotColor(ok: boolean, neutral?: boolean): string {
  if (ok) return 'var(--color-success)';
  if (neutral) return 'var(--color-text-muted)';
  return 'var(--color-danger)';
}

function providerDotColor(status: string): string {
  if (status === 'healthy' || status === 'enabled') return 'var(--color-success)';
  if (status === 'degraded') return 'var(--color-warning)';
  if (status === 'unhealthy' || status === 'down' || status === 'unavailable' || status === 'disabled') return 'var(--color-danger)';
  return 'var(--color-text-muted)';
}

/* ── Component ──────────────────────────────────────────────────────────── */

export function StatusPage() {
  const { t } = useTranslation();
  const [activeTab, setActiveTab] = useState('overview');
  const canReadSettings = usePermission('settings:read');

  const { data: settings, loading: settingsLoading, error: settingsError, refetch: refetchSettings } = useApi<SystemSettings>(
    () => systemApi.getSettings(),
    ['admin', 'system', 'settings'],
    { skip: !canReadSettings },
  );
  const { data: health } = useApi<ReadinessResponse>(
    () => systemApi.checkReady(),
    ['admin', 'system', 'readiness'],
  );
  const { data: instancesData } = useApi<InstancesResponse>(
    () => systemApi.listInstances().catch((): InstancesResponse => ({ instances: [], count: 0, services: {} })),
    ['admin', 'instances'],
  );
  const { data: providersData } = useApi<{ data: Provider[] }>(
    () => providerApi.list({ ...ADMIN_LIST_FULL_PAGE_PARAMS }),
    ['admin', 'providers', 'list', 'status'],
  );
  const { data: providerHealthData } = useApi<{ data: ProviderHealth[] }>(
    () => systemApi.listProviderHealth(),
    ['admin', 'providers', 'health', 'list'],
  );
  // Service Metrics cards now read from /api/admin/ops-metrics/current
  // (the legacy /api/admin/service-metrics endpoint was deleted in
  // spec §10.5). The flat sample list is reshaped per-service by
  // groupOpsSamples() below so the existing ServiceCard renderer keeps
  // its current shape; richer per-thing views land in T36-T41.
  const { data: opsCurrent, refetch: refetchMetrics } = useApi<OpsMetricSample[]>(
    () => opsMetricsApi.current().catch((): OpsMetricSample[] => []),
    ['admin', 'ops-metrics', 'current', 'status'],
  );
  const serviceMetrics = useMemo<ServiceMetricsResponse>(
    () => groupOpsSamples(opsCurrent),
    [opsCurrent],
  );

  // Auto-refresh ops metrics every 15s when Overview tab is active.
  useEffect(() => {
    if (activeTab !== 'overview') return;
    const interval = setInterval(() => { refetchMetrics(); }, 15_000);
    return () => clearInterval(interval);
  }, [activeTab, refetchMetrics]);

  if (canReadSettings && settingsLoading && !settings) return <LoadingSpinner />;
  if (canReadSettings && settingsError) {
    return <ErrorBanner message={settingsError.message} onRetry={refetchSettings} />;
  }

  const uptimeDisplay = settings
    ? (Math.round(settings.uptime / 3600) > 0
        ? `${Math.round(settings.uptime / 3600)}h`
        : `${Math.round(settings.uptime / 60)}m`)
    : '—';
  const instanceCount = instancesData?.count ?? '-';
  const dbStatus = health?.checks?.database ?? 'unknown';
  const hubStatus = health?.checks?.hub ?? 'unknown';
  const configVersion = settings?.version ?? '—';

  const dbOk = dbStatus === 'ok' || dbStatus === 'healthy';
  const hubOk = hubStatus === 'ok' || hubStatus === 'healthy';
  const hubNeutral = hubStatus === 'not_configured';

  const providers = providersData?.data ?? [];
  const healthMap = new Map((providerHealthData?.data ?? []).map(h => [h.providerId, h]));

  return (
    <Stack gap="lg">
      <PageHeader title={t('pages:status.title')} />
      {!canReadSettings && (
        <p className={styles.helperText} role="status">
          {t('pages:status.settingsSummaryUnavailable')}
        </p>
      )}

      <Tabs value={activeTab} onValueChange={setActiveTab}>
        <TabsList>
          <TabsTrigger value="overview">{t('pages:status.tabOverview')}</TabsTrigger>
          <TabsTrigger value="providers">{t('pages:status.tabProviders', { count: providers.length })}</TabsTrigger>
          <TabsTrigger value="jobs">{t('pages:status.tabJobs')}</TabsTrigger>
        </TabsList>

        {/* Overview Tab */}
        <TabsContent value="overview">
          <Stack gap="md">
            {/* Stats Row (de-emphasized) */}
            <div className={styles.statsGridCompact}>
              <div className={styles.statCardCompact}>
                <div className={styles.statValueCompact}>{uptimeDisplay}</div>
                <div className={styles.statLabelCompact}>{t('pages:status.uptime')}</div>
              </div>
              <div className={styles.statCardCompact}>
                <div className={styles.statValueCompact}>{settings?.version ?? '—'}</div>
                <div className={styles.statLabelCompact}>{t('pages:status.version')}</div>
              </div>
              <div className={styles.statCardCompact}>
                <div className={styles.statValueCompact}>{String(instanceCount)}</div>
                <div className={styles.statLabelCompact}>{t('pages:status.instances')}</div>
              </div>
              <div className={styles.statCardCompact}>
                <div className={styles.statValueCompact}>{settings?.goVersion ?? '—'}</div>
                <div className={styles.statLabelCompact}>{t('pages:status.node')}</div>
              </div>
              <div className={styles.statCardCompact}>
                <div className={styles.statValueCompact}>{settings?.logLevel ?? '—'}</div>
                <div className={styles.statLabelCompact}>{t('pages:status.logLevel')}</div>
              </div>
              <div className={styles.statCardCompact}>
                <div
                  className={styles.statValueCompact}
                  style={{
                    color: settings == null ? 'var(--color-text-muted)'
                      : settings.maintenanceMode ? 'var(--color-danger)' : 'var(--color-success)',
                  }}
                >
                  {settings == null ? '—' : settings.maintenanceMode ? t('pages:status.maintenanceOn') : t('pages:status.maintenanceOff')}
                </div>
                <div className={styles.statLabelCompact}>{t('pages:status.maintenance')}</div>
              </div>
            </div>

            {/* Scope hint: /status reports the Control Plane's own
                health (DB + Hub) and CP-managed surfaces (cache, provider
                health, jobs). Cross-service fleet health lives at
                /infrastructure/nodes — every service registers itself
                there as a Thing. Linking instead of duplicating keeps
                the Hub-centric model consistent. */}
            <p className={styles.scopeHint}>
              {t('pages:status.scopeHint')}{' '}
              <Link to="/infrastructure/nodes" className={styles.scopeLink}>
                {t('pages:status.scopeAllServicesLink')}
              </Link>
            </p>

            {/* Infrastructure Bar (compact) */}
            <div className={styles.infraBar}>
              <div className={styles.infraItem}>
                <span className={styles.infraDot} style={{ background: dotColor(dbOk) }} />
                DB {t(`pages:status.dbStatus_${dbStatus}`, dbStatus)}
              </div>
              <div className={styles.infraItem}>
                <span className={styles.infraDot} style={{ background: dotColor(hubOk, hubNeutral) }} />
                Hub {hubStatus === 'not_configured' ? t('pages:status.hubNotConfigured') : t(`pages:status.hubStatus_${hubStatus}`, hubStatus)}
              </div>
              <div className={styles.infraItem}>
                Config {configVersion}
              </div>
            </div>

            {/* Fetch errors */}
            {serviceMetrics?.fetchErrors && serviceMetrics.fetchErrors.length > 0 && (
              <div className={styles.fetchWarning}>
                {t('pages:status.fetchErrorsWarning')}: {t('pages:status.fetchErrorCount', { count: serviceMetrics.fetchErrors.length })}
              </div>
            )}

            {/* Service Cards */}
            {!serviceMetrics || Object.keys(serviceMetrics.services).length === 0 ? (
              <div className={styles.emptyText}>{t('pages:status.noHealthyInstances')}</div>
            ) : (
              Object.entries(serviceMetrics.services).map(([svcName, svc]) => {
                const svcInstances = (instancesData?.instances ?? []).filter((i: any) => i.service === svcName);
                const svcSummary = instancesData?.services[svcName] ?? { healthy: 0, total: 0, degraded: 0, unhealthy: 0, offline: 0 };
                return (
                  <ServiceCard
                    key={svcName}
                    serviceName={svcName}
                    metricSet={svc}
                    instances={svcInstances}
                    serviceSummary={svcSummary}
                  />
                );
              })
            )}

            {/* Recent Errors mini-widget — sits at the bottom of Overview
                so the Status page surfaces ERROR-level diag events without
                pulling the operator into the dedicated /infrastructure/errors
                view (T36). The widget self-renders the empty state on
                permission errors. */}
            <RecentErrorsWidget />
          </Stack>
        </TabsContent>

        {/* Providers Tab */}
        <TabsContent value="providers">
          {providers.length === 0 ? (
            <Card>
              <div className={styles.emptyText}>{t('pages:status.noProviders')}</div>
            </Card>
          ) : (
            <div className={styles.providerGrid}>
              {providers.map(provider => {
                const h = healthMap.get(provider.id);
                const status = !provider.enabled ? 'disabled' : (h?.status ?? 'unknown');

                return (
                  <Card key={provider.id}>
                    <div className={styles.providerHeader}>
                      <span className={styles.providerName}>
                        {provider.displayName || provider.name}
                      </span>
                      <span className={styles.providerStatus}>
                        <span className={styles.dot} style={{ background: providerDotColor(status) }} />
                        {t(`pages:status.providerStatus_${status}`, status.charAt(0).toUpperCase() + status.slice(1))}
                        <Tooltip content={t('pages:status.providerStatusTooltip')}>
                          <button type="button" aria-label={t('pages:status.ariaHelpProviderStatus')} className={styles.helpIconBtn}>&#9432;</button>
                        </Tooltip>
                      </span>
                    </div>
                    <div className={styles.providerMetrics}>
                      <div>
                        <div className={styles.kvLabel}>{t('pages:status.errorRate')}</div>
                        <div className={styles.kvValue}>{h ? `${(h.errorRate * 100).toFixed(1)}%` : '-'}</div>
                      </div>
                      <div>
                        <div className={styles.kvLabel}>{t('pages:status.latency')}</div>
                        <div className={styles.kvValue}>{h ? `${Math.round(h.avgLatencyMs)}ms` : '-'}</div>
                      </div>
                      <div>
                        <div className={styles.kvLabel}>{t('pages:status.samples')}</div>
                        <div className={styles.kvValue}>{h?.sampleCount?.toLocaleString() ?? '-'}</div>
                      </div>
                    </div>
                  </Card>
                );
              })}
            </div>
          )}
        </TabsContent>

        {/* Jobs Tab — redirects to Infrastructure > Scheduled Jobs */}
        <TabsContent value="jobs">
          <Card>
            <Stack gap="md" align="start">
              <p className={styles.runtimeDesc}>{t('pages:status.jobsMovedNotice')}</p>
              <Link to="/infrastructure/jobs">
                <Button variant="primary" size="sm">{t('pages:status.jobsMovedLink')}</Button>
              </Link>
            </Stack>
          </Card>
        </TabsContent>

      </Tabs>
    </Stack>
  );
}

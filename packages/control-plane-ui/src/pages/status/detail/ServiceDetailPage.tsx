import { useMemo, useState } from 'react';
import { useParams, Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '../../../hooks/useApi';
import { systemApi, opsMetricsApi } from '@/api/services';
import type { ServiceMetricsResponse, ServiceInstanceInfo } from '@/api/services/infrastructure/misc/system';
import type { OpsMetricSample } from '@/api/services/infrastructure/ops/opsmetrics';
import { Card, Stack, LoadingSpinner } from '@/components/ui';
import { formatTokens } from '@/lib/format';
import { groupOpsSamples } from '../services/groupOpsSamples';
import styles from './ServiceDetailPage.module.css';

/* ── Helpers ──────────────────────────────────────────────────────── */

function MetricItem({ value, label, color }: { value: string; label: string; color?: string }) {
  return (
    <div className={styles.metricItem}>
      <div className={styles.metricValue} style={color ? { color } : undefined}>{value}</div>
      <div className={styles.metricLabel}>{label}</div>
    </div>
  );
}

function summaryDotColor(s: { healthy: number; total: number; degraded: number; unhealthy: number; offline: number }): string {
  if (s.unhealthy > 0 || s.offline > 0) return 'var(--color-danger)';
  if (s.degraded > 0) return 'var(--color-warning)';
  if (s.healthy === s.total && s.total > 0) return 'var(--color-success)';
  return 'var(--color-text-muted)';
}

function instanceStatusColor(status: string): string {
  const normalized = status.trim().toLowerCase();
  if (normalized === 'online' || normalized === 'healthy') return 'var(--color-success)';
  if (normalized === 'degraded') return 'var(--color-warning)';
  if (normalized === 'offline' || normalized === 'unhealthy') return 'var(--color-danger)';
  return 'var(--color-text-muted)';
}

function formatDateTime(value: string | null | undefined): string {
  if (!value) return '-';
  const ts = new Date(value);
  if (Number.isNaN(ts.getTime())) return '-';
  return ts.toLocaleString();
}

interface InstancesResponse {
  instances: ServiceInstanceInfo[];
  count: number;
  services: Record<string, { healthy: number; total: number; degraded: number; unhealthy: number; offline: number }>;
}

/* ── Component ────────────────────────────────────────────────────── */

export function ServiceDetailPage() {
  const { t } = useTranslation();
  const { serviceName } = useParams<{ serviceName: string }>();

  const [selectedBreakdown, setSelectedBreakdown] = useState<string>('');

  // Sources (per spec §10.5): /api/admin/service-metrics is gone.
  // Live process telemetry comes from /ops-metrics/current; the flat
  // sample stream is reshaped per-service by groupOpsSamples() below
  // so this page keeps its current cell layout. Histogram-derived p50/p99
  // are zero in the new shape — a dedicated per-thing detail page
  // layers timeseries fetches on top.
  const { data: opsCurrent, loading: metricsLoading } = useApi<OpsMetricSample[]>(
    () => opsMetricsApi.current().catch((): OpsMetricSample[] => []),
    ['admin', 'ops-metrics', 'current', 'service-detail'],
  );
  const metricsData = useMemo<ServiceMetricsResponse>(
    () => groupOpsSamples(opsCurrent),
    [opsCurrent],
  );

  const { data: instancesData } = useApi<InstancesResponse>(
    () => systemApi.listInstances().catch((): InstancesResponse => ({ instances: [], count: 0, services: {} })),
    ['admin', 'instances'],
  );

  if (metricsLoading && !opsCurrent) return <LoadingSpinner />;

  const svc = serviceName ? metricsData?.services[serviceName] : undefined;
  const allInstances = instancesData?.instances ?? [];
  const instances = allInstances.filter((inst) => inst.service === serviceName);
  const serviceSummary = serviceName && instancesData?.services[serviceName]
    ? instancesData.services[serviceName]
    : { healthy: 0, total: instances.length, degraded: 0, unhealthy: 0, offline: 0 };

  const breakdowns = svc?.breakdowns ?? {};
  const breakdownKeys = Object.keys(breakdowns);

  // Auto-select first breakdown key if none selected
  const activeBreakdownKey = selectedBreakdown && breakdowns[selectedBreakdown]
    ? selectedBreakdown
    : breakdownKeys[0] ?? '';
  const activeBreakdown = activeBreakdownKey ? breakdowns[activeBreakdownKey] : undefined;

  return (
    <Stack gap="lg">
      {/* Back link */}
      <Link to="/status" className={styles.backLink}>
        {t('pages:status.backToStatus')}
      </Link>

      {/* Header */}
      <div className={styles.headerRow}>
        <span className={styles.dot} style={{ background: summaryDotColor(serviceSummary), width: 12, height: 12 }} />
        <span className={styles.serviceName}>{serviceName}</span>
        <span className={styles.instanceBadge}>
          {t('pages:status.instancesCount', { count: serviceSummary.total })}
        </span>
      </div>

      {/* Business Metrics */}
      {svc && (
        <div className={styles.metricsGrid}>
          {serviceName === 'control-plane' && (<>
            <MetricItem value={Number(svc.metrics.requestsTotal ?? 0).toLocaleString()} label={t('pages:status.requestsTotal')} />
            <MetricItem value={`${Number(svc.metrics.requestDurationP50Ms ?? 0).toFixed(1)}ms`} label={t('pages:status.requestDurationP50')} />
            <MetricItem value={`${Number(svc.metrics.requestDurationP99Ms ?? 0).toFixed(1)}ms`} label={t('pages:status.requestDurationP99')} />
            <MetricItem value={Number(svc.metrics.authFailuresTotal ?? 0).toLocaleString()} label={t('pages:status.authFailures')} />
            <MetricItem value={Number(svc.metrics.iamDenialsTotal ?? 0).toLocaleString()} label={t('pages:status.iamDenials')} />
          </>)}
          {serviceName === 'ai-gateway' && (<>
            <MetricItem value={Number(svc.metrics.requestsTotal ?? 0).toLocaleString()} label={t('pages:status.requestsTotal')} />
            <MetricItem value={`${Number(svc.metrics.requestDurationP50Ms ?? 0).toFixed(1)}ms`} label={t('pages:status.requestDurationP50')} />
            <MetricItem value={`${Number(svc.metrics.requestDurationP99Ms ?? 0).toFixed(1)}ms`} label={t('pages:status.requestDurationP99')} />
            <MetricItem value={formatTokens(Number(svc.metrics.tokensPromptTotal ?? 0))} label={t('pages:status.tokensPrompt')} />
            <MetricItem value={formatTokens(Number(svc.metrics.tokensCompletionTotal ?? 0))} label={t('pages:status.tokensCompletion')} />
            <MetricItem value={Number(svc.metrics.errorsTotal ?? 0).toLocaleString()} label={t('pages:status.errorsTotal')} />
          </>)}
          {serviceName === 'compliance-proxy' && (<>
            <MetricItem value={Number(svc.metrics.connectionsActive ?? 0).toLocaleString()} label={t('pages:status.connectionsActive')} />
            <MetricItem value={Number(svc.metrics.connectionsTotal ?? 0).toLocaleString()} label={t('pages:status.connectionsTotal')} />
            <MetricItem value={Number(svc.metrics.connectionsRejected ?? 0).toLocaleString()} label={t('pages:status.connectionsRejected')} />
            <MetricItem value={`${Number(svc.metrics.tlsHandshakeP50Ms ?? 0).toFixed(1)}ms`} label={t('pages:status.tlsHandshake')} />
            <MetricItem value={`${((Number(svc.metrics.certCacheHitRate ?? 0)) * 100).toFixed(1)}%`} label={t('pages:status.certCacheHitRate')} />
            <MetricItem value={Number(svc.metrics.auditQueueDepth ?? 0).toLocaleString()} label={t('pages:status.auditQueueDepth')} />
            <MetricItem
              value={svc.metrics.redisAvailable ? t('pages:status.redisUp') : t('pages:status.redisDown')}
              label={t('pages:status.redisStatus')}
              color={svc.metrics.redisAvailable ? 'var(--color-success)' : 'var(--color-danger)'}
            />
          </>)}
        </div>
      )}

      {/* Metric Breakdown */}
      {breakdownKeys.length > 0 && (
        <Card>
          <div className={styles.breakdownSection}>
            <div className={styles.breakdownHeader}>
              <span className={styles.breakdownTitle}>{t('pages:status.metricBreakdown')}</span>
              <select
                className={styles.breakdownSelect}
                value={activeBreakdownKey}
                onChange={(e) => setSelectedBreakdown(e.target.value)}
              >
                {breakdownKeys.map((key) => (
                  <option key={key} value={key}>
                    {breakdowns[key].label}
                  </option>
                ))}
              </select>
            </div>

            {activeBreakdown && activeBreakdown.items.length > 0 && (
              <table className={styles.breakdownTable}>
                <thead>
                  <tr>
                    <th>{activeBreakdown.label}</th>
                    <th>{t('common:count')}</th>
                    {activeBreakdown.items.some((it) => it.p50Ms != null) && <th>p50</th>}
                    {activeBreakdown.items.some((it) => it.p99Ms != null) && <th>p99</th>}
                  </tr>
                </thead>
                <tbody>
                  {activeBreakdown.items.map((item) => (
                    <tr key={item.value}>
                      <td>{item.value}</td>
                      <td>{item.count.toLocaleString()}</td>
                      {activeBreakdown.items.some((it) => it.p50Ms != null) && (
                        <td>{item.p50Ms != null ? `${item.p50Ms.toFixed(1)}ms` : '-'}</td>
                      )}
                      {activeBreakdown.items.some((it) => it.p99Ms != null) && (
                        <td>{item.p99Ms != null ? `${item.p99Ms.toFixed(1)}ms` : '-'}</td>
                      )}
                    </tr>
                  ))}
                </tbody>
              </table>
            )}

            {activeBreakdown && activeBreakdown.items.length === 0 && (
              <p className={styles.emptyText}>{t('pages:status.noBreakdownData', 'No breakdown data available.')}</p>
            )}
          </div>
        </Card>
      )}

      {/* Runtime */}
      {svc && (
        <Card>
          <div className={styles.sectionTitle}>{t('pages:status.runtime')}</div>
          <div className={styles.runtimeGrid}>
            <MetricItem value={String(svc.runtime.goroutines)} label={t('pages:status.goroutines')} />
            <MetricItem value={`${svc.runtime.heapAllocMB.toFixed(1)} MB`} label={t('pages:status.heapAlloc')} />
            <MetricItem value={`${svc.runtime.heapSysMB.toFixed(1)} MB`} label={t('pages:status.heapSys')} />
            <MetricItem value={`${svc.runtime.gcPauseP50Ms.toFixed(2)}ms`} label={t('pages:status.gcPause')} />
            <MetricItem value={String(svc.runtime.gcCount)} label={t('pages:status.gcCount')} />
            <MetricItem value={String(svc.runtime.threads)} label={t('pages:status.threads')} />
          </div>
        </Card>
      )}

      {/* Instances */}
      <Card>
        <div className={styles.sectionTitle}>
          {t('pages:status.instances')} ({instances.length})
        </div>
        {instances.length > 0 ? (
          <table className={styles.instanceTable}>
            <thead>
              <tr>
                <th>{t('pages:status.colInstance')}</th>
                <th>{t('pages:status.colStatus')}</th>
                <th>{t('pages:status.colVersion')}</th>
                <th>{t('pages:status.colRegisteredAt')}</th>
                <th>{t('pages:status.colLastHeartbeat')}</th>
              </tr>
            </thead>
            <tbody>
              {instances.map((inst) => {
                const registeredAt = formatDateTime(inst.registeredAt);
                const ago = inst.lastHeartbeatAt
                  ? t('pages:status.agoSeconds', { seconds: Math.round((Date.now() - new Date(inst.lastHeartbeatAt).getTime()) / 1000) })
                  : '-';
                const statusColor = instanceStatusColor(inst.status);
                return (
                  <tr key={inst.instanceId}>
                    <td><code style={{ fontSize: 'var(--g-font-size-xs)' }}>{inst.instanceId}</code></td>
                    <td>
                      <span className={styles.dot} style={{ background: statusColor }} />
                      <span style={{ color: statusColor }}>{' '}{inst.status}</span>
                    </td>
                    <td>{inst.version}</td>
                    <td>{registeredAt}</td>
                    <td>{ago}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        ) : (
          <p className={styles.emptyText}>{t('pages:status.noServicesRegistered')}</p>
        )}
      </Card>
      {/* DELETE /api/admin/instances/:id was removed in spec §10.5;
          instance lifecycle is owned by Hub's Thing Registry now. */}
    </Stack>
  );
}

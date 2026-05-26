import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import type { ServiceMetricSet, ServiceInstanceInfo } from '@/api/services/infrastructure/misc/system';
import styles from './ServiceCard.module.css';
import { formatTokens } from '@/lib/format';

interface ServiceCardProps {
  serviceName: string;
  metricSet: ServiceMetricSet;
  instances: ServiceInstanceInfo[];
  serviceSummary: { healthy: number; total: number; degraded: number; unhealthy: number; offline: number };
}

/* ── Helpers ────────────────────────────────────────────────────────── */

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

/* ── Component ──────────────────────────────────────────────────────── */

export function ServiceCard({
  serviceName,
  metricSet,
  instances,
  serviceSummary,
}: ServiceCardProps) {
  const { t } = useTranslation();
  const [runtimeOpen, setRuntimeOpen] = useState(false);
  const [instancesOpen, setInstancesOpen] = useState(false);

  const svc = metricSet;

  return (
    <div className={styles.card}>
      {/* ── Header ─────────────────────────────────────────────── */}
      <div className={styles.header}>
        <div className={styles.headerLeft}>
          <span className={styles.dot} style={{ background: summaryDotColor(serviceSummary) }} />
          <span className={styles.serviceName}>{serviceName}</span>
          <span className={styles.instanceSummary}>
            {serviceSummary.healthy} {t('pages:status.healthy')} / {serviceSummary.total} {t('pages:status.instances').toLowerCase()}
          </span>
        </div>
        <Link to={`/status/services/${serviceName}`} className={styles.viewLink}>
          {t('pages:status.viewDetail')}
        </Link>
      </div>

      {/* ── Business Metrics ───────────────────────────────────── */}
      <div className={styles.metricsRow}>
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

      {/* ── Runtime (collapsed) ────────────────────────────────── */}
      <button
        type="button"
        className={styles.collapseToggle}
        onClick={() => setRuntimeOpen(prev => !prev)}
      >
        <span className={runtimeOpen ? styles.arrowOpen : styles.arrow}>&#9654;</span>
        {t('pages:status.runtime')}
      </button>
      {runtimeOpen && (
        <div className={styles.runtimeGrid}>
          <MetricItem value={String(svc.runtime.goroutines)} label={t('pages:status.goroutines')} />
          <MetricItem value={`${svc.runtime.heapAllocMB.toFixed(1)} MB`} label={t('pages:status.heapAlloc')} />
          <MetricItem value={`${svc.runtime.heapSysMB.toFixed(1)} MB`} label={t('pages:status.heapSys')} />
          <MetricItem value={`${svc.runtime.gcPauseP50Ms.toFixed(2)}ms`} label={t('pages:status.gcPause')} />
          <MetricItem value={String(svc.runtime.gcCount)} label={t('pages:status.gcCount')} />
          <MetricItem value={String(svc.runtime.threads)} label={t('pages:status.threads')} />
        </div>
      )}

      {/* ── Instances (collapsed) ──────────────────────────────── */}
      <button
        type="button"
        className={styles.collapseToggle}
        onClick={() => setInstancesOpen(prev => !prev)}
      >
        <span className={instancesOpen ? styles.arrowOpen : styles.arrow}>&#9654;</span>
        {t('pages:status.instances')} ({instances.length})
      </button>
      {instancesOpen && instances.length > 0 && (
        <div className={styles.tableWrapper}>
          <table className={styles.table}>
            <thead>
              <tr>
                <th>{t('pages:status.colInstance')}</th>
                <th>{t('pages:status.colStatus')}</th>
                <th>{t('pages:status.colRegisteredAt')}</th>
                <th>{t('pages:status.colLastHeartbeat')}</th>
              </tr>
            </thead>
            <tbody>
              {instances.map(inst => {
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
                    <td>{registeredAt}</td>
                    <td>{ago}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

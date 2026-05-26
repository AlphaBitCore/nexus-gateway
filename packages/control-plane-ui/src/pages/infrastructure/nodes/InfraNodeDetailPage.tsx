/**
 * Node detail page — renders the 5-tab IA:
 *
 *   Overview · Configuration · Runtime · Metrics · Diagnostics
 *
 * The "Configuration" tab is owned by ConfigurationTab, which renders the
 * merged 4-column view, force-resync surface, and override editor wiring.
 * Config data is consumed via the applied-config endpoint.
 */
import { useParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { hubApi } from '@/api/services/infrastructure/nodes/hub';
import type { Node } from '@/api/services/infrastructure/nodes/hub';
import {
  Stack, Card, Badge, Skeleton, ErrorBanner, Breadcrumb,
  Tabs, TabsList, TabsTrigger, TabsContent,
} from '@/components/ui';
import { ConfigurationTab } from '../_shared/tabs/config/ConfigurationTab';
import { MetadataPanel } from '../_shared/panels/MetadataPanel';
import { MetricsTab } from '../_shared/tabs/metrics/MetricsTab';
import { LogsTab } from '../_shared/tabs/logs/LogsTab';
import { RuntimeStateTab } from '../_shared/tabs/runtime/RuntimeStateTab';
import { NodeLoginHistoryTab } from '../_shared/tabs/runtime/NodeLoginHistoryTab';
import { NodeTrafficTab } from '../_shared/tabs/runtime/NodeTrafficTab';
import { ThingStatsTab } from '../_shared/tabs/metrics/ThingStatsTab';
import { isThingStatsType } from '../_shared/tabs/metrics/thingStatsMetricCatalog';
import styles from './InfraNodeDetailPage.module.css';

function statusVariant(status: string): 'success' | 'warning' | 'default' | 'danger' {
  const map: Record<string, 'success' | 'warning' | 'default' | 'danger'> = {
    online: 'success',
    enrolled: 'warning',
    offline: 'default',
    'out-of-sync': 'danger',
    revoked: 'danger',
  };
  return map[status.toLowerCase()] ?? 'default';
}

function InfoRow({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className={styles.infoRow}>
      <dt className={styles.infoLabel}>{label}</dt>
      <dd className={styles.infoValue}>{value ?? '—'}</dd>
    </div>
  );
}

// Format a process-uptime duration starting at the given ISO timestamp.
// We round to the nearest minute under an hour, then nearest 10 minutes
// for "Xh Ym", then days. Falls back to the raw timestamp when parsing
// fails. Returns "—" when no anchor was reported yet.
function formatUptime(processStartedAt: string | null | undefined): string {
  if (!processStartedAt) return '—';
  const t0 = Date.parse(processStartedAt);
  if (Number.isNaN(t0)) return processStartedAt;
  const secs = Math.max(0, Math.floor((Date.now() - t0) / 1000));
  if (secs < 60) return `${secs}s`;
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${mins}m`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) {
    const m = Math.floor((mins - hours * 60) / 10) * 10;
    return m === 0 ? `${hours}h` : `${hours}h ${m}m`;
  }
  const days = Math.floor(hours / 24);
  const h = hours - days * 24;
  return h === 0 ? `${days}d` : `${days}d ${h}h`;
}

export default function InfraNodeDetailPage() {
  const { t } = useTranslation();
  const { id } = useParams<{ id: string }>();

  const { data: node, loading: nodeLoading, error: nodeError, refetch: refetchNode } = useApi<Node>(
    () => hubApi.getNode(id ?? ''),
    ['admin', 'node', id],
    { skip: !id },
  );

  if (nodeLoading && !node) return <Skeleton.DetailPageSkeleton />;
  if (nodeError) return <ErrorBanner message={nodeError.message} onRetry={refetchNode} />;
  if (!node) return null;

  return (
    <Stack gap="lg">
      <Breadcrumb items={[
        { label: t('pages:infrastructure.nodesTitle'), to: '/infrastructure/nodes' },
        { label: node.name },
      ]} />

      {/* Header */}
      <div className={styles.headerRow}>
        <h1 className={styles.headerName}>{node.name}</h1>
        <Badge variant="outline">{node.type}</Badge>
        <Badge variant={statusVariant(node.status)}>{node.status}</Badge>
      </div>

      <Tabs defaultValue="overview">
        <TabsList>
          <TabsTrigger value="overview">
            {t('pages:infrastructure.appliedConfig.tabOverview')}
          </TabsTrigger>
          <TabsTrigger value="configuration">
            {t('pages:infrastructure.configuration.tab')}
          </TabsTrigger>
          {/* Runtime introspection (e31-s7) goes through Hub's reverse-call to
              the thing's /debug/runtime. Agent things sit behind NAT on user
              machines and cannot be reverse-called — Hub returns 501 for
              type=agent (runtime_bridge.go:62). Hide the tab rather than
              render a perpetual error. */}
          {node.type !== 'agent' && (
            <TabsTrigger value="runtime">
              {t('pages:infrastructure.runtime.tab')}
            </TabsTrigger>
          )}
          <TabsTrigger value="metrics">
            {t('pages:infrastructure.appliedConfig.tabMetrics')}
          </TabsTrigger>
          <TabsTrigger value="diagnostics">
            {t('pages:infrastructure.appliedConfig.tabDiagnostics')}
          </TabsTrigger>
          {node.type === 'agent' && (
            <TabsTrigger value="loginHistory">
              {t('pages:nodeDetail.tabs.loginHistory')}
            </TabsTrigger>
          )}
          {(node.type === 'agent' || node.type === 'ai-gateway' || node.type === 'compliance-proxy') && (
            <TabsTrigger value="traffic">
              {t('pages:nodeDetail.tabs.traffic')}
            </TabsTrigger>
          )}
          {isThingStatsType(node.type) && (
            <TabsTrigger value="stats">
              {t('pages:nodeDetail.tabs.stats', { defaultValue: 'Stats' })}
            </TabsTrigger>
          )}
        </TabsList>

        {/* Node info */}
        <TabsContent value="overview">
          <Card>
            <h2 className={styles.sectionTitle}>{t('pages:infrastructure.nodeInfo')}</h2>
            <dl className={styles.infoGrid}>
              <InfoRow label={t('pages:infrastructure.hostname')} value={node.hostname || node.name} />
              <InfoRow label={t('pages:infrastructure.thingId')} value={<code className={styles.mono}>{node.id}</code>} />
              {node.physicalId && (
                <InfoRow label={t('pages:infrastructure.physicalIdFull')} value={<code className={styles.mono}>{node.physicalId}</code>} />
              )}
              <InfoRow label={t('pages:infrastructure.primaryIp')} value={node.primaryIp ? <code className={styles.mono}>{node.primaryIp}</code> : null} />
              {node.boundUserDisplayName && (
                <InfoRow
                  label={t('pages:infrastructure.boundUser')}
                  value={
                    <span>
                      {node.boundUserDisplayName}
                      {node.boundUserEmail && <span style={{ color: 'var(--color-text-muted)' }}> · {node.boundUserEmail}</span>}
                    </span>
                  }
                />
              )}
              <InfoRow label={t('pages:infrastructure.os')} value={node.os ? `${node.os}${node.osVersion ? ' ' + node.osVersion : ''}` : null} />
              <InfoRow label={t('pages:infrastructure.nodeType')} value={node.type} />
              <InfoRow label={t('pages:infrastructure.status')} value={<Badge variant={statusVariant(node.status)}>{node.status}</Badge>} />
              <InfoRow label={t('pages:infrastructure.version')} value={node.version} />
              <InfoRow label={t('pages:infrastructure.role')} value={node.role} />
              <InfoRow label={t('pages:infrastructure.listenAddress')} value={node.listen_address} />
              <InfoRow label={t('pages:infrastructure.metricsUrl')} value={node.metrics_url} />
              <InfoRow label={t('pages:infrastructure.authType')} value={node.auth_type} />
              <InfoRow label={t('pages:infrastructure.connProtocol')} value={node.conn_protocol} />
              <InfoRow label={t('pages:infrastructure.createdAt')} value={new Date(node.created_at).toLocaleString()} />
              <InfoRow label={t('pages:infrastructure.lastSeen')} value={node.last_seen_at ? new Date(node.last_seen_at).toLocaleString() : null} />
              <InfoRow
                label={t('pages:infrastructure.processStartedAt')}
                value={node.processStartedAt ? new Date(node.processStartedAt).toLocaleString() : null}
              />
              <InfoRow
                label={t('pages:infrastructure.uptime')}
                value={<span title={node.processStartedAt ?? undefined}>{formatUptime(node.processStartedAt)}</span>}
              />
            </dl>
          </Card>

          {/* Metadata — flexible JSONB labels written by selfreg
              / enrollment / sysinfo; rendered below the core InfoRow grid
              so the Overview tab keeps existing layout for repeat visitors. */}
          <Card>
            <MetadataPanel metadata={node.metadata ?? null} />
          </Card>
        </TabsContent>

        {/* Configuration — merged template / override / applied view */}
        <TabsContent value="configuration">
          <Card>
            <ConfigurationTab
              thingId={id ?? ''}
              thingType={node.type}
              appliedOutcomes={node.appliedOutcomes ?? null}
            />
          </Card>
        </TabsContent>

        {/* Runtime introspection (e31-s7) — live in-memory snapshot from the
            service. Server-side things only (see comment on the TabsTrigger). */}
        {node.type !== 'agent' && (
          <TabsContent value="runtime">
            <RuntimeStateTab thingId={id ?? ''} />
          </TabsContent>
        )}

        {/* Ops metrics (runtime + business catalog per spec §11.6) */}
        <TabsContent value="metrics">
          <MetricsTab thingId={id ?? ''} thingType={node.type} />
        </TabsContent>

        {/* Diagnostic events scoped to this thing (cross-tab time-axis sync per §11.8) */}
        <TabsContent value="diagnostics">
          <LogsTab thingId={id ?? ''} />
        </TabsContent>

        {/* Login history — device assignments scoped to this node.
            Only Agent things bind to a human user via DeviceAssignment;
            backend services (gateway / proxy / hub / cp) never have rows. */}
        {node.type === 'agent' && (
          <TabsContent value="loginHistory">
            <Card>
              <NodeLoginHistoryTab nodeId={id ?? ''} />
            </Card>
          </TabsContent>
        )}

        {/* Traffic — recent AI requests emitted by this node. Only meaningful
            for the three data-plane producers that populate traffic_event.thing_id:
            agent (originating device), ai-gateway / compliance-proxy (processing
            service instance). Hub / control-plane never emit traffic_event rows. */}
        {(node.type === 'agent' || node.type === 'ai-gateway' || node.type === 'compliance-proxy') && (
          <TabsContent value="traffic">
            <Card>
              <NodeTrafficTab nodeId={id ?? ''} nodeType={node.type} nodeName={node.name} />
            </Card>
          </TabsContent>
        )}

        {/* Stats — per-Thing pre-aggregated metrics from Hub's thing_metric_rollup_*
            cascade (5m / 1h / 1d / 1mo). Renders KPI cards + trend small-multiples
            + per-dimension breakdown tabs. For Agent things, falls back to the
            "rollup disabled" banner when Hub's enableAgentRollup toggle is OFF. */}
        {isThingStatsType(node.type) && (
          <TabsContent value="stats">
            <Card>
              <ThingStatsTab thingId={id ?? ''} thingType={node.type} />
            </Card>
          </TabsContent>
        )}
      </Tabs>
    </Stack>
  );
}

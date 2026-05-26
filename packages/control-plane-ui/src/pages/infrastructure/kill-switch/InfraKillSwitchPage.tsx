import { useState, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { hubApi } from '@/api/services/infrastructure/nodes/hub';
import type { ConfigHistoryEvent, Node } from '@/api/services/infrastructure/nodes/hub';
import { complianceApi } from '@/api/services/compliance/compliance';
import {
  PageHeader, Stack, Card, Button, Badge, AlertDialog,
  DataTable, LoadingSpinner, ErrorBanner,
} from '@/components/ui';
import type { DataTableColumn } from '@/components/ui';
import styles from './InfraKillSwitchPage.module.css';

// Per-type rollup that drives the "Compliance Proxies: 3 of 3 active"
// status panel. Engaged means the node's appliedConfig.killswitch is on,
// i.e. it has stopped TLS bumping. Active = the inverse.
interface FleetRollup {
  total: number;
  engaged: number;
  active: number;
}

// rollupForNodes walks the nodes list and counts engaged vs active for
// the per-Thing-type status breakdown. Nodes whose appliedConfig has
// not yet arrived are counted as active (the conservative reading —
// they were last known to be bumping).
function rollupForNodes(nodes: Node[] | undefined): FleetRollup {
  const out: FleetRollup = { total: 0, engaged: 0, active: 0 };
  if (!nodes) return out;
  for (const node of nodes) {
    out.total += 1;
    const applied = node.appliedConfig as Record<string, unknown> | null;
    const ks = applied?.killswitch as Record<string, unknown> | undefined;
    if (ks?.engaged === true) {
      out.engaged += 1;
    } else {
      out.active += 1;
    }
  }
  return out;
}

export default function InfraKillSwitchPage() {
  const { t } = useTranslation('pages');
  const [confirmOpen, setConfirmOpen] = useState(false);

  // Compliance-proxy and agent nodes are fetched separately so the
  // per-type status panel can show distinct counts even though the
  // kill-switch toggle fan-out (handled CP-side per PR-6) targets
  // both. Two independent queryKeys keep the React Query caches from
  // colliding.
  const {
    data: proxyNodes,
    loading: proxyNodesLoading,
    error: proxyNodesError,
    refetch: refetchProxyNodes,
  } = useApi(
    () => hubApi.listNodes({ type: 'compliance-proxy' }),
    ['admin', 'nodes', 'kill-switch', 'compliance-proxy'],
  );

  const {
    data: agentNodes,
    loading: agentNodesLoading,
    error: agentNodesError,
    refetch: refetchAgentNodes,
  } = useApi(
    () => hubApi.listNodes({ type: 'agent' }),
    ['admin', 'nodes', 'kill-switch', 'agent'],
  );

  // History fetched per-type and merged client-side. The Hub /config-sync/history
  // endpoint filters by a single nodeType; we issue two queries and concatenate
  // so the operator sees a unified timeline of every kill-switch toggle.
  const {
    data: proxyHistory,
    loading: proxyHistoryLoading,
    error: proxyHistoryError,
    refetch: refetchProxyHistory,
  } = useApi(
    () => hubApi.listConfigHistory({ nodeType: 'compliance-proxy', configKey: 'killswitch' }),
    ['admin', 'config-sync-history', 'kill-switch', 'compliance-proxy'],
  );

  const {
    data: agentHistory,
    loading: agentHistoryLoading,
    error: agentHistoryError,
    refetch: refetchAgentHistory,
  } = useApi(
    () => hubApi.listConfigHistory({ nodeType: 'agent', configKey: 'killswitch' }),
    ['admin', 'config-sync-history', 'kill-switch', 'agent'],
  );

  const proxyRollup = useMemo(() => rollupForNodes(proxyNodes?.nodes), [proxyNodes]);
  const agentRollup = useMemo(() => rollupForNodes(agentNodes?.nodes), [agentNodes]);

  // Fleet-wide state derived from the union of both Thing types:
  // the kill switch is considered ENGAGED if ANY compliance-proxy or
  // agent has it engaged. This matches operator intent — once even one
  // node has stopped bumping, the operator's emergency action is in
  // flight and the UI should not mislead them with a "DISENGAGED" badge.
  const fleetEngaged = useMemo(() => {
    const allNodes = [...(proxyNodes?.nodes ?? []), ...(agentNodes?.nodes ?? [])];
    for (const node of allNodes) {
      const applied = node.appliedConfig as Record<string, unknown> | null;
      const ks = applied?.killswitch as Record<string, unknown> | undefined;
      if (ks?.engaged === true) return true;
    }
    return false;
  }, [proxyNodes, agentNodes]);

  // Most-recent toggle metadata (changedBy / changedAt / reason) is
  // surfaced from the first node that carries an appliedConfig.killswitch
  // block. Used by the "Current Status" panel.
  const killSwitchMeta = useMemo(() => {
    const allNodes = [...(proxyNodes?.nodes ?? []), ...(agentNodes?.nodes ?? [])];
    for (const node of allNodes) {
      const applied = node.appliedConfig as Record<string, unknown> | null;
      const ks = applied?.killswitch as Record<string, unknown> | undefined;
      if (ks != null) {
        return {
          changedBy: (ks.changedBy as string) ?? 'system',
          changedAt: (ks.changedAt as string) ?? node.updated_at,
          reason: (ks.reason as string) ?? '',
        };
      }
    }
    return { changedBy: 'unknown', changedAt: '', reason: '' };
  }, [proxyNodes, agentNodes]);

  // Single toggle mutation: the CP admin API at /api/admin/compliance/killswitch
  // owns the canonical kill-switch toggle. It (a) fans out across both
  // compliance-proxy AND agent template rows in one call, (b) stamps a
  // dedicated `kill-switch.toggle` admin-audit event the SIEM bridge keys
  // off, and (c) gates on the narrow `admin:kill-switch.toggle` IAM verb.
  // The generic /api/admin/config-sync/update endpoint accepts a single
  // nodeType, emits a `node.update` audit row, and gates on the broader
  // `admin:settings.update` verb — all of which are wrong for kill-switch.
  const toggleMutation = useMutation(
    (nextEngaged: boolean) => complianceApi.setKillSwitch({ engaged: nextEngaged }),
    {
      successMessage: t('infrastructure.killSwitchToggled', 'Kill switch toggled'),
      invalidateQueries: [
        ['admin', 'nodes', 'kill-switch', 'compliance-proxy'],
        ['admin', 'nodes', 'kill-switch', 'agent'],
        ['admin', 'config-sync-history', 'kill-switch', 'compliance-proxy'],
        ['admin', 'config-sync-history', 'kill-switch', 'agent'],
      ],
      onSuccess: () => {
        refetchProxyNodes();
        refetchAgentNodes();
        refetchProxyHistory();
        refetchAgentHistory();
      },
    },
  );

  const handleConfirmToggle = () => {
    const nextState = !fleetEngaged;
    setConfirmOpen(false);
    void toggleMutation.mutate(nextState);
  };

  // Merged history: concatenate both per-type lists then sort newest-first
  // on createdAt. Filtering by configKey is already done at the API call,
  // so every row is a kill-switch toggle.
  const mergedHistory = useMemo<ConfigHistoryEvent[]>(() => {
    const proxy = proxyHistory?.events ?? [];
    const agent = agentHistory?.events ?? [];
    const out = [...proxy, ...agent];
    out.sort((a, b) => {
      const at = new Date(a.createdAt).getTime();
      const bt = new Date(b.createdAt).getTime();
      return bt - at;
    });
    return out;
  }, [proxyHistory, agentHistory]);

  const historyColumns: DataTableColumn<ConfigHistoryEvent>[] = [
    {
      key: 'createdAt',
      label: t('infrastructure.timestamp', 'Timestamp'),
      render: (row) => new Date(row.createdAt).toLocaleString(),
      sortable: true,
    },
    {
      key: 'nodeType',
      label: t('infrastructure.nodeType', 'Type'),
      render: (row) => row.nodeType === 'agent'
        ? t('infrastructure.typeAgent', 'Agent')
        : t('infrastructure.typeComplianceProxy', 'Compliance Proxy'),
      sortable: true,
    },
    { key: 'action', label: t('infrastructure.action', 'Action'), sortable: true },
    { key: 'actorName', label: t('infrastructure.actor', 'Actor'), sortable: true },
    { key: 'newVersion', label: t('infrastructure.version'), sortable: true },
  ];

  const anyLoading = (proxyNodesLoading && !proxyNodes) || (agentNodesLoading && !agentNodes);
  if (anyLoading) return <LoadingSpinner />;

  // Show a non-fatal banner if EITHER per-type query failed, but still
  // render the page using whichever data did arrive — operators may
  // need to engage the kill switch even when one fleet is unreachable.
  const fleetError = proxyNodesError ?? agentNodesError;
  if (fleetError && !proxyNodes && !agentNodes) {
    return <ErrorBanner message={fleetError.message} onRetry={() => { refetchProxyNodes(); refetchAgentNodes(); }} />;
  }

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('infrastructure.killSwitchTitle')}
        subtitle={t('infrastructure.killSwitchDescription')}
      />

      {/* Toggle control */}
      <Card>
        <div className={styles.toggleSection}>
          <Badge variant={fleetEngaged ? 'danger' : 'success'}>
            {fleetEngaged
              ? t('infrastructure.killSwitchEngaged', 'ENGAGED')
              : t('infrastructure.killSwitchDisengaged', 'Normal operation')}
          </Badge>
          <Button
            variant={fleetEngaged ? 'secondary' : 'danger'}
            size="md"
            loading={toggleMutation.loading}
            onClick={() => setConfirmOpen(true)}
          >
            {fleetEngaged
              ? t('infrastructure.disengageKillSwitch', 'Disengage Kill Switch')
              : t('infrastructure.engageKillSwitch', 'Engage Kill Switch')}
          </Button>
        </div>
      </Card>

      {/* Per-type status breakdown */}
      <Card>
        <div className={styles.sectionTitle}>{t('infrastructure.currentStatus', 'Current Status')}</div>
        <div className={styles.statusGrid}>
          <div>
            <div className={styles.kvLabel}>{t('infrastructure.killSwitchStatusComplianceProxy', 'Compliance Proxies')}</div>
            <div className={styles.kvValue}>
              {proxyRollup.engaged > 0
                ? t('infrastructure.killSwitchStatusEngaged', { count: proxyRollup.engaged, total: proxyRollup.total })
                : t('infrastructure.killSwitchStatusActive', { count: proxyRollup.active, total: proxyRollup.total })}
            </div>
          </div>
          <div>
            <div className={styles.kvLabel}>{t('infrastructure.killSwitchStatusAgent', 'Agents')}</div>
            <div className={styles.kvValue}>
              {agentRollup.engaged > 0
                ? t('infrastructure.killSwitchStatusEngaged', { count: agentRollup.engaged, total: agentRollup.total })
                : t('infrastructure.killSwitchStatusActive', { count: agentRollup.active, total: agentRollup.total })}
            </div>
          </div>
          <div>
            <div className={styles.kvLabel}>{t('infrastructure.changedBy', 'Changed By')}</div>
            <div className={styles.kvValue}>{killSwitchMeta.changedBy || '—'}</div>
          </div>
          <div>
            <div className={styles.kvLabel}>{t('infrastructure.changedAt', 'Changed At')}</div>
            <div className={styles.kvValue}>
              {killSwitchMeta.changedAt ? new Date(killSwitchMeta.changedAt).toLocaleString() : '—'}
            </div>
          </div>
          {killSwitchMeta.reason && (
            <div>
              <div className={styles.kvLabel}>{t('infrastructure.reason', 'Reason')}</div>
              <div className={styles.kvValue}>{killSwitchMeta.reason}</div>
            </div>
          )}
        </div>
      </Card>

      {/* Merged history */}
      <Card>
        <div className={styles.sectionTitle}>{t('infrastructure.killSwitchHistory', 'Kill Switch History')}</div>
        {(proxyHistoryLoading || agentHistoryLoading) && mergedHistory.length === 0 ? (
          <LoadingSpinner size="sm" />
        ) : (proxyHistoryError || agentHistoryError) && mergedHistory.length === 0 ? (
          <ErrorBanner
            message={(proxyHistoryError ?? agentHistoryError)?.message ?? 'history error'}
            onRetry={() => { refetchProxyHistory(); refetchAgentHistory(); }}
          />
        ) : (
          <DataTable<ConfigHistoryEvent>
            columns={historyColumns}
            data={mergedHistory}
            hideSearch
            emptyMessage={t('infrastructure.noHistory', 'No kill switch history')}
            loading={proxyHistoryLoading || agentHistoryLoading}
            frameless
          />
        )}
      </Card>

      {/* Confirmation dialog */}
      <AlertDialog
        open={confirmOpen}
        onOpenChange={setConfirmOpen}
        title={
          fleetEngaged
            ? t('infrastructure.confirmDisengageTitle', 'Disengage Kill Switch?')
            : t('infrastructure.confirmEngageTitle', 'Engage Kill Switch?')
        }
        description={
          fleetEngaged
            ? t('infrastructure.confirmDisengageDesc', 'This will resume normal TLS bumping across compliance proxies and agents.')
            : t('infrastructure.confirmEngageDesc', 'This will immediately stop TLS bumping on ALL Compliance Proxies and Agents in the fleet. Use only for emergency: a bad provider rollout, a hook regression blocking legitimate traffic, or a NetworkExtension panic.')
        }
        confirmLabel={
          fleetEngaged
            ? t('infrastructure.disengageKillSwitch', 'Disengage Kill Switch')
            : t('infrastructure.engageKillSwitch', 'Engage Kill Switch')
        }
        cancelLabel={t('infrastructure.cancel', 'Cancel')}
        onConfirm={handleConfirmToggle}
        variant={fleetEngaged ? 'default' : 'danger'}
      />
    </Stack>
  );
}

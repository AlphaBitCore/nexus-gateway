/**
 * NodeTrafficTab — recent traffic events emitted by a specific node (thing_id).
 *
 * Reuses the canonical `getColumnsForSource` from `/traffic` so the columns
 * shown here exactly match what the main page renders when filtered by the
 * corresponding source tab. Rows are clickable and open the same
 * `TrafficEventDrawer` used by `/traffic`, so operators can drill into a
 * single event without leaving the node-detail page.
 *
 * Calls `GET /api/admin/traffic?thingId=:nodeId&limit=50`. The semantic of
 * "emitted" varies by nodeType: for `agent` it's the originating device;
 * for `ai-gateway` / `compliance-proxy` it's the processing service. The
 * subtitle below the table makes this explicit, and the source filter
 * passed to `getColumnsForSource` picks the right column set for each.
 *
 * The "View all" link jumps to `/traffic?thingId=…&source=…` — combined
 * with the URL-driven tab state on the Traffic page, the user lands on
 * the same source tab they were viewing here.
 */
import { useCallback, useEffect, useLayoutEffect, useState } from 'react';
import { Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { api } from '@/api/client';
import { devicesApi } from '@/api/services';
import { DataTable, ErrorBanner, Skeleton } from '@/components/ui';
import type { TrafficEvent } from '@/api/types';
import { getColumnsForSource, type TrafficSourceFilter } from '@/pages/traffic/list/TrafficTab';
import { TrafficEventDrawer, DRAWER_MS } from '@/pages/traffic/audit-drawer/trafficAuditDrawer';

interface Props {
  nodeId: string;
  /** Drives the subtitle wording, source filter, and the "view full traffic" link. */
  nodeType: string;
  /** Optional human-readable name passed into the cross-link as ?thingName=. */
  nodeName?: string;
}

/**
 * Pick the `TrafficSourceFilter` value that matches the calling node's role.
 * Returning `''` (All) for unrecognised types keeps the panel from breaking
 * if a new node type gains a traffic surface before this map is updated.
 */
function sourceForNodeType(nodeType: string): TrafficSourceFilter {
  if (nodeType === 'agent') return 'agent';
  if (nodeType === 'compliance-proxy') return 'proxy';
  if (nodeType === 'ai-gateway') return 'vk';
  return '';
}

export function NodeTrafficTab({ nodeId, nodeType, nodeName }: Props) {
  const { t } = useTranslation();
  const source = sourceForNodeType(nodeType);

  // Subtitle disambiguates the "traffic" semantic, which differs by node
  // type — without this the table silently means two unrelated things.
  const subtitleKey =
    nodeType === 'agent'
      ? 'pages:nodeDetail.traffic.subtitleAgent'
      : nodeType === 'compliance-proxy'
        ? 'pages:nodeDetail.traffic.subtitleProxy'
        : 'pages:nodeDetail.traffic.subtitleGateway';

  // Cross-link to the main Live Traffic page pre-filtered by thingId AND
  // pre-selecting the source tab that matches this node's role.
  const fullTrafficHref = (() => {
    const qs = new URLSearchParams();
    qs.set('thingId', nodeId);
    if (nodeName) qs.set('thingName', nodeName);
    if (source) qs.set('source', source);
    return `/traffic?${qs.toString()}`;
  })();

  const { data, loading, error, refetch } = useApi<{ data: TrafficEvent[]; total: number }>(
    () =>
      api
        .get<{ data: TrafficEvent[]; total: number }>('/api/admin/traffic', {
          thingId: nodeId,
          limit: '50',
          offset: '0',
        })
        .catch(() => ({ data: [], total: 0 })),
    ['admin', 'nodes', 'traffic', nodeId],
  );

  // Drawer state — same animate-in pattern as TrafficTab so the transition
  // doesn't flicker when a different row is clicked rapidly.
  const [selectedEntry, setSelectedEntry] = useState<TrafficEvent | null>(null);
  const [drawerVisible, setDrawerVisible] = useState(false);

  const closeDrawer = useCallback(() => {
    setDrawerVisible(false);
    window.setTimeout(() => setSelectedEntry(null), DRAWER_MS);
  }, []);

  useLayoutEffect(() => {
    if (!selectedEntry) {
      setDrawerVisible(false);
      return;
    }
    setDrawerVisible(false);
    const raf = window.requestAnimationFrame(() => {
      window.requestAnimationFrame(() => setDrawerVisible(true));
    });
    return () => window.cancelAnimationFrame(raf);
  }, [selectedEntry?.id]);

  useEffect(() => {
    if (!selectedEntry) return;
    const onKey = (ev: KeyboardEvent) => {
      if (ev.key === 'Escape') closeDrawer();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [selectedEntry, closeDrawer]);

  // Compliance visibility: bypassBundles is a fleet-wide agent_settings
  // default, so the exemption count is the same for every agent node. Surface
  // it here — the place an auditor reviews a node's traffic — so a "this app
  // is not inspected" carve-out can never be silently in effect. Only agent
  // nodes have a bypass list; the GET is shared (same queryKey) with the
  // Device Defaults settings page, so it costs no extra round-trip there.
  const { data: agentSettings } = useApi(
    () => devicesApi.getAgentSettings().catch(() => null),
    ['admin', 'settings', 'device-defaults'],
  );
  const exemptCount = source === 'agent' ? (agentSettings?.bypassBundles?.length ?? 0) : 0;

  const columns = getColumnsForSource(source, t);

  if (loading) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner error={error} onRetry={refetch} />;

  return (
    <div>
      <p style={{ color: 'var(--color-text-muted)', fontSize: 'var(--g-font-size-sm)', margin: '0 0 var(--g-space-3) 0' }}>
        {t(subtitleKey)}
      </p>
      {exemptCount > 0 && (
        <p
          style={{
            display: 'inline-flex',
            alignItems: 'center',
            gap: 'var(--g-space-2)',
            color: 'var(--color-warning-text)',
            background: 'var(--color-warning-bg)',
            border: '1px solid var(--color-warning-border)',
            borderRadius: 'var(--g-radius-md)',
            padding: 'var(--g-space-1) var(--g-space-3)',
            fontSize: 'var(--g-font-size-sm)',
            margin: '0 0 var(--g-space-3) 0',
          }}
        >
          {t('pages:nodeDetail.traffic.exemptBadge', { count: exemptCount })}
        </p>
      )}
      <DataTable
        hideSearch
        columns={columns}
        data={data?.data ?? []}
        emptyMessage={t('pages:nodeDetail.traffic.noTraffic')}
        onRowClick={setSelectedEntry}
      />
      <div style={{ marginTop: 'var(--g-space-3)', fontSize: 'var(--g-font-size-sm)' }}>
        <Link to={fullTrafficHref}>{t('pages:nodeDetail.traffic.viewAll')} →</Link>
      </div>

      {selectedEntry && (
        <TrafficEventDrawer
          selectedEntry={selectedEntry}
          drawerVisible={drawerVisible}
          onClose={closeDrawer}
        />
      )}
    </div>
  );
}

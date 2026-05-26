import { useState, useRef, useCallback, useLayoutEffect, useEffect } from 'react';
import clsx from 'clsx';
import { useTranslation } from 'react-i18next';
import { useSearchParams } from 'react-router-dom';
import { TrafficFileSinkNotice } from '../list/TrafficFileSinkNotice';
import { useApi } from '../../../hooks/useApi';
import { systemApi } from '@/api/services';
import {
  DataTable,
  Badge,
  statusToVariant,
  LoadingSpinner,
  ErrorBanner,
  Stack,
  ListPagination,
  DEFAULT_ADMIN_LIST_PAGE_SIZE,
  type AdminListPageSize,
} from '@/components/ui';
import type { BadgeVariant } from '@/components/ui/Badge/Badge';
import type { TrafficEvent, TrafficStorageResponse } from '../../../api/types';
import { DRAWER_MS, TrafficEventDrawer } from '../audit-drawer/trafficAuditDrawer';
import { LiveTrafficActiveFiltersBar } from '../filters/LiveTrafficActiveFiltersBar';
import { LiveTrafficFilterPanel } from '../filters/LiveTrafficFilterPanel';
import { ComplianceTagChipList } from '../list/ComplianceTagChips';
import { LatencyMini } from '@/components/charts/LatencyMini';
import { formatTime, formatUsdSci, formatTokens } from '@/lib/format';
import {
  EMPTY_LIVE_TRAFFIC_FILTERS,
  buildTrafficAuditLogQueryParams,
  toDatetimeLocalValue,
  type LiveTrafficFiltersState,
  type TrafficSourceFilter,
} from '../filters/liveTrafficFilters';
import css from '../analytics/TrafficAnalyticsPage.module.css';

/* -- Source filter type -- */

export type { TrafficSourceFilter } from '../filters/liveTrafficFilters';

interface TrafficTabProps {
  source: TrafficSourceFilter;
}

/* -- Helpers -- */

// Names only \u2014 when a row's name field is empty we render an em-dash rather
// than falling back to a raw UUID/id. Operators triage by name; showing
// truncated IDs added noise without helping identify the entity.
function resolvedName(name?: string | null, _id?: string | null): string {
  if (name) return name;
  return '\u2014';
}

// Identity accessors: the backend writes a structured identity JSON whose
// populated sub-fields vary by source (vk: user+credential, agent: device+user,
// proxy: usually empty). Fall back to entity/org fields when identity is absent.
function idUser(e: TrafficEvent): { id?: string | null; name?: string | null } {
  const u = e.identity?.user;
  if (u) return { id: u.id, name: u.name };
  if (e.entityType === 'user') return { id: e.entityId, name: e.entityName };
  return {};
}
// idProject mirrors idUser for application-VK traffic: ai-gateway calls
// made with VKType="application" land with entityType="project" and
// identity.project populated. Pre-fix these rows leaked through as
// entityType="user" with a VK-name slug in entityId — undisplayable.
function idProject(e: TrafficEvent): { id?: string | null; name?: string | null } {
  const p = e.identity?.project;
  if (p) return { id: p.id, name: p.name };
  if (e.entityType === 'project') return { id: e.entityId, name: e.entityName };
  return {};
}
function idDevice(e: TrafficEvent): { id?: string | null; name?: string | null } {
  const d = e.identity?.device;
  if (d) return { id: d.id, name: d.hostname };
  if (e.entityType === 'device') return { id: e.entityId, name: e.entityName };
  // Fallback for agent traffic: traffic_event.thing_id / thing_name are
  // always populated by Hub on agent uploads (the device is the emitting
  // agent itself). identity / entityType aren't filled today because the
  // agent's local audit pipeline doesn't run user attribution, but the
  // thing identity is enough to display SOMETHING in the Device column.
  if (e.thingId) return { id: e.thingId, name: e.thingName || e.thingId };
  return {};
}
function idCredential(e: TrafficEvent): { id?: string | null; name?: string | null } {
  const c = e.identity?.vk;
  if (c) return { id: c.id, name: c.name };
  return {};
}

const SOURCE_BADGE_VARIANT: Record<string, BadgeVariant> = {
  vk: 'info',
  proxy: 'warning',
  agent: 'success',
};

function SourceBadge({ source }: { source: string }) {
  const variant = SOURCE_BADGE_VARIANT[source] ?? 'default';
  return <Badge variant={variant}>{source}</Badge>;
}

/**
 * Compact cell renderer for `complianceTags` in table columns — shows the
 * first 2 chips, then a `+N more` overflow chip so the column width stays
 * predictable even for events with many tags.
 */
function TagsCell({ tags }: { tags?: readonly string[] }) {
  if (!tags || tags.length === 0) return <>-</>;
  const visible = tags.slice(0, 2);
  const overflow = tags.length - visible.length;
  return (
    <ComplianceTagChipList
      tags={overflow > 0 ? [...visible, `+${overflow} more`] : visible}
    />
  );
}

/* -- Per-source column definitions -- */

 
export function getColumnsForSource(source: TrafficSourceFilter, t: (key: string, opts?: any) => string) {
  const timeCol = {
    key: 'timestamp',
    label: t('pages:traffic.colTime'),
    render: (r: TrafficEvent) => formatTime(r.timestamp),
  };
  const statusCol = {
    key: 'statusCode',
    label: t('pages:traffic.colStatus'),
    render: (r: TrafficEvent) => (
      <Badge variant={statusToVariant(r.statusCode && r.statusCode < 400 ? 'healthy' : 'error')}>
        {r.statusCode ?? '-'}
      </Badge>
    ),
  };
  // Use the LatencyMini "row" variant so each list cell shows total +
  // a 3px segmented bar conveying the Us / Upstream split at a glance.
  // Hover surfaces the full breakdown including TTFB and hooks when present.
  // Historical rows with NULL phase columns render a single neutral bar —
  // still total-comparable across rows without faking a breakdown.
  const latencyCol = {
    key: 'latencyMs',
    label: t('pages:traffic.colLatency'),
    render: (r: TrafficEvent) => (
      <LatencyMini
        size="row"
        latencyMs={r.latencyMs}
        upstreamTtfbMs={r.upstreamTtfbMs}
        upstreamTotalMs={r.upstreamTotalMs}
        requestHooksMs={r.requestHooksMs}
        responseHooksMs={r.responseHooksMs}
      />
    ),
  };
  const hookCol = {
    key: 'requestHookDecision',
    label: t('pages:traffic.colHook'),
    render: (r: TrafficEvent) => r.requestHookDecision ?? '-',
  };

  if (source === 'vk') {
    return [
      timeCol,
      // Requested side: literal model the client sent in `{model: "..."}`.
      // For "auto" this is the string "auto"; for code-pinned requests it
      // is e.g. "gpt-4o". Provider is empty for OpenAI-style requests
      // because clients don't pin a provider.
      {
        key: 'requestedModel',
        label: t('pages:traffic.colRequestedModel'),
        render: (r: TrafficEvent) => r.modelName ?? '-',
      },
      // Routed side: provider + model the request was actually dispatched
      // to after routing rules + quota downgrade. Metrics, quota, and
      // cost math all key off this side.
      {
        key: 'routedTarget',
        label: t('pages:traffic.colRoutedTarget'),
        render: (r: TrafficEvent) => {
          const p = r.routedProviderName ?? '';
          const m = r.routedModelName ?? '';
          if (!p && !m) return '-';
          if (p && m) return `${p} / ${m}`;
          return p || m;
        },
      },
      { key: 'user', label: t('pages:traffic.colUser'), render: (r: TrafficEvent) => { const u = idUser(r); return resolvedName(u.name, u.id); } },
      { key: 'orgName', label: t('pages:traffic.colOrganization'), render: (r: TrafficEvent) => resolvedName(r.orgName, r.orgId) },
      { key: 'project', label: t('pages:traffic.colProject'), render: (r: TrafficEvent) => resolvedName(r.identity?.project?.name, r.identity?.project?.id) },
      { key: 'credential', label: t('pages:traffic.colVirtualKey'), render: (r: TrafficEvent) => { const c = idCredential(r); return resolvedName(c.name, c.id); } },
      statusCol,
      latencyCol,
      { key: 'totalTokens', label: t('pages:traffic.colTokens'), render: (r: TrafficEvent) => r.totalTokens != null ? formatTokens(r.totalTokens) : '-' },
      { key: 'upstreamCostUsd', label: t('pages:traffic.colCost'), render: (r: TrafficEvent) => {
          // E58-S5: upstream cost is derived from per-component math (the
          // same decomposition the audit drawer's Costs panel renders).
          // The dropped `estimated_cost_usd` column was the gateway's pre-
          // computed total; we now recompute from tokens × per-million
          // prices snapshotted at LEFT JOIN time. Returns `-` when the
          // pricing snapshot is incomplete (passthrough / deleted model)
          // OR when this is a non-LLM row (no prompt/completion tokens).
          const inputPM = r.modelInputPricePerMillion;
          const outputPM = r.modelOutputPricePerMillion;
          const cacheReadPM = r.modelCachedInputReadPricePerMillion;
          const cacheWritePM = r.modelCachedInputWritePricePerMillion;
          const promptT = r.promptTokens ?? 0;
          const completionT = r.completionTokens ?? 0;
          const cacheReadT = r.cacheReadTokens ?? 0;
          const cacheWriteT = r.cacheCreationTokens ?? 0;
          const uncachedT = Math.max(0, promptT - cacheReadT - cacheWriteT);
          const uncachedCost = (inputPM != null && uncachedT > 0) ? (uncachedT * inputPM / 1_000_000) : 0;
          const cachedReadCost = (cacheReadPM != null && cacheReadT > 0) ? (cacheReadT * cacheReadPM / 1_000_000) : 0;
          const cachedWriteCost = (cacheWritePM != null && cacheWriteT > 0) ? (cacheWriteT * cacheWritePM / 1_000_000) : 0;
          const outputCost = (outputPM != null && completionT > 0) ? (completionT * outputPM / 1_000_000) : 0;
          const total = uncachedCost + cachedReadCost + cachedWriteCost + outputCost;
          return total > 0 ? formatUsdSci(total) : '-';
        } },
      hookCol,
      {
        key: 'cacheStatus',
        label: t('pages:traffic.colCache'),
        render: (r: TrafficEvent) => {
          if (!r.cacheStatus) return <span>-</span>;
          // cacheStatus is the unified rollup (HIT | MISS); gateway/provider
          // breakdown lives in the audit drawer.
          const cls = r.cacheStatus === 'HIT' ? css.hit : css.miss;
          return (
            <span className={clsx(css.cacheBadge, cls)}>
              {t(`pages:traffic.cacheStatus.${r.cacheStatus}`)}
            </span>
          );
        },
      },
    ];
  }

  if (source === 'proxy') {
    return [
      timeCol,
      { key: 'targetHost', label: t('pages:traffic.colTargetHost'), render: (r: TrafficEvent) => r.targetHost ?? '-' },
      { key: 'sourceIp', label: t('pages:traffic.colSourceIp'), render: (r: TrafficEvent) => r.sourceIp ?? '-' },
      { key: 'method', label: t('pages:traffic.colMethod'), render: (r: TrafficEvent) => r.method ?? '-' },
      {
        key: 'path',
        label: t('pages:traffic.colPath'),
        render: (r: TrafficEvent) => {
          const p = r.path ?? '-';
          return p.length > 40 ? <span title={p}>{p.slice(0, 40)}&hellip;</span> : p;
        },
      },
      statusCol,
      latencyCol,
      { key: 'bumpStatus', label: t('pages:traffic.colBumpStatus'), render: (r: TrafficEvent) => r.bumpStatus ?? '-' },
      hookCol,
      { key: 'complianceTags', label: t('pages:traffic.detail.complianceTags'), render: (r: TrafficEvent) => <TagsCell tags={r.complianceTags} /> },
    ];
  }

  if (source === 'agent') {
    return [
      timeCol,
      { key: 'targetHost', label: t('pages:traffic.colTargetHost'), render: (r: TrafficEvent) => r.targetHost ?? '-' },
      {
        key: 'path',
        label: t('pages:traffic.colPath'),
        render: (r: TrafficEvent) => {
          const p = r.path ?? '-';
          return p.length > 40 ? <span title={p}>{p.slice(0, 40)}&hellip;</span> : p;
        },
      },
      { key: 'device', label: t('pages:traffic.colDevice'), render: (r: TrafficEvent) => { const d = idDevice(r); return resolvedName(d.name, d.id); } },
      { key: 'user', label: t('pages:traffic.colUser'), render: (r: TrafficEvent) => { const u = idUser(r); return resolvedName(u.name, u.id); } },
      { key: 'sourceProcess', label: t('pages:traffic.colProcess'), render: (r: TrafficEvent) => r.sourceProcess ?? '-' },
      { key: 'action', label: t('pages:traffic.colAction'), render: (r: TrafficEvent) => r.action ?? '-' },
      statusCol,
      latencyCol,
      hookCol,
      { key: 'complianceTags', label: t('pages:traffic.detail.complianceTags'), render: (r: TrafficEvent) => <TagsCell tags={r.complianceTags} /> },
    ];
  }

  // All traffic (source === '')
  return [
    timeCol,
    { key: 'source', label: t('pages:traffic.colSource'), render: (r: TrafficEvent) => <SourceBadge source={r.source} /> },
    { key: 'targetHost', label: t('pages:traffic.colTarget'), render: (r: TrafficEvent) => r.targetHost ?? '-' },
    { key: 'method', label: t('pages:traffic.colMethod'), render: (r: TrafficEvent) => r.method ?? '-' },
    {
      key: 'path',
      label: t('pages:traffic.colPath'),
      render: (r: TrafficEvent) => {
        const p = r.path ?? '-';
        return p.length > 40 ? <span title={p}>{p.slice(0, 40)}&hellip;</span> : p;
      },
    },
    statusCol,
    latencyCol,
    hookCol,
    { key: 'entity', label: t('pages:traffic.colEntity'), render: (r: TrafficEvent) => resolvedName(r.entityName, r.entityId) },
    { key: 'orgName', label: t('pages:traffic.colOrganization'), render: (r: TrafficEvent) => resolvedName(r.orgName, r.orgId) },
  ];
}

/* -- Traffic Tab -- */

export function TrafficTab({ source }: TrafficTabProps) {
  const { t } = useTranslation();
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

  const [offset, setOffset] = useState(0);
  const [pageLimit, setPageLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);
  // Cross-link entry point: /traffic?thingId=<thing-id>&thingName=<label>
  // is set by the node-detail "View all traffic" link so this page opens
  // pre-filtered to traffic emitted by that node. After init we treat the
  // URL params as a one-way OUTPUT of the filter state — see the effect
  // below that mirrors thingId / thingName back into the URL whenever the
  // applied filter set changes. Without that mirror, a user clearing the
  // Node filter or switching tabs would leave a stale `?thingId=` in the
  // address bar (bug repro: open from node detail, click "Clear filters",
  // URL still shows thingId).
  const [searchParams, setSearchParams] = useSearchParams();
  // Default to 24h range so the quick-range button shows as selected
  const [defaultFilters] = useState<LiveTrafficFiltersState>(() => {
    const end = new Date();
    const start = new Date(end.getTime() - 24 * 3600_000);
    return {
      ...EMPTY_LIVE_TRAFFIC_FILTERS,
      startTime: toDatetimeLocalValue(start),
      endTime: toDatetimeLocalValue(end),
      thingId: searchParams.get('thingId') ?? '',
      _thingLabel: searchParams.get('thingName') ?? '',
    };
  });
  const [draftFilters, setDraftFilters] = useState<LiveTrafficFiltersState>(defaultFilters);
  const [appliedFilters, setAppliedFilters] = useState<LiveTrafficFiltersState>(defaultFilters);
  const [applyTick, setApplyTick] = useState(0);
  const [showAdvanced, setShowAdvanced] = useState(false);

  // Reset filters when switching tabs, but keep the time range
  useEffect(() => {
    setDraftFilters((prev) => ({
      ...EMPTY_LIVE_TRAFFIC_FILTERS,
      startTime: prev.startTime,
      endTime: prev.endTime,
    }));
    setAppliedFilters((prev) => ({
      ...EMPTY_LIVE_TRAFFIC_FILTERS,
      startTime: prev.startTime,
      endTime: prev.endTime,
    }));
    setOffset(0);
  }, [source]);

  // Mirror the active Node filter into the URL so the address bar reflects
  // what's actually applied. Replace-history so rapid filter edits don't
  // pollute the browser back stack. Only thingId + thingName are mirrored —
  // the rest of the filter state stays local because that's a lot of params
  // to drag around for a casual reload (and the time range is the only
  // other state worth preserving, which already round-trips via the time
  // preset buttons).
  useEffect(() => {
    const next = new URLSearchParams(searchParams);
    const id = appliedFilters.thingId.trim();
    const label = appliedFilters._thingLabel.trim();
    if (id) {
      next.set('thingId', id);
      if (label) next.set('thingName', label);
      else next.delete('thingName');
    } else {
      next.delete('thingId');
      next.delete('thingName');
    }
    // Skip the write when nothing actually changes to avoid a redundant
    // navigation tick.
    if (next.toString() !== searchParams.toString()) {
      setSearchParams(next, { replace: true });
    }
  }, [appliedFilters.thingId, appliedFilters._thingLabel, searchParams, setSearchParams]);

  const storage = useApi<TrafficStorageResponse>(
    () => systemApi.getTrafficStorage(),
    ['admin', 'audit', 'traffic', 'storage'],
  );
  const trafficQueryable = storage.data?.traffic?.queryable === true;

  // Merge source prop into filters for the API call
  const effectiveFilters = { ...appliedFilters, source: source || appliedFilters.source };

  const logs = useApi<{ data: TrafficEvent[]; total: number }>(
    () =>
      systemApi.listTrafficEvents(
        buildTrafficAuditLogQueryParams(effectiveFilters, { limit: pageLimit, offset }),
      ),
    ['admin', 'audit', 'traffic', 'logs', offset, appliedFilters, pageLimit, source, applyTick],
    { skip: !trafficQueryable || !storage.data },
  );

  const [refreshing, setRefreshing] = useState(false);
  const refreshTimerRef = useRef<ReturnType<typeof setTimeout>>(undefined);

  const handleRefresh = useCallback(() => {
    setRefreshing(true);
    void storage.refetch();
    void logs.refetch();
    clearTimeout(refreshTimerRef.current);
    refreshTimerRef.current = setTimeout(() => setRefreshing(false), 800);
  }, [storage, logs]);

  const handleApplyFilters = useCallback(() => {
    setOffset(0);
    setAppliedFilters({ ...draftFilters });
    setApplyTick((n) => n + 1);
  }, [draftFilters]);

  const handleClearAllFilters = useCallback(() => {
    setDraftFilters(EMPTY_LIVE_TRAFFIC_FILTERS);
    setAppliedFilters(EMPTY_LIVE_TRAFFIC_FILTERS);
    setOffset(0);
  }, []);

  if (storage.loading && !storage.data) return <LoadingSpinner />;
  if (storage.error) return <ErrorBanner message={storage.error.message} onRetry={storage.refetch} />;
  if (!storage.data?.traffic) return <LoadingSpinner />;

  const trafficConfig = storage.data.traffic;

  if (!trafficConfig.enabled) {
    return (
      <div>
        <p className={css.disabledMessage}>{t('pages:traffic.auditDisabled')}</p>
      </div>
    );
  }

  if (!trafficQueryable) {
    return (
      <Stack direction="horizontal" gap="md" align="start" className={css.notQueryableCard}>
        <div className={css.notQueryableIcon}>&#9432;</div>
        <div>
          <p className={css.notQueryableTitle}>
            {t('pages:traffic.notQueryable')}
          </p>
          <p className={css.notQueryableBody}>
            <TrafficFileSinkNotice variant="compact" filePath={trafficConfig.filePath} />
          </p>
        </div>
      </Stack>
    );
  }

  if (logs.loading && !logs.data) return <LoadingSpinner />;
  if (logs.error) return <ErrorBanner message={logs.error.message} onRetry={logs.refetch} />;

  const entries = logs.data?.data ?? [];
  const total = logs.data?.total ?? 0;
  const columns = getColumnsForSource(source, t);

  return (
    <Stack gap="md">
      <LiveTrafficFilterPanel
        value={draftFilters}
        onPatch={(patch) => setDraftFilters((d) => ({ ...d, ...patch }))}
        onApply={handleApplyFilters}
        onClear={handleClearAllFilters}
        source={source}
        showAdvanced={showAdvanced}
        onToggleAdvanced={() => setShowAdvanced((s) => !s)}
        onRefresh={handleRefresh}
        refreshing={refreshing}
      />

      <LiveTrafficActiveFiltersBar applied={appliedFilters} />

      {total === 0 ? <p className={css.helperText}>{t('pages:traffic.noRowsMatch')}</p> : null}

      <div data-testid="traffic-table" className={css.tableWrapper}>
        <DataTable hideSearch
          pageSize={pageLimit}
          columns={columns}
          data={entries}
          emptyMessage={t('pages:traffic.emptyTraffic')}
          onRowClick={(row) => {
            if (selectedEntry?.id === row.id) closeDrawer();
            else setSelectedEntry(row);
          }}
        />
      </div>

      {selectedEntry && (
        <TrafficEventDrawer
          selectedEntry={selectedEntry}
          drawerVisible={drawerVisible}
          onClose={closeDrawer}
          titleId="traffic-analytics-live-drawer-title"
        />
      )}

      <ListPagination
        offset={offset}
        limit={pageLimit}
        total={total}
        onOffsetChange={setOffset}
        onLimitChange={setPageLimit}
      />
    </Stack>
  );
}

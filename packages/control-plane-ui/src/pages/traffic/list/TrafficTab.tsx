import { useState, useRef, useCallback, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { TrafficFileSinkNotice } from '../list/TrafficFileSinkNotice';
import { useApi } from '../../../hooks/useApi';
import { systemApi } from '@/api/services';
import {
  DataTable,
  LoadingSpinner,
  ErrorBanner,
  Stack,
  ListPagination,
  Button,
} from '@/components/ui';
import type { TrafficEvent, TrafficStorageResponse } from '../../../api/types';
import { TrafficEventDrawer } from '../audit-drawer/trafficAuditDrawer';
import { LiveTrafficActiveFiltersBar } from '../filters/LiveTrafficActiveFiltersBar';
import { LiveTrafficFilterPanel } from '../filters/LiveTrafficFilterPanel';
import {
  buildTrafficAuditLogQueryParams,
  countLiveTrafficFilters,
  parseTrafficNavParams,
  type TrafficSourceFilter,
} from '../filters/liveTrafficFilters';
import { getColumnsForSource } from './trafficColumns';
import { useTrafficNav } from './useTrafficNav';
import { useTrafficFilters } from './useTrafficFilters';
import css from '../analytics/TrafficAnalyticsPage.module.css';

/* -- Source filter type -- */

export type { TrafficSourceFilter } from '../filters/liveTrafficFilters';

/* -- Per-source column definitions -- */

export { getColumnsForSource } from './trafficColumns';

interface TrafficTabProps {
  source: TrafficSourceFilter;
}

/* -- Traffic Tab -- */

export function TrafficTab({ source }: TrafficTabProps) {
  const { t } = useTranslation();
  const { selectedEntry, setSelectedEntry, drawerVisible, closeDrawer } = useTrafficNav();

  const {
    offset,
    setOffset,
    pageLimit,
    setPageLimit,
    draftFilters,
    setDraftFilters,
    appliedFilters,
    applyTick,
    showAdvanced,
    setShowAdvanced,
    handleApplyFilters,
    handleClearAllFilters,
    applyFilterPatch,
    searchParams,
    setSearchParams,
  } = useTrafficFilters(source);

  // Web-assistant navigation consumer (#17 C1, e90-s4 §5). The "Chat with
  // Nexus" assistant navigates to /traffic with ?eventId (drill into one event)
  // and/or ?status / ?model (pre-filter the live list). Because the assistant
  // popup floats over the CURRENT page, the SPA does NOT remount TrafficTab on a
  // query-only change — so this MUST be a searchParams-reactive effect, not a
  // mount-time initializer (a mount-only seed silently no-ops on the primary
  // path, and a StrictMode double-invoke would wipe it). After applying, the
  // consumed params are stripped ("consume OR drop") so they never linger.
  //
  // Loop-safety: this strips ONLY eventId/status/model and never touches
  // thingId/thingName, so it cannot ping-pong with the Node-filter mirror effect
  // above; the strip write is guarded by a value-equality check, and once the
  // params are gone a re-run early-returns. Drawer: it only sets selectedEntry
  // and lets the existing layout effect choreograph the open animation — it must
  // NOT poke setDrawerVisible (the two would fight over the rAF).
  //
  // Fetch lifetime: the event fetch is guarded by a monotonic request id (latest
  // ?eventId wins) and a mounted flag — NOT by the effect's cleanup. Stripping
  // ?eventId immediately re-runs this effect; an effect-cleanup `cancelled` flag
  // would abort the still-pending fetch before it resolves and the drawer would
  // never open. The req-id is only bumped on a real new eventId, so the
  // strip-triggered re-run (which early-returns) leaves the in-flight fetch live.
  const navEventReqRef = useRef(0);
  const navMountedRef = useRef(true);
  // Re-arm on setup (not just clear on teardown) so a StrictMode mount/unmount/
  // remount leaves the flag true for the live instance.
  useEffect(() => {
    navMountedRef.current = true;
    return () => { navMountedRef.current = false; };
  }, []);

  useEffect(() => {
    if ((searchParams.get('source') ?? '') !== source) return;
    const nav = parseTrafficNavParams(searchParams);
    if (!nav.hasNav) return;

    if (Object.keys(nav.filterPatch).length > 0) {
      applyFilterPatch(nav.filterPatch);
    }

    if (nav.eventId) {
      const reqId = ++navEventReqRef.current;
      void systemApi
        .getTrafficEvent(nav.eventId)
        .then((evt) => {
          // Apply only if still mounted and not superseded by a newer ?eventId.
          // Re-key the drawer layout effect; do NOT touch drawerVisible.
          if (navMountedRef.current && navEventReqRef.current === reqId) {
            setSelectedEntry(evt);
          }
        })
        .catch(() => {
          // Event gone (reaped / not the caller's → 404) or unauthorized: leave
          // the drawer closed rather than faking an open on a missing event.
        });
    }

    const next = new URLSearchParams(searchParams);
    for (const k of nav.consumedKeys) next.delete(k);
    if (next.toString() !== searchParams.toString()) {
      setSearchParams(next, { replace: true });
    }
  }, [source, searchParams, setSearchParams, applyFilterPatch, setSelectedEntry]);

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

  const handleRefreshFilteredEmpty = useCallback(() => {
    handleClearAllFilters();
    handleRefresh();
  }, [handleClearAllFilters, handleRefresh]);

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
  const hasUserSearchOrFilters = countLiveTrafficFilters(appliedFilters) > 0;

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

      <LiveTrafficActiveFiltersBar
        applied={appliedFilters}
        onRemove={applyFilterPatch}
        onClearAll={handleClearAllFilters}
      />

      {total === 0 ? (
        <div data-testid="traffic-table" className={css.emptyStatePanel}>
          {hasUserSearchOrFilters ? (
            <>
              <h2 className={css.emptyStateTitle}>{t('pages:traffic.noFilteredDataTitle')}</h2>
              <p className={css.emptyStateDescription}>
                {t('pages:traffic.noFilteredDataDescription')}
              </p>
              <Button
                variant="primary"
                onClick={handleRefreshFilteredEmpty}
                disabled={refreshing}
                loading={refreshing}
                className={css.emptyStateRefreshButton}
              >
                {t('pages:traffic.refresh')}
              </Button>
            </>
          ) : (
            <h2 className={css.noDataTitle}>{t('pages:traffic.emptyTraffic')}</h2>
          )}
        </div>
      ) : (
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
      )}

      {selectedEntry && (
        <TrafficEventDrawer
          selectedEntry={selectedEntry}
          drawerVisible={drawerVisible}
          onClose={closeDrawer}
          titleId="traffic-analytics-live-drawer-title"
        />
      )}

      {total > 0 ? (
        <ListPagination
          offset={offset}
          limit={pageLimit}
          total={total}
          onOffsetChange={setOffset}
          onLimitChange={setPageLimit}
        />
      ) : null}
    </Stack>
  );
}

import { useState, useCallback, useEffect } from 'react';
import { useSearchParams } from 'react-router-dom';
import { DEFAULT_ADMIN_LIST_PAGE_SIZE, type AdminListPageSize } from '@/components/ui';
import {
  EMPTY_LIVE_TRAFFIC_FILTERS,
  type LiveTrafficFiltersState,
  type TrafficSourceFilter,
} from '../filters/liveTrafficFilters';

interface TrafficFilterSnapshot {
  offset: number;
  pageLimit: AdminListPageSize;
  draftFilters: LiveTrafficFiltersState;
  appliedFilters: LiveTrafficFiltersState;
  showAdvanced: boolean;
}

const trafficFilterSnapshots = new Map<TrafficSourceFilter, TrafficFilterSnapshot>();

function cloneFilters(filters: LiveTrafficFiltersState): LiveTrafficFiltersState {
  return {
    ...filters,
    complianceTags: [...filters.complianceTags],
  };
}

/**
 * useTrafficFilters — filter draft/applied state, pagination offset/limit, and
 * the Node-filter↔URL mirror for the Traffic list. Each source tab keeps its
 * own snapshot so filters applied in one tab do not bleed into another tab.
 */
export function useTrafficFilters(source: TrafficSourceFilter) {
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
  const [defaultFilters] = useState<LiveTrafficFiltersState>(() => {
    const urlSource = searchParams.get('source') ?? '';
    const shouldSeedFromUrl = urlSource === source;
    return {
      ...EMPTY_LIVE_TRAFFIC_FILTERS,
      thingId: shouldSeedFromUrl ? searchParams.get('thingId') ?? '' : '',
      _thingLabel: shouldSeedFromUrl ? searchParams.get('thingName') ?? '' : '',
    };
  });
  const [initialSnapshot] = useState(() => trafficFilterSnapshots.get(source));
  const [offset, setOffset] = useState(initialSnapshot?.offset ?? 0);
  const [pageLimit, setPageLimit] = useState<AdminListPageSize>(
    initialSnapshot?.pageLimit ?? DEFAULT_ADMIN_LIST_PAGE_SIZE,
  );
  const [draftFilters, setDraftFilters] = useState<LiveTrafficFiltersState>(
    () => cloneFilters(initialSnapshot?.draftFilters ?? defaultFilters),
  );
  const [appliedFilters, setAppliedFilters] = useState<LiveTrafficFiltersState>(
    () => cloneFilters(initialSnapshot?.appliedFilters ?? defaultFilters),
  );
  const [applyTick, setApplyTick] = useState(0);
  const [showAdvanced, setShowAdvanced] = useState(initialSnapshot?.showAdvanced ?? false);

  // Cache the tab's local filter state so unmounting inactive Radix tabs does
  // not make their filters leak to the next tab or disappear when switching back.
  useEffect(() => {
    trafficFilterSnapshots.set(source, {
      offset,
      pageLimit,
      draftFilters: cloneFilters(draftFilters),
      appliedFilters: cloneFilters(appliedFilters),
      showAdvanced: false,
    });
  }, [source, offset, pageLimit, draftFilters, appliedFilters, showAdvanced]);

  // Mirror the active Node filter into the URL so the address bar reflects
  // what's actually applied. Replace-history so rapid filter edits don't
  // pollute the browser back stack. Only thingId + thingName are mirrored —
  // the rest of the filter state stays local because that's a lot of params
  // to drag around for a casual reload (and the time range is the only
  // other state worth preserving, which already round-trips via the time
  // preset buttons).
  useEffect(() => {
    if ((searchParams.get('source') ?? '') !== source) return;
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

  const handleApplyFilters = useCallback(() => {
    setOffset(0);
    setAppliedFilters({ ...draftFilters });
    setApplyTick((n) => n + 1);
  }, [draftFilters]);

  const handleClearAllFilters = useCallback(() => {
    setDraftFilters(cloneFilters(EMPTY_LIVE_TRAFFIC_FILTERS));
    setAppliedFilters(cloneFilters(EMPTY_LIVE_TRAFFIC_FILTERS));
    setOffset(0);
  }, []);

  // Merge a partial filter patch into BOTH draft and applied state and re-run the
  // query (reset offset + bump the apply tick). Used by the web-assistant deep-link
  // nav consumer in TrafficTab to pre-filter the live list from ?status / ?model
  // without reaching into the hook's private setters.
  const applyFilterPatch = useCallback((patch: Partial<LiveTrafficFiltersState>) => {
    setDraftFilters((d) => ({ ...d, ...patch }));
    setAppliedFilters((a) => ({ ...a, ...patch }));
    setOffset(0);
    setApplyTick((n) => n + 1);
  }, []);

  return {
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
    // Exposed for the deep-link nav consumer's "consume OR drop" URL strip — the
    // hook already owns the useSearchParams pair for the Node-filter↔URL mirror.
    searchParams,
    setSearchParams,
  };
}

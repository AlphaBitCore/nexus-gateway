import { EMPTY_LIVE_TRAFFIC_FILTERS, type LiveTrafficFiltersState } from './liveTrafficFilters';

/**
 * Filter state for a drawer correlation pivot: RESET everything and keep only
 * the single target id. Pivot means "jump from this event to that id's whole
 * slice" — merging into the currently applied filters would silently
 * intersect (e.g. a requestId filter still applied after a session pivot
 * shows one row, not the conversation), so the pivot replaces the filter
 * state wholesale. The source tab is not part of this state (TrafficTab
 * merges its own `source` prop into the query).
 */
export function correlationPivotPatch(
  field: 'requestId' | 'endUserId' | 'sessionId',
  value: string,
): LiveTrafficFiltersState {
  // Fresh complianceTags array so the shared EMPTY constant's reference
  // never leaks into mutable filter state.
  return { ...EMPTY_LIVE_TRAFFIC_FILTERS, complianceTags: [], [field]: value };
}

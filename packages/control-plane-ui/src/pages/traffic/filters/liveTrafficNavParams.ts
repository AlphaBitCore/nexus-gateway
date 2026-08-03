/**
 * One-shot navigation params for the Traffic page. Producers: the
 * "Chat with Nexus" web assistant (?eventId / ?status / ?model) and the
 * error-governance "View in Traffic" cross-link (?errorCode). Consumed
 * then stripped by TrafficTab's nav-consumer effect.
 */
import { utcToLocalInputSeconds } from '@/lib/format';
import type { LiveTrafficFiltersState, LiveTrafficStatusRange } from './liveTrafficFilters';

/* -- Traffic-page navigation params -- */

/**
 * Parsed result of the "Chat with Nexus" assistant's navigation query params.
 * The assistant navigates to `/traffic` with optional params to pre-focus the
 * page: `?eventId` drills into one event (open its drawer), `?status` / `?model`
 * pre-filter the live list. `consumedKeys` lists which nav keys were present so
 * the consumer can strip them after applying — the params are one-shot
 * ("consume OR drop") and must never linger in the address bar.
 */
export interface TrafficNavParams {
  /** Traffic event id to open in the drawer, or null when absent/empty. */
  eventId: string | null;
  /** Partial filter state to merge into draft + applied (model / status). */
  filterPatch: Partial<LiveTrafficFiltersState>;
  /** Nav keys that were present and must be stripped from the URL. */
  consumedKeys: string[];
  /** True when at least one nav param was present. */
  hasNav: boolean;
}

// The assistant's `status` directive is the kernel's StatusRange vocabulary
// ("4xx" | "5xx" | "error"). "error" (server-side failures) folds into the
// "5xx" range because the Live-Traffic status control is a single-select that
// cannot express "4xx and 5xx" at once. "2xx" passes through for completeness.
// Anything unrecognized yields null (the param is still stripped, just unused).
function navStatusToRange(status: string): LiveTrafficStatusRange | null {
  switch (status) {
    case '2xx':
    case '4xx':
    case '5xx':
      return status;
    case 'error':
      return '5xx';
    default:
      return null;
  }
}

/**
 * Parse the assistant navigation params off the current URL search params.
 * Pure (no side effects) so it can be unit-tested directly and reused by the
 * reactive consumer effect in TrafficTab.
 */
export function parseTrafficNavParams(searchParams: URLSearchParams): TrafficNavParams {
  const eventId = searchParams.get('eventId');
  const status = searchParams.get('status');
  const model = searchParams.get('model');
  const errorCode = searchParams.get('errorCode');
  const modelExact = searchParams.get('modelExact');
  const provider = searchParams.get('provider');
  const from = searchParams.get('from');
  const to = searchParams.get('to');

  const filterPatch: Partial<LiveTrafficFiltersState> = {};
  const consumedKeys: string[] = [];

  if (eventId !== null) consumedKeys.push('eventId');
  if (status !== null) {
    consumedKeys.push('status');
    const range = navStatusToRange(status);
    if (range !== null) filterPatch.statusRange = range;
  }
  if (model !== null) {
    consumedKeys.push('model');
    // An empty `?model=` is stripped but applies no filter.
    if (model) {
      filterPatch.modelUsed = model;
      filterPatch._modelLabel = model;
    }
  }
  if (errorCode !== null) {
    // Set by the error-governance "View in Traffic" cross-link. Same
    // one-shot semantics as the other nav params; empty value strips
    // without filtering.
    consumedKeys.push('errorCode');
    if (errorCode) filterPatch.errorCode = errorCode;
  }
  if (modelExact !== null) {
    // Error-governance cross-link: exact class boundary on the served
    // model ("__none__" = rows with no model). modelUsed's substring
    // semantics stay untouched for the assistant's ?model param.
    consumedKeys.push('modelExact');
    if (modelExact) filterPatch.modelExact = modelExact;
  }
  if (provider !== null) {
    // Error-governance cross-link: the group's serving provider (exact
    // match on the list's provider filter — same COALESCE(routed,
    // requested) expression on both sides).
    consumedKeys.push('provider');
    if (provider) filterPatch.provider = provider;
  }
  // from/to carry the error-governance aggregation window (RFC3339) so the
  // drill-down's row count matches the group's count instead of silently
  // widening to all history. Filter state stores datetime-local strings in
  // the user's DISPLAY timezone (the same convention the time inputs use),
  // second-precise so the window round-trips exactly. Unparseable values
  // are stripped without filtering.
  if (from !== null) {
    consumedKeys.push('from');
    if (from && !Number.isNaN(new Date(from).getTime())) {
      filterPatch.startTime = utcToLocalInputSeconds(from);
    }
  }
  if (to !== null) {
    consumedKeys.push('to');
    if (to && !Number.isNaN(new Date(to).getTime())) {
      filterPatch.endTime = utcToLocalInputSeconds(to);
    }
  }

  return {
    eventId: eventId || null,
    filterPatch,
    consumedKeys,
    hasNav: consumedKeys.length > 0,
  };
}

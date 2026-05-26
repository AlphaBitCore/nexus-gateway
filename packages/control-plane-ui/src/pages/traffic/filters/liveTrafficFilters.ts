/**
 * Live Traffic / client traffic audit list filters — shared by Traffic page and Traffic → Live Traffic tab.
 * Query keys must match GET /api/admin/traffic (see gateway `buildTrafficAuditLogWhere`).
 */

import { formatDateTime, localInputToUTC } from '@/lib/format';

export type TrafficSourceFilter = '' | 'vk' | 'proxy' | 'agent';

export type LiveTrafficStatusRange = '' | '2xx' | '4xx' | '5xx';
// Two user-visible values (HIT | MISS). Drill-down on gateway/provider
// internal states lives in the audit drawer, not in this filter.
export type LiveTrafficCacheStatus = '' | 'HIT' | 'MISS';

export interface LiveTrafficFiltersState {
  provider: string;
  /** UI-only: Prisma provider id for loading models after provider is chosen (not sent to audit API). */
  _providerId: string;
  userId: string;
  orgId: string;
  virtualKeyId: string;
  projectId: string;
  modelUsed: string;
  /** UI-only label for chips (model picker). */
  _modelLabel: string;
  /** UI-only labels for scoped org / project / VK chips. */
  _orgLabel: string;
  _projectLabel: string;
  _vkLabel: string;
  requestId: string;
  requestHookDecision: string;
  responseHookDecision: string;
  statusRange: LiveTrafficStatusRange;
  statusCode: string;
  cacheStatus: LiveTrafficCacheStatus;
  startTime: string;
  endTime: string;
  // Source-specific filters
  source: string;        // comma-separated: "vk", "proxy", "agent"
  targetHost: string;
  path: string;
  deviceId: string;
  _deviceLabel: string;  // UI-only for chip display
  _userLabel: string;    // UI-only for chip display
  /**
   * thingId filters traffic_event.thing_id directly — the Thing instance that
   * emitted the event (agent device, ai-gateway, or compliance-proxy). Set by
   * the node-detail "View all traffic" cross-link; users normally don't enter
   * it manually because thing IDs are opaque strings.
   */
  thingId: string;
  _thingLabel: string;   // UI-only display name for chip (denorm'd thing.name)
  sourceProcess: string;
  bumpStatus: string;
  /**
   * Compliance tags to filter by (AND semantics — each entry adds another
   * `?tag=` query param). Namespaced values like `severity:confidential` or
   * `compliance:pii`.
   */
  complianceTags: string[];
}

export const EMPTY_LIVE_TRAFFIC_FILTERS: LiveTrafficFiltersState = {
  provider: '',
  _providerId: '',
  userId: '',
  orgId: '',
  virtualKeyId: '',
  projectId: '',
  modelUsed: '',
  _modelLabel: '',
  _orgLabel: '',
  _projectLabel: '',
  _vkLabel: '',
  requestId: '',
  requestHookDecision: '',
  responseHookDecision: '',
  statusRange: '',
  statusCode: '',
  cacheStatus: '',
  startTime: '',
  endTime: '',
  source: '',
  targetHost: '',
  path: '',
  deviceId: '',
  _deviceLabel: '',
  _userLabel: '',
  thingId: '',
  _thingLabel: '',
  sourceProcess: '',
  bumpStatus: '',
  complianceTags: [],
};

/** `datetime-local` value from a Date in the browser's local timezone. */
export function toDatetimeLocalValue(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/**
 * Convert a `<input type="datetime-local">` string to a UTC RFC3339
 * instant in the user's display TZ. Forwards to [localInputToUTC].
 * Returns '' for invalid input so the caller can omit the param without throwing.
 */
export function toRFC3339WithOffset(localDateTime: string): string {
  if (!localDateTime) return '';
  try {
    return localInputToUTC(localDateTime);
  } catch {
    return '';
  }
}

/**
 * Serialize filters for `GET /api/admin/traffic`. Returns `URLSearchParams`
 * instead of a plain object because the backend accepts **repeatable** query
 * params for compliance tags (`?tag=severity:confidential&tag=compliance:pii`),
 * and plain objects collapse repeats into a single key.
 */
export function buildTrafficAuditLogQueryParams(
  filters: LiveTrafficFiltersState,
  pagination: { limit: number; offset: number },
): URLSearchParams {
  const params = new URLSearchParams();
  params.set('limit', String(pagination.limit));
  params.set('offset', String(pagination.offset));
  const t = (s: string) => s.trim();
  const setIf = (key: string, value: string) => {
    if (value) params.set(key, value);
  };
  setIf('provider', t(filters.provider));
  setIf('userId', t(filters.userId));
  setIf('orgId', t(filters.orgId));
  setIf('virtualKeyId', t(filters.virtualKeyId));
  setIf('projectId', t(filters.projectId));
  setIf('modelUsed', t(filters.modelUsed));
  setIf('requestId', t(filters.requestId));
  setIf('hookDecision', t(filters.requestHookDecision));
  setIf('responseHookDecision', t(filters.responseHookDecision));
  if (filters.cacheStatus) params.set('cacheStatus', filters.cacheStatus);
  if (t(filters.startTime)) {
    const start = toRFC3339WithOffset(filters.startTime);
    if (start) params.set('startTime', start);
  }
  if (t(filters.endTime)) {
    const end = toRFC3339WithOffset(filters.endTime);
    if (end) params.set('endTime', end);
  }
  setIf('source', t(filters.source));
  setIf('targetHost', t(filters.targetHost));
  setIf('path', t(filters.path));
  setIf('deviceId', t(filters.deviceId));
  setIf('thingId', t(filters.thingId));
  setIf('sourceProcess', t(filters.sourceProcess));
  setIf('bumpStatus', t(filters.bumpStatus));
  for (const tag of filters.complianceTags) {
    const trimmed = tag.trim();
    if (trimmed) params.append('tag', trimmed);
  }

  const codeRaw = t(filters.statusCode);
  const codeNum = parseInt(codeRaw, 10);
  if (codeRaw.length > 0 && !Number.isNaN(codeNum) && codeNum >= 100 && codeNum <= 599) {
    params.set('statusCode', String(codeNum));
  } else if (filters.statusRange === '2xx' || filters.statusRange === '4xx' || filters.statusRange === '5xx') {
    params.set('statusRange', filters.statusRange);
  }

  return params;
}

const LABELS: Partial<Record<keyof LiveTrafficFiltersState, string>> = {
  provider: 'Provider',
  userId: 'Virtual key',
  orgId: 'Organization',
  virtualKeyId: 'Virtual key',
  projectId: 'Project',
  modelUsed: 'Model',
  requestId: 'Request ID',
  requestHookDecision: 'Request hook',
  responseHookDecision: 'Response hook',
  statusRange: 'HTTP status class',
  statusCode: 'HTTP status code',
  cacheStatus: 'Cache',
  startTime: 'From',
  endTime: 'To',
  targetHost: 'Target',
  path: 'Path',
  deviceId: 'Device',
  thingId: 'Node',
  sourceProcess: 'Process',
  bumpStatus: 'Bump status',
  complianceTags: 'Compliance tag',
};

const STATUS_RANGE_LABEL: Record<Exclude<LiveTrafficStatusRange, ''>, string> = {
  '2xx': '2xx success',
  '4xx': '4xx client error',
  '5xx': '5xx server error',
};

/** Human-readable active filter lines for summary chips (uses applied filters only). */
export function describeLiveTrafficFilters(filters: LiveTrafficFiltersState): string[] {
  const lines: string[] = [];
  const t = (s: string) => s.trim();
  if (t(filters.provider)) lines.push(`${LABELS.provider}: ${t(filters.provider)}`);
  if (t(filters.virtualKeyId)) {
    const vkLine =
      t(filters._vkLabel) || (t(filters.virtualKeyId) ? `${t(filters.virtualKeyId).slice(0, 8)}…` : '');
    if (vkLine) lines.push(`Virtual key: ${vkLine}`);
  } else if (t(filters.userId)) {
    lines.push(`User: ${t(filters._userLabel) || `${t(filters.userId).slice(0, 8)}…`}`);
  }
  if (t(filters.orgId)) {
    lines.push(`${LABELS.orgId}: ${t(filters._orgLabel) || `${t(filters.orgId).slice(0, 8)}…`}`);
  }
  if (t(filters.projectId)) {
    lines.push(`${LABELS.projectId}: ${t(filters._projectLabel) || `${t(filters.projectId).slice(0, 8)}…`}`);
  }
  if (t(filters.modelUsed)) {
    lines.push(
      `${LABELS.modelUsed}: ${t(filters._modelLabel) || t(filters.modelUsed)}`,
    );
  }
  if (t(filters.requestId)) lines.push(`${LABELS.requestId}: ${t(filters.requestId)}`);
  if (t(filters.requestHookDecision)) lines.push(`${LABELS.requestHookDecision}: ${t(filters.requestHookDecision)}`);
  if (t(filters.responseHookDecision)) lines.push(`${LABELS.responseHookDecision}: ${t(filters.responseHookDecision)}`);
  const codeRaw = t(filters.statusCode);
  const codeNum = parseInt(codeRaw, 10);
  if (codeRaw.length > 0 && !Number.isNaN(codeNum) && codeNum >= 100 && codeNum <= 599) {
    lines.push(`HTTP ${codeNum}`);
  } else if (filters.statusRange && STATUS_RANGE_LABEL[filters.statusRange]) {
    lines.push(`HTTP: ${STATUS_RANGE_LABEL[filters.statusRange]}`);
  }
  if (filters.cacheStatus) lines.push(`Cache: ${filters.cacheStatus}`);
  if (t(filters.startTime)) {
    try {
      lines.push(`${LABELS.startTime}: ${formatDateTime(filters.startTime)}`);
    } catch {
      lines.push(`${LABELS.startTime}: ${t(filters.startTime)}`);
    }
  }
  if (t(filters.endTime)) {
    try {
      lines.push(`${LABELS.endTime}: ${formatDateTime(filters.endTime)}`);
    } catch {
      lines.push(`${LABELS.endTime}: ${t(filters.endTime)}`);
    }
  }
  if (t(filters.targetHost)) lines.push(`Target: ${t(filters.targetHost)}`);
  if (t(filters.path)) lines.push(`Path: ${t(filters.path)}`);
  if (t(filters.deviceId)) {
    lines.push(`Device: ${t(filters._deviceLabel) || `${t(filters.deviceId).slice(0, 8)}…`}`);
  }
  if (t(filters.thingId)) {
    lines.push(`Node: ${t(filters._thingLabel) || t(filters.thingId)}`);
  }
  if (t(filters.sourceProcess)) lines.push(`Process: ${t(filters.sourceProcess)}`);
  if (t(filters.bumpStatus)) lines.push(`Bump status: ${t(filters.bumpStatus)}`);
  for (const tag of filters.complianceTags) {
    const trimmed = tag.trim();
    if (trimmed) lines.push(`${LABELS.complianceTags}: ${trimmed}`);
  }
  return lines;
}

export function countLiveTrafficFilters(filters: LiveTrafficFiltersState): number {
  return describeLiveTrafficFilters(filters).length;
}

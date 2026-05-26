/**
 * Diagnostic events admin API service — bound to `/api/admin/diag-events/*`.
 *
 * Reads `thing_diag_event` (newest-first paginated list, top-N message-hash
 * groups, FATAL/crash cohorts) from the CP store. All three endpoints are
 * read-only and gated by `admin:ReadObservability`.
 *
 * Cursor semantics: the wire cursor is a base64-encoded `(occurred_at, id)`
 * pair, treated as opaque by callers — never parse / decode in the UI.
 */

import { api } from '../../../client';

export type DiagLevel = 'debug' | 'info' | 'warn' | 'error' | 'fatal';

/** One row from `thing_diag_event` (occurred_at + id pair is unique). */
export interface DiagEvent {
  id: string;
  nodeId: string;
  nodeType: string;
  occurredAt: string;
  receivedAt: string;
  level: DiagLevel | string;
  eventType: string;
  source: string;
  message: string;
  messageHash: string;
  attrs?: Record<string, unknown>;
  stackTrace?: string;
  repeatCount: number;
  agentVersion?: string;
  osInfo?: Record<string, unknown>;
}

/** One 5-minute bucket of a DiagGroup's sparkline series. */
export interface DiagGroupBucket {
  /** Bucket start (ISO). 5-minute aligned. */
  ts: string;
  count: number;
}

/**
 * One bucket from `/groups`: a (level, message_hash) group with counts.
 * `buckets` is a 5-min sparkline; `silenced` is true when an active
 * diag_silence row matches `(messageHash, maxLevel)`.
 *
 * The CP store uses `maxLevel` (computed via `MAX(level)`). The `level`
 * alias is kept for older consumers; prefer `maxLevel`.
 */
export interface DiagGroup {
  /** @deprecated use `maxLevel`. */
  level?: string;
  maxLevel: string;
  messageHash: string;
  sampleMessage: string;
  source: string;
  affectedNodes: number;
  totalOccurrences: number;
  firstSeen: string;
  lastSeen: string;
  buckets: DiagGroupBucket[];
  silenced: boolean;
}

/** One row from `/crash-cohorts`: FATAL events grouped by client surface. */
export interface CrashCohort {
  agentVersion: string;
  os: string;
  osVersion: string;
  affectedNodes: number;
  totalCrashes: number;
  firstSeenAt: string;
  lastSeenAt: string;
}

export interface ListDiagEventsParams {
  nodeId?: string;
  /** Single level filter. The CP handler validates against debug|info|warn|error|fatal. */
  level?: DiagLevel | string;
  /** Single event_type filter — one of error / crash / lifecycle / watchdog. Empty = no filter. */
  eventType?: string;
  source?: string;
  from?: string;
  to?: string;
  /** ILIKE substring against `message`. */
  q?: string;
  limit?: number;
  /** Opaque cursor returned in `nextCursor` from a prior page. */
  cursor?: string;
}

export interface DiagEventListResponse {
  data: DiagEvent[];
  nextCursor: string;
}

export interface DiagGroupsParams {
  from: string;
  to: string;
  nodeType?: string;
  /** Single event_type filter — one of error / crash / lifecycle / watchdog. Empty = no filter. */
  eventType?: string;
}

export interface CrashCohortsParams {
  from: string;
  to: string;
}

function qs(p: Record<string, string | number | undefined>): string {
  const u = new URLSearchParams();
  for (const [k, v] of Object.entries(p)) {
    if (v == null) continue;
    const s = String(v);
    if (s !== '') u.set(k, s);
  }
  const out = u.toString();
  return out ? `?${out}` : '';
}

export const diagEventsApi = {
  list: (p: ListDiagEventsParams = {}) =>
    api.get<DiagEventListResponse>(
      `/api/admin/diag-events${qs({
        nodeId: p.nodeId,
        level: p.level,
        eventType: p.eventType,
        source: p.source,
        from: p.from,
        to: p.to,
        q: p.q,
        limit: p.limit,
        cursor: p.cursor,
      })}`,
    ),

  groups: (p: DiagGroupsParams) =>
    api
      .get<{ data: DiagGroup[] }>(`/api/admin/diag-events/groups${qs({
        from: p.from,
        to: p.to,
        nodeType: p.nodeType,
        eventType: p.eventType,
      })}`)
      .then((r) => r.data),

  crashCohorts: (p: CrashCohortsParams) =>
    api
      .get<{ data: CrashCohort[] }>(`/api/admin/diag-events/crash-cohorts${qs({
        from: p.from,
        to: p.to,
      })}`)
      .then((r) => r.data),
};

// ── Silence registry ────────────────────────────────────────────────────

export interface DiagSilence {
  id: string;
  messageHash: string;
  level: DiagLevel | string;
  silencedBy: string;
  silencedAt: string;
  /** ISO timestamp; null = permanent silence. */
  expiresAt: string | null;
  reason?: string;
}

export interface CreateDiagSilenceRequest {
  messageHash: string;
  level: DiagLevel;
  /** Seconds. 0 = permanent. Server caps at 30 days. */
  ttlSeconds?: number;
  reason?: string;
}

export const diagSilencesApi = {
  list: () =>
    api
      .get<{ data: DiagSilence[] }>('/api/admin/diag-silences')
      .then((r) => r.data),
  create: (body: CreateDiagSilenceRequest) =>
    api
      .post<{ silence: DiagSilence }>('/api/admin/diag-silences', body)
      .then((r) => r.silence),
  remove: (id: string) => api.delete(`/api/admin/diag-silences/${id}`),
};

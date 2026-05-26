/**
 * Dead-letter queue admin API service — bound to /api/admin/observability/dlq/*.
 *
 * Reads traffic_event_dlq rows + retries a single row back onto its
 * original MQ subject. CP proxies both endpoints to Hub /api/hub/dlq
 * with JWT + IAM check + AdminAuditLog wrap (the audit row attributes
 * each retry to the operator).
 *
 * IAM:
 *   - List  → admin:observability-dlq.read
 *   - Retry → admin:observability-dlq.manage
 */

import { api } from '../../../client';

/** One row from traffic_event_dlq. Payload bytes are intentionally absent
 *  from list responses (could be MB-scale); only payloadSize is surfaced.
 */
export interface DlqRow {
  id: string;
  msgId: string;
  subject: string;
  deliveryCount: number;
  lastError?: string;
  firstSeenAt: string;
  dlqInsertedAt: string;
  payloadSize: number;
}

export interface DlqListResponse {
  rows: DlqRow[];
  /** Opaque cursor — pass back as `cursor` to fetch the next page.
   *  Empty when the page returned every remaining row. */
  nextCursor?: string;
}

export interface DlqListQuery {
  subject?: string;
  /** 1..200, default 50. */
  limit?: number;
  cursor?: string;
}

export interface DlqRetryResponse {
  ok: boolean;
  subject: string;
  /** Set when the republish succeeded but the row-DELETE failed. The MQ
   *  message has been re-enqueued; the lingering DLQ row will be picked
   *  up on the next retry cycle if the underlying bug persists. */
  deleteWarn?: boolean;
}

function buildQuery(q?: DlqListQuery): string {
  if (!q) return '';
  const parts: string[] = [];
  if (q.subject) parts.push(`subject=${encodeURIComponent(q.subject)}`);
  if (q.limit) parts.push(`limit=${q.limit}`);
  if (q.cursor) parts.push(`cursor=${encodeURIComponent(q.cursor)}`);
  return parts.length === 0 ? '' : '?' + parts.join('&');
}

export const dlqApi = {
  /** GET /api/admin/observability/dlq?subject=&limit=&cursor= */
  list: (q?: DlqListQuery) =>
    api.get<DlqListResponse>(`/api/admin/observability/dlq${buildQuery(q)}`),

  /** POST /api/admin/observability/dlq/{id}/retry */
  retry: (id: string) =>
    api.post<DlqRetryResponse>(`/api/admin/observability/dlq/${encodeURIComponent(id)}/retry`),
};

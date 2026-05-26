/**
 * Diagnostic-mode admin API service — bound to `/api/admin/agents/*`.
 *
 * Diagnostic mode opens a time-bounded window during which an agent emits
 * verbose telemetry (DEBUG-level diag events, full traces). Windows live in
 * `thing_diag_mode_window` with a denormalized `diagModeUntil` mirror on
 * `thing.metadata` so the agent shadow surfaces the active state without an
 * extra join.
 *
 * Wire shapes follow the Go structs in
 * `packages/control-plane/internal/store/opsmetrics_store.go` (DiagModeWindow,
 * EnableDiagModeParams, BulkAgentFilter).
 *
 * Bulk semantics: the bulk endpoint may return **207 Multi-Status** when one
 * or more per-thing enables fail. Callers must treat a 207 as a partial
 * success — inspect each `items[i].ok` to drive the UI rather than
 * short-circuiting on the top-level `ok` flag.
 *
 * Authorship caveat: `setBy` on the returned window is null when the caller
 * authenticated with an admin API key (no admin user). The audit log is the
 * source of truth for the actor display in that case.
 */

import { api } from '../../../client';

/** One row from `thing_diag_mode_window` (active or historical). */
export interface DiagModeWindow {
  id: string;
  nodeId: string;
  nodeType?: string;
  startedAt: string;
  endedAt: string;
  setBy?: string | null;
  reason?: string | null;
  createdAt: string;
}

export interface EnableDiagModeRequest {
  /** RFC3339 future timestamp; CP enforces `until <= now + 24h`. */
  until: string;
  reason?: string;
}

export interface BulkDiagModeFilter {
  nodeIds?: string[];
  agentVersion?: string;
  os?: string;
}

export interface BulkDiagModeRequest {
  filter: BulkDiagModeFilter;
  until: string;
  reason?: string;
}

export interface BulkDiagModeItem {
  nodeId: string;
  ok: boolean;
  error?: string;
}

export interface BulkDiagModeResult {
  ok: boolean;
  total: number;
  failed: number;
  items: BulkDiagModeItem[];
}

export const diagModeApi = {
  /**
   * Open (or replace) a diag-mode window for a single thing. Returns the
   * persisted window in `{ window: { ... } }`.
   */
  enable: (thingId: string, body: EnableDiagModeRequest) =>
    api.post<{ window: DiagModeWindow }>(
      `/api/admin/agents/${encodeURIComponent(thingId)}/diagnostic-mode`,
      body,
    ),

  /** Close the active window for `thingId`. */
  disable: (thingId: string) =>
    api.delete(`/api/admin/agents/${encodeURIComponent(thingId)}/diagnostic-mode`),

  /** All windows with `ended_at > NOW()`. */
  list: () => api.get<{ data: DiagModeWindow[] }>('/api/admin/agents/diagnostic-mode').then((r) => r.data),

  /**
   * Bulk-enable diag mode against a filter. The CP caps filter resolution at
   * 500 things; a wider filter returns 400 `TOO_MANY_THINGS`. A 207 response
   * means partial success — inspect `items[i].ok`.
   */
  bulk: (body: BulkDiagModeRequest) =>
    api.post<BulkDiagModeResult>('/api/admin/agents/diagnostic-mode/bulk', body),
};

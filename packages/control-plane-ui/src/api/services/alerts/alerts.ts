/**
 * Unified alerting admin API service — bound to `/api/admin/alerts/*`.
 *
 * The CP BFF thin-forwards these calls to Nexus Hub's `/api/v1/admin/alerts/*`
 * endpoints. Hub owns the source of truth for alert instances, rules, channels,
 * and dispatch history; the CP layer only gates on IAM + session auth.
 *
 * Scope of this module:
 *   - Alert inbox (list / detail / ack / resolve)
 *   - Alert rule registry (list / get / update / reset to defaults)
 *   - Notification channel registry (CRUD + test fire)
 *
 * Terminology note: "Alert" here is the unified runtime model from the
 * unified-alerting effort. It replaces the legacy `QuotaAlert` model
 * (quota-scoped, CP-local) and the `ProxyAlert` model (proxy-scoped,
 * returned from `GET /api/admin/proxy/alerts`). Both predecessors were
 * deleted in the same change set that introduced this service.
 */

import { api } from '../../client';

export type AlertSeverity = 'critical' | 'high' | 'medium' | 'low' | 'info';
export type AlertState = 'firing' | 'acknowledged' | 'resolved';

/** A single alert instance surfaced by Hub's runtime evaluator. */
export interface Alert {
  id: string;
  ruleId: string;
  sourceType: string;
  targetKey: string;
  targetLabel: string;
  severity: AlertSeverity;
  state: AlertState;
  message: string;
  details: Record<string, unknown>;
  firedAt: string;
  lastSeenAt: string;
  duplicateCount: number;
  acknowledgedBy?: string | null;
  acknowledgedAt?: string | null;
  resolvedAt?: string | null;
  resolvedBy?: string | null;
  resolvedReason?: string | null;
}

export interface AlertListResponse {
  alerts: Alert[];
  total: number;
}

/** One row of the `alert.dispatches` table (per-channel send attempt). */
export interface AlertDispatch {
  id: string;
  channelId: string;
  channelName: string;
  success: boolean;
  statusCode?: number | null;
  errorMsg?: string | null;
  attemptedAt: string;
}

export interface AlertDetailResponse extends Alert {
  dispatches: AlertDispatch[];
}

/**
 * A registered alert rule. `params` is a rule-specific JSON blob whose shape
 * is declared by `paramsSchema` (a JSON Schema document used by the UI to
 * render the rule editor).
 */
export interface AlertRule {
  id: string;
  displayName: string;
  sourceType: string;
  defaultSeverity: AlertSeverity;
  requiresAck: boolean;
  enabled: boolean;
  params: Record<string, unknown>;
  paramsSchema: Record<string, unknown>;
  cooldownSec: number;
  /**
   * Per-DeviceGroup filter. NULL = fleet-wide (default). Non-NULL = rule
   * only fires for events whose target device is a member of this group.
   * Matches the backend's `group_id_filter` column.
   */
  groupIdFilter?: string | null;
  updatedAt: string;
}

/** A notification channel (webhook, Slack, email, PagerDuty). */
export interface AlertChannel {
  id: string;
  name: string;
  type: 'webhook' | 'slack' | 'email' | 'pagerduty';
  enabled: boolean;
  severities: AlertSeverity[];
  sourceTypes: string[];
  config: Record<string, unknown>;
}

/** Query params accepted by `GET /api/admin/alerts`. */
export interface ListAlertsParams {
  state?: AlertState[];
  severity?: AlertSeverity[];
  sourceType?: string[];
  ruleId?: string;
  targetQuery?: string;
  since?: string;
  until?: string;
  offset?: number;
  limit?: number;
}

/**
 * Build the `?…` suffix for `GET /api/admin/alerts`. Returns an empty string
 * (no leading `?`) when no params are set, so `alertsApi.list()` hits the
 * canonical collection URL.
 *
 * Repeated-value params (`state`, `severity`, `sourceType`) are emitted as
 * multiple key=value pairs (e.g. `state=firing&state=acknowledged`) — this
 * matches Hub's handler, which reads them via `url.Query()["state"]`.
 */
function buildListQuery(p: ListAlertsParams): string {
  const u = new URLSearchParams();
  p.state?.forEach((s) => u.append('state', s));
  p.severity?.forEach((s) => u.append('severity', s));
  p.sourceType?.forEach((s) => u.append('sourceType', s));
  if (p.ruleId) u.set('ruleId', p.ruleId);
  if (p.targetQuery) u.set('targetQuery', p.targetQuery);
  if (p.since) u.set('since', p.since);
  if (p.until) u.set('until', p.until);
  if (p.offset !== undefined) u.set('offset', String(p.offset));
  if (p.limit !== undefined) u.set('limit', String(p.limit));
  const s = u.toString();
  return s ? `?${s}` : '';
}

export const alertsApi = {
  // ── Alert inbox ───────────────────────────────────────────────────────────

  list: (p: ListAlertsParams = {}) =>
    api.get<AlertListResponse>(`/api/admin/alerts${buildListQuery(p)}`),

  detail: (id: string) =>
    api.get<AlertDetailResponse>(`/api/admin/alerts/${encodeURIComponent(id)}`),

  ack: (id: string, reason?: string) =>
    api.post<Alert>(`/api/admin/alerts/${encodeURIComponent(id)}/ack`, { reason }),

  resolve: (id: string, reason?: string) =>
    api.post<Alert>(`/api/admin/alerts/${encodeURIComponent(id)}/resolve`, { reason }),

  // ── Rule registry ─────────────────────────────────────────────────────────

  listRules: (params?: {
    limit?: number;
    offset?: number;
    /** Free-text match on id or displayName (case-insensitive substring). */
    search?: string;
    /** Restrict to enabled (true) or disabled (false). Omit for both. */
    enabled?: boolean;
    /** Filter by defaultSeverity (info / low / medium / high / critical). */
    severity?: AlertSeverity;
    /** Filter by sourceType (exact match). */
    sourceType?: string;
  }) => {
    const u = new URLSearchParams();
    if (params?.limit !== undefined) u.set('limit', String(params.limit));
    if (params?.offset !== undefined) u.set('offset', String(params.offset));
    if (params?.search) u.set('search', params.search);
    if (params?.enabled !== undefined) u.set('enabled', String(params.enabled));
    if (params?.severity) u.set('severity', params.severity);
    if (params?.sourceType) u.set('sourceType', params.sourceType);
    const qs = u.toString();
    return api.get<{ rules: AlertRule[]; total: number; limit: number; offset: number }>(
      `/api/admin/alerts/rules${qs ? `?${qs}` : ''}`,
    );
  },

  getRule: (id: string) =>
    api.get<AlertRule>(`/api/admin/alerts/rules/${encodeURIComponent(id)}`),

  updateRule: (
    id: string,
    body: Partial<Pick<AlertRule, 'enabled' | 'params' | 'cooldownSec' | 'requiresAck' | 'defaultSeverity' | 'groupIdFilter'>>,
  ) => api.put<AlertRule>(`/api/admin/alerts/rules/${encodeURIComponent(id)}`, body),

  resetRule: (id: string) =>
    api.post<AlertRule>(`/api/admin/alerts/rules/${encodeURIComponent(id)}/reset`, {}),

  // ── Channel registry ──────────────────────────────────────────────────────

  listChannels: () => api.get<{ channels: AlertChannel[] }>('/api/admin/alerts/channels'),

  getChannel: (id: string) =>
    api.get<AlertChannel>(`/api/admin/alerts/channels/${encodeURIComponent(id)}`),

  createChannel: (body: Omit<AlertChannel, 'id'>) =>
    api.post<AlertChannel>('/api/admin/alerts/channels', body),

  updateChannel: (id: string, body: Partial<AlertChannel>) =>
    api.put<AlertChannel>(`/api/admin/alerts/channels/${encodeURIComponent(id)}`, body),

  deleteChannel: (id: string) =>
    api.delete(`/api/admin/alerts/channels/${encodeURIComponent(id)}`),

  testChannel: (id: string) =>
    api.post<{ success: boolean; statusCode?: number; errorMsg?: string }>(
      `/api/admin/alerts/channels/${encodeURIComponent(id)}/test`,
      {},
    ),
};

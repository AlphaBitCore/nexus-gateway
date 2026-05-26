/**
 * Compliance proxy admin API service — bound to the `/api/admin/compliance/*`
 * routes:
 *
 *   - killswitch + history
 *   - unified exemption list (grants + PENDING requests) + single-row GET
 *     by id under `/api/admin/compliance/exemptions/{id}`
 *   - grant create / patch / delete; approve & reject of pending requests
 *
 * The unified list endpoint returns rows discriminated by `kind`; per-kind
 * fields are nullable on the opposite kind. See OpenAPI spec at
 * docs/users/api/openapi/admin/e27-s1-compliance-exemption-grants.yaml.
 *
 * Alert channels, thresholds, and custom checks have moved to the unified
 * alerting surface under `/api/admin/alerts/*` — see `./alerts` (`alertsApi`).
 *
 * Domain types (`UnifiedExemptionRow`, …) are imported from `./proxy` to
 * stay the single source of truth across UI pages.
 */

import { api } from '../../client';
import type {
  CreateExemptionRequest,
  ExemptionListResponse,
  ExemptionListTab,
  UnifiedExemptionRow,
} from '../infrastructure/misc/proxy';

// Re-export domain types so page components can import everything they need
// from a single "compliance" barrel without also reaching into `./proxy`.
export type {
  ExemptionListResponse,
  ExemptionListTab,
  ExemptionStatus,
  UnifiedExemptionRow,
  CreateExemptionRequest,
} from '../infrastructure/misc/proxy';

/** POST response shape: new state plus broadcast fan-out counters. */
export interface KillSwitchToggleResponse {
  engaged: boolean;
  version: number;
  thingsNotified: number;
  thingsOnline: number;
}

// ── Unified compliance audit types ─────────────────────────────────────────

export type ComplianceSource = 'ai-gateway' | 'compliance-proxy' | 'agent';

/** A single unified compliance traffic event row (all three enforcement layers). */
export interface ComplianceAuditRow {
  id: string;
  source: ComplianceSource;
  transactionId: string;
  sourceIp: string;
  targetHost: string;
  method: string | null;
  path: string | null;
  statusCode: number | null;
  requestHookDecision: string | null;
  requestHookReasonCode: string | null;
  bumpStatus: string | null;
  latencyMs: number | null;
  timestamp: string;
  complianceTags: string[];
}

export interface ComplianceAuditListParams {
  source?: ComplianceSource | '';
  hookDecision?: string;
  complianceTags?: string[];
  sourceIp?: string;
  targetHost?: string;
  startTime?: string;
  endTime?: string;
  limit?: number;
  offset?: number;
}

export interface ComplianceAuditListResponse {
  data: ComplianceAuditRow[];
  total: number;
}

// ── Trinity per-layer stats ────────────────────────────────────────────────

export interface TrinityLayerStats {
  totalEvents: number;
  decisions: {
    APPROVE: number;
    MODIFY: number;
    BLOCK_SOFT: number;
    REJECT_HARD: number;
    ABSTAIN: number;
  };
  blockCount: number;
  blockRate: number;
  bumpBreakdown?: {
    BUMP_SUCCESS: number;
    BUMP_FAILED_PASSTHROUGH: number;
    BUMP_EXEMPT: number;
    BUMP_DISABLED: number;
  };
  coveragePercent?: number;
}

export interface TrinityStats {
  period: { start: string; end: string };
  aiGateway: TrinityLayerStats;
  complianceProxy: TrinityLayerStats;
  agent: TrinityLayerStats;
}

// ── Global compliance overview dashboard ──────────────────────────────────────

export interface ComplianceDashboardKPIs {
  totalRequests: number;
  totalBlocked: number;
  overallBlockRate: number;
  tlsCoveragePercent: number;
  hookErrorRate: number;
}

export interface ComplianceDashboardHookHealth {
  total: number;
  byDecision: {
    allow: number;
    deny: number;
    error: number;
    unknown: number;
  };
  topReasonCodes: { label: string; count: number }[];
  latencyP50: number | null;
  latencyP95: number | null;
  latencyP99: number | null;
}

export interface ComplianceDashboardTopBlocked {
  byTarget: { label: string; count: number }[];
  byReasonCode: { label: string; count: number }[];
  bySourceIp: { label: string; count: number }[];
}

export interface ComplianceDashboardData {
  period: { start: string; end: string };
  kpis: ComplianceDashboardKPIs;
  trinity: TrinityStats;
  hookHealth: ComplianceDashboardHookHealth;
  topBlocked: ComplianceDashboardTopBlocked;
}

export const complianceApi = {
  // --- Kill switch ---------------------------------------------------------
  // Read-side data lives on the generic config-sync surface:
  //   - current per-node applied state → hubApi.listNodes({ type })
  //   - toggle history → hubApi.listConfigHistory({ nodeType, configKey: 'killswitch' })
  // Only the canonical fan-out POST lives on the dedicated compliance/killswitch
  // route (its responsibility is to atomically push to both compliance-proxy
  // AND agent template rows + stamp `kill-switch.toggle` SIEM audit).

  setKillSwitch(req: { engaged: boolean }): Promise<KillSwitchToggleResponse> {
    return api.post('/api/admin/compliance/killswitch', req);
  },

  // --- Exemptions (unified list + grant CRUD + pending approve/reject) ----

  listExemptions(params?: {
    tab?: ExemptionListTab;
    limit?: number;
    offset?: number;
  }): Promise<ExemptionListResponse> {
    const qs = new URLSearchParams();
    if (params?.tab) qs.set('tab', params.tab);
    if (params?.limit !== undefined) qs.set('limit', String(params.limit));
    if (params?.offset !== undefined) qs.set('offset', String(params.offset));
    const s = qs.toString();
    return api.get(`/api/admin/compliance/exemption-grants${s ? `?${s}` : ''}`);
  },

  getExemption(id: string): Promise<UnifiedExemptionRow> {
    return api.get(`/api/admin/compliance/exemptions/${id}`);
  },

  createExemptionGrant(input: CreateExemptionRequest): Promise<unknown> {
    const body: Record<string, unknown> = {
      sourceIP: input.sourceIp,
      targetHost: input.targetHost,
      durationMinutes: input.durationMinutes,
      reason: input.reason,
    };
    if (input.effectiveFrom) body.effectiveFrom = input.effectiveFrom;
    return api.post('/api/admin/compliance/exemption-grants', body);
  },

  patchExemptionGrant(id: string, body: { inactive: boolean }): Promise<unknown> {
    return api.patch(`/api/admin/compliance/exemption-grants/${id}`, body);
  },

  /**
   * Creates a PENDING exemption_request row. Reviewed via Approve/Reject on
   * the unified list (rows where kind=pending). Does not write exemptions
   * until approved.
   */
  createPendingExemptionRequest(input: {
    transactionId: string;
    sourceIp: string;
    targetHost: string;
    reason: string;
    durationMinutes: number;
    requestedBy: string;
  }): Promise<unknown> {
    return api.post('/api/admin/exemption-requests', input);
  },

  deleteExemptionGrant(id: string): Promise<void> {
    return api.delete(`/api/admin/compliance/exemption-grants/${id}`);
  },

  /** Approve a PENDING exemption_request. Body is empty per handler contract. */
  approveExemption(id: string): Promise<{
    id: string;
    status: string;
    grantId?: string;
    version?: number;
    thingsNotified?: number;
    thingsOnline?: number;
    reapplied?: boolean;
  }> {
    return api.post(`/api/admin/compliance/exemptions/${id}/approve`, {});
  },

  /** Reject a PENDING exemption_request; `reason` is required for audit trail. */
  rejectExemption(id: string, reason: string): Promise<{ id: string; status: string }> {
    return api.post(`/api/admin/compliance/exemptions/${id}/reject`, { reason });
  },

  // --- Unified compliance audit (all three enforcement layers) -------------

  listAuditEvents(params: ComplianceAuditListParams = {}): Promise<ComplianceAuditListResponse> {
    const qs = new URLSearchParams();
    if (params.source) qs.set('source', params.source);
    if (params.hookDecision) qs.set('hookDecision', params.hookDecision);
    if (params.complianceTags?.length) qs.set('complianceTags', params.complianceTags.join(','));
    if (params.sourceIp) qs.set('sourceIp', params.sourceIp);
    if (params.targetHost) qs.set('targetHost', params.targetHost);
    if (params.startTime) qs.set('startTime', params.startTime);
    if (params.endTime) qs.set('endTime', params.endTime);
    if (params.limit !== undefined) qs.set('limit', String(params.limit));
    if (params.offset !== undefined) qs.set('offset', String(params.offset));
    const s = qs.toString();
    return api.get(`/api/admin/compliance/audit${s ? `?${s}` : ''}`);
  },

  getAuditEvent(id: string): Promise<unknown> {
    return api.get(`/api/admin/compliance/audit/${id}`);
  },

  // --- Trinity per-layer stats (ai-gateway + compliance-proxy + agent) ----

  getTrinityStats(startTime?: string, endTime?: string): Promise<TrinityStats> {
    const qs = new URLSearchParams();
    if (startTime) qs.set('startTime', startTime);
    if (endTime) qs.set('endTime', endTime);
    const s = qs.toString();
    return api.get(`/api/admin/compliance/trinity${s ? `?${s}` : ''}`);
  },

  // --- Global compliance overview dashboard --------------------------------

  getOverview(startTime: string, endTime: string): Promise<ComplianceDashboardData> {
    const qs = new URLSearchParams({ startTime, endTime });
    return api.get(`/api/admin/compliance/overview?${qs.toString()}`);
  },

  buildOverviewExportUrl(params: { startTime: string; endTime: string }): string {
    const qs = new URLSearchParams({ startTime: params.startTime, endTime: params.endTime });
    return `/api/admin/compliance/overview/export?${qs.toString()}`;
  },
};

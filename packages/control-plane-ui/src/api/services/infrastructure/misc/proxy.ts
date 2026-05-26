/**
 * Forward-proxy API service — proxy status, connections, kill switch,
 * reject stats, reject config, and compliance coverage.
 */
import { api } from '../../../client';

export interface ProxyStatus {
  status: 'ok' | 'shutting_down';
  uptimeSeconds: number;
  connectionsActive: number;
  bumpEnabled: boolean;
  redisConnected: boolean;
}

export interface ProxyConnection {
  connectionId: string;
  sourceIp: string;
  targetHost: string;
  bumpStatus: string;
  streamCount: number;
  duration: string;
}


export interface RejectConfig {
  defaultLevel: 0 | 1 | 2;
  contactInfo: string;
  policyOverrides: Record<string, number>;
  updatedAt: string | null;
  updatedBy: string | null;
}

export interface ComplianceCoverage {
  coveragePercent: number;
  breakdown: Record<string, number>;
  period: { start: string; end: string };
}

export interface LabelCount {
  label: string;
  count: number;
}

export interface HookHealthStats {
  total: number;
  byDecision: { allow: number; deny: number; error: number; unknown: number };
  topReasonCodes: LabelCount[];
  latencyP50: number | null;
  latencyP95: number | null;
  latencyP99: number | null;
  period: { start: string; end: string };
}

export interface RejectStats {
  totalRejects: number;
  topTargets: LabelCount[];
  topReasonCodes: LabelCount[];
  bySource: LabelCount[];
  period: { start: string; end: string };
}

/** Lifecycle bucket of a row in the unified exemption list. */
export type ExemptionStatus = 'effective' | 'oncoming' | 'expired' | 'pending';

/** Tab value accepted by GET /api/admin/compliance/exemption-grants. */
export type ExemptionListTab = 'all' | ExemptionStatus;

/**
 * Unified row returned by the exemption list endpoint. `kind` discriminates
 * grants (compliance_exemption_grant) from pending requests (exemption_request).
 * Per-kind fields are null on the opposite kind; see the OpenAPI spec at
 * docs/users/api/openapi/admin/e27-s1-compliance-exemption-grants.yaml for authoritative
 * nullability.
 */
export interface UnifiedExemptionRow {
  kind: 'grant' | 'pending';
  status: ExemptionStatus;
  id: string;
  sourceIp: string;
  targetHost: string;
  reason: string;
  durationMinutes: number;
  createdAt: string;

  // Grant-only (null on pending rows)
  effectiveFrom: string | null;
  expiresAt: string | null;
  approvedBy: string | null;
  inactive: boolean | null;
  activatedAt: string | null;

  // Pending-only (null on grant rows)
  transactionId: string | null;

  // Optional on grants, required on pending — exposed as nullable here
  requestedBy: string | null;
}

export interface ExemptionListResponse {
  rows: UnifiedExemptionRow[];
  total: number;
}

export interface CreateExemptionRequest {
  sourceIp: string;
  targetHost: string;
  durationMinutes: number;
  reason: string;
  /** Optional RFC3339; omit for immediate effect. */
  effectiveFrom?: string;
}


export const proxyApi = {
  getStatus(): Promise<ProxyStatus> {
    return api.get('/api/admin/proxy/health');
  },

  getConnections(targetHost?: string): Promise<{ connections: ProxyConnection[]; total: number }> {
    const params = targetHost ? `?targetHost=${encodeURIComponent(targetHost)}` : '';
    return api.get(`/api/admin/proxy/connections${params}`);
  },


  getComplianceCoverage(startTime: string, endTime: string): Promise<ComplianceCoverage> {
    return api.get(`/api/admin/proxy/compliance/coverage?startTime=${encodeURIComponent(startTime)}&endTime=${encodeURIComponent(endTime)}`);
  },

  getRejectConfig(): Promise<RejectConfig> {
    return api.get('/api/admin/proxy/reject-config');
  },

  updateRejectConfig(config: Partial<RejectConfig>): Promise<RejectConfig> {
    return api.put('/api/admin/proxy/reject-config', config);
  },

  getHookHealth(startTime: string, endTime: string): Promise<HookHealthStats> {
    const qs = new URLSearchParams({ startTime, endTime }).toString();
    return api.get(`/api/admin/proxy/compliance/hook-health?${qs}`);
  },

  getRejectStats(startTime: string, endTime: string): Promise<RejectStats> {
    const qs = new URLSearchParams({ startTime, endTime }).toString();
    return api.get(`/api/admin/proxy/compliance/reject-stats?${qs}`);
  },

  buildComplianceExportUrl(params: {
    startTime: string;
    endTime: string;
    sourceIp?: string;
    targetHost?: string;
    decision?: string;
  }): string {
    const qs = new URLSearchParams();
    qs.set('startTime', params.startTime);
    qs.set('endTime', params.endTime);
    if (params.sourceIp) qs.set('sourceIp', params.sourceIp);
    if (params.targetHost) qs.set('targetHost', params.targetHost);
    if (params.decision) qs.set('decision', params.decision);
    return `/api/admin/proxy/compliance/export?${qs.toString()}`;
  },
};

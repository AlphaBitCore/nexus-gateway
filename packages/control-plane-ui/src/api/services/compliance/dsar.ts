/**
 * DSAR (Data Subject Access Request) service.
 *
 * Compliance officers file, view, and fulfill GDPR/CCPA data subject
 * rights requests through the dashboard. Two request types:
 *
 *   ACCESS  — exports the subject's audit rows (returned in the
 *             fulfill response so the operator can save / forward to
 *             the data subject)
 *   ERASURE — anonymises the subject from both audit tables (we null
 *             out subject identifier columns rather than deleting rows
 *             — Article 17(3)(e) carve-out for legal claims)
 */

import { api } from '../../client';

export type DSARRequestType = 'ACCESS' | 'ERASURE';
export type DSARRequestStatus = 'PENDING' | 'IN_PROGRESS' | 'COMPLETED' | 'REJECTED';

export interface DSARRequest {
  id: string;
  subjectId: string;
  contact: string | null;
  type: DSARRequestType;
  status: DSARRequestStatus;
  notes: string | null;
  completedAt: string | null;
  outcome: Record<string, unknown> | null;
  createdAt: string;
  createdBy: string;
  updatedAt: string;
  updatedBy: string | null;
}

export interface DSARCreateInput {
  subjectId: string;
  contact?: string | null;
  type: DSARRequestType;
  notes?: string | null;
}

export interface DSARUpdateInput {
  notes?: string | null;
  status?: DSARRequestStatus;
}

export interface DSARFulfillResponse {
  request: DSARRequest;
  /** Present only for ACCESS requests. */
  export?: {
    vk: Array<Record<string, unknown>>;
    proxy: Array<Record<string, unknown>>;
  };
  /** Present only for ERASURE requests. */
  outcome?: {
    mode: 'erasure';
    vkRowsAnonymised: number;
    proxyRowsAnonymised: number;
    fulfilledAt: string;
  };
}

export const dsarApi = {
  list(params?: {
    status?: DSARRequestStatus;
    limit?: number;
    offset?: number;
  }): Promise<{ requests: DSARRequest[]; total: number; limit: number; offset: number }> {
    const u = new URLSearchParams();
    if (params?.status) u.set('status', params.status);
    if (params?.limit !== undefined) u.set('limit', String(params.limit));
    if (params?.offset !== undefined) u.set('offset', String(params.offset));
    const qs = u.toString();
    return api.get(`/api/admin/dsar${qs ? `?${qs}` : ''}`);
  },
  get(id: string): Promise<DSARRequest> {
    return api.get(`/api/admin/dsar/${encodeURIComponent(id)}`);
  },
  create(input: DSARCreateInput): Promise<DSARRequest> {
    return api.post('/api/admin/dsar', input);
  },
  update(id: string, input: DSARUpdateInput): Promise<DSARRequest> {
    return api.put(`/api/admin/dsar/${encodeURIComponent(id)}`, input);
  },
  fulfill(id: string): Promise<DSARFulfillResponse> {
    return api.post(`/api/admin/dsar/${encodeURIComponent(id)}/fulfill`, {});
  },
};

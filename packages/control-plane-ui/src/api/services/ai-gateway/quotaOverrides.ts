/**
 * Quota Override API service — per-entity exceptions that override quota policy defaults.
 */
import { api } from '../../client';

export interface QuotaOverride {
  id: string;
  targetType: string;
  targetId: string;
  targetName?: string;
  targetOrgId?: string;
  targetOrgName?: string;
  reason?: string;
  costLimitUsd?: number;
  tokenLimit?: number;
  enforcementMode?: string;
  periodType?: string;
  createdBy?: string;
  createdAt: string;
  updatedAt: string;
}

export interface CreateQuotaOverrideInput {
  targetType: string;
  targetId: string;
  reason?: string;
  costLimitUsd?: number;
  tokenLimit?: number;
  enforcementMode?: string;
  periodType?: string;
}

export const quotaOverrideApi = {
  list: (params?: Record<string, string>) =>
    api.get<{ data: QuotaOverride[]; total: number }>('/api/admin/quota-overrides', params),
  get: (id: string) =>
    api.get<QuotaOverride>(`/api/admin/quota-overrides/${id}`),
  create: (data: CreateQuotaOverrideInput) =>
    api.post<QuotaOverride>('/api/admin/quota-overrides', data),
  update: (id: string, data: Partial<CreateQuotaOverrideInput>) =>
    api.put<QuotaOverride>(`/api/admin/quota-overrides/${id}`, data),
  delete: (id: string) =>
    api.delete(`/api/admin/quota-overrides/${id}`),
};

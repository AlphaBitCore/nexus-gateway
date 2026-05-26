/**
 * Quota Policy API service — manage budget policies for users, VKs, projects, and organizations.
 */
import { api } from '../../client';

export interface QuotaPolicy {
  id: string;
  name: string;
  description?: string;
  scope: string;
  organizationId?: string;
  vkType?: string;
  periodType: string;
  costLimitUsd?: number;
  tokenLimit?: number;
  enforcementMode: string;
  alertThresholds: number[];
  priority: number;
  enabled: boolean;
  createdBy?: string;
  createdAt: string;
  updatedAt: string;
}

export interface CreateQuotaPolicyInput {
  name: string;
  description?: string;
  scope: string;
  organizationId?: string;
  vkType?: string;
  periodType: string;
  costLimitUsd?: number;
  tokenLimit?: number;
  enforcementMode: string;
  alertThresholds?: number[];
  priority?: number;
  enabled?: boolean;
}

export const quotaPolicyApi = {
  list: (params?: Record<string, string>) =>
    api.get<{ data: QuotaPolicy[]; total: number }>('/api/admin/quota-policies', params),
  get: (id: string) =>
    api.get<QuotaPolicy>(`/api/admin/quota-policies/${id}`),
  create: (data: CreateQuotaPolicyInput) =>
    api.post<QuotaPolicy>('/api/admin/quota-policies', data),
  update: (id: string, data: Partial<CreateQuotaPolicyInput>) =>
    api.put<QuotaPolicy>(`/api/admin/quota-policies/${id}`, data),
  delete: (id: string) =>
    api.delete(`/api/admin/quota-policies/${id}`),
};

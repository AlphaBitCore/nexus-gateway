import { api } from '../../client';
import type { VirtualKey, VirtualKeyAllowedModelRef } from '../../types';

export interface CreateVirtualKeyInput {
  name: string;
  projectId?: string;
  sourceApp?: string;
  enabled?: boolean;
  rateLimitRpm?: number;
  allowedModels?: VirtualKeyAllowedModelRef[];
  expiresAt?: string;
  /**
   * `application` for service / shared keys (managed under
   * /security/virtual-keys); `personal` for per-user keys (managed
   * under /account?tab=virtualKeys). Defaults to `personal` server-side
   * when omitted.
   */
  vkType?: 'application' | 'personal';
}

export type UpdateVirtualKeyInput = Partial<CreateVirtualKeyInput>;

export const virtualKeyApi = {
  list: (params?: Record<string, string>) =>
    api.get<{ data: VirtualKey[]; total: number }>('/api/admin/virtual-keys', params),

  get: (id: string) =>
    api.get<VirtualKey>(`/api/admin/virtual-keys/${id}`),

  create: (data: CreateVirtualKeyInput) =>
    api.post<VirtualKey>('/api/admin/virtual-keys', data),

  update: (id: string, data: UpdateVirtualKeyInput) =>
    api.put<VirtualKey>(`/api/admin/virtual-keys/${id}`, data),

  delete: (id: string) =>
    api.delete(`/api/admin/virtual-keys/${id}`),

  regenerate: (id: string) =>
    api.post<{ secretKey: string }>(`/api/admin/virtual-keys/${id}/regenerate`, {}),

  approve: (id: string) =>
    api.post(`/api/admin/virtual-keys/${id}/approve`, {}),

  reject: (id: string, reason: string) =>
    api.post(`/api/admin/virtual-keys/${id}/reject`, { reason }),

  renew: (id: string, expiresAt: string) =>
    api.post(`/api/admin/virtual-keys/${id}/renew`, { expiresAt }),

  revoke: (id: string) =>
    api.post(`/api/admin/virtual-keys/${id}/revoke`, {}),
};

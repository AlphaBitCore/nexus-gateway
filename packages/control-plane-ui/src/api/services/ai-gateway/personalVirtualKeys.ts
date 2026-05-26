/**
 * Personal Virtual Key API service — manage the current user's own personal VKs.
 */
import { api } from '../../client';
import type { VirtualKey } from '../../types';

export interface CreatePersonalVKInput {
  name: string;
  sourceApp?: string;
  rateLimitRpm?: number;
}

export const personalVKApi = {
  list: () =>
    api.get<{ data: VirtualKey[]; total: number }>('/api/my/virtual-keys'),

  create: (data: CreatePersonalVKInput) =>
    api.post<VirtualKey>('/api/my/virtual-keys', data),

  update: (id: string, data: Partial<CreatePersonalVKInput>) =>
    api.put<VirtualKey>(`/api/my/virtual-keys/${id}`, data),

  delete: (id: string) =>
    api.delete(`/api/my/virtual-keys/${id}`),

  regenerate: (id: string) =>
    api.post<{ key: string }>(`/api/my/virtual-keys/${id}/regenerate`, {}),
};

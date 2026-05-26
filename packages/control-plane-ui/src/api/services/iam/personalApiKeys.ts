/**
 * Personal API Key service — manage the current user's own admin API keys.
 * Routes: /api/my/api-keys (no IAM gate, ownership enforced server-side).
 */
import { api } from '../../client';
import type { AdminApiKey } from '../../types';

export interface CreatePersonalApiKeyInput {
  name: string;
  expiresAt?: string;
}

export const personalApiKeyApi = {
  list: () =>
    api.get<{ data: AdminApiKey[] }>('/api/my/api-keys'),

  create: (data: CreatePersonalApiKeyInput) =>
    api.post<AdminApiKey & { key: string }>('/api/my/api-keys', data),

  delete: (id: string) =>
    api.delete(`/api/my/api-keys/${id}`),

  regenerate: (id: string) =>
    api.post<{ id: string; key: string; keyPrefix: string }>(`/api/my/api-keys/${id}/regenerate`, {}),
};

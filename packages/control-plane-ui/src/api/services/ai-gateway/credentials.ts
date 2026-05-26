import { api } from '../../client';
import type {
  Credential,
  CredentialProbeResult,
  ReliabilityConfigResponse,
  ReliabilityThresholds,
} from '../../types';

export interface CreateCredentialInput {
  name: string;
  providerId: string;
  apiKey: string;
  enabled?: boolean;
  expiresAt?: string;
  selectionWeight?: number;
}

export type UpdateCredentialInput = Partial<Pick<CreateCredentialInput, 'name' | 'apiKey' | 'selectionWeight'>> & {
  enabled?: boolean;
  expiresAt?: string | null;
  status?: string;
  retireAt?: string | null;
};

export const credentialApi = {
  list: (params?: Record<string, string>) =>
    api.get<{ data: Credential[]; total: number }>('/api/admin/credentials', params),

  get: (id: string) =>
    api.get<Credential>(`/api/admin/credentials/${id}`),

  create: (data: CreateCredentialInput) =>
    api.post<Credential>('/api/admin/credentials', data),

  update: (id: string, data: UpdateCredentialInput) =>
    api.put<Credential>(`/api/admin/credentials/${id}`, data),

  delete: (id: string) =>
    api.delete(`/api/admin/credentials/${id}`),

  circuitReset: (id: string) =>
    api.post<{ ok: boolean }>(`/api/admin/credentials/${id}/circuit-reset`, {}),

  // Synchronous reliability probe + per-credential threshold overrides.
  probe: (id: string, timeoutSeconds?: number) =>
    api.post<CredentialProbeResult>(`/api/admin/credentials/${id}/probe`, { timeoutSeconds: timeoutSeconds ?? 5 }),

  updateReliabilityOverrides: (id: string, overrides: ReliabilityThresholds | null) =>
    api.put<{ id: string; reliabilityOverrides: ReliabilityThresholds | null }>(
      `/api/admin/credentials/${id}/reliability-overrides`,
      overrides ?? null,
    ),
};

// Global reliability thresholds (Settings page).
export const reliabilitySettingsApi = {
  get: () => api.get<ReliabilityConfigResponse>('/api/admin/settings/credential-reliability'),
  update: (body: ReliabilityThresholds) =>
    api.put<ReliabilityConfigResponse>('/api/admin/settings/credential-reliability', body),
};

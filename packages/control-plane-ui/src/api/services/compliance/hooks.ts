/**
 * Hooks API service.
 */
import { api } from '../../client';
import type { HookConfig, AdminHookImplementationsResponse, AdminHookListResponse, HookExecutionChain, HookReorderResponse } from '../../types';

export interface HookWritePayload {
  name: string;
  type: string;
  stage: string;
  implementationId?: string;
  priority: number;
  timeoutMs: number;
  failBehavior: string;
  enabled: boolean;
  config: Record<string, unknown>;
  category?: string | null;
  endpoint?: string;
  /**
   * Subset of ingress codes this hook applies to (ALL / AI_GATEWAY /
   * COMPLIANCE_PROXY / AGENT). Omit to keep the
   * server-side default (`{ALL}`); the API rejects an explicit empty array.
   */
  applicableIngress?: string[];
}

export interface HookTestRequestBody {
  input?: unknown;
}

export interface HookReorderRequestBody {
  stage: string;
  ids: string[];
}

export type HookUpdatePayload = Partial<HookWritePayload>;

export const hookApi = {
  list: (params?: Record<string, string>) =>
    api.get<AdminHookListResponse>('/api/admin/hooks', params),

  get: (id: string) =>
    api.get<HookConfig>(`/api/admin/hooks/${id}`),

  create: (data: HookWritePayload) =>
    api.post<HookConfig>('/api/admin/hooks', data),

  update: (id: string, data: HookUpdatePayload) =>
    api.put<HookConfig>(`/api/admin/hooks/${id}`, data),

  delete: (id: string) =>
    api.delete(`/api/admin/hooks/${id}`),

  // Canonical endpoint; /test is a backend alias.
  dryRun: (id: string, data: HookTestRequestBody) =>
    api.post<Record<string, unknown>>(`/api/admin/hooks/${id}/dry-run`, data),

  // Alias kept for backwards compatibility with callers that predate dryRun.
  test: (id: string, data: HookTestRequestBody) =>
    api.post<Record<string, unknown>>(`/api/admin/hooks/${id}/dry-run`, data),

  getImplementations: () =>
    api.get<AdminHookImplementationsResponse>('/api/admin/hooks/implementations'),

  getExecutionChain: () =>
    api.get<HookExecutionChain>('/api/admin/hooks/execution-chain'),

  reorder: (data: HookReorderRequestBody) =>
    api.post<HookReorderResponse>('/api/admin/hooks/reorder', data),
};

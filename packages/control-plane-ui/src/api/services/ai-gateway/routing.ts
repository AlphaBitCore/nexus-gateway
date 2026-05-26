/**
 * Routing rules API service.
 */
import { api } from '../../client';
import type { RetryPolicy, RoutingRule, AdminRoutingRuleListResponse } from '../../types';

export interface RoutingRuleWritePayload {
  name: string;
  description?: string;
  strategyType: string;
  config: unknown;
  matchConditions?: unknown;
  priority: number;
  pipelineStage?: number;
  /** Use `null` to clear the chain (gateway accepts JSON null). */
  fallbackChain?: Array<{ providerId: string; modelId: string }> | null;
  /**
   * Per-rule retry override.
   *   omit field → leave the persisted value untouched (PUT only)
   *   `null`     → clear the override; rule inherits the YAML default
   *   `{…}`      → persist the partial override verbatim
   */
  retryPolicy?: RetryPolicy | null;
  enabled: boolean;
}

export type RoutingRuleUpdatePayload = Partial<RoutingRuleWritePayload>;

export interface RoutingRuleListParams {
  q?: string;
  strategyType?: string;
  enabled?: string;
  limit?: string | number;
  offset?: string | number;
  [key: string]: string | number | undefined;
}

export interface RoutingSimulateRequest {
  modelId: string;
  endpointType: string;
  messages?: Array<{ role: string; content: string }>;
}

export interface RoutingSimulateStage {
  stage: number;
  decision: string;
  durationMs: number;
}

export interface RoutingSimulateTrace {
  ruleId?: string;
  ruleName?: string;
  strategyType: string;
  decision: string;
  durationMs: number;
}

export interface RoutingSimulateTarget {
  providerId: string;
  providerName: string;
  modelId: string;
  /** Customer-facing identifier (e.g. "gpt-4o") that clients send as `{model: "..."}`. */
  modelCode: string;
  modelName: string;
  providerModelId: string;
  source: string;
  providerFormat?: string;
  schemaMode?: string;
}

export interface RoutingSimulateBranch {
  providerId: string;
  providerName: string;
  modelId: string;
  modelCode: string;
  modelName: string;
  providerModelId: string;
  probability: number;
  path: string;
  matched: boolean;
  note?: string;
}

export interface RoutingNarrowingSummary {
  allowModelIds: string[];
  denyModelIds: string[];
  allowProviderIds: string[];
  denyProviderIds: string[];
}

export interface RoutingSimulateResponse {
  request: { modelId: string; endpointType: string };
  originalModelId: string;
  substituted: boolean;
  ruleId?: string;
  ruleName?: string;
  stages: RoutingSimulateStage[];
  trace: RoutingSimulateTrace[];
  targets: RoutingSimulateTarget[];
  recoveryTargets: RoutingSimulateTarget[];
  branches?: RoutingSimulateBranch[];
  narrowingSummary?: RoutingNarrowingSummary;
  warnings?: string[];
}

export const routingApi = {
  list: (params?: RoutingRuleListParams) =>
    api.get<AdminRoutingRuleListResponse>('/api/admin/routing-rules', params as Record<string, string>),

  get: (id: string) =>
    api.get<RoutingRule>(`/api/admin/routing-rules/${id}`),

  create: (data: RoutingRuleWritePayload) =>
    api.post<RoutingRule>('/api/admin/routing-rules', data),

  update: (id: string, data: RoutingRuleUpdatePayload) =>
    api.put<RoutingRule>(`/api/admin/routing-rules/${id}`, data),

  /** Partial update — PATCH semantics: only fields present in `data` are changed. */
  patch: (id: string, data: RoutingRuleUpdatePayload) =>
    api.patch<RoutingRule>(`/api/admin/routing-rules/${id}`, data),

  delete: (id: string) =>
    api.delete(`/api/admin/routing-rules/${id}`),

  simulate: (data: RoutingSimulateRequest) =>
    api.post<RoutingSimulateResponse>('/api/admin/routing-rules/simulate', data),
};

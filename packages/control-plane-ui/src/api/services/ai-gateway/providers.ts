/**
 * Provider API service — typed functions for all provider endpoints.
 * Single source of truth for URLs, request shapes, and response types.
 */
import { api } from '../../client';
import { withPrefix } from '@/lib/deploymentPrefix';
import type {
  ApiProviderTemplate,
  Provider,
  ProviderAnalytics,
  ProviderHealth,
  Model,
  CreateModelInput,
} from '../../types';

export interface ProviderConnectivityResult {
  success: boolean;
  statusCode?: number;
  endpoint: string;
  hasApiKey?: boolean;
  latencyMs?: number;
  error?: string;
  body?: string;
}

/** A single model entry returned by the discover-models endpoint. */
export interface DiscoveredModel {
  id: string;
  suggestedType: 'chat' | 'embedding' | 'image' | 'audio';
}

/** Response shape for POST /admin/providers/discover-models. */
export type DiscoverModelsResult =
  | { success: true; models: DiscoveredModel[] }
  | { success: false; error: string; code?: string };

export interface CreateProviderInput {
  name: string;
  displayName?: string;
  description?: string;
  baseUrl: string;
  /**
   * Canonical wire adapter for this provider — one of the nine
   * `providers.Format` slugs exposed by the Control Plane handler
   * (`openai`, `anthropic`, `gemini`, `glm`, `deepseek`, `azure-openai`,
   * `minimax`, `bedrock`, `vertex`). Required on create; operators may
   * change it on update. Replaces the legacy name-based adapter
   * inference in the AI Gateway.
   */
  adapterType: string;
  enabled?: boolean;
  region?: string;
  apiVersion?: string;
  pathPrefix?: string;
  headers?: Record<string, string>;
  /**
   * Optional inline models to register atomically with the provider.
   * Backend wraps the provider + models + credential INSERTs in one
   * transaction so a duplicate name or a (providerId, providerModelId)
   * collision rolls the entire create back — no half-populated state.
   */
  models?: Array<{
    providerModelId: string;
    name: string;
    description?: string;
    type: string;
    features?: string[];
    inputPricePerMillion?: number;
    outputPricePerMillion?: number;
    cachedInputReadPricePerMillion?: number;
    cachedInputWritePricePerMillion?: number;
    maxContextTokens?: number;
    maxOutputTokens?: number;
    aliases?: string[];
  }>;
  /** Optional inline credential; encrypted server-side before insert. */
  credential?: {
    name: string;
    apiKey: string;
    rotationState?: string;
  };
}

export interface CreateProviderResponse extends Provider {
  models?: Model[];
  credential?: { id: string; name: string };
}

export interface UpdateProviderInput extends Partial<CreateProviderInput> {}

export interface ProviderListParams {
  q?: string;
  enabled?: string;
  limit?: string | number;
  offset?: string | number;
  [key: string]: string | number | undefined;
}

export const providerApi = {
  list: (params?: ProviderListParams) =>
    api.get<{ data: Provider[]; total: number }>('/api/admin/providers', params as Record<string, string>),

  get: (id: string) =>
    api.get<Provider>(`/api/admin/providers/${id}`),

  create: (data: CreateProviderInput) =>
    api.post<CreateProviderResponse>('/api/admin/providers', data),

  update: (id: string, data: UpdateProviderInput) =>
    api.put<Provider>(`/api/admin/providers/${id}`, data),

  delete: (id: string) =>
    api.delete(`/api/admin/providers/${id}`),

  getHealth: (id: string) =>
    api.get<ProviderHealth>(`/api/admin/providers/${id}/health`),

  getModels: (id: string) =>
    api.get<{ data: Model[] }>(`/api/admin/providers/${id}/models`),

  addModel: (providerId: string, data: CreateModelInput) =>
    api.post<Model>(`/api/admin/providers/${providerId}/models`, data),

  getAnalytics: (id: string) =>
    api.get<ProviderAnalytics>(`/api/admin/analytics/provider/${id}`),

  // Built-in provider template catalog is owned by the UI: static JSON under
  // `public/provider-templates/`. The wizard fetches `index.json` for the list
  // and `<name>.json` for the selected template's full detail (meta + models).
  // No backend round-trip — adding a template is "drop a JSON file".
  getTemplates: async (): Promise<{ data: ApiProviderTemplate[] }> => {
    const res = await fetch(withPrefix('/provider-templates/index.json'), { cache: 'no-cache' });
    if (!res.ok) throw new Error(`load provider templates index: ${res.status}`);
    const json = await res.json() as { templates: ApiProviderTemplate[] };
    return { data: json.templates };
  },

  getTemplateDetail: async (name: string): Promise<ApiProviderTemplate> => {
    const res = await fetch(withPrefix(`/provider-templates/${encodeURIComponent(name)}.json`), { cache: 'no-cache' });
    if (!res.ok) throw new Error(`load provider template ${name}: ${res.status}`);
    return await res.json() as ApiProviderTemplate;
  },

  testExisting: (providerId: string, credentialId?: string) =>
    api.post<ProviderConnectivityResult>(`/api/admin/providers/${providerId}/test`, { credentialId: credentialId ?? undefined }),

  testConnection: (data: { name: string; adapterType: string; baseUrl: string; apiKey: string }) =>
    api.post<ProviderConnectivityResult>('/api/admin/providers/test-connection', data),

  /**
   * Discover models from a provider's /v1/models endpoint.
   * Only supported for OpenAI / OpenAI-compatible adapters.
   * Returns the parsed success/failure result — callers must check `success`.
   */
  discoverModels: (data: { adapterType: string; baseUrl: string; apiKey: string }) =>
    api.post<DiscoverModelsResult>('/api/admin/providers/discover-models', data),
};

/**
 * Prompt Cache 3-Tier Config API service.
 *
 * Backs the `/ai-gateway/cache` page (Provider Prompt Cache section) and
 * the `/ai-gateway/providers/:id` Cache tab.
 *
 * Storage shapes mirror Go structs in `packages/shared/cacheconfig/types.go`.
 * Boolean / number fields are optional at every tier — absence means
 * "inherit from a lower tier or fall back to the code default".
 */
import { api } from '../../client';

// ── Tier 1: global ──────────────────────────────────────────────────────

export interface CacheGlobalConfig {
  normaliser_enabled?: boolean;
  cache_master_kill_switch?: boolean;
}

// ── Tier 2: per adapter family ──────────────────────────────────────────

export interface CacheAdapterConfig {
  /** Anthropic family knobs. */
  marker_inject_enabled?: boolean;
  marker_boundary3_enabled?: boolean;
  /** Gemini family knobs. */
  cache_enabled?: boolean;
  min_system_chars?: number;
  ttl_seconds?: number;
  circuit_breaker_threshold?: number;
  circuit_breaker_open_secs?: number;
  /** Per-rule_id override map. Rule metadata (regex, body_path, adapter) is code-baked. */
  rules?: Record<string, CacheRuleOverride>;
}

export interface CacheRuleOverride {
  enabled?: boolean;
  dry_run_always?: boolean;
}

// ── Tier 3: per-provider override ───────────────────────────────────────

export interface CacheProviderConfig {
  marker_inject_enabled?: boolean;
  marker_boundary3_enabled?: boolean;
  cache_enabled?: boolean;
  min_system_chars?: number;
  ttl_seconds?: number;
  circuit_breaker_threshold?: number;
  circuit_breaker_open_secs?: number;
}

// ── Effective view + Overrides listing ──────────────────────────────────

export type CacheSource = 'provider-override' | 'adapter-default' | 'global-default' | 'code-default';

export interface CacheEffectiveResponse {
  provider_id: string;
  provider_name: string;
  adapter_type: string;
  effective: Record<string, boolean | number>;
  sources: Record<string, CacheSource>;
  rules?: Record<string, CacheRuleOverride>;
}

export interface CacheOverrideDiff {
  inherited: unknown;
  override: unknown;
  inherited_source: CacheSource;
}

export interface CacheOverrideRow {
  provider_id: string;
  provider_name: string;
  adapter_type: string;
  overridden_keys: string[];
  diff: Record<string, CacheOverrideDiff>;
  updated_at?: string;
  updated_by?: string;
}

export interface CacheOverridesList {
  items: CacheOverrideRow[];
  total: number;
}

// ── API surface ─────────────────────────────────────────────────────────

export interface CacheAdaptersList {
  items: Record<string, CacheAdapterConfig>;
  total: number;
}

export const cacheApi = {
  getGlobal: () => api.get<CacheGlobalConfig>('/api/admin/cache/global'),

  putGlobal: (input: CacheGlobalConfig) =>
    api.put<CacheGlobalConfig>('/api/admin/cache/global', input),

  listAdapters: () => api.get<CacheAdaptersList>('/api/admin/cache/adapters'),

  getAdapter: (adapterType: string) =>
    api.get<CacheAdapterConfig>(`/api/admin/cache/adapter/${encodeURIComponent(adapterType)}`),

  putAdapter: (adapterType: string, input: CacheAdapterConfig) =>
    api.put<CacheAdapterConfig>(`/api/admin/cache/adapter/${encodeURIComponent(adapterType)}`, input),

  getProvider: (providerId: string) =>
    api.get<CacheProviderConfig>(`/api/admin/cache/provider/${encodeURIComponent(providerId)}`),

  putProvider: (providerId: string, input: CacheProviderConfig) =>
    api.put<CacheProviderConfig>(`/api/admin/cache/provider/${encodeURIComponent(providerId)}`, input),

  deleteProvider: (providerId: string) =>
    api.delete(`/api/admin/cache/provider/${encodeURIComponent(providerId)}`),

  getEffective: (providerId: string) =>
    api.get<CacheEffectiveResponse>(`/api/admin/cache/effective?provider_id=${encodeURIComponent(providerId)}`),

  listOverrides: () => api.get<CacheOverridesList>('/api/admin/cache/overrides'),
};

/** Adapter families used by the UI to drive per-family field visibility. */
export type CacheAdapterFamily = 'anthropic' | 'gemini' | 'none';

export function familyOf(adapterType: string): CacheAdapterFamily {
  if (adapterType === 'anthropic' || adapterType === 'bedrock') return 'anthropic';
  if (adapterType === 'gemini' || adapterType === 'vertex') return 'gemini';
  return 'none';
}

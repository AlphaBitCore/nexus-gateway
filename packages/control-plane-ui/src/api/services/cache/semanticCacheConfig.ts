/**
 * Semantic Cache Config API service — admin semantic-cache singleton
 * endpoints and the embedding probe endpoint.
 *
 * Endpoints:
 *   GET  /api/admin/semantic-cache/config
 *   PUT  /api/admin/semantic-cache/config
 *   POST /api/admin/providers/:id/embedding-probe
 *
 * queryKey conventions:
 *   ['admin', 'semantic-cache', 'config']
 *   ['admin', 'providers', 'embedding-probe', providerId]
 */
import { api } from '../../client';
import type { SemanticCacheConfig, ProbeResult } from '../../types';

export interface SemanticCacheUpdateInput {
  embeddingProviderId: string | null;
  embeddingModelId: string | null;
  embeddingDimension: number | null;
  enabled: boolean;
  /** Optional fleet tuning. Omit → preserve current value. */
  threshold?: number;
  /** Optional fleet tuning. Omit → preserve current value. */
  varyBy?: 'none' | 'user' | 'vk' | 'org';
  /** Optional fleet tuning. Omit → preserve current value. */
  embedStrategy?:
    | 'last_user'
    | 'system_plus_last_user'
    | 'recent_turns'
    | 'head_plus_tail'
    | 'full_truncated';
  /** Optional fleet tuning. Omit → preserve current value. */
  allowCrossModel?: boolean;
}

export const semanticCacheConfigApi = {
  getConfig: (): Promise<SemanticCacheConfig> =>
    api.get<SemanticCacheConfig>('/api/admin/semantic-cache/config'),

  saveConfig: (input: SemanticCacheUpdateInput): Promise<SemanticCacheConfig> =>
    api.put<SemanticCacheConfig>('/api/admin/semantic-cache/config', input),

  runProbe: (providerId: string): Promise<ProbeResult> =>
    api.post<ProbeResult>(`/api/admin/providers/${providerId}/embedding-probe`, {}),
};

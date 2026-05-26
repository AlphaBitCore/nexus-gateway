/**
 * Extract Cache (L1) Config API service.
 *
 * Endpoints:
 *   GET /api/admin/extract-cache/config
 *   PUT /api/admin/extract-cache/config
 *
 * queryKey: ['admin', 'extract-cache', 'config']
 */
import { api } from '../../client';

export interface ExtractCacheConfig {
  id: 'singleton';
  enabled: boolean;
  /** Range [60, 604800]. Default 3600. */
  ttlSeconds: number;
  /**
   * Fleet-wide gate for whether classifyCachePreLookup honours a
   * freshness-detector match by skipping BOTH L1 and L2 lookups.
   * Surfaced in the UI on the Freshness rules card (the field's
   * effect spans both cache layers) even though it physically lives
   * on this singleton row.
   */
  applyFreshnessRules: boolean;
  updatedAt: string;
  updatedBy: string | null;
}

export interface ExtractCacheUpdateInput {
  enabled: boolean;
  ttlSeconds: number;
  applyFreshnessRules: boolean;
}

export const extractCacheConfigApi = {
  getConfig: (): Promise<ExtractCacheConfig> =>
    api.get<ExtractCacheConfig>('/api/admin/extract-cache/config'),

  saveConfig: (input: ExtractCacheUpdateInput): Promise<ExtractCacheConfig> =>
    api.put<ExtractCacheConfig>('/api/admin/extract-cache/config', input),
};

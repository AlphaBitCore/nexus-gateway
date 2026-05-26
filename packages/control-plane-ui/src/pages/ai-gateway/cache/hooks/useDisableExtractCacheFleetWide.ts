/**
 * Hook: disable extract (L1) cache fleet-wide.
 *
 * Used by the StatusStrip emergency dropdown. Fetches the current
 * ExtractCacheConfig and resubmits it with `enabled: false`, preserving
 * ttlSeconds + applyFreshnessRules so the other admin-tunable fields
 * are not silently reset.
 */
import { useMutation } from '@/hooks/useMutation';
import { extractCacheConfigApi } from '@/api/services/cache/extractCacheConfig';

interface UseDisableExtractCacheFleetWideOptions {
  successMessage?: string;
  errorMessage?: string;
}

export function useDisableExtractCacheFleetWide(options?: UseDisableExtractCacheFleetWideOptions) {
  const { mutate: disable, loading } = useMutation(
    async (_: void) => {
      const current = await extractCacheConfigApi.getConfig();
      return extractCacheConfigApi.saveConfig({
        enabled: false,
        ttlSeconds: current.ttlSeconds,
        applyFreshnessRules: current.applyFreshnessRules,
      });
    },
    {
      invalidateQueries: [['admin', 'extract-cache', 'config']],
      successMessage: options?.successMessage,
      errorMessage: options?.errorMessage,
    },
  );
  return { disable, loading };
}

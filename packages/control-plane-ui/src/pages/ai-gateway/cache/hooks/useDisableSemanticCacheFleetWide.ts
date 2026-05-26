/**
 * Hook: disable semantic cache fleet-wide.
 *
 * Used by:
 *   - The StatusStrip emergency button on the Cache admin page
 *   - The Traffic Audit Drawer's inline "Disable semantic cache" action
 *     when a problematic semantic-hit shows up in an audit row
 *
 * Renamed from the legacy useDisableL2FleetWide as part of dropping
 * L1-L4 tier nomenclature from the admin surface. Identical semantics:
 * fetches the current SemanticCacheConfig and resubmits it with
 * `enabled: false`, preserving the rest of the singleton row.
 */
import { useMutation } from '@/hooks/useMutation';
import { semanticCacheConfigApi } from '@/api/services/cache/semanticCacheConfig';

interface UseDisableSemanticCacheFleetWideOptions {
  successMessage?: string;
  errorMessage?: string;
}

export function useDisableSemanticCacheFleetWide(options?: UseDisableSemanticCacheFleetWideOptions) {
  const { mutate: disable, loading } = useMutation(
    async (_: void) => {
      const current = await semanticCacheConfigApi.getConfig();
      return semanticCacheConfigApi.saveConfig({
        embeddingProviderId: current.embeddingProviderId ?? null,
        embeddingModelId: current.embeddingModelId ?? null,
        embeddingDimension: current.embeddingDimension ?? null,
        enabled: false,
      });
    },
    {
      invalidateQueries: [['admin', 'semantic-cache', 'config']],
      successMessage: options?.successMessage,
      errorMessage: options?.errorMessage,
    },
  );
  return { disable, loading };
}

import { useQuery, keepPreviousData } from '@tanstack/react-query';

interface UseApiResult<T> {
  data: T | null;
  loading: boolean;
  error: Error | null;
  refetch: () => void;
}

/**
 * Data-fetching hook backed by TanStack React Query.
 *
 * `queryKey` must include every input that affects the fetcher result (plus a stable domain prefix)
 * so caches dedupe, prefetch, and invalidation stay predictable. React Query stores under `['api', ...queryKey]`.
 *
 * staleTime is **0 by default** and refetchOnMount is **'always'** —
 * an admin dashboard prizes freshness over skipping network calls. With
 * 'always', every nav-back (and every remount) refetches, so a row the
 * user just edited elsewhere shows up-to-date data without each mutator
 * remembering to call invalidateQueries. The 'true' setting was
 * insufficient: keepPreviousData + same-tick remount races could leave
 * the staleness check returning fresh, silently skipping the refetch
 * (the bug surfaced when users navigated away from a list and back to
 * find it still showing yesterday's data).
 *
 * Set `staleMs` explicitly when a view needs to coalesce repeat reads
 * (e.g. the action catalog, which is immutable across a session). When
 * staleMs > 0, refetchOnMount falls back to 'true' so the cache is
 * actually respected.
 */
export function useApi<T>(
  fetcher: () => Promise<T>,
  queryKey: readonly unknown[],
  options?: {
    skip?: boolean;
    /**
     * Auto-refetch interval in milliseconds. Used by views that need live
     * state without WebSocket plumbing (e.g. ReliabilityPanel polls
     * the credential row every 8 s to surface live circuit + auth-fails).
     * Omit (or set to 0) for the default static fetch.
     */
    refetchInterval?: number;
    /**
     * Override staleTime in milliseconds. Default 0 (always refetch on
     * mount + remount). Set a positive value for views that should
     * tolerate cached data across a session (e.g. immutable catalogs).
     */
    staleMs?: number;
  },
): UseApiResult<T> {
  const skip = options?.skip ?? false;
  const refetchInterval = options?.refetchInterval && options.refetchInterval > 0 ? options.refetchInterval : false;
  const staleTime = options?.staleMs ?? 0;
  // 'always' on the default (staleMs=0) path forces a refetch on every
  // mount/nav-back regardless of cache state. When the caller opts into a
  // positive staleMs (e.g. an immutable catalog), respect their intent
  // and fall back to 'true' so the cache window actually applies.
  const refetchOnMount: 'always' | true = staleTime > 0 ? true : 'always';

  const { data, isPending, error, refetch } = useQuery<T, Error>({
    queryKey: ['api', ...queryKey],
    queryFn: fetcher,
    enabled: !skip,
    refetchOnWindowFocus: true,
    refetchOnMount,
    retry: 1,
    staleTime,
    refetchInterval,
    // Keep previous data visible while a new fetch runs (queryKey changed).
    // Without this, data becomes undefined on every filter/search change,
    // which triggers if (loading && !data) skeleton early-returns and unmounts
    // the search input — causing page jumps and focus loss.
    placeholderData: keepPreviousData,
  });

  const loading = skip ? false : (isPending && !data);

  return {
    data: skip ? null : (data ?? null),
    loading,
    error: error ?? null,
    refetch: () => { void refetch(); },
  };
}

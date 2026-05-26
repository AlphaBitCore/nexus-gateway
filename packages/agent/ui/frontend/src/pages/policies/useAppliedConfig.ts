import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { agentApi } from '@/api/agent';
import type { AppliedConfig } from '@/api/agent';

// useAppliedConfig is the single React Query subscription every
// /policies/* page shares. Caching is in-memory only (no localStorage)
// — the daemon's GET_APPLIED_CONFIG IPC is cheap (decode of in-memory
// shadow snapshot) so refetching costs nothing. staleTime=5s avoids
// thrashing during fast in-page navigation, refetchInterval=10s keeps
// the data fresh enough that a CP change becomes visible without the
// user clicking Refresh.
export function useAppliedConfig() {
  return useQuery({
    queryKey: ['agent', 'applied-config'],
    queryFn: () => agentApi.getAppliedConfig() as Promise<AppliedConfig>,
    staleTime: 5_000,
    refetchInterval: 10_000,
  });
}

// useRefreshPolicies wraps the REFRESH_POLICIES IPC + post-success
// query invalidation. Returned tuple: [trigger, state]. Components mount
// the trigger on a button onClick; state carries the in-flight flag and
// the last error message so the UI can render a spinner / banner.
export function useRefreshPolicies() {
  const queryClient = useQueryClient();
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const trigger = async () => {
    setRefreshing(true);
    setError(null);
    try {
      const resp = await agentApi.refreshPolicies();
      if (!resp.ok && resp.error) {
        setError(resp.error);
      }
      await queryClient.invalidateQueries({ queryKey: ['agent', 'applied-config'] });
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setRefreshing(false);
    }
  };
  return { refreshing, error, trigger, clearError: () => setError(null) };
}

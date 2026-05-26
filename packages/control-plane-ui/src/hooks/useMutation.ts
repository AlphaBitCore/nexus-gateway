import { useMutation as useRQMutation, useQueryClient, type QueryKey } from '@tanstack/react-query';
import { useToast } from '../context/ToastContext';

interface UseMutationOptions<TOutput> {
  onSuccess?: (data: TOutput) => void;
  /**
   * Called after a failed mutation. The default toast still fires unless
   * the consumer opts out via `silentError: true`. Use onError for inline
   * error surfaces that want to keep the previous result visible instead of
   * toasting on failure.
   */
  onError?: (err: Error) => void;
  /** Suppress the default error toast so the consumer can present its own. */
  silentError?: boolean;
  successMessage?: string;
  errorMessage?: string;
  /**
   * After success, invalidate these query key prefixes (same shape as useQuery keys, e.g. `['api','projects']`).
   */
  invalidateQueries?: readonly QueryKey[];
}

/**
 * Mutation hook backed by TanStack React Query.
 * Optionally invalidates cached queries so list/detail views refetch after writes.
 */
export function useMutation<TInput, TOutput>(
  mutationFn: (input: TInput) => Promise<TOutput>,
  options?: UseMutationOptions<TOutput>,
) {
  const { addToast } = useToast();
  const queryClient = useQueryClient();

  const mutation = useRQMutation<TOutput, Error, TInput>({
    mutationFn,
    onSuccess: async (data) => {
      addToast(options?.successMessage ?? 'Operation completed successfully', 'success');
      if (options?.invalidateQueries?.length) {
        await Promise.all(
          options.invalidateQueries.map((qk) => {
            // useApi registers queries under ['api', ...key]; normalise so callers
            // don't need to remember the prefix.
            const key = Array.isArray(qk) && qk[0] === 'api' ? qk : ['api', ...qk as unknown[]];
            return queryClient.invalidateQueries({ queryKey: key });
          }),
        );
      }
      options?.onSuccess?.(data);
    },
    onError: (err) => {
      if (!options?.silentError) {
        addToast(options?.errorMessage ?? err.message, 'error');
      }
      options?.onError?.(err);
    },
  });

  return {
    mutate: mutation.mutateAsync,
    loading: mutation.isPending,
    error: mutation.error ?? null,
  };
}

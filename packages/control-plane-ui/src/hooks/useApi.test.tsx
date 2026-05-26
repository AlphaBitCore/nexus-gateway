import { describe, it, expect, vi } from 'vitest';
import { renderHook, waitFor, act } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { type ReactNode } from 'react';
import { useApi } from './useApi';

function createWrapper() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 0, gcTime: 0 } },
  });
  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  );
}

describe('useApi', () => {
  it('returns data on success', async () => {
    const fetcher = vi.fn().mockResolvedValue({ name: 'test' });

    const { result } = renderHook(() => useApi(fetcher, ['key1']), {
      wrapper: createWrapper(),
    });

    await waitFor(() => expect(result.current.data).toEqual({ name: 'test' }));
    expect(result.current.loading).toBe(false);
    expect(result.current.error).toBeNull();
  });

  it('returns error on fetch failure', async () => {
    const fetcher = vi.fn().mockRejectedValue(new Error('Network error'));

    const { result } = renderHook(() => useApi(fetcher, ['key2']), {
      wrapper: createWrapper(),
    });

    await waitFor(() => expect(result.current.error).not.toBeNull(), { timeout: 3000 });
    expect(result.current.error!.message).toBe('Network error');
    expect(result.current.data).toBeNull();
  });

  it('does not fetch when skip=true', () => {
    const fetcher = vi.fn().mockResolvedValue({ name: 'test' });

    const { result } = renderHook(
      () => useApi(fetcher, ['key3'], { skip: true }),
      { wrapper: createWrapper() },
    );

    expect(result.current.loading).toBe(false);
    expect(result.current.data).toBeNull();
    expect(fetcher).not.toHaveBeenCalled();
  });

  it('refetch triggers a new fetch', async () => {
    let callCount = 0;
    const fetcher = vi.fn().mockImplementation(() =>
      Promise.resolve({ count: ++callCount }),
    );

    const { result } = renderHook(() => useApi(fetcher, ['key4']), {
      wrapper: createWrapper(),
    });

    await waitFor(() => expect(result.current.data).toEqual({ count: 1 }));

    await act(async () => { result.current.refetch(); });

    await waitFor(() => expect(result.current.data).toEqual({ count: 2 }));
  });
});

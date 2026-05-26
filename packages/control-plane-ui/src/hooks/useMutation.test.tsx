import { describe, it, expect, vi } from 'vitest';
import { renderHook, waitFor, act } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { type ReactNode } from 'react';
import { useMutation } from './useMutation';

vi.mock('../context/ToastContext', () => ({
  useToast: () => ({ addToast: vi.fn() }),
}));

function createWrapper() {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false } },
  });
  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  );
}

describe('useMutation', () => {
  it('starts with loading=false', () => {
    const fn = vi.fn().mockResolvedValue({ id: '1' });
    const { result } = renderHook(() => useMutation(fn), { wrapper: createWrapper() });
    expect(result.current.loading).toBe(false);
    expect(result.current.error).toBeNull();
  });

  it('resolves and calls onSuccess', async () => {
    const fn = vi.fn().mockResolvedValue({ id: '1' });
    const onSuccess = vi.fn();

    const { result } = renderHook(
      () => useMutation(fn, { onSuccess }),
      { wrapper: createWrapper() },
    );

    await act(async () => {
      await result.current.mutate({ name: 'test' });
    });

    expect(onSuccess).toHaveBeenCalledWith({ id: '1' });
    expect(result.current.loading).toBe(false);
  });

  it('sets error on failure', async () => {
    const fn = vi.fn().mockRejectedValue(new Error('Server error'));

    const { result } = renderHook(() => useMutation(fn), { wrapper: createWrapper() });

    await act(async () => {
      try { await result.current.mutate({ name: 'test' }); } catch { /* expected */ }
    });

    await waitFor(() => expect(result.current.error).not.toBeNull());
    expect(result.current.error!.message).toBe('Server error');
  });

  it('invalidates queries before onSuccess', async () => {
    const queryClient = new QueryClient({
      defaultOptions: { mutations: { retry: false } },
    });
    const invalidateSpy = vi.spyOn(queryClient, 'invalidateQueries');
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    );
    const fn = vi.fn().mockResolvedValue({ ok: true });
    const onSuccess = vi.fn();

    const { result } = renderHook(
      () =>
        useMutation(fn, {
          invalidateQueries: [['api', 'admin', 'widgets']],
          onSuccess,
        }),
      { wrapper },
    );

    await act(async () => {
      await result.current.mutate({ name: 'test' });
    });

    expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: ['api', 'admin', 'widgets'] });
    expect(onSuccess).toHaveBeenCalledWith({ ok: true });
  });
});

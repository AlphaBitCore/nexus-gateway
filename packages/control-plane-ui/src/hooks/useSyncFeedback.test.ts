/**
 * Unit test — useSyncFeedback calls hubApi.listNodes and shows toast.
 */
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useSyncFeedback } from './useSyncFeedback';

const mockAddToast = vi.fn();
const mockListNodes = vi.fn();

vi.mock('@/api/services/infrastructure/nodes/hub', () => ({
  hubApi: {
    listNodes: (...args: unknown[]) => mockListNodes(...args),
  },
}));

vi.mock('@/context/ToastContext', () => ({
  useToast: () => ({ addToast: mockAddToast }),
}));

vi.mock('react-i18next', () => ({
  useTranslation: () => ({
    t: (key: string, opts?: Record<string, unknown>) =>
      opts?.count != null ? `${key}:${opts.count}` : key,
  }),
}));

beforeEach(() => {
  vi.clearAllMocks();
});

describe('useSyncFeedback', () => {
  it('calls hubApi.listNodes after invocation', async () => {
    mockListNodes.mockResolvedValue({ nodes: [], total: 5, page: 1, pageSize: 1 });

    const { result } = renderHook(() => useSyncFeedback());

    await act(async () => {
      await result.current('ai-gateway');
    });

    expect(mockListNodes).toHaveBeenCalledWith({
      type: 'ai-gateway',
      status: 'online',
      pageSize: 1,
    });
    expect(mockAddToast).toHaveBeenCalledTimes(1);
  });

  it('does not show toast when no online nodes', async () => {
    mockListNodes.mockResolvedValue({ nodes: [], total: 0, page: 1, pageSize: 1 });

    const { result } = renderHook(() => useSyncFeedback());

    await act(async () => {
      await result.current('agent');
    });

    expect(mockListNodes).toHaveBeenCalled();
    expect(mockAddToast).not.toHaveBeenCalled();
  });

  it('silently fails on API error', async () => {
    mockListNodes.mockRejectedValue(new Error('Admin API unreachable'));

    const { result } = renderHook(() => useSyncFeedback());

    await act(async () => {
      await result.current('compliance-proxy');
    });

    expect(mockListNodes).toHaveBeenCalled();
    expect(mockAddToast).not.toHaveBeenCalled();
  });
});

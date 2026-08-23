import { describe, it, expect, vi, afterEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useTimeouts } from '@/hooks/useTimeouts';

// The invariant: nothing this hook arms may outlive the component. The
// assertion is on pending-timer count rather than on the crash it prevents,
// because the crash needs the environment to be torn down — which a test
// running inside that environment cannot stage.
describe('useTimeouts', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it('cancels every armed timer when the component unmounts', () => {
    vi.useFakeTimers();
    const { result, unmount } = renderHook(() => useTimeouts());
    act(() => {
      result.current(() => {}, 2000);
      result.current(() => {}, 90_000);
    });
    expect(vi.getTimerCount()).toBe(2);

    unmount();

    expect(vi.getTimerCount()).toBe(0);
  });

  it('still runs the callback while mounted', () => {
    vi.useFakeTimers();
    const spy = vi.fn();
    const { result } = renderHook(() => useTimeouts());
    act(() => {
      result.current(spy, 1000);
    });
    expect(spy).not.toHaveBeenCalled();
    act(() => {
      vi.advanceTimersByTime(1000);
    });
    expect(spy).toHaveBeenCalledTimes(1);
  });

  it('does not accumulate handles for timers that already fired', () => {
    // A long-lived page that copies to the clipboard fifty times should not be
    // holding fifty dead handles.
    vi.useFakeTimers();
    const { result } = renderHook(() => useTimeouts());
    act(() => {
      result.current(() => {}, 100);
    });
    act(() => {
      vi.advanceTimersByTime(100);
    });
    act(() => {
      result.current(() => {}, 5000);
    });
    // Only the still-pending one remains; the fired one removed itself.
    expect(vi.getTimerCount()).toBe(1);
  });

  it('keeps a stable identity across renders so it is safe as a dependency', () => {
    const { result, rerender } = renderHook(() => useTimeouts());
    const first = result.current;
    rerender();
    expect(result.current).toBe(first);
  });
});

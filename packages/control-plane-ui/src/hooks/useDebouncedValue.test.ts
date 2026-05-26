import { describe, it, expect, vi } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useDebouncedValue } from './useDebouncedValue';

describe('useDebouncedValue', () => {
  it('returns initial value immediately', () => {
    const { result } = renderHook(() => useDebouncedValue('hello', 300));
    expect(result.current).toBe('hello');
  });

  it('debounces value changes', async () => {
    vi.useFakeTimers();

    const { result, rerender } = renderHook(
      ({ value }) => useDebouncedValue(value, 300),
      { initialProps: { value: 'a' } },
    );

    expect(result.current).toBe('a');

    rerender({ value: 'ab' });
    expect(result.current).toBe('a'); // Not updated yet

    rerender({ value: 'abc' });
    expect(result.current).toBe('a'); // Still debouncing

    act(() => {
      vi.advanceTimersByTime(300);
    });

    expect(result.current).toBe('abc'); // Now updated to latest

    vi.useRealTimers();
  });

  it('resets timer on each change', () => {
    vi.useFakeTimers();

    const { result, rerender } = renderHook(
      ({ value }) => useDebouncedValue(value, 500),
      { initialProps: { value: 'x' } },
    );

    rerender({ value: 'xy' });
    act(() => { vi.advanceTimersByTime(400); });
    expect(result.current).toBe('x'); // Timer reset, not fired yet

    rerender({ value: 'xyz' });
    act(() => { vi.advanceTimersByTime(400); });
    expect(result.current).toBe('x'); // Timer reset again

    act(() => { vi.advanceTimersByTime(500); });
    expect(result.current).toBe('xyz'); // Now fires

    vi.useRealTimers();
  });
});

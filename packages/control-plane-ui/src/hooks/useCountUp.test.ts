import { describe, it, expect } from 'vitest';
import { renderHook } from '@testing-library/react';
import { useCountUp } from './useCountUp';

describe('useCountUp', () => {
  it('returns 0 for target 0', () => {
    const { result } = renderHook(() => useCountUp(0));
    expect(result.current).toBe(0);
  });

  it('does not crash and returns a number for positive target', () => {
    const { result } = renderHook(() => useCountUp(100));
    // In jsdom, rAF exists but may not tick — value is 0 or progressing
    expect(typeof result.current).toBe('number');
    expect(result.current).toBeGreaterThanOrEqual(0);
  });

  it('does not crash without window.matchMedia', () => {
    // jsdom doesn't have matchMedia — hook should not throw
    expect(() => {
      renderHook(() => useCountUp(500));
    }).not.toThrow();
  });

  it('handles negative target', () => {
    const { result } = renderHook(() => useCountUp(-10));
    // Should not crash
    expect(typeof result.current).toBe('number');
  });
});

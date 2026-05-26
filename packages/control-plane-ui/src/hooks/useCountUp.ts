/**
 * Animates a number from 0 to the target value on mount.
 * Respects prefers-reduced-motion.
 */
import { useState, useEffect, useRef } from 'react';

export function useCountUp(target: number, duration = 800): number {
  const [value, setValue] = useState(0);
  const skipAnimation = useRef(
    // Skip in test environments or when user prefers reduced motion
    typeof requestAnimationFrame === 'undefined' ||
    (typeof window !== 'undefined' && typeof window.matchMedia === 'function'
      && window.matchMedia('(prefers-reduced-motion: reduce)').matches),
  );

  useEffect(() => {
    if (skipAnimation.current || target === 0) {
      setValue(target);
      return;
    }

    const start = performance.now();
    let raf: number;

    const tick = (now: number) => {
      const elapsed = now - start;
      const progress = Math.min(elapsed / duration, 1);
      // Ease-out cubic for natural deceleration
      const eased = 1 - Math.pow(1 - progress, 3);
      setValue(Math.round(eased * target));
      if (progress < 1) raf = requestAnimationFrame(tick);
    };

    raf = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(raf);
  }, [target, duration]);

  return value;
}

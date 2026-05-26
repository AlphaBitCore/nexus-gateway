/**
 * TopLoader — thin progress bar at the top of the viewport during navigation.
 *
 * Renders inside <Suspense fallback={...}> — appears when a lazy chunk is loading.
 * Simulates progress (0→90% fast, then slows) and completes to 100% on unmount.
 */
import { useState, useEffect, useRef } from 'react';
import clsx from 'clsx';
import styles from './TopLoader.module.css';

export function TopLoader() {
  const [width, setWidth] = useState(0);
  const [complete, setComplete] = useState(false);
  const rafRef = useRef<number>(0);

  useEffect(() => {
    let progress = 10;
    setWidth(progress);

    const tick = () => {
      // Fast to 30%, medium to 60%, slow to 90%
      if (progress < 30) progress += 3;
      else if (progress < 60) progress += 1.5;
      else if (progress < 90) progress += 0.3;
      setWidth(Math.min(progress, 92));
      rafRef.current = requestAnimationFrame(tick);
    };

    // Start after a small delay (avoid flash on fast loads)
    const timer = setTimeout(() => {
      rafRef.current = requestAnimationFrame(tick);
    }, 100);

    return () => {
      clearTimeout(timer);
      cancelAnimationFrame(rafRef.current);
      // Complete animation on unmount
      setWidth(100);
      setComplete(true);
    };
  }, []);

  if (width === 0) return null;

  return (
    <div
      className={clsx(styles.bar, complete && styles.barComplete)}
      style={{ width: `${width}%` }}
      role="progressbar"
      aria-valuenow={Math.round(width)}
      aria-valuemin={0}
      aria-valuemax={100}
    />
  );
}

import { useCallback, useEffect, useRef } from 'react';

/**
 * Arms setTimeout callbacks that are cancelled when the component unmounts.
 *
 * A bare `setTimeout(() => setX(...), ms)` inside a component keeps running
 * after that component is gone, and its callback then updates state on an
 * unmounted tree. Usually that is merely wasted work; when the surrounding
 * environment has also gone away — a test runner tearing down jsdom, a page
 * mid-navigation — React reaches for `window` during the update and throws
 * `ReferenceError: window is not defined`. That is not hypothetical: it took CI
 * down from a 2-second "copied!" reset that outlived its dialog.
 *
 * The exposure scales with the delay, so the long ones are the dangerous ones —
 * a 90-second refresh timer is almost guaranteed to fire after the user has
 * navigated away.
 *
 * Usage:
 *
 *   const armTimeout = useTimeouts();
 *   const copy = () => { setCopied(true); armTimeout(() => setCopied(false), 2000); };
 *
 * Returns a stable `arm` function, so it is safe in a useCallback dependency
 * list. Timers that fire normally remove themselves from the tracking set, so a
 * long-lived component does not accumulate handles.
 */
export function useTimeouts(): (fn: () => void, ms: number) => void {
  const timers = useRef<Set<ReturnType<typeof setTimeout>>>(new Set());

  useEffect(() => {
    // Captured once: reading timers.current inside the cleanup would re-read a
    // ref that React may already have reset.
    const pending = timers.current;
    return () => {
      pending.forEach(clearTimeout);
      pending.clear();
    };
  }, []);

  return useCallback((fn: () => void, ms: number) => {
    const handle = setTimeout(() => {
      timers.current.delete(handle);
      fn();
    }, ms);
    timers.current.add(handle);
  }, []);
}

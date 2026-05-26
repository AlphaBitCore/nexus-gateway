import { useEffect, useRef, useCallback } from 'react';

const IDLE_EVENTS = ['mousedown', 'keydown', 'touchstart', 'scroll'] as const;

interface IdleTimeoutOptions {
  timeoutMs: number;
  warningMs: number;
  onWarning: () => void;
  /** Called on any user-activity event (mousedown / keydown / touchstart / scroll). */
  onActivity?: () => void;
  onTimeout: () => void;
  enabled: boolean;
}

export function useIdleTimeout({ timeoutMs, warningMs, onWarning, onActivity, onTimeout, enabled }: IdleTimeoutOptions) {
  const warningTimer = useRef<ReturnType<typeof setTimeout>>(undefined);
  const timeoutTimer = useRef<ReturnType<typeof setTimeout>>(undefined);

  const resetTimers = useCallback(() => {
    clearTimeout(warningTimer.current);
    clearTimeout(timeoutTimer.current);

    warningTimer.current = setTimeout(() => {
      onWarning();
    }, warningMs);

    timeoutTimer.current = setTimeout(() => {
      onTimeout();
    }, timeoutMs);
  }, [timeoutMs, warningMs, onWarning, onTimeout]);

  useEffect(() => {
    if (!enabled) return;

    resetTimers();

    const handler = () => {
      resetTimers();
      onActivity?.();
    };
    for (const evt of IDLE_EVENTS) {
      document.addEventListener(evt, handler, { passive: true });
    }

    return () => {
      clearTimeout(warningTimer.current);
      clearTimeout(timeoutTimer.current);
      for (const evt of IDLE_EVENTS) {
        document.removeEventListener(evt, handler);
      }
    };
  }, [enabled, resetTimers, onActivity]);
}

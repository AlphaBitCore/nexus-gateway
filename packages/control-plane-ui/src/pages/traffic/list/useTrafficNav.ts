import { useState, useCallback, useLayoutEffect, useEffect } from 'react';
import type { TrafficEvent } from '../../../api/types';
import { DRAWER_MS } from '../audit-drawer/trafficAuditDrawer';
import { useTimeouts } from '@/hooks/useTimeouts';

/**
 * useTrafficNav — drawer selection + animate-in/Escape lifecycle for the
 * Traffic list. Owns `selectedEntry` / `drawerVisible` and the close handler,
 * preserving the exact open/close animation timing and keyboard behavior.
 */
export function useTrafficNav() {
  const [selectedEntry, setSelectedEntry] = useState<TrafficEvent | null>(null);
  const [drawerVisible, setDrawerVisible] = useState(false);

  // The drawer clears its row only after the close animation; without
  // cancellation that timer outlives an unmount and updates a dead tree.
  const armTimeout = useTimeouts();
  const closeDrawer = useCallback(() => {
    setDrawerVisible(false);
    armTimeout(() => setSelectedEntry(null), DRAWER_MS);
  }, [armTimeout]);

  useLayoutEffect(() => {
    if (!selectedEntry) {
      setDrawerVisible(false);
      return;
    }
    setDrawerVisible(false);
    const raf = window.requestAnimationFrame(() => {
      window.requestAnimationFrame(() => setDrawerVisible(true));
    });
    return () => window.cancelAnimationFrame(raf);
  }, [selectedEntry?.id]);

  useEffect(() => {
    if (!selectedEntry) return;
    const onKey = (ev: KeyboardEvent) => {
      if (ev.key === 'Escape') closeDrawer();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [selectedEntry, closeDrawer]);

  return {
    selectedEntry,
    setSelectedEntry,
    drawerVisible,
    closeDrawer,
  };
}

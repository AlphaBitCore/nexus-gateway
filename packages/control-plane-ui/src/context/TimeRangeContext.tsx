/**
 * Global time-range context (S45, M5-12).
 *
 * A single React context that holds the operator's chosen time window
 * (start / end ISO strings + a preset label) so multiple pages can stay
 * in sync. Mounted high in the component tree (App.tsx) so every page
 * has access via the `useTimeRange` hook.
 *
 * Why a context instead of per-page state?
 *
 *   - The compliance dashboard, discovery page, unified audit timeline
 *     and home dashboard widgets all want a "last 7 days" view by
 *     default. Without a shared store, each page resets to its own
 *     default and the operator types the same date range four times.
 *   - The picker is rendered once in a global banner above the page
 *     outlet so it is visible across navigation.
 *   - Persisted to localStorage so a page reload preserves the chosen
 *     range. The window is bounded sanity-check'd to the last 90 days
 *     when restored, so a stale localStorage entry from a year ago
 *     doesn't blow past the dashboards' 90-day caps.
 */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from 'react';

export type TimeRangePreset = '24h' | '7d' | '30d' | '90d' | 'custom';

export interface TimeRange {
  start: string; // ISO 8601
  end: string;   // ISO 8601
  preset: TimeRangePreset;
}

interface TimeRangeContextValue {
  range: TimeRange;
  setRange: (range: TimeRange) => void;
  setPreset: (preset: Exclude<TimeRangePreset, 'custom'>) => void;
}

const STORAGE_KEY = 'nexusGlobalTimeRange';
const DEFAULT_PRESET: Exclude<TimeRangePreset, 'custom'> = '7d';
const MAX_AGE_MS = 90 * 24 * 60 * 60 * 1000;

export function presetToRange(preset: Exclude<TimeRangePreset, 'custom'>): TimeRange {
  const end = new Date();
  const start = new Date(end);
  switch (preset) {
    case '24h': start.setHours(start.getHours() - 24); break;
    case '7d': start.setDate(start.getDate() - 7); break;
    case '30d': start.setDate(start.getDate() - 30); break;
    case '90d': start.setDate(start.getDate() - 90); break;
  }
  return { start: start.toISOString(), end: end.toISOString(), preset };
}

function loadFromStorage(): TimeRange {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return presetToRange(DEFAULT_PRESET);
    const parsed = JSON.parse(raw) as Partial<TimeRange>;
    if (
      typeof parsed.start === 'string' &&
      typeof parsed.end === 'string' &&
      typeof parsed.preset === 'string'
    ) {
      // Sanity check: drop stale custom ranges that pre-date the 90-day
      // cap so the dashboards do not 400 on restore.
      const start = new Date(parsed.start);
      const end = new Date(parsed.end);
      if (
        !isNaN(start.getTime()) &&
        !isNaN(end.getTime()) &&
        end > start &&
        end.getTime() - start.getTime() <= MAX_AGE_MS
      ) {
        return {
          start: parsed.start,
          end: parsed.end,
          preset: parsed.preset as TimeRangePreset,
        };
      }
    }
  } catch {
    // Fall through to default.
  }
  return presetToRange(DEFAULT_PRESET);
}

function saveToStorage(range: TimeRange): void {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(range));
  } catch {
    // Private mode: silently skip.
  }
}

const TimeRangeContext = createContext<TimeRangeContextValue | null>(null);

export function TimeRangeProvider({ children }: { children: ReactNode }) {
  const [range, setRangeState] = useState<TimeRange>(() => loadFromStorage());

  // For preset ranges, recompute the start/end on a slow interval so the
  // window stays "rolling" rather than frozen at the moment the operator
  // first selected the preset. 60-second tick is fast enough for the
  // typical compliance review use case and slow enough to be invisible.
  useEffect(() => {
    if (range.preset === 'custom') return;
    const interval = setInterval(() => {
      setRangeState((prev) => {
        if (prev.preset === 'custom') return prev;
        return presetToRange(prev.preset);
      });
    }, 60_000);
    return () => clearInterval(interval);
  }, [range.preset]);

  const setRange = useCallback((next: TimeRange) => {
    setRangeState(next);
    saveToStorage(next);
  }, []);

  const setPreset = useCallback((preset: Exclude<TimeRangePreset, 'custom'>) => {
    const next = presetToRange(preset);
    setRangeState(next);
    saveToStorage(next);
  }, []);

  const value = useMemo<TimeRangeContextValue>(
    () => ({ range, setRange, setPreset }),
    [range, setRange, setPreset],
  );

  return <TimeRangeContext.Provider value={value}>{children}</TimeRangeContext.Provider>;
}

/**
 * Hook for any component that needs the current time range. Returns the
 * default 7d preset when called outside the provider so test renders
 * don't crash — pages that opt in get the live shared range.
 */
export function useTimeRange(): TimeRangeContextValue {
  const ctx = useContext(TimeRangeContext);
  if (!ctx) {
    return {
      range: presetToRange(DEFAULT_PRESET),
      setRange: () => undefined,
      setPreset: () => undefined,
    };
  }
  return ctx;
}

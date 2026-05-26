/**
 * Wails Dashboard theme provider.
 *
 * Thin wrapper that wires @nexus-gateway/ui-shared's theme module into the
 * Agent Dashboard. Mirrors the CP-UI provider (same ThemeContext + useTheme
 * hook) so cross-bundle ui-shared components consume brand/theme identically.
 *
 * Differences from CP-UI:
 *   - localStorage keys namespaced for the Dashboard
 *     (`nexus-dashboard-theme-mode` + `nexus-dashboard-theme-id`)
 *   - Themes fetched from Wails-embedded `/themes/*.json`
 *   - Hub-pushed `agent_settings.themeId` overrides the local pick
 *     remotely without operator interaction.
 */
import { useCallback, useEffect, useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import { useQuery } from '@tanstack/react-query';
import { agentApi, type AppliedConfig } from '@/api/agent';
import {
  DEFAULT_THEME,
  ThemeContext,
  applyFavicon,
  applyThemeFont,
  applyThemeTokens,
  clearThemeTokens,
  loadTheme,
  type ThemeConfig,
  type ThemeContextValue,
  type ThemeMode,
} from '@nexus-gateway/ui-shared';

// Re-export so local consumers (Settings.tsx, etc.) can import from this module.
export { useTheme } from '@nexus-gateway/ui-shared';
export type { ThemeMode } from '@nexus-gateway/ui-shared';

const MODE_STORAGE_KEY = 'nexus-dashboard-theme-mode';
const THEME_ID_STORAGE_KEY = 'nexus-dashboard-theme-id';

function getSystemPreference(): 'light' | 'dark' {
  if (typeof window === 'undefined') return 'light';
  return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

function applyTheme(resolved: 'light' | 'dark'): void {
  document.documentElement.setAttribute('data-theme', resolved);
  document.documentElement.classList.toggle('dark', resolved === 'dark');
}

export function ThemeProvider({ children }: { children: ReactNode }) {
  const [mode, setModeState] = useState<ThemeMode>(() => {
    if (typeof window === 'undefined') return 'system';
    const stored = localStorage.getItem(MODE_STORAGE_KEY) as ThemeMode | null;
    return stored && ['light', 'dark', 'system'].includes(stored) ? stored : 'system';
  });

  // userThemeId = the operator's local pick (persisted to localStorage).
  // Hub override (admin-pushed via agent_settings.themeId) wins when present,
  // but we keep the user's pick alive so removing the Hub override falls
  // back to it instead of resetting to 'default'.
  const [userThemeId, setUserThemeIdState] = useState<string>(() => {
    if (typeof window === 'undefined') return 'default';
    return localStorage.getItem(THEME_ID_STORAGE_KEY) || 'default';
  });

  const [systemPref, setSystemPref] = useState<'light' | 'dark'>(getSystemPreference);
  const [theme, setTheme] = useState<ThemeConfig>(DEFAULT_THEME);

  // Subscribe to the daemon's applied-config snapshot. Shares queryKey
  // with usePolicies/useAppliedConfig so React Query dedupes — one fetch
  // serves both consumers. refetchInterval=30s is more relaxed than the
  // Policies page (10s) since theme rarely changes at runtime.
  const { data: appliedConfig } = useQuery({
    queryKey: ['agent', 'applied-config'],
    queryFn: () => agentApi.getAppliedConfig() as Promise<AppliedConfig>,
    staleTime: 5_000,
    refetchInterval: 30_000,
    // The daemon may be offline (Dashboard launched without agent
    // running). Don't retry-storm; one attempt, fall back to user pick.
    retry: false,
  });

  const hubThemeId = appliedConfig?.deviceDefaults?.themeId ?? '';
  // Hub-pushed value is authoritative; absent / empty → user's local pick.
  const themeId = hubThemeId || userThemeId;
  const resolvedMode = mode === 'system' ? systemPref : mode;

  // Track OS dark-mode toggle so the Dashboard follows it live when the
  // user has not made an explicit override.
  useEffect(() => {
    const mq = window.matchMedia('(prefers-color-scheme: dark)');
    const handler = (e: MediaQueryListEvent) => setSystemPref(e.matches ? 'dark' : 'light');
    mq.addEventListener('change', handler);
    return () => mq.removeEventListener('change', handler);
  }, []);

  useEffect(() => {
    applyTheme(resolvedMode);
  }, [resolvedMode]);

  // Load + apply theme. The cancel flag handles Vitest teardown races
  // (matches the CP-UI provider pattern).
  useEffect(() => {
    let cancelled = false;
    clearThemeTokens();

    loadTheme(themeId).then((config) => {
      if (cancelled) return;
      setTheme(config);
      applyThemeTokens(config);
      if (config.typography?.fontUrl) applyThemeFont(config.typography.fontUrl);
      if (config.brand.favicon) applyFavicon(config.brand.favicon);
    });

    return () => {
      cancelled = true;
    };
  }, [themeId]);

  const setMode = useCallback((next: ThemeMode) => {
    setModeState(next);
    try {
      localStorage.setItem(MODE_STORAGE_KEY, next);
    } catch {
      // ignore — runtime preference still applies for the session
    }
  }, []);

  // setThemeId updates the operator's local pick. When Hub has pushed an
  // override (hubThemeId non-empty), the override stays authoritative and
  // the user's pick only takes effect after admin clears the push —
  // managed fleet branding shouldn't be overridable per-machine.
  const setThemeId = useCallback((id: string) => {
    setUserThemeIdState(id);
    try {
      localStorage.setItem(THEME_ID_STORAGE_KEY, id);
    } catch {
      // ignore
    }
  }, []);

  const value = useMemo<ThemeContextValue>(
    () => ({ mode, resolvedMode, setMode, theme, themeId, setThemeId, brand: theme.brand }),
    [mode, resolvedMode, setMode, theme, themeId, setThemeId],
  );

  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>;
}

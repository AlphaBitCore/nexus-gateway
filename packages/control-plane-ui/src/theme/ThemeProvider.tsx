// CP-UI ThemeProvider.
//
// Thin wrapper that wires the shared theme module (@nexus-gateway/ui-shared)
// into the Control Plane UI. Adds CP-UI-specific concerns: localStorage
// persistence, HTTP theme fetch via fetch('/themes/<id>.json'), and the
// optional /brand.json / /theme.json deployment-level overrides.
//
// The Agent Dashboard ships a parallel provider (packages/agent/ui/frontend/
// src/theme/ThemeProvider.tsx) that uses the same ui-shared helpers but
// sources themes from its embedded Wails assets.

import * as React from 'react';
import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react';
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

const MODE_STORAGE_KEY = 'nexus-theme-mode';
const THEME_ID_STORAGE_KEY = 'nexus-theme-id';

function getSystemPreference(): 'light' | 'dark' {
  if (typeof window === 'undefined') return 'light';
  return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

function applyThemeToDOM(resolved: 'light' | 'dark'): void {
  document.documentElement.setAttribute('data-theme', resolved);
  document.documentElement.classList.toggle('dark', resolved === 'dark');
}

export function ThemeProvider({ children }: { children: React.ReactNode }) {
  const [mode, setModeState] = useState<ThemeMode>(() => {
    if (typeof window === 'undefined') return 'system';
    return (localStorage.getItem(MODE_STORAGE_KEY) as ThemeMode) || 'system';
  });

  const [themeId, setThemeIdState] = useState<string>(() => {
    if (typeof window === 'undefined') return 'default';
    return localStorage.getItem(THEME_ID_STORAGE_KEY) || 'default';
  });

  const [systemPref, setSystemPref] = useState<'light' | 'dark'>(getSystemPreference);
  const [theme, setTheme] = useState<ThemeConfig>(DEFAULT_THEME);

  const resolvedMode = mode === 'system' ? systemPref : mode;

  useEffect(() => {
    const mq = window.matchMedia('(prefers-color-scheme: dark)');
    const handler = (e: MediaQueryListEvent) => setSystemPref(e.matches ? 'dark' : 'light');
    mq.addEventListener('change', handler);
    return () => mq.removeEventListener('change', handler);
  }, []);

  useEffect(() => {
    applyThemeToDOM(resolvedMode);
  }, [resolvedMode]);

  // Load + apply theme. The cancel flag guards against the unmount-during-
  // load race under Vitest, where the test environment tears down before
  // loadTheme's promise resolves and an unguarded setState triggers a
  // "state update after teardown" warning. Real users never unmount the
  // provider mid-flight; the flag is cheap insurance.
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

  const setMode = useCallback((newMode: ThemeMode) => {
    setModeState(newMode);
    localStorage.setItem(MODE_STORAGE_KEY, newMode);
  }, []);

  const setThemeId = useCallback((id: string) => {
    setThemeIdState(id);
    localStorage.setItem(THEME_ID_STORAGE_KEY, id);
  }, []);

  const value = useMemo<ThemeContextValue>(
    () => ({ mode, resolvedMode, setMode, theme, themeId, setThemeId, brand: theme.brand }),
    [mode, resolvedMode, setMode, theme, themeId, setThemeId],
  );

  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>;
}

// Re-export the context so legacy local imports of `useTheme` keep working
// during the rename sweep. Once that sweep lands, useTheme.ts becomes
// a pure re-export from @nexus-gateway/ui-shared.
export { ThemeContext };

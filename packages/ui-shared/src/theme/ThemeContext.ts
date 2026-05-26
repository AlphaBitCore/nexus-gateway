// Theme React context — shared between CP-UI and Agent-UI ThemeProviders.
//
// Each app's ThemeProvider wraps <ThemeContext.Provider> locally so it can
// supply app-specific sourcing (CP-UI fetches HTTP; Agent-UI reads embedded
// Wails assets). The context itself + the useTheme hook are shared so
// ui-shared components can consume `useTheme()` without knowing which app
// they're rendered inside.

import { createContext, useContext } from 'react';
import type { BrandIdentity, ThemeConfig } from './ThemeConfig';

export type ThemeMode = 'light' | 'dark' | 'system';
export type ResolvedThemeMode = 'light' | 'dark';

export interface ThemeContextValue {
  /** User-selected mode (may be 'system'). */
  mode: ThemeMode;
  /** Concrete mode after resolving 'system'. */
  resolvedMode: ResolvedThemeMode;
  setMode: (mode: ThemeMode) => void;

  /** Currently loaded theme. */
  theme: ThemeConfig;
  /** Theme ID — matches the filename under /themes/. */
  themeId: string;
  /** Switch to a different theme (loads from /themes/{id}.json). */
  setThemeId: (id: string) => void;

  /**
   * Convenience accessor for `theme.brand`. Components that only need the
   * brand identity (productName, logoMark, tagline, favicon) can destructure
   * `const { brand } = useTheme()` without reaching through `.theme`.
   */
  brand: BrandIdentity;
}

export const ThemeContext = createContext<ThemeContextValue | null>(null);

/**
 * Read the active theme + mode. Throws when used outside a <ThemeProvider>
 * — components that depend on theme MUST be rendered inside one. The error
 * is intentional: a silent fallback would hide misconfiguration.
 */
export function useTheme(): ThemeContextValue {
  const ctx = useContext(ThemeContext);
  if (!ctx) {
    throw new Error('useTheme must be used inside a <ThemeProvider>');
  }
  return ctx;
}

// Theme loader — fetches a ThemeConfig + applies it to the DOM.
//
// Lookup order (highest priority first):
//   1. /theme.json                   — deployment-level forced override
//   2. /themes/<themeId>.json        — selected theme
//   3. DEFAULT_THEME (in-memory)     — fallback
//
// Application:
//   - Layer 2 tokens (lightTokens/darkTokens) → scoped <style> block
//     injected into <head>; flips automatically with [data-theme="..."].
//   - Layer 1 overrides (typography, radii, spacing, fontSizes, effects)
//     → same <style> block, scoped to :root (mode-independent).
//   - Logo/favicon/fontUrl → applied via <link> / <img> imperative helpers.
//
// Brand asset URLs (logo, favicon) are NOT injected as tokens — consumers
// read them from useTheme().brand and render <img>.

import type { ThemeConfig } from './ThemeConfig';

const STYLE_ELEMENT_ID = 'nexus-theme-overrides';
const FONT_LINK_ID = 'nexus-theme-font';

/**
 * Built-in default. Used when /theme.json and /themes/default.json both
 * fail (offline, asset 404, parse error). Keeps the UI rendering instead
 * of crashing on the very first paint.
 */
export const DEFAULT_THEME: ThemeConfig = {
  id: 'default',
  displayName: 'AlphaBitCore',
  brand: {
    productName: 'AlphaBitCore',
    tagline: 'Innovate. Invest. Thrive',
    logoMark: '/brand/prime-console/logo-mark.svg',
    logoFull: '/brand/prime-console/logo-wordmark.svg',
    logoTagline: '/brand/prime-console/logo-tagline.svg',
    favicon: '/brand/prime-console/logo-mark.svg',
  },
};

/**
 * Fetch a theme by ID. Returns DEFAULT_THEME on any error so the UI never
 * crashes on a missing/malformed theme file.
 */
export async function loadTheme(themeId: string): Promise<ThemeConfig> {
  // Deployment-level override takes absolute priority.
  try {
    const response = await fetch('/theme.json');
    if (response.ok) {
      const config = (await response.json()) as ThemeConfig;
      return { ...DEFAULT_THEME, ...config };
    }
  } catch {
    // ignore — fall through
  }

  // Named theme from the catalogue.
  if (themeId && themeId !== 'default') {
    try {
      const response = await fetch(`/themes/${themeId}.json`);
      if (response.ok) {
        const config = (await response.json()) as ThemeConfig;
        return { ...DEFAULT_THEME, ...config };
      }
    } catch {
      // ignore — fall through
    }
  }

  // Try default.json explicitly — operators may customise it.
  try {
    const response = await fetch('/themes/default.json');
    if (response.ok) {
      const config = (await response.json()) as ThemeConfig;
      return { ...DEFAULT_THEME, ...config };
    }
  } catch {
    // ignore — use in-memory DEFAULT_THEME
  }

  return DEFAULT_THEME;
}

/**
 * Apply a theme's token overrides as a scoped <style> block.
 *
 * - lightTokens → `:root, [data-theme="light"]` (mode-flippable)
 * - darkTokens  → `[data-theme="dark"]`
 * - layer1 + typography → `:root` (mode-independent)
 */
export function applyThemeTokens(theme: ThemeConfig): void {
  clearThemeTokens();

  const rules: string[] = [];

  // Layer 1 (mode-independent) + typography
  const layer1Decls: string[] = [];
  if (theme.typography?.fontSans) layer1Decls.push(`  --g-font-sans: ${theme.typography.fontSans};`);
  if (theme.typography?.fontDisplay) layer1Decls.push(`  --g-font-display: ${theme.typography.fontDisplay};`);
  if (theme.typography?.fontMono) layer1Decls.push(`  --g-font-mono: ${theme.typography.fontMono};`);

  if (theme.layer1?.radii) {
    for (const [key, value] of Object.entries(theme.layer1.radii)) {
      if (value) layer1Decls.push(`  --g-radius-${key}: ${value};`);
    }
  }
  if (theme.layer1?.spacing) {
    for (const [key, value] of Object.entries(theme.layer1.spacing)) {
      if (value) layer1Decls.push(`  --g-space-${key}: ${value};`);
    }
  }
  if (theme.layer1?.fontSizes) {
    for (const [key, value] of Object.entries(theme.layer1.fontSizes)) {
      if (value) layer1Decls.push(`  --g-font-size-${key}: ${value};`);
    }
  }
  if (theme.layer1?.effects) {
    for (const [key, value] of Object.entries(theme.layer1.effects)) {
      if (value) layer1Decls.push(`  --g-effect-${key}: ${value};`);
    }
  }
  if (layer1Decls.length > 0) {
    rules.push(`:root {\n${layer1Decls.join('\n')}\n}`);
  }

  // Layer 2 light
  if (theme.lightTokens && Object.keys(theme.lightTokens).length > 0) {
    const decls = Object.entries(theme.lightTokens)
      .filter(([, v]) => v != null)
      .map(([k, v]) => `  --${k}: ${v};`)
      .join('\n');
    rules.push(`:root, [data-theme="light"] {\n${decls}\n}`);
  }

  // Layer 2 dark
  if (theme.darkTokens && Object.keys(theme.darkTokens).length > 0) {
    const decls = Object.entries(theme.darkTokens)
      .filter(([, v]) => v != null)
      .map(([k, v]) => `  --${k}: ${v};`)
      .join('\n');
    rules.push(`[data-theme="dark"] {\n${decls}\n}`);
  }

  if (rules.length === 0) return;

  const style = document.createElement('style');
  style.id = STYLE_ELEMENT_ID;
  style.textContent = rules.join('\n\n');
  document.head.appendChild(style);
}

/** Remove the injected theme <style> block and font <link>. */
export function clearThemeTokens(): void {
  document.getElementById(STYLE_ELEMENT_ID)?.remove();
  document.getElementById(FONT_LINK_ID)?.remove();
}

/** Load a theme's webfont stylesheet (Google Fonts URL). */
export function applyThemeFont(url: string): void {
  document.getElementById(FONT_LINK_ID)?.remove();
  const link = document.createElement('link');
  link.id = FONT_LINK_ID;
  link.rel = 'stylesheet';
  link.href = url;
  document.head.appendChild(link);
}

/** Swap the browser favicon. */
export function applyFavicon(href: string): void {
  let link = document.querySelector<HTMLLinkElement>('link[rel="icon"]');
  if (!link) {
    link = document.createElement('link');
    link.rel = 'icon';
    document.head.appendChild(link);
  }
  link.href = href;
}

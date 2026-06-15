import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import {
  DEFAULT_THEME,
  loadTheme,
  applyThemeTokens,
  clearThemeTokens,
  applyThemeFont,
  applyFavicon,
} from '../../src/theme/themeLoader';
import type { ThemeConfig } from '../../src/theme/ThemeConfig';

const STYLE_ID = 'nexus-theme-overrides';
const FONT_ID = 'nexus-theme-font';

function jsonResponse(body: unknown, ok = true): Response {
  return {
    ok,
    json: async () => body,
  } as unknown as Response;
}

describe('loadTheme — fetch priority chain', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('uses /theme.json (deployment override) above all else, merged onto the default', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse({ id: 'forced', displayName: 'Forced' }),
    );
    vi.stubGlobal('fetch', fetchMock);
    const theme = await loadTheme('morningstar');
    expect(theme.id).toBe('forced');
    // Merged onto DEFAULT_THEME, so unset brand fields survive.
    expect(theme.brand.productName).toBe(DEFAULT_THEME.brand.productName);
    expect(fetchMock).toHaveBeenCalledWith('/theme.json');
    // Override wins before the named theme is ever fetched.
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it('falls through to the named theme when /theme.json is absent', async () => {
    const fetchMock = vi.fn(async (url: string) =>
      url === '/themes/morningstar.json'
        ? jsonResponse({ id: 'morningstar', displayName: 'Morningstar' })
        : jsonResponse({}, false),
    );
    vi.stubGlobal('fetch', fetchMock);
    const theme = await loadTheme('morningstar');
    expect(theme.id).toBe('morningstar');
    expect(fetchMock).toHaveBeenCalledWith('/themes/morningstar.json');
  });

  it('skips the named fetch when themeId is "default" and uses /themes/default.json', async () => {
    const fetchMock = vi.fn(async (url: string) =>
      url === '/themes/default.json'
        ? jsonResponse({ id: 'default', displayName: 'Operator Default' })
        : jsonResponse({}, false),
    );
    vi.stubGlobal('fetch', fetchMock);
    const theme = await loadTheme('default');
    expect(theme.displayName).toBe('Operator Default');
    // Never requested /themes/default/... via the named branch (id === 'default').
    expect(fetchMock).not.toHaveBeenCalledWith('/themes/default/default.json');
  });

  it('returns the in-memory DEFAULT_THEME when every fetch fails', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse({}, false)));
    const theme = await loadTheme('morningstar');
    expect(theme).toEqual(DEFAULT_THEME);
  });

  it('survives a thrown fetch (offline) and returns the default', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('offline')));
    const theme = await loadTheme('morningstar');
    expect(theme).toEqual(DEFAULT_THEME);
  });
});

describe('applyThemeTokens', () => {
  beforeEach(() => clearThemeTokens());
  afterEach(() => clearThemeTokens());

  it('injects a scoped <style> with layer1, typography, light + dark tokens', () => {
    const theme: ThemeConfig = {
      ...DEFAULT_THEME,
      typography: { fontSans: 'Inter', fontDisplay: 'Lexend', fontMono: 'Menlo' },
      layer1: {
        radii: { md: '8px', none: '' }, // empty value is skipped
        spacing: { '4': '1rem', '0': '' },
        fontSizes: { base: '16px', xs: '' },
        effects: { glow: '0 0 8px', shadow: '' },
      },
      lightTokens: { 'color-primary': '#3b518a', 'color-bg': null as unknown as string },
      darkTokens: { 'color-primary': '#9db4ff' },
    };
    applyThemeTokens(theme);
    const style = document.getElementById(STYLE_ID);
    expect(style).toBeTruthy();
    const css = style!.textContent!;
    expect(css).toContain('--g-font-sans: Inter;');
    expect(css).toContain('--g-radius-md: 8px;');
    expect(css).not.toContain('--g-radius-none'); // empty value filtered
    expect(css).toContain('--g-space-4: 1rem;');
    expect(css).not.toContain('--g-space-0'); // empty value filtered
    expect(css).toContain('--g-font-size-base: 16px;');
    expect(css).not.toContain('--g-font-size-xs'); // empty value filtered
    expect(css).toContain('--g-effect-glow: 0 0 8px;');
    expect(css).not.toContain('--g-effect-shadow'); // empty value filtered
    expect(css).toContain(':root, [data-theme="light"]');
    expect(css).toContain('--color-primary: #3b518a;');
    expect(css).not.toContain('--color-bg:'); // null token filtered
    expect(css).toContain('[data-theme="dark"]');
    expect(css).toContain('--color-primary: #9db4ff;');
  });

  it('replaces a previously injected style block (no duplicates)', () => {
    applyThemeTokens({ ...DEFAULT_THEME, lightTokens: { 'color-primary': '#111' } });
    applyThemeTokens({ ...DEFAULT_THEME, lightTokens: { 'color-primary': '#222' } });
    expect(document.querySelectorAll(`#${STYLE_ID}`).length).toBe(1);
    expect(document.getElementById(STYLE_ID)!.textContent).toContain('#222');
  });

  it('injects nothing when the theme has no tokens or layer1', () => {
    applyThemeTokens({ ...DEFAULT_THEME });
    expect(document.getElementById(STYLE_ID)).toBeNull();
  });
});

describe('clearThemeTokens', () => {
  it('removes both the style block and the font link', () => {
    applyThemeTokens({ ...DEFAULT_THEME, lightTokens: { 'color-primary': '#111' } });
    applyThemeFont('https://fonts.example/x.css');
    expect(document.getElementById(STYLE_ID)).toBeTruthy();
    expect(document.getElementById(FONT_ID)).toBeTruthy();
    clearThemeTokens();
    expect(document.getElementById(STYLE_ID)).toBeNull();
    expect(document.getElementById(FONT_ID)).toBeNull();
  });
});

describe('applyThemeFont', () => {
  afterEach(() => document.getElementById(FONT_ID)?.remove());

  it('adds a stylesheet link with the font URL', () => {
    applyThemeFont('https://fonts.example/Inter.css');
    const link = document.getElementById(FONT_ID) as HTMLLinkElement;
    expect(link.rel).toBe('stylesheet');
    expect(link.href).toContain('Inter.css');
  });

  it('replaces an existing font link rather than stacking', () => {
    applyThemeFont('https://fonts.example/a.css');
    applyThemeFont('https://fonts.example/b.css');
    expect(document.querySelectorAll(`#${FONT_ID}`).length).toBe(1);
    expect((document.getElementById(FONT_ID) as HTMLLinkElement).href).toContain('b.css');
  });
});

describe('applyFavicon', () => {
  afterEach(() => {
    document.querySelectorAll('link[rel="icon"]').forEach((l) => l.remove());
  });

  it('creates an icon link when none exists', () => {
    document.querySelectorAll('link[rel="icon"]').forEach((l) => l.remove());
    applyFavicon('/brand/logo.svg');
    const link = document.querySelector<HTMLLinkElement>('link[rel="icon"]');
    expect(link).toBeTruthy();
    expect(link!.href).toContain('/brand/logo.svg');
  });

  it('updates the existing icon link href in place', () => {
    const existing = document.createElement('link');
    existing.rel = 'icon';
    existing.href = '/old.svg';
    document.head.appendChild(existing);
    applyFavicon('/new.svg');
    const links = document.querySelectorAll('link[rel="icon"]');
    expect(links.length).toBe(1);
    expect((links[0] as HTMLLinkElement).href).toContain('/new.svg');
  });
});

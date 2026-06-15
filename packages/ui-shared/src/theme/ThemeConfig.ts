// ThemeConfig — the contract a theme pack must satisfy.
//
// A theme pack is a JSON file at /themes/<id>.json (or the deployment-level
// /theme.json) that drives the entire UI's brand + visual appearance.
// Dropping a new JSON file conforming to this shape rebrands every screen
// in both the Control Plane UI (admin browser app) and the Agent Dashboard
// (Wails desktop app) — no source-code edits required.
//
// Two-layer separation:
//   - `brand`         non-visual identity (name, logo, favicon, tagline)
//   - `typography`    Layer 1 font overrides (mode-independent)
//   - `layer1`        Layer 1 non-color overrides (radius, spacing, effects)
//                     NOTE: Layer 1 colour palette (--g-gray-*, --g-blue-*) is
//                     deliberately NOT exposed — themes override semantic
//                     tokens via lightTokens/darkTokens, never raw palette.
//   - `lightTokens`   Layer 2 semantic overrides for light mode
//   - `darkTokens`    Layer 2 semantic overrides for dark mode
//   - `charts`        skin-aware chart palette overrides

/** Layer 2 semantic token names. Generated from light.css. */
export type SemanticTokenName =
  // Brand / interactive
  | 'color-primary'
  | 'color-primary-foreground'
  | 'color-primary-hover'
  // Semantic status
  | 'color-success'
  | 'color-success-light'
  | 'color-success-dark'
  | 'color-success-bg'
  | 'color-success-soft'
  | 'color-warning'
  | 'color-warning-light'
  | 'color-warning-dark'
  | 'color-warning-bg'
  | 'color-warning-soft'
  | 'color-warning-border'
  | 'color-warning-text'
  | 'color-warning-fg'
  | 'color-warning-contrast'
  | 'color-warning-surface'
  | 'color-status-warning'
  | 'color-danger'
  | 'color-danger-light'
  | 'color-danger-dark'
  | 'color-danger-bg'
  | 'color-danger-text'
  | 'color-danger-border'
  | 'color-danger-700'
  | 'color-info'
  | 'color-info-light'
  | 'color-info-dark'
  | 'color-info-bg'
  | 'color-violet'
  | 'color-violet-light'
  | 'color-violet-dark'
  | 'color-violet-bg'
  // Overlays
  | 'color-overlay'
  | 'color-overlay-strong'
  // Surfaces
  | 'color-bg'
  | 'color-surface'
  | 'color-surface-2'
  | 'color-surface-secondary'
  | 'color-surface-hover'
  | 'color-surface-raised'
  | 'color-surface-overlay'
  | 'color-surface-alt'
  | 'color-surface-muted'
  | 'color-bg-subtle'
  | 'color-bg-primary'
  | 'color-bg-input'
  | 'color-bg-elevated'
  | 'color-bg-secondary'
  | 'color-bg-translucent'
  | 'color-bg-hover'
  | 'color-border'
  | 'color-border-light'
  | 'color-border-strong'
  | 'color-border-subtle'
  | 'color-border-muted'
  | 'color-divider'
  | 'color-muted'
  | 'color-muted-foreground'
  | 'color-muted-bg'
  | 'color-muted-dark'
  | 'color-muted-light'
  | 'color-accent'
  | 'color-accent-foreground'
  | 'color-ring'
  | 'color-focus'
  | 'color-link'
  | 'color-code-bg'
  | 'color-code-fg'
  | 'color-primary-light'
  | 'color-primary-dark'
  | 'color-primary-500'
  | 'color-tab-active'
  | 'color-title-gradient'
  | 'page-shell-glow'
  | 'page-shell-glow-opacity'
  // Text
  | 'color-text'
  | 'color-text-primary'
  | 'color-text-secondary'
  | 'color-text-muted'
  | 'color-text-tertiary'
  | 'color-text-placeholder'
  | 'color-text-inverse'
  // Sidebar
  | 'sidebar-width'
  | 'sidebar-width-icon'
  | 'sidebar-bg'
  | 'sidebar-surface'
  | 'sidebar-text'
  | 'sidebar-text-active'
  | 'sidebar-hover'
  | 'sidebar-active-bg'
  | 'sidebar-active-border'
  | 'sidebar-accent'
  | 'sidebar-accent-foreground'
  | 'sidebar-border'
  | 'sidebar-section-label'
  | 'sidebar-shadow'
  // Shadows (theme-sensitive)
  | 'shadow-xs'
  | 'shadow-sm'
  | 'shadow-md'
  | 'shadow-lg'
  | 'shadow-xl';

/** Named chart palette slots — kept stable across themes. */
export type ChartSemanticName =
  | 'requests'
  | 'tokens'
  | 'cost'
  | 'errors'
  | 'cacheHits'
  | 'prompt'
  | 'completion'
  | 'gatewaySavings'
  | 'promptCache'
  | 'totalSavings';

/** Effect tokens — wire visual "personality" differences between themes. */
export type EffectTokenName =
  | 'glow-primary'         // Button hover box-shadow glow
  | 'glow-active'          // Sidebar active item box-shadow glow
  | 'card-hover-shadow'    // Card :hover box-shadow lift
  | 'card-edge-light'      // Card top-edge inset highlight
  | 'accent-bar'           // Dashboard metric card top accent bar opacity
  | 'accent-glow'          // Dashboard metric card accent glow opacity
  | 'live-dot'             // Header live status indicator display (none | block)
  | 'toast-spring'         // Toast entrance easing — themes can use spring curves
  | 'ambient-gradient'     // Body background-image atmosphere
  | 'header-blur';         // Header backdrop-filter blur on scroll

/** Spacing scale (numeric, 4px base). Half-values supported below 3. */
export type SpacingScaleKey =
  | '0' | '0-5' | '1' | '1-5' | '2' | '2-5' | '3' | '4' | '5' | '6' | '8' | '10' | '12';

/** Radius scale. */
export type RadiusScaleKey = 'sm' | 'md' | 'lg' | 'xl' | 'full';

/** Font-size scale. */
export type FontSizeScaleKey = 'xxs' | 'xs' | 'sm' | 'base' | 'md' | 'lg' | 'xl' | '2xl' | '3xl';

/** Non-visual brand identity. */
export interface BrandIdentity {
  /** Product name shown in UI (sidebar / header / login). */
  productName: string;
  /** Marketing tagline shown on the login page. */
  tagline?: string;
  /** Square logo (sidebar collapsed, login mark). */
  logoMark?: string;
  /** Wide logo (sidebar expanded). Optional — falls back to logoMark + productName. */
  logoFull?: string;
  /** Small tagline artwork shown with logoFull in supported brand lockups. */
  logoTagline?: string;
  /** Decorative watermark on the login page. */
  logoWatermark?: string;
  /** Browser favicon URL. */
  favicon?: string;
}

/** Mode-independent typography overrides. */
export interface ThemeTypography {
  /** Google Fonts stylesheet URL to load. */
  fontUrl?: string;
  /** Override --g-font-sans. */
  fontSans?: string;
  /** Override --g-font-display. */
  fontDisplay?: string;
  /** Override --g-font-mono. */
  fontMono?: string;
}

/**
 * Layer 1 overrides — radius, spacing, effects, font-sizes.
 *
 * Colour palette (--g-gray-*, --g-blue-*, etc.) is deliberately NOT exposed.
 * Themes change colours via lightTokens/darkTokens (Layer 2 semantic), never
 * by overriding Layer 1 palette — that would silently break every semantic
 * derivation across all themes.
 */
export interface ThemeLayer1 {
  radii?: Partial<Record<RadiusScaleKey, string>>;
  spacing?: Partial<Record<SpacingScaleKey, string>>;
  fontSizes?: Partial<Record<FontSizeScaleKey, string>>;
  effects?: Partial<Record<EffectTokenName, string>>;
}

/** Skin-aware chart palette. */
export interface ThemeChartConfig {
  /** Ordered multi-series palette. */
  series?: { light: readonly string[]; dark: readonly string[] };
  /** Pie / donut palette. */
  pie?: { light: readonly string[]; dark: readonly string[] };
  /** Named per-metric colours. */
  semantic?: Partial<Record<ChartSemanticName, { light: string; dark: string }>>;
}

/** A complete theme pack. */
export interface ThemeConfig {
  /** Unique ID, matches the JSON filename under /themes/<id>.json. */
  id: string;
  /** Human-readable name shown in the theme picker. */
  displayName: string;
  /** Optional one-line catalogue description. */
  description?: string;

  brand: BrandIdentity;
  typography?: ThemeTypography;
  layer1?: ThemeLayer1;
  /**
   * Layer 2 semantic overrides applied in light mode.
   * Also accepts `g-effect-*` keys for mode-specific effect overrides
   * (e.g., an ambient gradient that only renders in dark mode).
   */
  lightTokens?: Partial<Record<SemanticTokenName | `g-effect-${EffectTokenName}`, string>>;
  darkTokens?: Partial<Record<SemanticTokenName | `g-effect-${EffectTokenName}`, string>>;
  charts?: ThemeChartConfig;
}

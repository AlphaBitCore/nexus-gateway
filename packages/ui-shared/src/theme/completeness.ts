// Theme completeness contract — the set of semantic tokens that a theme
// pack's `lightTokens` and `darkTokens` MUST cover. Used by
// `scripts/check-theme-completeness.mjs` to fail CI when a theme pack
// leaves required tokens undefined (which would silently fall back to the
// previously loaded theme — most visibly Nexus's default palette bleeding
// through a customer's branded screen).
//
// This list mirrors the canonical Layer 2 tokens defined in
// packages/ui-shared/src/styles/light.css. Add new entries here when a new
// semantic token enters the design system — the same PR that adds the
// token to light.css/dark.css should add it here so every theme is forced
// to cover it.

import type { SemanticTokenName } from './ThemeConfig';

/**
 * Semantic tokens that EVERY theme's lightTokens + darkTokens MUST define.
 *
 * Scope: only the tokens that materially change the brand look — primary,
 * surfaces, text, borders, status colours, sidebar chrome. Pure aliases
 * (e.g., `color-bg-subtle` → `color-muted`) and rarely-customised tokens
 * (e.g., `shadow-xs`) are excluded to keep the contract focused.
 */
export const REQUIRED_THEME_TOKENS: readonly SemanticTokenName[] = [
  // Brand / interactive
  'color-primary',
  'color-primary-foreground',
  'color-primary-hover',

  // Semantic status (only the base — light/dark/bg derive)
  'color-success',
  'color-success-light',
  'color-success-dark',
  'color-warning',
  'color-warning-light',
  'color-warning-dark',
  'color-danger',
  'color-danger-light',
  'color-danger-dark',
  'color-info',
  'color-info-light',

  // Surfaces (the brand-defining background/foreground)
  'color-bg',
  'color-surface',
  'color-surface-hover',
  'color-surface-raised',
  'color-border',
  'color-border-light',
  'color-border-strong',
  'color-divider',
  'color-muted',
  'color-muted-foreground',
  'color-accent',
  'color-accent-foreground',
  'color-ring',
  'color-title-gradient',
  'page-shell-glow',
  'page-shell-glow-opacity',

  // Text
  'color-text',
  'color-text-secondary',
  'color-text-muted',
  'color-text-inverse',

  // Sidebar (high-visibility brand surface)
  'sidebar-bg',
  'sidebar-surface',
  'sidebar-text',
  'sidebar-text-active',
  'sidebar-hover',
  'sidebar-active-bg',
  'sidebar-active-border',
  'sidebar-accent',
  'sidebar-accent-foreground',
  'sidebar-border',
  'sidebar-section-label',
  'sidebar-shadow',
];

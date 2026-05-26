---
doc: theme-architecture
area: cross-cutting
service: ui
tier: 1
updated: 2026-05-20
---

# Theme Architecture

> **Tier 2 architecture doc.** Read this when adding a new theme pack, changing the brand abstraction, touching `packages/ui-shared/src/theme/**`, or wiring a new visual-personality knob. The CSS-token compliance binding lives in `design-tokens-architecture.md` (sister doc); this one covers the layer on top: how a customer rebrands the entire UI by dropping in a single JSON file.

The goal: **one theme pack JSON, applied across both Control Plane UI and Agent Dashboard, rebrands every screen** (sidebar, header, login, banners, charts, fonts, effect-token visual personality) without source-code edits.

---

## 1. Conceptual model

```
┌─────────────────────────────────────────────────────────────────┐
│  Theme pack JSON  (packages/<app>/public/themes/<id>.json)      │
│  Defines: brand identity + typography + Layer 1 overrides       │
│           + Layer 2 light/dark token maps + chart palette       │
└──────────────────────────────┬──────────────────────────────────┘
                               │ fetched + parsed
                               ▼
┌─────────────────────────────────────────────────────────────────┐
│  ui-shared/src/theme/themeLoader.ts                             │
│    loadTheme()         → ThemeConfig                            │
│    applyThemeTokens()  → injects <style id="nexus-theme-…">     │
│    applyThemeFont()    → adds <link rel="stylesheet">           │
│    applyFavicon()      → swaps <link rel="icon">                │
└──────────────────────────────┬──────────────────────────────────┘
                               │ context populated
                               ▼
┌─────────────────────────────────────────────────────────────────┐
│  ui-shared/src/theme/ThemeContext.ts                            │
│    useTheme() → { mode, resolvedMode, setMode,                  │
│                   theme, themeId, setThemeId, brand }           │
└──────────────────────────────┬──────────────────────────────────┘
                               │ consumed by
                               ▼
   ┌──────────────────────┐         ┌──────────────────────┐
   │ Control Plane UI     │         │ Agent Dashboard      │
   │ /theme/ThemeProvider │         │ /theme/ThemeProvider │
   │ (HTTP fetch)         │         │ (Wails embed fetch)  │
   └──────────────────────┘         └──────────────────────┘
```

Two orthogonal axes:
- **mode** — `light` / `dark` / `system` (system → OS preference). Switches via the `data-theme` attribute on `<html>`.
- **theme** — `default` / `morningstar` / `rbc` / customer-supplied. Each is a separate JSON file under `public/themes/`.

A complete look = **theme × mode** cartesian product. Every theme JSON defines BOTH `lightTokens` and `darkTokens`.

---

## 2. The two-layer token system (recap)

Inherited from `design-tokens-architecture.md`:

- **Layer 1 — `--g-*`** raw tokens in `packages/ui-shared/src/styles/global.css` (theme-agnostic numbers / colors).
- **Layer 2 — `--color-*` `--sidebar-*` `--shadow-*`** semantic tokens in `light.css` / `dark.css` (Layer 1 aliases that flip with `data-theme`).

Components consume Layer 2 by default; Layer 1 only when the value is semantically neutral (raw spacing, font-family, radii).

Theme packs override these tokens via injected `<style>` blocks — they never touch the CSS files themselves.

---

## 3. ThemeConfig contract (the JSON shape)

Defined in `packages/ui-shared/src/theme/ThemeConfig.ts`. A theme pack is a JSON file conforming to:

```ts
interface ThemeConfig {
  id: string;                 // filename: morningstar → "morningstar"
  displayName: string;        // human-readable, shown in theme picker
  description?: string;

  brand: BrandIdentity;       // non-visual product identity
  typography?: ThemeTypography;
  layer1?: ThemeLayer1;       // mode-independent Layer 1 overrides
  lightTokens?: Partial<...>; // Layer 2 light + g-effect-* light
  darkTokens?: Partial<...>;  // Layer 2 dark + g-effect-* dark
  charts?: ThemeChartConfig;  // per-theme Recharts palette
}
```

### 3a. `brand: BrandIdentity` — non-visual identity

```ts
{
  productName: string;     // REQUIRED — replaces all "Nexus Gateway" literals
  tagline?: string;        // shown on login page
  logoMark?: string;       // URL — square (sidebar collapsed, login mark)
  logoFull?: string;       // URL — wide (sidebar expanded)
  logoTagline?: string;    // URL — small tagline artwork for supported brand lockups
  logoWatermark?: string;  // URL — decorative on login page
  favicon?: string;        // URL — browser tab icon
}
```

Consumed by: Sidebar (productName + logoMark, or logoMark + logoFull + logoTagline when a theme supplies a complete lockup), Header (productName), LoginPage (logoMark + logoWatermark + productName + tagline), CallbackPage (productName), ForgotPasswordPage (tagline), SetupBanner / SetupWizard / StepSummary / PersonalVKList / AccountApiKeysTab (productName via `t(key, { productName: brand.productName })` interpolation).

`productName` is REQUIRED (not optional) — TypeScript guarantees it's non-null at the consumer.

### 3b. `typography: ThemeTypography` — mode-independent font

```ts
{
  fontUrl?: string;     // Google Fonts stylesheet URL — themeLoader adds <link>
  fontSans?: string;    // overrides --g-font-sans
  fontDisplay?: string; // overrides --g-font-display
  fontMono?: string;    // overrides --g-font-mono
}
```

Default theme ships **Geist + Geist Mono** (Vercel, SIL OFL, free for commercial use). Other commercially-free options: Inter, IBM Plex Sans (morningstar), Noto Sans (rbc), Space Grotesk, Albert Sans, JetBrains Mono.

### 3c. `layer1: ThemeLayer1` — Layer 1 non-color overrides

```ts
{
  radii?:     Partial<Record<'sm'|'md'|'lg'|'xl'|'full', string>>;
  spacing?:   Partial<Record<'0'|'0-5'|'1'|...|'12', string>>;
  fontSizes?: Partial<Record<'xxs'|'xs'|...|'3xl', string>>;
  effects?:   Partial<Record<EffectTokenName, string>>;
}
```

**⚠ Layer 1 colour palette (`--g-gray-*`, `--g-blue-*`) is deliberately NOT exposed.** Themes change colours via `lightTokens` / `darkTokens` (Layer 2 semantic). Overriding Layer 1 palette breaks every semantic derivation across all themes — the type system prevents this at compile time, the runtime ignores any palette keys, and `check-effect-tokens.mjs` won't catch typos in palette names (because they're never valid).

### 3d. `lightTokens` / `darkTokens` — Layer 2 + mode-specific effects

```ts
Partial<Record<SemanticTokenName | `g-effect-${EffectTokenName}`, string>>
```

The full `SemanticTokenName` union (40+ keys) is generated from `light.css` — see `packages/ui-shared/src/theme/ThemeConfig.ts`. Effect tokens may go here when the theme wants different visual personality per mode (e.g., default ships `ambient-gradient` only in dark for an atmospheric glow).

### 3e. `charts: ThemeChartConfig` — Recharts palette per theme

```ts
{
  series?: { light: readonly string[]; dark: readonly string[] };
  pie?:    { light: readonly string[]; dark: readonly string[] };
  semantic?: Partial<Record<ChartSemanticName, { light: string; dark: string }>>;
}
```

Recharts requires JS color strings (not CSS variables), so this field lets a theme rebrand chart data series alongside the rest of the UI. When unset, falls back to the built-in `LIGHT_SERIES` / `DARK_SERIES` palette in `chartColors.ts`. Consumers use `useChartSeriesColors()` / `useChartPieColors()` / `useChartSemanticColor()` / `useChartPhaseColor()` hooks, which read `useTheme().theme.charts` automatically.

---

## 4. Effect tokens — visual personality

`--g-effect-*` tokens are the dial themes use to express visual character WITHOUT changing colours. Ten knobs exist (defined in `global.css`), each consumed by exactly one component:

| Token | Consumer | What it controls |
|---|---|---|
| `glow-primary` | Button.module.css `.primary:hover` | Primary button hover shadow |
| `glow-active` | Sidebar.module.css `.navLinkActive` | Sidebar active item glow |
| `card-hover-shadow` | Card.module.css `.card:hover` | Card hover lift shadow |
| `card-edge-light` | Card.module.css `.card` | Card top-edge inset highlight (overhead light) |
| `accent-bar` | DashboardPage.module.css `.metricCard::before` | Metric card top accent bar opacity |
| `accent-glow` | DashboardPage.module.css `.metricCard::after` | Metric card accent glow opacity |
| `live-dot` | Header.module.css `.roleBadge::before` | Live status dot display (none / block) |
| `toast-spring` | ToastContainer.module.css `.toast` | Toast entrance easing curve |
| `ambient-gradient` | base.css `body` + Shell.module.css | Body background atmosphere image |
| `header-blur` | Header.module.css `.headerScrolled` | Frosted-glass blur on scroll |

Defaults are intentionally bland (`none` / `0` / `ease-out`) so the unstyled experience is calm. Themes opt in to a "personality" by setting any of these in `layer1.effects` (mode-independent) or in `lightTokens` / `darkTokens` (mode-specific). The `check-effect-tokens.mjs` lint requires every defined token to have ≥1 consumer somewhere in the codebase — adding a dead knob fails CI.

---

## 5. Loading lifecycle

```
App boot → main.tsx imports CSS in fixed order:
   base.css → global.css → light.css → dark.css → animations.css → utilities.css

ThemeProvider mounts → reads localStorage for mode + themeId
   ↓
Effect[resolvedMode] applies → <html data-theme="light|dark">
   ↓
Effect[themeId] runs loadTheme(themeId):
   1. fetch('/theme.json')              ← deployment-level forced override
   2. fetch('/themes/<themeId>.json')   ← named theme
   3. fetch('/themes/default.json')     ← fallback
   4. in-memory DEFAULT_THEME           ← last resort
   ↓
applyThemeTokens(theme)  → inject <style id="nexus-theme-overrides">
applyThemeFont(fontUrl)  → add <link id="nexus-theme-font">
applyFavicon(favicon)    → swap <link rel="icon">
   ↓
useTheme() consumers re-render via React context
```

The injected `<style>` block is keyed (`id="nexus-theme-overrides"`); switching themes removes the old block and injects a new one. No stale tokens linger.

---

## 6. Adding a new theme pack — step by step

1. **Copy the template**:
   ```
   cp packages/control-plane-ui/public/themes/default.json \
      packages/control-plane-ui/public/themes/<your-id>.json
   ```
2. **Mirror to Agent-UI**:
   ```
   cp packages/control-plane-ui/public/themes/<your-id>.json \
      packages/agent/ui/frontend/public/themes/<your-id>.json
   ```
3. **Edit your theme JSON**:
   - Set `id` = filename (without `.json`)
   - Set `displayName` = picker label
   - Fill `brand.productName` (REQUIRED — `check-brand-strings.mjs` will fail without it)
   - Override `lightTokens` + `darkTokens` for the 46 required semantic tokens (`check-theme-completeness.mjs` enforces the full set)
   - Add `typography.fontUrl` + `fontSans/fontDisplay/fontMono` for custom fonts
   - Optionally tune `layer1.effects` for visual personality
   - Optionally override `charts.series` / `pie` / `semantic` for chart palette
4. **Register in the picker** — add to `THEME_PACK_OPTIONS` in `packages/control-plane-ui/src/components/ui/Header/Header.tsx`.
5. **Verify**:
   ```
   npm run check:theme-completeness   # 46 required tokens × 2 modes
   npm run check:brand-strings        # no leaked literals elsewhere
   npm run check:design-tokens        # no hex outside the JSON
   npm run check:effect-tokens        # no typo'd or dead effect tokens
   npm run check:i18n                 # locale parity unchanged
   ```
6. **Smoke test** in the browser at `http://localhost:3000` → Header → theme picker → select your theme → verify in both light and dark mode.

---

## 7. Agent Dashboard (Wails) particularities

Agent-UI's `ThemeProvider` mirrors CP-UI's — same ui-shared module, same `useTheme` hook — but with two differences:

1. **localStorage key** namespaced (`nexus-dashboard-theme-mode` + `nexus-dashboard-theme-id`) so the Dashboard doesn't share preferences with the browser app.
2. **Theme source** = Wails embed. `vite build` packs `packages/agent/ui/frontend/public/themes/*.json` into `dist/`, which the Wails Go binary serves via `embed.FS` at runtime. Fetching `/themes/<id>.json` resolves to the embedded file — identical mechanism as CP-UI, identical loader code.
3. **No theme picker UI today**. The Dashboard ships `default` as the user-selected theme; future Hub-pushed `agent_settings.themeId` will flip it remotely without operator interaction (S2.3, planned).

When Hub-pushed `themeId` lands, the Agent daemon will receive it via `agent_settings` shadow (Cat A pull-only model — [[feedback_thing_config_pull_model]]), expose it via the Wails bridge (`GetThemeId` / `OnThemeIdChanged`), and the `ThemeProvider` will call `setThemeId(newId)` to hot-switch.

---

## 8. Enforcement (the 5 lint guardrails)

| Script | What it catches | Pre-commit trigger |
|---|---|---|
| `check-design-tokens` | hex/rgb/hsl literals in `.module.css` or `style={{}}`; raw numeric inline styles | any `.tsx` / `.css` |
| `check-theme-completeness` | a theme pack missing one of the 46 required semantic tokens in light or dark | `/themes/*.json`, `completeness.ts`, `light.css` |
| `check-brand-strings` | `"Nexus Gateway"` literal anywhere outside theme JSON (would defeat brand abstraction) | any frontend `.tsx` / `.ts` / `.json` |
| `check-effect-tokens` | dead `--g-effect-*` (defined but unused) or undefined references (typo) | `global.css` or any `.module.css` / `.tsx` |
| `check-i18n` | en/zh/es locale key drift | any `locales/*.json` |

All are wired into both `npm run check:all` and `.githooks/pre-commit`. Bypassing requires explicit user approval per CLAUDE.md.

---

## 9. What this architecture does NOT cover (yet)

- **Multi-tenant per-request theme switching** — out of scope per product decision. Current scope: one theme active per deployment / per browser session.
- **Admin-uploaded theme JSON via API** — not implemented. Operators ship themes via the build (`public/themes/`) or by writing a `/brand.json` (deprecated `/theme.json`) to the served static directory.
- **Hub-pushed theme override for Agent fleet** — S2.3 (in flight). Schema design: `agent_settings.themeId: string` (Cat A pull-only, default `'default'`).

---

## 10. Sources

- `packages/ui-shared/src/theme/ThemeConfig.ts` — the type contract
- `packages/ui-shared/src/theme/themeLoader.ts` — fetch + apply
- `packages/ui-shared/src/theme/ThemeContext.ts` — React context + useTheme
- `packages/control-plane-ui/src/theme/useTheme.ts` — CP-UI re-export wrapper for the `useTheme` hook (the canonical consumer-facing import path inside CP-UI).
- `packages/ui-shared/src/theme/completeness.ts` — REQUIRED_THEME_TOKENS
- `packages/ui-shared/src/theme/chartColors.ts` — theme-aware chart palette + hooks
- `packages/control-plane-ui/src/theme/ThemeProvider.tsx` — CP-UI wrapper
- `packages/agent/ui/frontend/src/theme/ThemeProvider.tsx` — Agent-UI wrapper
- `packages/control-plane-ui/public/themes/*.json` — CP-UI theme packs
- `packages/agent/ui/frontend/public/themes/*.json` — Agent-UI theme packs (mirrored)
- `packages/ui-shared/src/styles/{global,light,dark,base,utilities,animations}.css` — the underlying CSS
- `scripts/check-{theme-completeness,brand-strings,effect-tokens}.mjs` — guardrails

## 11. Cross-references

- `design-tokens-architecture.md` — sister doc; the two-layer CSS token system this builds on.
- `recharts-theme-architecture.md` — chart palette sourcing details.
- `ui-shared-architecture.md` — the leaf-dependency boundary that lets both apps consume the theme module.
- `i18n-pipeline-architecture.md` — `productName` interpolation pattern for brand-aware copy.

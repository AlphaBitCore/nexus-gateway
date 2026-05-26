---
doc: design-tokens-architecture
area: cross-cutting
service: ui
tier: 1
updated: 2026-05-20
---

# Design Tokens Architecture

> **Tier 2 architecture doc.** Read this before touching CSS modules in `packages/control-plane-ui/src/`, `packages/ui-shared/src/`, or the agent dashboard frontend. The IDE-side enforcement is `.cursor/rules/design-tokens.mdc`; the lint is `scripts/check-design-tokens.mjs` (`npm run check:design-tokens`).

Nexus has two themes (light, dark) and one product surface with a heavy emphasis on theme-mode safety. Visual values are NEVER hardcoded — they flow through a two-layer CSS variable system so every component reacts to `data-theme` flips correctly.

---

## 1. The two layers

### Layer 1 — raw tokens

Live in `packages/ui-shared/src/styles/global.css`. They are theme-agnostic numbers / colors:

```
/* ─ Colour palettes (raw, never mode-flipped) ─────────────────────────── */
--g-white
--g-gray-50  --g-gray-100  --g-gray-200  --g-gray-300  --g-gray-400
--g-gray-500 --g-gray-600  --g-gray-700  --g-gray-800  --g-gray-900  --g-gray-950   /* 11 stops */
--g-blue-50..--g-blue-900       /* 9 stops */
--g-green-50..--g-green-700     /* 6 stops, non-contiguous */
--g-red-50..--g-red-700         /* 8 stops */
--g-orange-50..--g-orange-700   /* 8 stops */
--g-slate-50..--g-slate-950     /* 11 stops (sidebar) */
--g-violet-50..--g-violet-700   /* 6 stops, non-contiguous */
--g-abc-blue-600 --g-abc-cyan-400 --g-abc-green-500 --g-abc-mint-300   /* ABC accent palette */

/* ─ Spacing — non-contiguous mixed scale (NOT pure 4px steps) + named aliases ── */
--g-space-0   --g-space-0-5 --g-space-1   --g-space-1-5
--g-space-2   --g-space-2-5 --g-space-3   --g-space-4
--g-space-5   --g-space-6   --g-space-8   --g-space-10  --g-space-12
--g-space-xs  (= --g-space-1)
--g-space-md  (= --g-space-4)

/* ─ Radius ────────────────────────────────────────────────────────────── */
--g-radius      (10px — base)
--g-radius-xs   (4px)
--g-radius-sm   (6px)
--g-radius-md   (8px)
--g-radius-lg   (10px)
--g-radius-xl   (14px)
--g-radius-full (9999px)

/* ─ Typography ────────────────────────────────────────────────────────── */
--g-font-sans / --g-font-display / --g-font-mono
--g-font-size-xxs..--g-font-size-3xl   (8 stops: 10/12/13/14/16/18/20/24/30 px)
--g-font-weight-{normal,medium,semibold,bold}
--g-line-height-{tight,normal,relaxed}

/* ─ Shadows ───────────────────────────────────────────────────────────── */
--g-shadow-xs --g-shadow-sm --g-shadow-md --g-shadow-lg --g-shadow-xl

/* ─ Transitions ──────────────────────────────────────────────────────── */
--g-transition-fast --g-transition-normal --g-transition-slow

/* ─ Effect tokens — theme visual personality (10 knobs, default "off") ── */
--g-effect-glow-primary --g-effect-glow-active --g-effect-card-hover-shadow
--g-effect-card-edge-light --g-effect-accent-bar --g-effect-accent-glow
--g-effect-live-dot --g-effect-toast-spring --g-effect-ambient-gradient
--g-effect-header-blur

/* ─ Z-index ───────────────────────────────────────────────────────────── */
--g-z-sidebar (10) --g-z-header (50) --g-z-dropdown (100)
--g-z-overlay (300) --g-z-modal (400) --g-z-toast (500) --g-z-tooltip (600)
```

Components consume Layer 1 **only when the value is semantically neutral** (e.g., `--g-space-4` for an internal gap that doesn't have a named purpose).

### Layer 2 — semantic tokens

Live in `packages/ui-shared/src/styles/light.css` + `dark.css` (flat under `src/styles/`, NOT under a `themes/` subdir). They alias Layer 1 raw tokens to semantic names that components reference, e.g.:

```
/* Backgrounds & surfaces */
--color-bg --color-bg-elevated --color-bg-subtle --color-bg-input
--color-surface --color-surface-overlay --color-surface-raised

/* Text */
--color-text --color-text-secondary --color-text-tertiary --color-text-muted

/* Borders */
--color-border --color-border-light --color-divider

/* Semantic accents */
--color-accent --color-primary
--color-success --color-success-light --color-success-dark
--color-warning --color-danger --color-info --color-violet

/* Sidebar / shadows / radii / font / transitions / z-index aliases */
--sidebar-bg / --sidebar-fg / --sidebar-active-bg
--shadow-card / --shadow-lg / --shadow-sm
--radius-input
--font-size-body
--transition-hover
--z-sidebar
```

`light.css` defines 100+ semantic declarations; `dark.css` defines the same names with dark-mode values. The HTML `data-theme` attribute selects which file's `:root` is active.

**Components consume Layer 2 by default.** Dropping to Layer 1 is reserved for genuinely semantic-neutral spots.

### Layer 2-bridge — Tailwind v4 + shadcn (prime tokens)

`packages/ui-shared/src/styles/prime-shadcn-tokens.css` is a thin bridging layer that re-publishes the Layer 2 semantic names as the shadcn / Tailwind v4 token surface (`--primary`, `--background`, `--border`, `--color-brand-hover`, etc.) and mixes a few raw Layer-1 references (`--g-green-600`) where shadcn expects a stable hue. Components written against shadcn primitives read these bridged names; the chain `prime-shadcn-tokens.css → light.css/dark.css → global.css` still flips correctly under `data-theme`. Treat this file as Layer 2 (do not add raw hex / new palette stops to it).

## 2. Forbidden patterns

In `*.module.css`:

- ❌ Hex literals (`#fafafa`, `#1a1a1a`).
- ❌ `rgb(...)` / `rgba(...)` / `hsl(...)` / `hsla(...)` literals.

In `style={{}}` blocks in `*.tsx`:

- ❌ Hex / `rgb(a)` literals.
- ❌ Raw numeric `padding: 8` / `fontSize: 14` / `borderRadius: 4` / `gap: 12` / `boxShadow: '...'`.

These break theme/mode safety — they cement a light-mode color or a fixed pixel value into the component and don't flip with `data-theme`.

## 3. Three escape hatches

1. **Recharts colours** — must import from `packages/ui-shared/src/theme/chartColors.ts` (single sanctioned hex source). See `recharts-theme-architecture.md`.
2. **CSS variable bridges** — `style={{ '--foo': dynamicValue }}` is fine; this writes a CSS var that the component's stylesheet then reads.
3. **Runtime-computed dimensions** — `` style={{ paddingLeft: `${level * 20}px` }} `` is fine. These compute from runtime state and can't easily be expressed as pre-defined tokens.

Anything else needs explicit user approval.

## 4. Enforcement

| Tool | What it catches |
|---|---|
| `npm run check:design-tokens` | hex/rgb/hsl literals in CSS modules; raw numeric inline styles; stale `var(--xxx, FALLBACK)` patterns pointing to non-existent tokens |
| `npm run check:design-tokens:hints` | same plus migration suggestions when refactoring legacy code |
| `.cursor/rules/design-tokens.mdc` | IDE-time surfacing of the binding |
| `frontend-arch-review` skill | broader sweep including Recharts + inline-style audits |

## 5. Adding a new token

If you find yourself wanting a hex literal:

1. Stop. Decide if you need a new **semantic** token or just a new **raw** token.
2. Raw: add to `packages/ui-shared/src/styles/global.css` with a `--g-*` name + a comment explaining its purpose.
3. Semantic: add to both `light.css` and `dark.css`. Define both light + dark values up front.
4. Consume the semantic name in your component.
5. Re-run `npm run check:design-tokens` to confirm no leak.

Never add a Layer 2 token to just one theme file — that creates an undefined-var bug on the missing theme.

## 6. Theme switching

The root `<html data-theme="light|dark">` toggles which `*.css` file's `:root` selector matches. All semantic tokens re-resolve in CSS instantly; React doesn't re-render to pick up colour changes.

User preference is persisted in `localStorage` and re-applied on boot. System-preference auto-detect (via `prefers-color-scheme`) is also supported.

<!-- 💡 harvest: no new rule needed (existing design-tokens.mdc covers); skill `add-design-token` is small enough to fold into design-tokens.mdc "How to add a new token" section here. -->

## 7. Sources

- `packages/ui-shared/src/styles/global.css` — Layer 1 raw tokens (palettes, spacing, radii, shadows, transitions, effects, z-index).
- `packages/ui-shared/src/styles/light.css` + `dark.css` — Layer 2 semantic tokens (mode-flipped).
- `packages/ui-shared/src/styles/prime-shadcn-tokens.css` — Tailwind v4 + shadcn bridge over Layer 2.
- `packages/ui-shared/src/styles/base.css` — base element resets + body atmosphere wiring (`--g-effect-ambient-gradient`).
- `packages/ui-shared/src/styles/animations.css` — keyframe + transition primitives consumed across modules.
- `packages/ui-shared/src/styles/utilities.css` — utility classes referencing Layer 1 / Layer 2 names.
- `packages/control-plane-ui/src/**/*.module.css` — consumers.
- `scripts/check-design-tokens.mjs` — lint.
- `.cursor/rules/design-tokens.mdc` — IDE rule.

## 8. Cross-references

- `recharts-theme-architecture.md` — chart palette sourcing.
- `i18n-pipeline-architecture.md` — sister UI binding.
- `ui-shared-architecture.md` — where the token files live.
- `docs/users/features/cp-ui/overview.md` and others — feature docs that consume tokens.

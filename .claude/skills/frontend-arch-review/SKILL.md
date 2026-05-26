---
name: frontend-arch-review
description: >
  Audit the Control Plane UI + Agent Dashboard against the design-token / CSS
  framework architecture, including the Tailwind v4 + shadcn surface and the
  `prime-shadcn-tokens.css` `@theme {}` palette. Detects theme/mode-breaking
  offenders: hex / rgba / hsla literals in `*.module.css`, hex / rgba inside
  `style={{}}` blocks in `*.tsx`, raw numeric padding / margin / gap /
  fontSize / fontWeight / borderRadius / boxShadow / transition / zIndex in
  inline styles, stale `var(--xxx, FALLBACK)` patterns referencing token
  names that don't exist, Recharts charts that hardcode hex outside the
  sanctioned `chartColors.ts`, raw Tailwind palette utilities (`bg-white`,
  `text-gray-500`, `dark:bg-zinc-900`, etc.) that bypass the semantic
  shadcn tokens, and i18n hardcoded English strings (delegated to
  i18n-gap-check). Produces a Markdown report with per-file violation
  counts, suggested token mappings, and a punch list ranked by theme/mode
  impact. Trigger keywords: frontend arch review, frontend architecture
  review, design token check, theme audit, mode audit, css framework check,
  tailwind audit, shadcn audit, /frontend-arch-review.
user-invocable: true
---

# Frontend Architecture Review

Verifies that every visual value in `packages/control-plane-ui/`,
`packages/agent/ui/frontend/`, and `packages/ui-shared/` resolves through
the documented design-token layers — and that the recently-introduced
Tailwind v4 + shadcn surface stays inside the token system instead of
escaping back to raw palette utilities.

Current layer map:

- **Layer 1 — raw tokens.** `packages/ui-shared/src/styles/global.css`
  (`--g-*` legacy palette + spacing/radius/shadow/transition constants).
- **Layer 1.5 — Tailwind v4 `@theme {}` raw palette.** Defined in
  `packages/ui-shared/src/styles/prime-shadcn-tokens.css` (`--color-navy-900`,
  `--color-brand`, `--color-text-secondary`, `--font-sans`, …). Tailwind
  resolves utilities like `text-foreground` and arbitrary values like
  `bg-[var(--color-primary)]` against this block.
- **Mode flip.** `prime-shadcn-tokens.css` `:root` defines light values for
  the shadcn vars (`--background`, `--foreground`, `--card`, `--primary`,
  `--sidebar`, …); `.dark, [data-theme="dark"]` re-binds them. Theme
  switching is driven by `<html data-theme="…">` AND `<html class="dark">`
  (set together by `ThemeProvider`).
- **Layer 2 — semantic bridge.** `light.css` / `dark.css` map the legacy
  `--color-*` / `--sidebar-*` / `--shadow-*` names onto the shadcn vars so
  the 230+ existing CSS Modules keep working.
- **Component surface.** Mostly `*.module.css` (~234 files) plus a small but
  growing set of shadcn-style components in `packages/ui-shared/src/shadcn/`
  that use Tailwind utility classes, all of which must resolve through
  the token layers (never raw `bg-white` / `text-gray-500`).

This skill is the user's single entry point for "is the frontend
architecturally clean for theme + mode switching, including the new
Tailwind v4 + shadcn surface".

## What it detects

| Category | Where | Why it matters |
|---|---|---|
| Hex / rgba / hsla literals in `*.module.css` | `*.module.css` outside theme-definition files | Doesn't flip with `data-theme="dark"` / `.dark` → wrong contrast in dark mode |
| Hex / rgba inside `style={{}}` in `.tsx` | All `*.tsx` | Same — inline styles bypass token system |
| Numeric `padding/margin/gap/fontSize/fontWeight/borderRadius/transition/zIndex` in inline styles | All `*.tsx` | Doesn't break mode but breaks density/skin variation; structural inconsistency |
| Stale `var(--xxx, #fff)` references | `.tsx` and `.module.css` | The fallback hex is what actually renders if the token name was never defined — a silent breakage. Cleanup pass renames to the real semantic token and drops the fallback. |
| Recharts hex props (`fill="#…"` / `stroke="#…"`) | `.tsx` only | Should come from `packages/ui-shared/src/theme/chartColors.ts` so the chart flips with mode |
| Raw Tailwind palette utilities (`bg-white`, `text-gray-500`, `border-slate-200`, `dark:bg-zinc-900`, etc.) | `.tsx`, `cva()`/`cn()` strings | Tailwind utilities that resolve to raw palette hex bypass the semantic shadcn tokens — must use `bg-background` / `text-foreground` / `bg-card` / `bg-[var(--color-…)]` instead |
| Hardcoded English in JSX | `.tsx` | Delegated to `i18n-gap-check` skill |

## When to use

- User types `/frontend-arch-review` or asks to "audit frontend",
  "review CSS framework", "check theme/mode safety", "find hardcoded colors",
  "design-token compliance", "theme switching bugs".
- Before merging a UI-heavy PR — confirms the change introduced 0 new
  violations.
- After enabling a new skin / theme — verifies the skin's tokens actually
  reach every component.
- Periodic hygiene (monthly) to catch new violations as the codebase grows.

## Workflow

```
run lint scan → summarise per-category counts → top offending files →
recommend cleanup PRs ranked by theme/mode impact → defer to i18n-gap-check
for hardcoded-string review
```

### Step 1: Run the design-token guard

```bash
npm run check:design-tokens
```

The guard is `scripts/check-design-tokens.mjs`. It exits 0 when clean,
1 on any violation. JSON mode for tooling:

```bash
node scripts/check-design-tokens.mjs --json > /tmp/violations.json
```

For migration hints (literal → suggested token):

```bash
npm run check:design-tokens:hints
```

### Step 2: Group violations and prioritise

The script reports by category. For triage, sort by impact on theme/mode
switching:

| Priority | Category | Reason |
|---|---|---|
| P0 | `css-color-literal`, `inline-color-literal`, `inline-shadow-literal` | These literally break dark mode — wrong text/background contrast |
| P0 | Stale `var(--xxx, FALLBACK)` referencing undefined tokens | Token name never defined → fallback always wins → no theme response |
| P1 | Recharts hex in `fill="…"` / `stroke="…"` | Charts look wrong in dark mode |
| P2 | `inline-radius-literal`, `inline-transition-literal`, `inline-zindex-literal` | Doesn't break mode but locks visual density |
| P3 | `inline-spacing-literal`, `inline-fontsize-literal`, `inline-fontweight-literal` | Cosmetic consistency, no mode impact |

### Step 3: Scan for stale `var(--xxx, FALLBACK)` patterns

These bypass the lint script (the literal is inside `var(...)` so the lint
correctly skips), but if `--xxx` is never defined anywhere, the fallback
hex is what renders forever. Run this complementary check:

```bash
# List all undefined CSS variable references with hex fallbacks
grep -rEho "var\(--[a-z][a-z0-9-]*,\s*[^)]+\)" packages/{control-plane-ui,agent/ui/frontend,ui-shared}/src --include="*.tsx" --include="*.module.css" \
  | sort -u \
  | while read -r ref; do
      name=$(echo "$ref" | sed -E 's/var\((--[a-z0-9-]+).*/\1/')
      defined=$(grep -rE "^\s*${name}\s*:" packages/ui-shared/src/styles/ 2>/dev/null | head -1)
      [ -z "$defined" ] && echo "STALE: $ref"
    done
```

Common stale names: `--text-muted`, `--text-secondary`, `--border`,
`--surface`, `--surface-2`, `--latency-*`. Rename to the real semantic
token (`--color-text-muted`, `--color-border`, etc.) and drop the
fallback in the same edit.

### Step 4: Recharts audit

```bash
grep -rEn '(?:fill|stroke)="#[0-9a-fA-F]{3,8}"' \
  packages/control-plane-ui/src packages/agent/ui/frontend/src \
  --include='*.tsx' | grep -v '\.stories\.\|\.test\.'
```

Any hit must be replaced with `getPhaseColors(mode)[…]`,
`getSeriesColors(mode)[…]`, or `getSemanticColor(mode, …)` imported from
`@/theme/chartColors` (CP-UI) or `@nexus-gateway/ui-shared` (Agent-UI).
Both routes resolve to `packages/ui-shared/src/theme/chartColors.ts` —
the single sanctioned source of hex literals in the entire UI codebase.

### Step 5: Tailwind raw-palette audit (Tailwind v4 + shadcn)

Now that Tailwind v4 + shadcn ship in the bundle, utility classes are a
second surface that can bypass the token system. Raw palette utilities
(`bg-white`, `text-gray-500`, `border-slate-200`, `dark:bg-zinc-900`,
`from-red-500 to-amber-500`, …) resolve to the Tailwind default palette
hex values, not to our shadcn variables, so they ignore the theme bridge
and may also ignore mode flips (the `dark:` variant only flips if you
authored both halves).

```bash
# Raw Tailwind palette tokens used anywhere in .tsx / .ts (cva, cn, raw
# className strings). We intentionally include `dark:` variants since
# `dark:bg-zinc-900` is the most common offender pattern.
grep -rEon \
  "(?:^|[^a-z-])(?:dark:|hover:|focus:|active:|disabled:|group-hover:)*\
(?:bg|text|border|ring|fill|stroke|shadow|from|to|via|outline|decoration|placeholder|accent|caret|divide)-\
(?:white|black|gray|slate|zinc|neutral|stone|red|orange|amber|yellow|lime|green|emerald|teal|cyan|sky|blue|indigo|violet|purple|fuchsia|pink|rose)\
(?:-(?:50|100|200|300|400|500|600|700|800|900|950))?\\b" \
  packages/control-plane-ui/src packages/ui-shared/src packages/agent/ui/frontend/src \
  --include='*.tsx' --include='*.ts' 2>/dev/null \
  | grep -v '\.stories\.\|\.test\.\|chartColors\.ts'
```

If you get hits, the fix is one of:

1. **Surface tokens** — replace with shadcn semantic utilities:
   `bg-white` → `bg-background` (or `bg-card` for elevated surfaces),
   `bg-black` → `bg-foreground` (or `bg-primary`),
   `text-gray-500` → `text-muted-foreground`,
   `text-gray-900` → `text-foreground`,
   `border-gray-200` / `border-slate-200` → `border-border`,
   `bg-gray-50` → `bg-muted`, `bg-gray-100` → `bg-secondary`.
2. **Arbitrary-value Tailwind for legacy semantic tokens** — when the
   semantic role doesn't have a shadcn utility, use the bracket form
   pointing at the Layer 2 token: `text-[var(--color-text-secondary)]`,
   `bg-[var(--color-success-light)]`, `border-[var(--color-border-strong)]`.
3. **Mode-flip utility classes** — if you need different colors in light
   vs dark, use the semantic token (it already flips) rather than
   `bg-white dark:bg-zinc-900`.

The audit also rejects `dark:bg-zinc-900` style patterns because the
shadcn `--background` / `--foreground` variables already flip; manually
authoring both halves duplicates the bridge and creates drift.

### Step 6: Hand off i18n scan

Hardcoded English strings in JSX are out of scope here — delegate:

```
Run the i18n-gap-check skill for hardcoded-string detection.
```

### Step 7: Verify with a real theme flip

Quick manual verification — the lint can be green and the UI can still
look wrong if a component has structural issues beyond literal tokens:

1. Start CP-UI: `npm run dev:control-plane-ui`
2. Open browser, switch theme: localStorage `nexus-theme-mode` →
   `dark`, reload. `ThemeProvider` sets BOTH `<html data-theme="dark">`
   AND `<html class="dark">`; if either is missing the
   `prime-shadcn-tokens.css` `.dark` selector won't fire and Tailwind's
   `dark:` variant breaks — check both are present in DevTools.
3. Scan the 8-10 most-changed pages (recent commit list) for:
   - Text legibility (low contrast on dark bg)
   - Card surfaces (should resolve to `--card` `#141b2b`, not white)
   - Charts (should flip palette)
   - Hover popups / tooltips (should match Recharts Tooltip style)
4. Optionally exercise a non-default skin: drop a JSON in `/public/themes/`
   and set localStorage `nexus-theme-id` → its name.

## Output

Markdown report at `/tmp/frontend-arch-review-<UTC-timestamp>.md` with:

1. Executive summary (total violations, P0 count, theme/mode risk verdict)
2. Per-category breakdown with top 10 offending files each
3. Stale `var(--xxx, FALLBACK)` reference list (separate, since lint can't
   catch these)
4. Recharts hex-prop hits
5. i18n-gap-check link / recommendation
6. Suggested cleanup PR sequence ranked by impact

## Architecture reference

- `CLAUDE.md` → `Conventions → TypeScript / Control Plane UI` → the
  binding "Design-token strict" rule.
- `packages/ui-shared/src/styles/global.css` → Layer 1 legacy raw tokens
  (`--g-*` palette, spacing, radius, shadow, transition constants).
- `packages/ui-shared/src/styles/prime-shadcn-tokens.css` → Tailwind v4
  `@theme {}` raw palette + `:root` light / `.dark, [data-theme="dark"]`
  mode flip for the shadcn variables (`--background`, `--foreground`,
  `--card`, `--primary`, `--sidebar-*`, `--chart-*`, …). This is the
  source of truth for the theme bridge.
- `packages/ui-shared/src/styles/{light,dark}.css` → Layer 2 semantic
  bridge: maps the legacy `--color-*` / `--sidebar-*` / `--shadow-*`
  names onto the shadcn vars so the 230+ CSS Modules keep working
  without rewriting.
- `packages/control-plane-ui/src/styles/tailwind-app.css` /
  `packages/agent/ui/frontend/src/styles/tailwind-app.css` → the
  Tailwind v4 entry points. Imports order matters: `tailwindcss` →
  `tw-animate-css` → `shadcn/tailwind.css` → `prime-shadcn-tokens.css`.
- `packages/ui-shared/src/shadcn/` → shadcn-style components consumed
  by both apps. Variants are built with `cva()` + `cn()`
  (`tailwind-merge` + `clsx`); colors come from `bg-[var(--color-…)]`
  arbitrary values or shadcn semantic utilities (`bg-background`,
  `text-muted-foreground`, …).
- `packages/ui-shared/src/theme/chartColors.ts` → the single sanctioned
  hex-literal source (Recharts requires JS strings).
- `packages/control-plane-ui/src/theme/ThemeProvider.tsx` → wiring that
  sets BOTH `<html data-theme="…">` AND `<html class="dark">`, and
  loads skins from `/public/themes/`. The dual setter is mandatory:
  `prime-shadcn-tokens.css` keys off `.dark, [data-theme="dark"]` and
  Tailwind's `dark:` variant keys off `.dark` (or whatever
  `@custom-variant dark (&:is(.dark *))` resolves to).

## Allowed escape hatches (do not flag)

1. **Recharts colors** imported from `chartColors.ts` (the sanctioned source).
2. **CSS variable bridges**: `style={{ '--foo': dynamicValue }}` — used to
   pass runtime values into CSS classes.
3. **Runtime-computed dimensions**: template literals with `${}`
   interpolation in inline `padding/width/etc.` — e.g.
   `paddingLeft: \`${level * 20}px\``.
4. **Percentage / keyword borderRadius**: `'50%'` (circles), `'0'`,
   `'auto'`, `'inherit'`.
5. **`*.stories.tsx`** / **`*.test.tsx`** files.
6. **Token-definition CSS files**: `styles/{global,light,dark,base,
   utilities,animations,prime-shadcn-tokens}.css` are where raw hex
   legitimately lives (`@theme {}` block, `:root` defaults, `.dark`
   overrides).
7. **Arbitrary-value Tailwind utilities that point at our tokens**:
   `bg-[var(--color-primary)]`, `text-[var(--color-text-secondary)]`,
   `border-[var(--color-border)]` — these go through the token layer.
8. **shadcn semantic utilities**: `bg-background`, `bg-card`,
   `bg-popover`, `text-foreground`, `text-muted-foreground`,
   `border-border`, `ring-ring`, etc. — they resolve to the shadcn
   variables defined in `prime-shadcn-tokens.css` and flip with mode.

Any other waiver requires explicit user approval in chat.

---
doc: ui-shared-architecture
area: cross-cutting
service: ui
tier: 1
updated: 2026-05-20
---

# `packages/ui-shared` Architecture

> **Tier 2 architecture doc.** Read this when adding code to `packages/ui-shared/` or considering whether something belongs in `ui-shared` vs `control-plane-ui` vs the Agent UI. `ui-shared` has a strict **dependency-direction boundary** that, when crossed, makes the package no longer shareable.

`ui-shared` is the cross-bundle library used by **both** the Control Plane UI (admin web app) and the Agent Dashboard (Wails-bundled desktop UI). It exists to keep the design system, the i18n primitives, and small reusable atoms consistent across both surfaces.

---

## 1. The boundary (binding)

`ui-shared` MUST be a leaf in the import graph from each consumer's perspective:

- **`control-plane-ui` imports from `ui-shared`** ✓
- **agent dashboard imports from `ui-shared`** ✓
- **`ui-shared` imports from `control-plane-ui`** ✗ NEVER
- **`ui-shared` imports from `agent/ui/frontend`** ✗ NEVER
- **`ui-shared` imports from `agent/cmd/...`** ✗ NEVER

Once `ui-shared` reaches into either consumer, it stops being shareable.

Enforced by `scripts/check-ui-shared-boundary.mjs` (`npm run check:ui-shared-boundary`).

## 2. What `ui-shared` MAY contain

- **Layer 1 raw design tokens** (`src/styles/global.css`) and **Layer 2 semantic tokens** (`src/styles/light.css` + `src/styles/dark.css`, flat under `src/styles/` — there is no `themes/` subdirectory), plus the shadcn / Tailwind v4 bridge (`src/styles/prime-shadcn-tokens.css`) and the base / animations / utilities CSS that ship alongside them.
- **Layer 2 semantic design tokens** for the cross-cutting "card / sidebar / shadow" semantic names.
- **Pure presentation components** — `Button`, `Input`, `Select`, `Checkbox`, `Radio`, `DatePicker`, `Toast`, `Modal`, `Tooltip`. Components with no business knowledge.
- **Recharts wrappers** — `LineChart`, `BarChart`, `PieChart` with theme integration + `chartColors.ts`.
- **Shared i18n** — the `shared` namespace + the i18n setup helpers consumed by both bundles.
- **Theme hooks** — `useTheme`, `useChartSeriesColors`, `useChartPieColors`, `useChartSemanticColor`, `useChartPhaseColor`.
- **Format helpers** — locale-aware `formatNumber`, `formatDate`, `formatBytes`.

## 3. What `ui-shared` MUST NOT contain

- **API clients / fetchers.** Each bundle's API surface differs (CP-UI talks to `/api/admin/*`; agent UI talks to localhost `/local/*`). Cross-leak would tangle the two HTTP surfaces.
- **Routing.** Each bundle has its own router setup with its own route map.
- **Context providers that import from a specific bundle** (e.g., `CpAuthContext` belongs in `control-plane-ui`, not `ui-shared`).
- **State management stores.** Bundle-specific.
- **Business logic.** No "submitRoutingRule" / "enrollAgent" helpers — those belong in the bundle that owns the operation.
- **Anything that imports from `react-router-dom` if the agent UI doesn't use it** (the agent UI's routing differs from CP-UI's). Wrap routing-dependent components in their bundle.

## 4. Why the boundary is binding

Without it, `ui-shared` accumulates accidental coupling:

- "Let me just import this `useAuth` from cp-ui" → suddenly Agent UI requires `cp-ui` types.
- "This Toast needs to call `apiClient.notify(...)`" → cross-bundle coupling.

The result: a "shared" package that breaks builds in either consumer when the other consumer changes. Has happened before; the binding prevents recurrence.

## 5. Dependency direction visualised

```
                  ┌──────────────┐    ┌──────────────────────┐
                  │  control-    │    │  agent/ui/frontend   │
                  │  plane-ui    │    │  (Wails)             │
                  └──────┬───────┘    └──────────┬───────────┘
                         │                       │
                         ▼                       ▼
                  ┌─────────────────────────────────────┐
                  │       packages/ui-shared            │
                  │  (leaf — imports from neither)      │
                  └─────────────────────────────────────┘
                                 │
                                 ▼
                  External: react, react-i18next, recharts
```

## 6. The cross-bundle `shared` i18n namespace

Files: `packages/ui-shared/src/i18n/{en,zh,es}/shared.json` (no `locales/` segment — the lang directories sit directly under `i18n/`).

Consumed by both bundles via the i18n setup. Reusable atoms (`Save`, `Cancel`, `Online`, `Offline`, `Loading`, status badges) live here.

When the CP-UI bundle starts, it loads `pages.json` + `common.json` + `nav.json` + `shared.json`. The Agent UI loads `dashboard.json` + `shared.json`. Both surfaces resolve `shared:status.online` to the same translated text per locale.

## 7. Adding a new shared atom

1. Check: is it consumed by **both** bundles? If only one — it belongs in that bundle, not in `ui-shared`.
2. Avoid: anything that needs an API client, router, or bundle-specific context.
3. Implement under `packages/ui-shared/src/components/<Atom>.tsx`.
4. Add stories / examples (if storybook is wired) or co-located tests.
5. Add i18n keys for atom text (Button labels, Toast messages) to `shared.json` in all three locales.
6. Export from `packages/ui-shared/src/index.ts`.

## 8. Sources

- `packages/ui-shared/src/` — entire package.
- `packages/ui-shared/README.md` — package-level README with binding rules.
- `packages/control-plane-ui/package.json` — consumer dependency declaration.
- `packages/agent/ui/frontend/package.json` — consumer dependency declaration.

## 9. Cross-references

- `design-tokens-architecture.md` — Layer 1 / 2 tokens live here.
- `i18n-pipeline-architecture.md` — the `shared` namespace.
- `recharts-theme-architecture.md` — chartColors.ts lives here.
- `useapi-queryclient-architecture.md` — DOES NOT live here (bundle-specific).

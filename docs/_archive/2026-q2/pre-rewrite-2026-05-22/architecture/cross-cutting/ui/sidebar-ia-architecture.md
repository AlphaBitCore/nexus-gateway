---
doc: sidebar-ia-architecture
area: cross-cutting
service: ui
tier: 1
updated: 2026-05-20
---

# Sidebar Information Architecture

> **Tier 2 architecture doc.** Read this when adding, moving, or renaming a CP-UI route / section. Cross-link to `iam-identity-architecture.md` (IAM impact review binding). Skill: `.claude/skills/add-cp-ui-section/` walks the full procedure.

The sidebar is the navigation skeleton of the Control Plane UI. Its IA is data-driven from `shellRouteConfig.tsx` + `Sidebar.tsx`; getting the binding rules wrong silently breaks IAM, breadcrumbs, and the navigation experience.

---

## 1. The two data sources

| File | What it owns |
|---|---|
| `packages/control-plane-ui/src/routes/shellRouteConfig.tsx` | `NavSectionKey` enum + `NAV_SECTION_META` (titleKey per section) + route definitions with `allowedActions` (IAM) + `nav` metadata (sectionKey, labelKey, order) |
| `packages/control-plane-ui/src/components/ui/Sidebar/Sidebar.tsx` | Renderer + `navIconNameForPath(path)` switch (keyed on route path string) + `SidebarIconGlyph` (icon-name → SVG glyph) + expand/collapse state |

`shellRouteConfig.tsx` is the source of truth for both routes and their nav presence. `Sidebar.tsx` is purely the renderer — never add a sidebar item that has no corresponding route entry.

## 2. Section keys (canonical enum)

`NavSectionKey` is a closed union of 9 keys (see `shellRouteConfig.tsx`):

```ts
export type NavSectionKey =
  | 'overview' | 'aiGateway' | 'compliance' | 'alerts' | 'devices'
  | 'infrastructure' | 'iam' | 'system' | 'settings';
```

Each key carries metadata in `NAV_SECTION_META` — most importantly `titleKey`, which is passed to `t('nav:<titleKey>')` to render the section header. The table below lists the user-visible order and the i18n key used:

| sectionKey | titleKey (→ `t('nav:<titleKey>')`) | What it contains |
|---|---|---|
| `overview` | `overview` | Dashboard / Traffic / Analytics / Quota usage / Cache ROI |
| `aiGateway` | `aiGateway` | Providers & Models / Credentials / Credential reliability / Routing rules / Virtual keys / Quota policies / Quota overrides / Cache / Passthrough |
| `compliance` | `compliance` | Overview / Hooks / Rule packs / Interception domains / Exemptions / AI guard backend / Streaming / Payload capture / Operation logs / DSAR |
| `alerts` | `alerts` | Inbox / Rules / Channels |
| `devices` | `devicesSection` | Devices / Groups / Auth / Defaults |
| `infrastructure` | `infrastructure` | Nodes / Config sync / Overrides / Jobs / Observability config / Kill switch (single section, NOT multi-key) |
| `iam` | `iam` | Organizations / Projects / Users / Roles / Policies / Simulator / IdPs |
| `system` | `system` | System tools (AI Gateway simulator, …) |
| `settings` | `settingsSection` | Fleet-wide singleton settings (E61: embedding config + future singletons) |

Adding a NEW top-level section is rare and architecturally significant — discuss with the user before introducing one. The `titleKey` is a free string (it does NOT need to equal `sectionKey`); `devices`/`settings` use the suffixed `*Section` keys to avoid clashing with the same-named common term in other contexts.

## 3. Route entry shape

```tsx
{
  path: 'ai-gateway/providers',
  LazyPage: L.LazyProvidersListPage,
  allowedActions: ['admin:provider.read'],                       // IAM gate
  nav: {
    sectionKey: 'aiGateway',
    labelKey: 'providersModels',                                  // → t(`nav:${labelKey}`)
    to: '/ai-gateway/providers',
    allowedActions: ['admin:provider.read'],                      // mirror to nav-level
    order: 0,
  },
},
// Sub-routes (do NOT carry nav entries — they don't appear in the sidebar):
{ path: 'ai-gateway/providers/new', LazyPage: L.LazyProviderWizard, allowedActions: ['admin:provider.create'] },
{ path: 'ai-gateway/providers/:id', LazyPage: L.LazyProviderDetail, allowedActions: ['admin:provider.read'] },
```

Invariants:

1. **`allowedActions` is mirrored** on the top-level route AND its `nav` entry. The sidebar uses `nav.allowedActions` to hide the menu item from users without permission; the route uses its own `allowedActions` to gate the page itself. Drift between these two produces a silent 403 on click (user sees the menu item, route rejects).
2. **`order` is monotonic per `sectionKey`** but does NOT need to be contiguous (use 0, 10, 20, ... to allow future inserts without renumbering).
3. **Sub-routes (`/new`, `/:id`, `/:id/edit`) MUST NOT have `nav` entries.** Only the list route appears in the sidebar; sub-routes are reached via links/buttons within the list page.

## 4. Sidebar icon mapping

`Sidebar.tsx` resolves nav-item icons in two steps:

1. `navIconNameForPath(path: string): NavIconName` — switch keyed on the route path string (e.g. `case '/ai-gateway/providers': return 'cog'`). Multiple paths may share the same icon by sharing a `case` arm (e.g. `'/traffic'` and `'/status'` both return `'activity'`).
2. `SidebarIconGlyph({ name })` — switch keyed on the `NavIconName` union (`'grid'`, `'activity'`, `'chart'`, `'cog'`, `'key'`, `'route'`, `'shield-check'`, etc.) returning the SVG glyph.

Adding a new route means extending the path switch in `navIconNameForPath` and — if you want a new icon — adding both a `NavIconName` member and a `SidebarIconGlyph` case for the glyph:

```tsx
function navIconNameForPath(path: string): NavIconName {
  switch (path) {
    case '/ai-gateway/providers':      return 'cog';
    case '/ai-gateway/credentials':    return 'key';
    case '/ai-gateway/routing':        return 'route';
    // unknown path falls back to 'dot'
    default: return 'dot';
  }
}
```

The header icons (theme picker, language picker, user menu, bell) flow through a separate `MenuIcon({ name })` switch keyed on icon-name strings (`'bell'`, `'sun'`, `'moon'`, `'palette'`, `'globe'`, `'user'`, `'logout'`).

Convention: stable icons per route path. **Sweep for dead arms** when renaming routes — `Sidebar.tsx` doesn't fail at build time if `navIconNameForPath` references a path that no longer exists. The lint `scripts/check-sidebar-icon-mapping.mjs` (`npm run check:sidebar-icon-mapping`) reports orphan case arms by extracting every `case '/<path>':` from `Sidebar.tsx` and diffing against the known set of route paths in `shellRouteConfig.tsx`.

## 5. IAM impact review (binding)

Any change to a sidebar entry or route is subject to the 5-step IAM audit (see `.cursor/rules/iam-impact-review.mdc` + skill `iam-impact-review`):

1. UI `allowedActions` mirrors backend `iamMW(action)`.
2. Resource carve-out decision recorded.
3. Fixtures + seed updated if new resource.
4. Sidebar + breadcrumbs swept on rename.
5. Decisions recorded in plan / commit.

Drift between `nav.allowedActions` and the route's `iamMW(...)` is the most common cause of silent 403s — the menu renders, the click is rejected at the backend.

## 6. Breadcrumb derivation

Breadcrumbs come from the route's `path` segments + lookups against `nav.labelKey` for each segment. Most pages don't need bespoke breadcrumb config — the path-driven derivation works.

When you rename a path, ensure the corresponding `nav.labelKey` is updated; otherwise breadcrumbs render the raw segment.

## 7. Permission-aware rendering

The sidebar pre-filters items by current principal's `allowedActions`. A viewer-level user sees fewer menu items than a super-admin. This is **visual hiding only** — the IAM gate at the route level is what actually enforces access.

The two layers are intentional: hiding the menu reduces noise; the route gate prevents URL-poking access.

## 8. Sources

- `packages/control-plane-ui/src/routes/shellRouteConfig.tsx` — route + nav definitions.
- `packages/control-plane-ui/src/components/ui/Sidebar/Sidebar.tsx` — sidebar renderer + icon switch.
- `packages/control-plane-ui/src/i18n/locales/{en,zh,es}/nav.json` — sidebar labels.
- `.cursor/rules/iam-impact-review.mdc` — IAM audit binding.
- `.claude/skills/add-cp-ui-section/` — full add-section procedure.

## 9. Cross-references

- `iam-identity-architecture.md` — IAM gate definitions.
- `i18n-pipeline-architecture.md` — nav label keys.
- `useapi-queryclient-architecture.md` — page-level data fetching.
- `docs/users/features/cp-ui/*.md` — feature docs grouped by section.

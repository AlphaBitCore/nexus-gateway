# Recipe Adding A CP UI Section

*Audience: contributors adding a new page or section to the Control Plane admin UI.*

Adding a CP UI section touches six binding rules simultaneously: IAM wiring, i18n parity, design-token compliance, `useApi` queryKey shape, sidebar icon mapping, and a feature doc update. The `add-cp-ui-section` skill (`Skill('add-cp-ui-section')`) runs all eight steps with verification commands. The canonical reference is [`iam-identity-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/control-plane/iam-identity-architecture.md) and [`conventions.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/conventions.md).

---

## Before writing any component

Run the IAM impact review first (`Skill('iam-impact-review')`). Every new route needs an IAM action; the UI `allowedActions` and the backend `iamMW(action)` must match exactly. A mismatch produces silent 403s — users see the menu item but get a 403 on click.

---

## Step 1 — Decide route shape and IAM action

Choose a path following the existing convention: `/<section>/<resource>` for list views, `/<section>/<resource>/new` for creation, `/<section>/<resource>/:id` for detail. Identify which `sectionKey` the route belongs to — existing section keys are `overview | aiGateway | compliance | alerts | devices | iam | infrastructure | setup | status | system`. New section keys are rare; discuss before adding one.

Define the IAM action: `admin:<resource>.<verb>`. The resource name is kebab-case and must be registered in `packages/shared/identity/iam/catalog_data.go`. The verb is one of the closed-set verbs in `catalog.go` (`create | read | update | delete | approve | simulate | toggle | export` etc.).

## Step 2 — IAM impact review (binding — 5 steps)

1. Confirm the planned `allowedActions` in the route config matches the `iamMW(action)` in the handler.
2. Decide whether the resource needs its own entry in `catalog_data.go` (carve out when granting it shouldn't imply granting unrelated settings) or can reuse an existing resource.
3. If a new resource is added: update `tools/db-migrate/seed/seed.ts` so `NexusSuperAdmin`, `NexusAdminFullAccess`, and `NexusViewerAccess` policies include the new action; update `packages/control-plane/internal/identity/iam/managed.go` if a test fixture needs it.
4. Sweep `packages/control-plane-ui/src/components/ui/Sidebar/Sidebar.tsx` for stale `case` arms if renaming a route.
5. Record the decision in the PR description: "kept on `admin:settings.read`" or "carved out as `<resource>`".

## Step 3 — Register the route

In `packages/control-plane-ui/src/routes/shellRouteConfig.tsx`, add entries for list, create, and detail views:

```tsx
{
  path: '<section>/<resource>',
  LazyPage: L.Lazy<ResourceListPage>,
  allowedActions: ['admin:<resource>.read'],
  nav: {
    sectionKey: '<section>',
    labelKey: '<resource>.nav.label',
    to: '/<section>/<resource>',
    allowedActions: ['admin:<resource>.read'],
    order: <n>,
  },
},
{ path: '<section>/<resource>/new',   LazyPage: L.Lazy<ResourceCreatePage>, allowedActions: ['admin:<resource>.create'] },
{ path: '<section>/<resource>/:id',   LazyPage: L.Lazy<ResourceDetailPage>,  allowedActions: ['admin:<resource>.read'] },
```

## Step 4 — Implement the page component

Create `packages/control-plane-ui/src/pages/<section>/<Resource>Page.tsx`. Three binding conventions:

**`useApi` queryKey shape**: the queryKey domain prefix must be one of `'admin' | 'my' | 'user' | 'proxy'` followed by the resource and state vars:

```tsx
const { data } = useApi<ResourceListResponse>(
  ['admin', '<resource>', 'list', page, search],
  () => adminService.listResources({ page, search }),
);
```

**CSS variables only**: all visual values come from CSS custom properties — no hex codes, no numeric pixel values, no `rgba()` literals in `*.module.css` or inline `style={{}}`. Use the design tokens from `packages/control-plane-ui/src/styles/tokens/`.

**All user-visible strings via `t()`**: every label, button text, placeholder, error message, and tooltip must use `t('namespace:section.key')`. Never hardcode English strings in JSX.

## Step 5 — Add i18n keys to all three locales

Add keys to all three locale files in sync — `en`, `zh`, `es`:

- `packages/control-plane-ui/src/i18n/locales/en/pages.json` (or `nav.json` for sidebar label)
- `packages/control-plane-ui/src/i18n/locales/zh/pages.json`
- `packages/control-plane-ui/src/i18n/locales/es/pages.json`

Then copy to `packages/control-plane-ui/public/locales/` (the runtime-loaded bundle):

```bash
npm run check:i18n   # CI gate — fails if any locale is missing a key
```

A key that exists in `en` but is missing from `zh` or `es` shows the raw key string in non-English locales, which is confusing for users.

## Step 6 — Add the sidebar icon

In `packages/control-plane-ui/src/components/ui/Sidebar/Sidebar.tsx`, add a `case` arm for the new route's `sectionKey:labelKey` combination:

```tsx
case '<sectionKey>:<labelKey>':
  return <YourIcon />;
```

If renaming an existing route, remove the old `case` arm at the same time. Dead cases accumulate and cause icon-mapping bugs on rename.

## Step 7 — Write tests

Write a Vitest unit test for the page component covering render, interaction, and the IAM-gated negative case:

```typescript
it('renders the resource list', async () => { /* ... */ });
it('shows create button when user has create action', async () => { /* ... */ });
it('hides create button for viewer role', async () => { /* ... */ });
```

Then smoke-test the route using the `cp_curl` helper from `docs/developers/workflow/local-dev-debugging.md`:

```bash
# Positive: super-admin can reach the route
cp_login
cp_curl /api/admin/<resource>

# Negative: viewer-level user gets 403
# Switch to viewer account (diana@nexus.ai / viewer123) and confirm 403.
```

Both tests must pass before marking the section done.

## Step 8 — Update the feature doc

If a new section was added (rare), create `docs/users/features/cp-ui/<section>.md` following the existing template. If a new item was added to an existing section, update the relevant `docs/users/features/cp-ui/<existing-section>.md` to list the new page and its purpose.

## Verification commands

```bash
npm run check:i18n             # all locales in parity
npm run check:design-tokens    # no hex/rgb in CSS modules
npm run check:useapi-querykey  # queryKey domain prefix correct
npm run lint --workspace=packages/control-plane-ui
```

---

## What links break if you skip this

- **Skipping IAM step 2 (seed update)**: `NexusViewerAccess` does not include the new action; viewer-role users never see the new section even though it exists in the sidebar for super-admins. The only symptom is a 403 on first click, with no indication why.
- **Skipping i18n keys in zh/es**: the CP UI renders raw key strings in non-English locales, exposing implementation details to admins running in those languages.
- **Skipping queryKey domain prefix**: `useApi` cache invalidation may cross-contaminate queries from different admin resources, causing stale data to appear after mutations.
- **Skipping the negative IAM test**: UI-backend action drift goes undetected until a role boundary is tested in production.

---

## Canonical docs

- [`iam-identity-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/control-plane/iam-identity-architecture.md) — NRN format, action taxonomy, iamMW middleware, managed policies
- [`conventions.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/conventions.md) — TypeScript: i18n mandatory, design-token strict, useApi queryKey domain-prefix binding

**Adjacent wiki pages**: [Control Plane Overview](Control-Plane-Overview) · [Control Plane IAM Model](Control-Plane-IAM-Model) · [Recipe Adding An IAM Action](Recipe-Adding-An-IAM-Action) · [Recipe Index](Recipe-Index)

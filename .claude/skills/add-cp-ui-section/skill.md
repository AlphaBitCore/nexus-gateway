# add-cp-ui-section

Walk the multi-step procedure when adding a new CP-UI section, item, or route. Combines IAM impact review + i18n + design-token discipline + useApi shape + Sidebar mapping + tests + feature doc.

Use this skill when:

- Adding a new sidebar menu item / section.
- Adding a new admin API surface backed by a UI page.
- Renaming or moving an existing route.

This is the **single most error-prone** CP-UI workflow because it touches 6+ binding rules at once.

---

## The 8 steps

### 1. Decide the route shape

- Path: `/<section>/<resource>[/<id>]` — match existing patterns.
- `sectionKey`: one of `overview / aiGateway / compliance / alerts / devices / iam / infrastructure / setup / status / system`. New section keys are rare — discuss with the user first.
- `allowedActions`: the IAM action the route requires (`admin:<resource>.<verb>`).

### 2. IAM impact review (binding)

Run the `iam-impact-review` skill verbatim. The 5 steps:

1. UI `allowedActions` matches backend `iamMW(action)`.
2. Decide resource carve-out (new resource type or reuse).
3. If new resource: update `tools/db-migrate/seed/seed.ts` + `packages/control-plane/internal/iam/managed.go`.
4. Sidebar + breadcrumb sweep.
5. Record decisions in PR description.

This is **non-negotiable** for every new route.

### 3. Register the route

`packages/control-plane-ui/src/routes/shellRouteConfig.tsx`:

```tsx
{
  path: '<section>/<resource>',
  LazyPage: L.Lazy<Page>,
  allowedActions: ['admin:<resource>.read'],
  nav: { sectionKey: '<section>', labelKey: '<label>', to: '/<section>/<resource>', allowedActions: ['admin:<resource>.read'], order: <n> },
},
{ path: '<section>/<resource>/new', LazyPage: L.Lazy<Create>, allowedActions: ['admin:<resource>.create'] },
{ path: '<section>/<resource>/:id', LazyPage: L.Lazy<Detail>, allowedActions: ['admin:<resource>.read'] },
```

### 4. Implement the page component

`packages/control-plane-ui/src/pages/<section>/<Resource>Page.tsx`:

- Use `useApi` with proper queryKey shape: `['admin', '<resource>', 'list', ...stateVars]` (cross-ref `useapi-querykey.mdc`).
- All visual values via CSS variables (cross-ref `design-tokens.mdc`).
- All user-visible strings via `t('namespace:section.key')` (cross-ref `i18n-mandatory.mdc`).

### 5. Add i18n keys

To **all three** locale files:

- `packages/control-plane-ui/src/i18n/locales/en/pages.json` (or `common.json` / `nav.json`).
- `.../zh/...`.
- `.../es/...`.

Plus `nav.json` for the sidebar label.

Then copy to `packages/control-plane-ui/public/locales/`.

```bash
npm run check:i18n   # CI gate
```

### 6. Sidebar icon mapping

`packages/control-plane-ui/src/components/ui/Sidebar/Sidebar.tsx`:

```tsx
case '<sectionKey>:<labelKey>':
  return <YourIcon />;
```

Sweep for stale `case` arms when renaming. Dead cases accumulate otherwise.

### 7. Tests

- Vitest unit test for the page component (render + click + IAM-gated negative).
- Smoke test the route via `tests/lib/auth.sh`:

```bash
cp_login
cp_curl /api/admin/<resource>
```

Run a positive test (super-admin reaches the route) AND a negative test (viewer-level user gets 403).

### 8. Feature doc

If you added a new section (rare), create `docs/users/features/cp-ui/<section>.md` (per the existing template).

If you added a new item to an existing section, update the relevant `docs/users/features/cp-ui/<existing>.md` to list the new page.

## Verification before claiming done

```bash
npm run check:i18n
npm run check:design-tokens
npm run check:useapi-querykey
npm run lint --workspace=packages/control-plane-ui
```

Run the 5-point IAM verification (positive + negative test).

Report the IAM impact review summary in the PR description.

## Output

Final checklist to paste into the PR description:

```
CP-UI section add audit:
- Route: /<section>/<resource>
- IAM action: admin:<resource>.<verb>
- IAM review: <pasted from iam-impact-review skill>
- i18n: all 3 locales updated, check:i18n PASS
- Design tokens: no hex/rgb in *.module.css, check:design-tokens PASS
- useApi queryKey: ['admin', '<resource>', 'list', ...] shape, check:useapi-querykey PASS
- Sidebar icon: mapped
- Tests: positive + negative IAM PASS, Vitest PASS
- Feature doc: docs/users/features/cp-ui/<section>.md (added | updated | n/a)
```

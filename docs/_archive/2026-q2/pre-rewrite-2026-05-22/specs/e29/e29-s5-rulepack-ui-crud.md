# E29 S5 — Rule Pack UI CRUD

## Story

As a compliance operator I need the Rule Pack UI to give me full
create, edit, and delete flows for packs, plus first-class pages
for bind / unbind / override so I can build a policy without
touching the API.

## Scope

- `packages/control-plane-ui/src/pages/hooks/rulepacks/RulePackList.tsx`
- `packages/control-plane-ui/src/pages/hooks/rulepacks/RulePackDetail.tsx`
- `packages/control-plane-ui/src/pages/hooks/rulepacks/RulePackEditor.tsx` (new)
- `packages/control-plane-ui/src/pages/hooks/rulepacks/ImportPackModal.tsx`
- `packages/control-plane-ui/src/pages/hooks/rulepacks/BindPackModal.tsx`
  (retire the "modal-only" flow and promote to a routed page
  `BindPackPage.tsx`)
- `packages/control-plane-ui/src/pages/hooks/rulepacks/OverridesPanel.tsx`
  (promote to a routed page `OverridesPage.tsx`)
- `packages/control-plane-ui/src/i18n/locales/{en,zh,es}/pages.json`

## Tasks

1. Rule Pack list page:
   - Table columns: name, version, maintainer, rule count,
     install count, created.
   - `Create pack` button launches the new `RulePackEditor`.
   - Row actions: edit, delete (with confirm), view installs.
2. Rule Pack editor page:
   - Metadata form (name, version, maintainer, description).
   - Rules sub-table with inline add/edit/remove (category, severity,
     pattern, flags, description, labels).
   - Submit creates or updates via the new Admin API endpoints.
3. Rule Pack detail page:
   - Shows metadata, rules (read-only), installs list.
   - From here operators can go to bind / overrides pages via
     routed links.
4. Bind page:
   - Lists hooks that accept the rulepack engine; install / uninstall
     buttons.
5. Overrides page:
   - Per-install rules grid with disable + severity-override edits,
     saved via the existing `adminUpsertRulePackOverrides` endpoint.
6. i18n — new keys added to all three locale files with parity.

## Acceptance criteria

- Unit tests (Vitest) cover the form submissions and mutation
  error paths for create / update / delete on packs and installs.
- `useApi` query keys follow the mandatory `['admin', 'rule-packs',
  '<variant>', ...]` convention; no bare arrays.
- All new copy exists in `en / zh / es`.
- Running the UI locally: create pack → bind → send traffic via
  ai-gateway → observe `blocking_rule` persisted on the audit row.

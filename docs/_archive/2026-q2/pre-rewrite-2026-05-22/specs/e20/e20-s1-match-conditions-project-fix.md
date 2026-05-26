# E20 Story 1 — Match Conditions: project rename + UI selector modernization

**Epic:** 20 — Routing Rule Match Conditions Fix
**Story:** 1
**Status:** Draft — 2026-04-21
**Requirements:** `docs/developers/specs/e20/e20-routing-match-conditions-fix.md`
**OpenAPI:** `docs/users/api/openapi/admin/e20-s1-routing-rule-match-conditions.yaml`

## User Story

> **As a** platform admin,
> **I want** routing rule match conditions to actually filter by caller project (not by organization under a mislabeled field) and to use searchable, consistent selectors for models/providers/projects,
> **so that** a rule I save in the UI applies to exactly the requests I intended, and editing the rule feels like the rest of the admin surface.

## Context

- `packages/ai-gateway/internal/router/matcher.go:304` compares `conds.Organizations` against `ctx.VirtualKey.ProjectID` — the field name is legacy; the semantic is project-id all the way through.
- `packages/control-plane-ui/src/pages/config/routing/MatchConditionExtraFields.tsx:65` renders an `OrgTreeSelect` that writes **organization ids** into `matchConditions.organizations`. These never equal a project id — the filter is effectively broken.
- The same component uses hand-rolled `<input type="checkbox">` controls for providers, at least one user reports unable to toggle them, and the control is inconsistent with the rest of the admin surface.
- `src/pages/config/routing/MatchModelSelector.tsx` uses a native grouped `<select>` — not searchable when a provider has many models.
- The three routing hooks (`useRoutingRuleDetail`, `useRoutingRuleCreate`, `useRoutingRuleForm`) pass `[]` as `useApi` queryKey, violating CLAUDE.md's binding rule (must start with a domain prefix).

## Tasks

### T1. Backend — rename Organizations → Projects

Files:

- `packages/ai-gateway/internal/router/types.go`
- `packages/ai-gateway/internal/router/matcher.go`
- `packages/ai-gateway/internal/router/matcher_test.go`

Changes:

1. In `types.go`, rename field:
   ```go
   // before
   Organizations []string `json:"organizations,omitempty"` // legacy name for projectId
   // after
   Projects []string `json:"projects,omitempty"`
   ```
2. In `matcher.go` `RuleMatchesContext`, rename `conds.Organizations` → `conds.Projects`. Keep the VK-projectId comparison — the semantic was correct, only the name was wrong.
3. In `matcher_test.go`, rename every test-table field `organizations` → `projects`; add cases:
   - empty `projects` → match
   - `projects: ["p-1"]`, VK ProjectID = `p-1` → match
   - `projects: ["p-1"]`, VK ProjectID = `p-2` → no match
   - `projects: ["p-1"]`, no VK on context → no match

### T2. Backend — admin routing validation

Files:

- `packages/control-plane/internal/handler/admin_routing.go` (or the struct file it imports for matchConditions shape)
- `packages/control-plane/internal/handler/admin_routing_test.go`

Changes:

1. Update the request-body struct / validator to accept `matchConditions.projects: []string` and reject `matchConditions.organizations` with HTTP 422 `{ "error": "matchConditions.organizations has been renamed to matchConditions.projects" }`.
2. Mirror the rename anywhere the admin write path threads matchConditions through to the router config payload.
3. Add a table-driven test covering: POST with `projects` → 201; POST with `organizations` → 422; POST with both → 422.

### T3. Frontend — shared `MultiCombobox` primitive

If `src/components/ui/MultiCombobox/` does not already exist, build it:

- Props: `options: { value: string; label: string; group?: string }[]`, `value: string[]`, `onChange: (next: string[]) => void`, `placeholder?: string`, `disabled?: boolean`.
- UI: trigger shows selected count + placeholder; popover lists options grouped by `group` (optional), with a single search input that filters by label (case-insensitive substring).
- Keyboard: ArrowUp/Down to navigate, Space/Enter to toggle, Esc to close.
- Accessibility: trigger is `<button role="combobox" aria-expanded>`, popover options are `role="option"` with `aria-selected`.
- Vitest: render, open, search narrows, click toggles on+off, keyboard navigation toggles, controlled value updates render.

If a suitable primitive exists (`MultiSelect`, `Combobox`), wrap/extend it rather than duplicating.

### T4. Frontend — `ProjectMultiSelect`

File: `src/components/ui/ProjectMultiSelect/ProjectMultiSelect.tsx` (+ `index.ts`, + Vitest).

- Fetches `projectApi.list({ limit: '500' })` under queryKey `['admin', 'projects', 'list', 'match-conditions']`.
- Fetches `organizationApi.list({ limit: '500' })` under queryKey `['admin', 'organizations', 'list', 'match-conditions']`.
- Builds option labels as `{organizationName} / {projectName}` by joining projects to their org by `organizationId`.
- Wraps `MultiCombobox`; emits `string[]` of project ids.
- Loading state: render the MultiCombobox disabled with a loading placeholder; error state: render an `ErrorBanner` inline.

### T5. Frontend — `ProviderMultiSelect`

File: `src/components/ui/ProviderMultiSelect/ProviderMultiSelect.tsx` (+ `index.ts`, + Vitest).

- Takes `providerGroups: AdminModelsByProvider[]` as a prop (data already fetched at the page level — no second fetch).
- Filters to `g.provider?.enabled`, builds options `{ value: g.provider.id, label: g.provider.displayName ?? g.provider.name }`.
- Wraps `MultiCombobox`. Emits `string[]` of provider ids.

### T6. Frontend — `ModelMultiCombobox`

File: `src/pages/config/routing/ModelMultiCombobox.tsx` (kept page-local because it depends on the excludeModels prop + providerGroups shape; move to shared only if reused).

- Props match the existing `MatchModelSelector`: `selected: string[]`, `onChange: (ids: string[]) => void`, `providerGroups: AdminModelsByProvider[]`, `excludeModels: string[]`.
- Builds options grouped by provider display name, excludes any model id in `excludeModels`.
- Wraps `MultiCombobox` with `group` enabled. Emits `string[]` of model ids.
- Replaces `MatchModelSelector.tsx` (delete the old file per dev-phase policy).

### T7. Frontend — swap controls in MatchConditionExtraFields + MatchModelSelector usage

Files:

- `src/pages/config/routing/MatchConditionExtraFields.tsx`
- `src/pages/config/routing/form/MatchConditionsSection.tsx`
- `src/pages/config/routing/detail/RoutingRuleEditForm.tsx`
- `src/pages/config/routing/create/RoutingRuleCreatePage.tsx`

Changes:

1. `MatchConditionExtraFields` swaps its hand-rolled checkbox wrap → `ProviderMultiSelect`, and its `OrgTreeSelect` → `ProjectMultiSelect`. The prop `organizationIds` / `onChangeOrganizationIds` is renamed to `projectIds` / `onChangeProjectIds`. Delete `MatchConditionExtraFields.module.css` checkbox styles that are no longer used.
2. Every call site of `MatchConditionExtraFields` updates to pass `projectIds` + `onChangeProjectIds`.
3. Every call site of `MatchModelSelector` is replaced with `ModelMultiCombobox` (same prop shape). Delete `MatchModelSelector.tsx` after the swap.

### T8. Frontend — routing-rule-config rename

File: `src/pages/config/routing/routing-rule-config.ts` + `routing-rule-config.test.ts`.

Changes:

1. Rename `organizations` → `projects` in `MatchConditionsFormState`.
2. `parseMatchConditionsForm`: read `m.projects` only (no legacy alias).
3. `buildMatchConditionsPayload`: emit `projects` only.
4. Update the test file accordingly.

### T9. Frontend — state/form rename across routing pages

Files:

- `src/pages/config/routing/detail/useRoutingRuleDetail.ts`
- `src/pages/config/routing/create/useRoutingRuleCreate.ts`
- `src/pages/config/routing/form/useRoutingRuleForm.ts`
- `src/pages/config/routing/detail/RoutingRuleReadView.tsx`

Changes:

1. Rename every `matchOrgIds` form field / variable / setValue call → `matchProjectIds`.
2. Rename the hydration line `editForm.setValue('matchOrgIds', mc.organizations)` → `editForm.setValue('matchProjectIds', mc.projects)`.
3. In `RoutingRuleReadView`, read from `matchConditions.projects`, label the row using the new i18n key.

### T10. Frontend — queryKey fix

Files:

- `src/pages/config/routing/detail/useRoutingRuleDetail.ts:96-99`
- `src/pages/config/routing/create/useRoutingRuleCreate.ts:82-85`
- `src/pages/config/routing/form/useRoutingRuleForm.ts:108-111`

Change all three empty `[]` queryKeys to `['admin', 'models', 'grouped', 'include-empty']`.

### T11. i18n rename

Files:

- `src/i18n/locales/{en,zh,es}/pages.json`
- `public/locales/{en,zh,es}/pages.json` (post-build copy)

Changes:

1. Rename keys under `routing.*`:
   - `organizationProjectIds` → `projectsLabel`
   - `matchOrgIdsTooltip` → `matchProjectsTooltip`
   - `placeholderOrgIds` → `placeholderProjects`
   - `helpMatchOrganizations` → `helpMatchProjects`
   - `ariaHelpMatchOrganizations` → `ariaHelpMatchProjects` (if present)
2. Provide localized translations in all three languages; technical terms stay English.
3. Run key-count check across locales; counts must match.

### T12. Tests

- Go: `go test ./packages/ai-gateway/internal/router/... -race -count=1` and `go test ./packages/control-plane/internal/handler/... -race -count=1` must pass.
- Frontend: `npx vitest run` from `packages/control-plane-ui/` must pass. New tests for `MultiCombobox`, `ProjectMultiSelect`, `ProviderMultiSelect`, `ModelMultiCombobox`, updated `routing-rule-config.test.ts`.
- The existing diagnostic test `MatchConditionExtraFields.diag.test.tsx` is obsolete once the checkbox control is removed — delete it in the same change.

### T13. Manual verification

Start the stack (`./scripts/dev-start.sh` + `npm run dev:control-plane-ui` + `go run ./cmd/control-plane` + `go run ./cmd/ai-gateway`), then:

1. Open `/config/routing/{any-rule-id}`, click **Edit**.
2. Confirm **Match models** is a searchable multi-select. Type a partial model id; confirm filtering works; toggle two models; save.
3. Confirm **Match providers** is a searchable multi-select (no checkboxes). Toggle two providers; save.
4. Confirm **Match projects** is a searchable multi-select showing `{org} / {project}` labels. Select two projects; save.
5. Verify via DB: the row's `matchConditions` JSON contains `projects` (not `organizations`).
6. Send a test request with a VK belonging to a selected project → rule matches. Swap to a VK in a different project → rule does not match.

## Acceptance Criteria

- [ ] `matchConditions.organizations` is removed from the entire repo — no references in Go source, TypeScript source, tests, or i18n keys.
- [ ] `matchConditions.projects` filters correctly: selecting project P1 causes the rule to match only requests whose VK belongs to P1.
- [ ] Match-models control supports search-as-you-type and multi-select.
- [ ] Match-providers control supports search and multi-select (no hand-rolled checkboxes).
- [ ] Match-projects control shows `{org} / {project}` labels and emits project ids.
- [ ] Every `useApi` call in routing pages starts with at least `['admin', <resource>, ...]`.
- [ ] `go test ./...` and `npx vitest run` both pass.
- [ ] All new UI strings exist in en/zh/es locale files with matching key counts; copied to `public/locales/`.
- [ ] Architectural note: no impact — documented in the PR description, not in `docs/users/product/architecture.md`.

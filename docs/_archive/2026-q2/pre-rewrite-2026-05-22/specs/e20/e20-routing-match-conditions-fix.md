# E20 — Routing Rule Match Conditions Fix

**Status:** Draft — 2026-04-21
**Epic:** 20
**Depends on:** AI Gateway routing engine (`packages/ai-gateway/internal/router`), Control Plane admin routing API, Control Plane UI routing pages, Organization/Project admin data sources

## 1. Business Goal

Each routing rule carries a `matchConditions` block that filters *which* runtime requests the rule is considered for (by model id, by provider, and by caller project). Today the feature is half-working:

- The backend routing engine reads `matchConditions.organizations` and compares it against the **virtual key's project id** (`VirtualKey.ProjectID`), never against any organization id. The field name "organizations" is a legacy misnomer.
- The Control Plane UI, taking the legacy name at face value, renders an **organization** multi-select (`OrgTreeSelect`) and saves **organization ids** into `matchConditions.organizations`. Those ids never equal a project id, so the rule never matches.
- Separately, the UI uses a native grouped `<select>` for the match-models list (not searchable over ~hundreds of models) and hand-rolled `<input type="checkbox">` controls for matched providers (inconsistent with the rest of the admin surface, and at least one user report that the checkboxes could not be toggled).

This epic closes the semantic gap (project vs organization), modernizes the three match-condition selectors so they behave consistently with the rest of the admin UI, and brings the codebase in line with the dev-phase policy (no `@deprecated` legacy field; rename in place).

## 2. Scope

### In scope

- Rename `matchConditions.organizations` → `matchConditions.projects` end-to-end (Go router types + matcher, admin write API, UI form state, UI i18n keys). No DB migration: existing dev rows with the legacy key are rewritten by a seed reset or by re-saving the rule through the admin UI.
- Replace the Organizations `OrgTreeSelect` with a new **Projects multi-select** sourced from `projectApi.list` + `organizationApi.list` (to render `{orgName} / {projectName}` labels). No new backend endpoint — the two existing admin endpoints suffice.
- Replace the native grouped `<select>` for match-models with a searchable `ModelMultiCombobox` built on the shared `MultiCombobox` primitive.
- Replace the hand-rolled provider `<input type="checkbox">` wrap with a `ProviderMultiSelect` built on the same `MultiCombobox` primitive. This fixes the toggle-failure report by swapping the suspect control for a tested one.
- Fix the `useApi([])` queryKey violation in all three routing pages (`useRoutingRuleDetail.ts`, `useRoutingRuleCreate.ts`, `useRoutingRuleForm.ts`) per CLAUDE.md binding rule.
- Update Go unit tests for `RuleMatchesContext` to cover the renamed field and confirm project-id semantics.
- Update frontend tests for `routing-rule-config` (parse/build matchConditions) and the new selectors.

### Out of scope

- Adding new match dimensions (virtual key id, organization id, device group, tag). Deliberately deferred — current traffic signals used by routing are `modelId`, provider, and project (via VK). Adding organization or VK-level filters would change the router signature.
- Multi-select for the fallback/primary target pickers. Those are separate controls with their own control pattern (single provider + single model), not match conditions.
- A new RBAC scope for project lookups. Existing `admin:ListOrganizations` + `admin:ListProjects` are adequate for the routing rule editor (admin role today has both).
- Backfill of existing dev data. Dev-phase policy allows fresh seed; there is no installed user base with live `matchConditions.organizations` rows to preserve.

## 3. User Roles & Personas

| Role | Need met by this epic |
|---|---|
| **Platform Admin** | Express "this rule only applies to requests from project P1 or P2" and have the engine actually honor it. |
| **Provider Ops** | Select match-models from a searchable list when a provider has hundreds of registered models. |
| **Support Engineer** | Trust the UI — a checked checkbox means the provider filter is saved and applied by the routing engine. |

## 4. Functional Requirements

### F1 — Backend rename organizations → projects (MUST)

`packages/ai-gateway/internal/router/types.go` `MatchConditions` MUST expose a `Projects []string` field (JSON key `projects`) in place of `Organizations`. `RuleMatchesContext` in `matcher.go` MUST compare `Projects` against `ctx.VirtualKey.ProjectID` (unchanged semantics, correct name). No legacy alias.

### F2 — Control Plane admin write path (MUST)

`PUT /api/admin/routing-rules/:id` and `POST /api/admin/routing-rules` MUST accept `matchConditions.projects` (string array) and MUST reject `matchConditions.organizations` (422 with a clear error). Existing validator code paths that referenced `organizations` MUST be renamed in the same change.

### F3 — UI: projects multi-select (MUST)

The routing create + edit pages MUST render a `ProjectMultiSelect` control in place of `OrgTreeSelect`. The control MUST:
- Source options by fetching `projectApi.list({ limit: '500' })` and `organizationApi.list({ limit: '500' })` under disambiguated queryKeys.
- Render each option as `{organizationName} / {projectName}` for unambiguous identification.
- Support multiple selection, search-as-you-type over the composite label, and clear-all.
- Emit the selected array of **project ids** (not composite strings) into the form state.

### F4 — UI: models searchable multi-select (MUST)

The match-models control MUST be replaced with `ModelMultiCombobox`:
- Groups by provider, same data source as today (`systemApi.listModels({ includeEmptyProviders: 'true' })`).
- Searchable over model id and provider display name.
- Excludes models already selected as primary/fallback targets (current `excludeModels` prop behavior preserved).
- Emits a string array of model ids.

### F5 — UI: providers multi-select (MUST)

The match-providers control MUST be replaced with `ProviderMultiSelect`:
- Lists all enabled providers from the same `systemApi.listModels` response.
- Searchable by provider display name.
- Emits a string array of provider ids.

### F6 — queryKey domain prefix (MUST)

All three `useApi` call sites that fetch `systemApi.listModels({ includeEmptyProviders: 'true' })` MUST use the key `['admin', 'models', 'grouped', 'include-empty']` (exact shape), matching the binding rule in CLAUDE.md. Empty `[]` queryKeys MUST NOT remain in the routing pages after this epic.

### F7 — i18n rename (MUST)

All locale keys under `pages:routing.*` referring to "organization" in the match-conditions context (e.g. `organizationProjectIds`, `matchOrgIdsTooltip`, `placeholderOrgIds`, `helpMatchOrganizations`) MUST be renamed to `project` equivalents (`projects`, `matchProjectsTooltip`, `placeholderProjects`, `helpMatchProjects`). All three locale files (en/zh/es) MUST be updated and copied to `public/locales/`. Key counts MUST match across locales.

## 5. Non-Functional Requirements

### NF1 — Performance

The ProjectMultiSelect MUST fetch ≤ 500 projects and ≤ 500 organizations per load (acceptable for the envisioned large-enterprise scale; not a customer-tenant list). No pagination UI required at MVP.

### NF2 — Test coverage

- Go: table-driven tests on `RuleMatchesContext` covering empty projects (match), matching project (match), non-matching project (no match), and no VK on context (no match).
- Frontend: Vitest on `parseMatchConditionsForm` + `buildMatchConditionsPayload` for the renamed field. Vitest smoke tests on each new selector covering select / unselect / search.

### NF3 — Accessibility

Each new selector MUST be keyboard navigable and screen-reader labeled (`aria-label` or associated `<label>`). Checkboxes are replaced precisely because the current hand-rolled control has no proper label-to-input binding.

## 6. Constraints & Assumptions

- Routing rules are pre-GA; no backwards compatibility for `matchConditions.organizations`. Rules saved with the legacy key will fail validation after this change, and must be re-saved through the UI.
- Virtual keys already carry a single `ProjectID` field — the project filter remains a simple `contains` check (unchanged semantics).
- `MultiCombobox` primitive exists in the shared UI library (or will be added in Story 1 if not); the three new selectors wrap it. If the primitive is missing, building it is part of Story 1.

## 7. Glossary

- **Match conditions**: the per-rule filter block deciding whether a rule applies to a given request. Distinct from the rule's routing *strategy*, which decides which target wins once a rule applies.
- **Project**: the finest-grained caller bucket. A virtual key belongs to exactly one project; a project belongs to exactly one organization.
- **Organization**: the coarser tenant-like bucket; not currently used for routing filtering.
- **MultiCombobox**: the shared searchable multi-select UI primitive used across the admin surface.

## 8. Priority

| Req | MoSCoW |
|---|---|
| F1 Backend rename | Must |
| F2 Admin API validation | Must |
| F3 ProjectMultiSelect | Must |
| F4 ModelMultiCombobox | Must |
| F5 ProviderMultiSelect | Must |
| F6 queryKey prefix fix | Must |
| F7 i18n rename | Must |
| NF1 Performance (≤500 pre-page) | Should |
| NF2 Tests | Must |
| NF3 Accessibility | Should |

---

## 9. Story 2 — Remove redundant `RoutingRule.modelId` column

**Status:** Draft — 2026-04-22

### 9.1 Problem

`RoutingRule` carries a top-level `modelId` column (plus a `@@index([modelId])`) in addition to `matchConditions.models`. Both act as rule-matching filters — the routing engine AND's the direct `modelId` binding with every `matchConditions` dimension in `ruleMatches` (`packages/ai-gateway/internal/router/resolver.go:158-173`). The field was introduced as an index-friendly shortcut for fast rule lookup by model.

The Control Plane UI **never** exposes a form control to set or clear this column. Editing `matchConditions.models` in the UI leaves a stale `modelId` value behind when one was written by seed, admin API, or a prior schema version. Observed in production-adjacent dev: a rule seeded with `modelId='gpt-4o-mini'` had its `matchConditions.models` edited to `['moonshot-v1-8k']`; the rule silently stopped matching anything (the two filters were AND'd, contradictory), and traffic fell through to a catch-all rule that pointed at a disabled target.

The column is effectively hidden state that contradicts the UI's source-of-truth model and has no remaining reason to exist — matching-by-model is already expressed via `matchConditions.models`.

### 9.2 Business Goal

Eliminate the hidden filter surface so that `matchConditions` is the **sole** rule-matching truth source. A UI-driven edit cannot leave the rule in a state the UI cannot represent.

### 9.3 In Scope

- Drop `RoutingRule.modelId` column and its index in the Prisma schema + a new migration.
- Remove the field from Go models in `packages/control-plane/internal/store/routing_rule.go` (struct, SELECT/INSERT/UPDATE, `CreateRoutingRuleParams`, `UpdateRoutingRuleParams`).
- Remove the field from `packages/ai-gateway/internal/store/routing.go` (struct + SELECT).
- Remove the `rule.ModelID != nil && ...` check from `Resolver.ruleMatches` so matching depends only on `matchConditions`.
- Remove the `ModelID *string` field from create/update request bodies in `packages/control-plane/internal/handler/admin_routing.go`.
- Remove `modelId?: string` from the UI `RoutingRule` type in `packages/control-plane-ui/src/api/services/routing.ts`.
- Simplify the simulate-panel seeding in `detail/useRoutingRuleDetail.ts` (no longer reads `rule.modelId`).
- Clean `modelId:` entries from `tools/db-migrate/seed/seed-routing-rules.ts`.
- Ship an OpenAPI spec for the rule CRUD surface (`docs/users/api/openapi/admin/e20-s2-routing-rule-crud.yaml`) reflecting the final schema — there was no prior CRUD OpenAPI doc for routing rules.

### 9.4 Out of Scope

- Backfill / data migration for existing rows. Pre-GA, no installed user base; `prisma migrate dev` + seed reset is sufficient.
- Any change to `matchConditions.models` semantics.

### 9.5 Functional Requirements

| ID | Requirement | MoSCoW |
|---|---|---|
| F8 | `RoutingRule` MUST NOT expose a `modelId` field at the HTTP, Go, TS, or DB layers after this story. | Must |
| F9 | `Resolver.ruleMatches` MUST decide applicability using only the direct lookup against `MatchConditions` (and always-true when `matchConditions` is empty). | Must |
| F10 | The CP admin create/update endpoints MUST reject unknown fields cleanly (existing `c.Bind` behavior) — no special handling of `modelId` needed, the field is simply gone. | Must |
| F11 | `docs/users/api/openapi/admin/e20-s2-routing-rule-crud.yaml` MUST describe GET/POST/PATCH/DELETE `/api/admin/routing-rules` and `/api/admin/routing-rules/{id}` with the final schema. | Must |

### 9.6 Non-Functional Requirements

- **NF4** — A table-driven test in `resolver_test.go` MUST prove a rule with `matchConditions={"models":[X]}` matches only X-model requests, and a rule with empty match conditions matches every request. This locks the behavior the hidden `modelId` field used to corrupt.
- **NF5** — After the migration runs, `docker exec … psql … \d "RoutingRule"` MUST show no `modelId` column and no index on it.

### 9.7 Risks

- `load-balance-mini` currently has a contradictory `modelId='gpt-4o-mini'` AND `matchConditions.models=['moonshot-v1-8k']`. After this story, the contradiction disappears and the rule starts matching `moonshot-v1-8k` requests as originally intended. This is a **desired** behavior change, not a regression. Other seeded rules either have `modelId` NULL or have `modelId` consistent with `matchConditions.models` (semantically a no-op).


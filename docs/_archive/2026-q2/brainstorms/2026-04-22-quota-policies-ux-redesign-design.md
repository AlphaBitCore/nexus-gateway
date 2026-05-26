# Quota Policies UX Redesign

**Date:** 2026-04-22
**Status:** Approved — implementation pending
**Scope:** `packages/control-plane-ui/src/pages/config/quota-policies/*`, `packages/control-plane/internal/handler/admin_quota_policies.go`, i18n locale files, seed cleanup.

## 1. Context

The existing Create/Edit pages for quota policies
(`/config/quota-policies/new` and `/config/quota-policies/:id`) expose three
matching fields — `scope`, `vkType`, `organizationId` — as independent dropdowns
and a tree picker in a single "Scope & Matching" card. Two problems:

1. **No cascade.** Changing `scope` does not constrain the valid values of
   `vkType`. An admin can create `scope=user + vkType=application`, which is
   semantically dead — the AI-Gateway matcher
   (`packages/ai-gateway/internal/pipeline/quota/policy_cache.go:180-200`) never consults
   it, because the application-VK check chain has no `user` level.
2. **Wrong mental model.** The UI presents `scope × vkType × organizationId` as
   a free Cartesian product. In reality, the business scenarios collapse into a
   handful of fixed combinations. Admins have to learn the schema to use the
   form.

The `QuotaOverride` Create page
(`packages/control-plane-ui/src/pages/config/quota-overrides/QuotaOverrideCreate.tsx`)
is the inverse: one entity at a time, cascading `targetType → target`. It is
clear because it matches its mental model. Policies need the same treatment,
scaled to "a class of entities" instead of "one entity".

## 2. Business Scenarios — In / Out

The redesign is scoped by enumerating the concrete admin scenarios and
eliminating YAGNI combinations.

### In scope — 7 scenarios

| # | Scenario | Example |
|---|---|---|
| 1 | Organization budget ceiling | Company-wide $50,000/mo cap, or Engineering dept $20,000/mo |
| 2 | User personal budget — all users | Every user defaults to $300/mo for personal VK usage |
| 3 | User personal budget — per organization | Engineering users default to $500/mo, Sales users to $200/mo |
| 4 | Project budget default — all projects | Every project defaults to $10,000/mo for application VK usage |
| 5 | VK ceiling — all application VKs | Each application VK defaults to max $5,000/mo |
| 6 | VK ceiling — all personal VKs | Each personal VK defaults to max $200/mo |
| 7 | Per-entity exception | Handled by `QuotaOverride`, not this page |

### Out of scope — 4 rejected scenarios (YAGNI)

| Rejected | Why |
|---|---|
| Application/Personal VK ceiling scoped to a specific organization subtree | The organization budget (scenario 1) already caps total org spend. Per-type VK scoping inside an org is redundant detail with no additional enforcement power. |
| Project budget scoped to a specific organization subtree | Same reasoning as above. |
| Per-entity policy (e.g. "CSM-AI project cap $20k") | That is `QuotaOverride`'s job. Policies describe classes; overrides address individuals. Keeping the line bright prevents double-representation of the same rule. |

### Decision: the effective match space

After pruning YAGNI, the match model collapses to 4 policy types with fixed
field rules:

| Policy type | `scope` | `organizationId` | `vkType` |
|---|---|---|---|
| Organization budget | `organization` | required | must be null |
| User personal budget | `user` | optional (null = all users) | must be null |
| Project budget | `project` | must be null | must be null |
| Virtual key ceiling | `vk` | must be null | required (`personal` or `application`) |

No other combinations are legal. The frontend enforces this by only rendering
the fields that apply to the chosen type; the backend enforces it with a 400
validation error so API callers cannot bypass the UI.

## 3. Data Model — Unchanged

`QuotaPolicy` table in `tools/db-migrate/schema.prisma:339-359` stays as is:

```
id, name, description,
scope, organizationId, vkType,
periodType, costLimitUsd, tokenLimit,
enforcementMode, alertThresholds, priority, enabled,
createdBy, createdAt, updatedAt
```

Matching logic in `packages/ai-gateway/internal/pipeline/quota/policy_cache.go`
(`FindPolicy(scope, organizationID, vkType)`) stays as is. The field values
produced by this new UX are a strict subset of what the matcher already
supports — a valid refinement, not a model change.

## 4. Frontend UX

### 4.1 Page structure

Both `QuotaPolicyCreate.tsx` and `QuotaPolicyEdit.tsx` use the same 3-card
layout:

**Card 1 — Basic info** (unchanged from current): name, description.

**Card 2 — Policy type & scope**. Replaces the current "Scope & Matching" card.

Rendered as a single required `FormSelect` labeled "Policy type", with the 4
values:

- `organization` — label "Organization budget", help "Cap total spend for an organization or department."
- `user` — label "User personal budget", help "Default monthly budget per user for personal VK usage."
- `project` — label "Project budget", help "Default budget per project for application VK usage."
- `vk` — label "Virtual key ceiling", help "Default cap per individual virtual key."

Below the policy type, a **dynamic sub-region** renders fields that depend on
the chosen type:

- `organization`:
  - `Organization` field (required), `OrgTreeSelect`, no clear button.
  - Helper line: "This policy caps total spend for the selected organization."

- `user`:
  - `Organization filter` field (optional), `OrgTreeSelect`, allowClear.
  - Helper line: "Applies to personal VK usage of users in this organization. Leave empty to cover all users."

- `project`:
  - No additional fields.
  - Helper line: "Applies to application VK usage of all projects."

- `vk`:
  - `VK type` field (required), `FormSelect` with two values: `personal`, `application`. No "all" option — a VK-scope policy must pick a side.
  - Helper line: "Default ceiling per VK of this type. More specific policies (e.g. organization budget) still apply on top."

**Card 3 — Budget & enforcement** (unchanged): period type, cost limit, token
limit, enforcement mode, alert thresholds, priority, enabled.

### 4.2 Policy type transitions

When the user changes `Policy type`, the form resets the dependent fields so
stale values never leak across types:

- Switching to `organization`: clear `vkType`, keep `organizationId` (user can adjust).
- Switching to `user`: clear `vkType`, keep `organizationId` (as optional filter).
- Switching to `project`: clear both `organizationId` and `vkType`.
- Switching to `vk`: clear `organizationId`, reset `vkType` to empty (user must pick).

Implementation: `useEffect` on `watch('scope')` that calls `form.setValue` with
`shouldDirty: false` on the cleared fields. Same pattern as
`QuotaOverrideCreate.tsx:89-93` (`useEffect([targetType])`).

### 4.3 Edit page — handling pre-existing invalid combinations

Pre-GA the seed may contain policy rows with `scope=user + vkType=application`
or similar. On load, `QuotaPolicyEdit.tsx` silently drops `vkType` and
`organizationId` values that do not apply to the loaded `scope`, per the rules
in §2. First save cleans the row. No migration script, no warning banner —
consistent with the project's "no backcompat, no defer" pre-GA policy
(`CLAUDE.md`).

### 4.4 Form validation (client-side)

Client-side Zod schema is per-type via `z.discriminatedUnion` or an
equivalent refinement. Submit button stays disabled while required
type-specific fields are empty (`organizationId` for `organization`; `vkType`
for `vk`).

### 4.5 i18n keys

All user-visible strings live in `pages:quotaPolicies.*` in all three locale
files (`en`, `zh`, `es`). Keys to add/rename:

```
policyType          — "Policy type"
policyTypeTooltip
typeOrganization    — "Organization budget"
typeUser            — "User personal budget"
typeProject         — "Project budget"
typeVk              — "Virtual key ceiling"
helpOrganization    — "This policy caps total spend ..."
helpUser            — "Applies to personal VK usage of users ..."
helpProject         — "Applies to application VK usage of all projects."
helpVk              — "Default ceiling per VK of this type ..."
organizationRequired — "Organization is required for organization-budget policies"
vkTypeRequired       — "VK type is required for VK-ceiling policies"
orgFilterLabel       — "Organization filter"
```

Keys **retained** (still used by `QuotaPolicyList.tsx` for column display and
filters): `scope`, `scopeUser`, `scopeVk`, `scopeProject`, `scopeOrganization`,
`vkType`, `vkTypePersonal`, `vkTypeApplication`, `allTypes`, `organization`,
`allOrganizations`, `filterByScope`, `allScopes`.

Keys **deleted** (used only by the old Create/Edit form, no longer needed):
`scopeAndMatching`, `scopeTooltip`, `vkTypeTooltip`, `organizationId`,
`organizationIdTooltip`.

Technical terms (`VK`, `API`, `SSO`) stay in English across all locales.

## 5. Backend validation

`packages/control-plane/internal/handler/admin_quota_policies.go` —
`CreateQuotaPolicy` and `UpdateQuotaPolicy` gain a helper
`validateScopeCombination(scope, organizationId, vkType)` invoked after the
existing enum checks. Rules mirror §2:

```
scope=organization
  requires organizationId != nil && *organizationId != ""
  requires vkType == nil
scope=user
  requires vkType == nil
  (organizationId optional)
scope=project
  requires organizationId == nil
  requires vkType == nil
scope=vk
  requires organizationId == nil
  requires vkType != nil && (*vkType == "personal" || *vkType == "application")
```

Violations return HTTP 400 with `errJSON(<message>, "validation_error", "")`,
message pattern `"<field> is required when scope=<scope>"` or `"<field> must
not be set when scope=<scope>"`. Messages are English only per repo policy.

For `UpdateQuotaPolicy`, the effective values are the merge of the existing row
and the partial update body — validation runs against the merged result.

## 6. Seed & migration

No schema migration. The seed script in `tools/db-migrate/` is reviewed; any
`QuotaPolicy` seed rows that violate §2 rules are rewritten to valid
combinations (most likely by dropping the redundant `vkType` on `user`/`project`
scoped rows).

Existing non-seed policies in the pre-GA database, if any, will self-heal on
next edit (see §4.3) or can be deleted/recreated by admins.

## 7. Testing

### Frontend
- Vitest unit test for `QuotaPolicyCreate.tsx`:
  - Rendering: each policy-type choice shows the expected sub-fields and hides the rest.
  - Transitions: switching policy types clears stale values.
  - Submit payload: verify correct field presence/absence for each type.
  - Validation: submit disabled until required type-specific field filled.
- Vitest unit test for `QuotaPolicyEdit.tsx`:
  - Loading a policy row with stale fields silently drops them in the form state.
  - Saving a cleaned row sends a payload without the dropped fields.

### Backend
- `admin_quota_policies_test.go`:
  - Table-driven test for `validateScopeCombination` covering all legal and
    illegal combinations listed in §5.
  - HTTP-level test: POST/PUT returning 400 for each illegal combination with
    the expected error shape.

### Manual verification
- `./scripts/dev-start.sh` then open `http://localhost:3000/config/quota-policies/new`.
- For each of the 4 policy types, confirm:
  - sub-fields match §4.1.
  - helper text renders and is English-only.
  - Submit lands a policy row with the expected column set.
- Edit an existing row; confirm stale fields cleared on save.

## 8. Out of scope (explicit)

- **Preview / "matches N entities" count.** Nice-to-have but requires a new
  backend endpoint; deferred.
- **Priority field UX.** Left as a plain numeric input; priority semantics are
  fine, only the matching-field UX is the problem here.
- **Policy list page (`QuotaPolicyList.tsx`).** The list view keeps showing
  current columns; a column-level revamp can be a follow-up if needed.
- **Quota override page.** Already clean; no changes.
- **Data model / matcher changes.** Per §3.

## 9. Rollout

Pre-GA; no phased rollout, no feature flag. Single PR ships frontend + backend
+ seed updates + tests. Git revert is the rollback path.

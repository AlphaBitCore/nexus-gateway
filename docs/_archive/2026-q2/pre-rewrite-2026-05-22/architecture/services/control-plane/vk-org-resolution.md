---
doc: vk-org-resolution
area: service
service: control-plane
tier: 1
---

# Virtual Key org resolution — two join chains

> **Tier-2 architecture doc.** Read this before touching `packages/ai-gateway/internal/platform/store/virtualkey.go` (the `vkSelectSQL` constant), any audit-pipeline site that stamps `traffic_event.org_id` / `traffic_event.org_name`, or any analytics aggregation that groups by org. The same rule applies in the Control Plane store layer (`packages/control-plane/internal/ai/virtualkeys/vkstore/virtual_key.go`) when it surfaces an org-attached VK to admin handlers.

## TL;DR

A `VirtualKey` row resolves to its `Organization` through **two independent parent chains**:

| VK type | Chain |
|---|---|
| `application` | `VirtualKey.projectId` → `Project` → `Organization` |
| `personal` | `VirtualKey.ownerId` → `NexusUser` → `Organization` |

`vkSelectSQL` JOINs both, then `COALESCE`s the two results in `SELECT`, preferring the application chain when present. Caller-side code (`store.VirtualKey.OrganizationID`, `vkauth.VKMeta.OrganizationID`) sees the resolved value and does NOT add another fallback.

## Why two chains exist

The data model is asymmetric:

- **`application` VKs** are owned by a `Project` (`projectId NOT NULL`). The Project is the unit of org boundary; `VirtualKey.ownerId` is `NULL` because no individual user "owns" an application key. Org comes from `Project.organizationId`.
- **`personal` VKs** are owned by a `NexusUser` (`ownerId NOT NULL`). There is no Project — they are a user's self-service key for individual development. `VirtualKey.projectId` is `NULL`. Org comes from `NexusUser.organizationId`.

A single-chain JOIN would silently drop one of the two populations. `COALESCE` is the correct shape because both columns are mutually exclusive (a VK is either app-owned via Project or person-owned via Owner — never both).

## The SQL pattern

```sql
SELECT
  vk.id, vk.name, ...,
  COALESCE(p."organizationId", u."organizationId") AS organization_id,
  COALESCE(org.name,           u_org.name)         AS organization_name,
  COALESCE(org.timezone,       u_org.timezone)     AS organization_timezone
FROM "VirtualKey" vk
LEFT JOIN "Project"      p     ON vk."projectId" = p.id
LEFT JOIN "Organization" org   ON p."organizationId" = org.id
LEFT JOIN "NexusUser"    u     ON vk."ownerId" = u.id
LEFT JOIN "Organization" u_org ON u."organizationId" = u_org.id
```

The two `LEFT JOIN "Organization"` aliases (`org` and `u_org`) are the load-bearing piece. Don't try to "simplify" by reusing one alias — Postgres treats the second join as a separate relation, and the COALESCE depends on both being available row-by-row.

## Rules for contributors

When you add a new column to `vkSelectSQL` that joins through `Project`, `Organization`, or `NexusUser`:

1. Check whether the column has an equivalent in BOTH chains.
2. If yes (e.g. timezone, locale, parent org id, billing account), wrap the SELECT in `COALESCE(application_chain_value, personal_chain_value)` AND add the matching `LEFT JOIN` for whichever chain isn't already in the query.
3. If no (genuinely application-only or personal-only), document the asymmetry inline — future contributors will assume parity and "fix" the missing fallback.

When you add a new VK type beyond `application` / `personal` (e.g. `service-account`, `agent-bound`):
- Decide whether it has its own org-resolution path; extend the COALESCE to cover it.

When you add a new column to `traffic_event` that depends on the VK's parent (org timezone, billing account, primary AccountManager):
- Source it through the resolved `vk.OrganizationID` (single read), not through a fresh JOIN. Don't re-implement the precedence.

## Tests

`packages/ai-gateway/internal/platform/store/virtualkey_sql_test.go` pins string-level invariants on `vkSelectSQL`: five positive checks (3 `COALESCE` results for org_id / org_name / org_timezone, plus 2 `LEFT JOIN`s against `Organization`) and one negative check that the legacy single-chain form is gone. Anyone "simplifying" the SQL by dropping a chain trips these tests in CI before the change reaches review.

A real round-trip test against a live Postgres is out of scope — the string pins catch the most common regression class (accidental refactor revert) and the prod re-probe is the authoritative end-to-end verification on each deploy.

## What this doc does NOT cover

- **`identity` JSONB on `traffic_event`** — built separately in the audit pipeline and currently includes only `vk` + `user` sub-objects. The `api/types.ts` comment claims "personal VK: vk + user + (org)" but the runtime audit-record builder doesn't actually emit `identity.org` today. The admin UI displays the top-level `org_id` / `org_name` columns (which the SQL hotfix DOES correctly populate), so the user-visible problem is resolved. If a future epic depends on `identity.org`, that's a separate piece of work.
- **CP-side `VirtualKey` query** in `packages/control-plane/internal/ai/virtualkeys/vkstore/virtual_key.go` — currently has its own join logic for admin list/detail handlers; verify it follows the same dual-chain pattern when surfacing org info, or add a follow-up to align it with the data-plane store.

## References

- Source-of-truth SQL: `packages/ai-gateway/internal/platform/store/virtualkey.go:vkSelectSQL`.
- Tests: `packages/ai-gateway/internal/platform/store/virtualkey_sql_test.go`.
- Memory feedback link: `feedback_vk_org_dual_join_chain` (Claude private memory).
- Cursor rule: `.cursor/rules/vk-org-resolution.mdc`.

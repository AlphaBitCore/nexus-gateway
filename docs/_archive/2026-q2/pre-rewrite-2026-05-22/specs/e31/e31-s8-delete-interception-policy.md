# E31 S8 — Delete InterceptionPolicy feature

**Epic:** 31
**Story:** 8
**Status:** Draft — 2026-04-27
**Requirements:** inline (cleanup of half-implemented feature; no separate requirements doc)

## User Story

As an operator, I want the half-implemented `InterceptionPolicy` feature deleted from the codebase, so the system has one less dead concept and the introspection / cache-invalidation surfaces stop carrying noise that confuses both operators and developers.

## Background

`InterceptionPolicy` was conceived as an agent-side rule layer ("which domains the agent should inspect, passthrough, or deny" per `admin_policies.go:11-17`). The current implementation is incoherent:

- Backend: DB table, store, handlers, route registration — all wired.
- Frontend: list/detail/create pages exist but **never wired to the sidebar nav** (no `nav: { ... }` field on the routes), so users cannot reach them through normal navigation.
- Agent: the configsync.Manager has no applier for `interception_policies` — the key is **dead** on the agent side, contradicting the documented "Agent only" SCOPE.
- Compliance-proxy: receives `interception_policies` invalidations and incorrectly responds by clearing `CategoryAllowlists` (an apparent same-name accident; the SCOPE comment explicitly says compliance-proxy does NOT consume this feature).

Operator decision: this feature's job is already covered by the combination of `compliance/interception-domains` (which domains to intercept) + `config/hooks` (what to do with intercepted traffic). InterceptionPolicy adds no value and is wholly removed.

## Scope

### In

- Drop the `InterceptionPolicy` Prisma model + DB table (new migration).
- Delete every Go file, function, route, and config-key handling that exists solely for this feature.
- Delete the CP UI pages, API service module, type, lazy-page registration, and route entries.
- Delete the i18n top-level `policies` block from `en/zh/es` `pages.json` (this block belongs to InterceptionPolicy; IAM and Quota policies use different i18n keys).
- Remove the test mocks (`msw-handlers.ts`) that target `/api/admin/policies`.

### Out

- **IAM policies** (`/api/admin/iam/policies`, `IamPolicy*`, `iam.ts` service, `iam/policies` route): unrelated, untouched.
- **Quota policies** (`/api/admin/quota-policies`, `QuotaPolicyList`, `RegisterQuotaPolicyRoutes`): unrelated, untouched.
- **Device-group policies** (`device-groups.ts:AddGroupPolicy/UpdateGroupPolicy/RemoveGroupPolicy`): unrelated, untouched.
- **No backwards-compatibility shims, no @deprecated, no parallel paths.** Per project convention, this is a clean cut in pre-GA. A `git revert` is the rollback if needed.

## Disambiguation (CRITICAL — review checklist)

The word "policies" appears in **four unrelated subsystems**. Only the first is being deleted:

| Subsystem | Backend route | Frontend route | UI service | Status |
|---|---|---|---|---|
| **InterceptionPolicy** | `/api/admin/policies` | `/config/policies` (no nav) | `services/policies.ts` | 🗑 **DELETE** |
| IAM policies | `/api/admin/iam/policies` | `/iam/policies` | `services/iam.ts` | KEEP |
| Quota policies | `/api/admin/quota-policies` | `/config/quota-policies` | `services/quotaPolicies.ts` | KEEP |
| Device-group policies | `/api/admin/device-groups/:id/policies` | inside group detail page | `services/device-groups.ts` | KEEP |

When executing T2-T4 below, every grep / replace MUST be matched against these four columns — never blanket-search for "policy" or "policies".

## Tasks

### T1. DB migration — drop the table

Create new migration directory under `tools/db-migrate/migrations/<timestamp>_drop_interception_policy/` with `migration.sql`:

```sql
DROP TABLE IF EXISTS "InterceptionPolicy";
```

Update `tools/db-migrate/schema.prisma`: remove the `model InterceptionPolicy { ... }` block (lines ~11-21 of the current schema).

### T2. Backend Go — delete

Files to delete entirely:
- `packages/control-plane/internal/handler/admin_policies.go`
- `packages/control-plane/internal/store/interception_policy.go`

Files to edit (line-precision):
- `packages/control-plane/internal/handler/admin_routes.go` — remove `h.RegisterPolicyRoutes(g, iamMW)` (currently line 41). Do NOT touch the line above (`RegisterQuotaPolicyRoutes`).
- `packages/control-plane/internal/handler/admin_extras.go` — in `CacheFlush`:
  - Remove the two lines: `h.Hub.InvalidateConfig(ctx, "compliance-proxy", "interception_policies")` and `h.Hub.InvalidateConfig(ctx, "agent", "interception_policies")`.
  - Remove `"policies"` from the `categories := []string{...}` slice.
- `packages/control-plane/internal/store/safe_update.go` — remove `InterceptionPolicyUpdateColumns` map and its declaration.
- `packages/compliance-proxy/cmd/compliance-proxy/main.go` — remove the `case "interception_policies":` block (currently lines ~449-453). The block only invalidates `CategoryAllowlists`, which other config_keys (`interception_domains`, `domain_allowlist`) already do — no behavioral loss.

Verify nothing else imports `store.InterceptionPolicy`, `InterceptionPolicyListParams`, etc. after the deletes:

```
grep -rE 'InterceptionPolicy|interception_policies|InterceptionPolicyUpdateColumns' packages/ tools/
```

Expected: only the deleted files (in pre-deletion state) match. Post-deletion: zero matches.

### T3. CP UI — delete

Directories to delete entirely:
- `packages/control-plane-ui/src/pages/config/policies/` — contains `PolicyList.tsx`, `PolicyList.test.tsx`, `PolicyList.module.css`, `PolicyCreate.tsx`, `PolicyDetail.tsx`, `PolicyForm.tsx` (and any module.css siblings).

Files to delete entirely:
- `packages/control-plane-ui/src/api/services/policies.ts`

Files to edit:
- `packages/control-plane-ui/src/api/services/index.ts` — remove the two lines:
  - `export { policyApi } from './policies';`
  - `export type { PolicyWritePayload, PolicyUpdatePayload } from './policies';`
- `packages/control-plane-ui/src/api/types.ts` — remove `export interface Policy { ... }` (currently line 214). Do NOT remove `IamPolicyDocument`, `IamPolicy`, `IamPolicyAttachment`.
- `packages/control-plane-ui/src/routes/lazyPages.tsx` — remove `LazyConfigPoliciesPage`, `LazyPolicyCreate`, `LazyPolicyDetail` exports.
- `packages/control-plane-ui/src/routes/shellRouteConfig.tsx` — remove the three route entries:
  ```ts
  { path: 'config/policies', LazyPage: L.LazyConfigPoliciesPage },
  { path: 'config/policies/new', LazyPage: L.LazyPolicyCreate, allowedRoles: [...] },
  { path: 'config/policies/:id', LazyPage: L.LazyPolicyDetail, allowedRoles: [...] },
  ```
- `packages/control-plane-ui/src/test/msw-handlers.ts` — remove the two handlers `http.get('/api/admin/policies', ...)` and `http.get('/api/admin/policies/:id', ...)`, plus the surrounding mock data block (the `id: 'policy-1', description: 'Default rate limiting policy', ...` object) if it has no other consumers (verify before deleting).

Files NOT to touch (despite grep hits):
- `Sidebar/Sidebar.tsx` — its `policy` matches are all `/iam/policies` cases.
- `IamPolicy*.tsx`, `IamRoleDetail.tsx`, `IamGroupDetail.tsx`, `UserPermissionsTab.tsx`, `iam.ts` — IAM domain.
- `pages/config/quota-policies/QuotaPolicyList.tsx` — Quota domain.
- `device-groups.ts` — device-group policies.

### T4. i18n — delete

Edit each of the three locale files:
- `packages/control-plane-ui/src/i18n/locales/en/pages.json`
- `packages/control-plane-ui/src/i18n/locales/zh/pages.json`
- `packages/control-plane-ui/src/i18n/locales/es/pages.json`

In each, delete the **top-level** `"policies": { ... }` block (currently starting at line ~730 in all three locales). The block contains `title: "Policies"`, `subtitle: "Configure request interception policies"`, etc. — purely InterceptionPolicy.

Do NOT delete:
- `iam.policies` / `iam.policiesSubtitle` / nav `policies: "IAM Policies"` keys (IAM).
- `quotaPolicies.*` / nav `quotaPolicies` keys (Quota).
- `pipelinePolicies` / `policyTitle` / `policyDesc` / `policyNarrowing` keys (these are routing-rule "Policy Narrowing" copy, unrelated to InterceptionPolicy).
- `policiesCol`, `policyName`, `policy` inside fleet/IAM screens.

Verify: copy `pages.json` to `public/locales/<lang>/pages.json` after edits and confirm key counts match across the three locales (project convention — see CLAUDE.md i18n rule).

### T5. Verify

1. Build: `npm run build:control-plane-ui` (no missing-import errors).
2. Go: from repo root — `go build ./packages/control-plane/... ./packages/compliance-proxy/...` (no unresolved refs).
3. Tests:
   - `go test -race -count=1 ./packages/control-plane/... ./packages/compliance-proxy/...`
   - `cd packages/control-plane-ui && npx vitest run`
4. DB migration:
   - `cd tools/db-migrate && npx prisma migrate dev --name drop_interception_policy` — applies cleanly to a dev DB; `\d "InterceptionPolicy"` returns "Did not find any relation".
5. Final grep — all of the following must return ZERO matches in `packages/` and `tools/db-migrate/schema.prisma`:
   ```
   grep -rE 'InterceptionPolicy|interception_policies|InterceptionPolicyUpdateColumns'
   grep -rE 'PolicyWritePayload|PolicyUpdatePayload|policyApi'
   grep -rnE "path: 'config/policies'"
   ```
   (Hits inside `migrations/<old timestamp>_*/migration.sql` are expected and acceptable — historical migrations are immutable.)
6. Smoke: start CP + compliance-proxy + ai-gateway locally; navigate to `/config/policies` in CP UI — should 404. CacheFlush button (if exposed) should still succeed without hitting interception_policies.

## Acceptance Criteria

1. `InterceptionPolicy` table is dropped via a new migration.
2. No Go file imports `store.InterceptionPolicy*`; no Go code references the `interception_policies` config key.
3. CP UI build succeeds with `policies.ts`, `Policy` type, and `config/policies` routes removed.
4. i18n key counts match across en/zh/es; no `policies.title` key (the InterceptionPolicy one) remains in any locale.
5. The four "policies" sibling subsystems (IAM, Quota, Device-group, routing-rule narrowing) are demonstrably untouched: their lints pass, their tests pass, their routes still register.
6. Final grep checklist (T5 step 5) returns zero non-historical matches.
7. CacheFlush still works; compliance-proxy startup logs do not mention `interception_policies` after restart.
8. e31-s7 SDD updated separately (covered by task #19) to drop row 17 and gap B.

## Risks

- **Accidental IAM / Quota policy deletion.** The "Disambiguation" table + the explicit "DO NOT touch" list under each task is the primary mitigation. Reviewer must run all four sibling test suites.
- **Stale references in vendored / generated files.** `tsconfig.tsbuildinfo`, `dist/`, and Prisma generated client may carry stale references; rebuilding clears them. Not a code concern.
- **CacheFlush behavioral change.** Removing the two `interception_policies` invalidations means CacheFlush no longer triggers compliance-proxy's accidental allowlist clear via this path; the allowlist is already invalidated by the explicit `interception_domains` + `domain_allowlist` invalidations elsewhere, so no observable regression.
- **Pre-GA assumption.** This deletion assumes there are no production deployments holding rows in `InterceptionPolicy`. Per project memory `feedback_no_backcompat_dev_phase.md` this is the operating norm — but operator should confirm no demo/lab DB has data worth preserving before applying the migration.

## References

- Discovery context: 2026-04-27 conversation tracing the e31-s7 introspection coverage matrix; row 17 (`interception_policies`) revealed as a half-implemented dead path.
- Code SCOPE comment: `packages/control-plane/internal/handler/admin_policies.go:11-17` — declares "Agent only", contradicted by current implementation routing to compliance-proxy.
- Project convention: `CLAUDE.md` "Development-phase policy: no backward compatibility, no defer" — authorises clean delete without phased rollout.
- Follow-up SDD: `docs/developers/specs/e31/e31-s7-runtime-introspection.md` will drop coverage matrix row 17 and gap B once this story merges.

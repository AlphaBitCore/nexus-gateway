## E25-S1 — InterceptionDomain Admin CRUD

Status: delivered (2026-04-22)
Epic: E25 — Interception catalog admin surface

## 1. Problem

Compliance Proxy has consumed `interception_domain` + `interception_path`
rows from day one through
`store.ListEnabledInterceptionDomains`, and Nexus Hub's
`AgentInterceptionDomainsLoader` aggregates the same pair of tables for
the Agent's Category B pull. But the admin had no way to mutate those
rows through the UI or API — every row landed through the seed
script. Adding a domain, toggling a rule, or rolling out a new adapter
required a DB migration-style workflow.

Additional facts the spec teased apart:

- `domain_allowlist` was listed as a separate Category B shadow key for
  `compliance-proxy`. In practice it is a derived projection of enabled
  `interception_domain.host_pattern` rows — not an independent config.
  CP's reducer already cascades an `interception_domains`
  invalidation to both `CategoryInterceptionDomains` and
  `CategoryAllowlists` caches. A separate admin surface would be
  redundant and would create drift between the two views.
- The Prisma enums (`HostMatchType`, `PathMatchType`, `PathAction`,
  `DefaultPathAction`, `FailureAction`, `NetworkZone`) were already
  declared and used by the live schema; the admin surface should reuse
  them as-is.

## 2. Goal

1. Admin can list, filter, create, update, and delete
   `InterceptionDomain` rows via the Control Plane API + UI, including
   the nested `InterceptionPath[]` collection.
2. Every write invalidates exactly ONE shadow key
   (`compliance-proxy:interception_domains`); `domain_allowlist` cascades
   implicitly and does not need a second push.
3. The existing read-only runtime loader
   (`ListEnabledInterceptionDomains`) keeps its signature unchanged so
   CP's reducer and Hub's Cat B loader continue to consume the same
   row shape byte-for-byte.
4. The UI exposes a single admin surface at
   `/compliance/interception-domains` (list + detail with inline path
   editor). No separate `/compliance/domain-allowlist` page —
   documentation + in-UI note explain the derivation.

## 3. Non-goals

- Editing individual rows in `domain_allowlist` — it is derived.
- Bulk import / reorder endpoints — domains + paths are ordered by the
  `priority` column; reorder is a plain `UpdateInterceptionPath` call.
- Per-adapter JSON-schema validation of `adapterConfig` — free-form JSON
  remains adapter-defined at runtime.

## 4. Architecture

```
Admin UI
  │  /compliance/interception-domains
  ▼
Control Plane (Echo)
  │  /api/admin/interception-domains* (8 endpoints)
  │  /api/admin/traffic-adapters (catalog GET)
  │  ├── IAM gate: admin:ReadSettings / admin:UpdateSettings
  │  └── h.DB.*                                 pgx + raw SQL
  ▼
PostgreSQL
  interception_domain  ─┐
  interception_path ────┘  (ON DELETE CASCADE)

Control Plane on successful write:
  ├─ h.Hub.InvalidateConfig("compliance-proxy", "interception_domains")
  ├─ h.incrementConfigVersion(ctx)
  └─ h.Audit.Log(entity=interceptionDomain|interceptionPath)

Nexus Hub (existing — no change)
  ├─ Shadow push → compliance-proxy WebSocket
  └─ AgentInterceptionDomainsLoader → agent Category B pull
```

## 5. Design decisions

| Decision | Choice | Rationale |
|---|---|---|
| Single route | List + detail at `/compliance/interception-domains`; detail at `:id` | Matches the existing `/compliance/exemptions` pattern; keeps the Compliance sidebar section tight. |
| Paths location | Inline sub-table on the detail page | Paths are tightly coupled to their domain — they cascade on delete, priority ordering is scoped per domain. A separate route would force extra navigation for every rule tweak. |
| Allowlist surface | Note banner only | `domain_allowlist` is derived; surfacing a second list would invite drift. |
| Invalidation fan-out | One push to `compliance-proxy:interception_domains` | CP reducer already cascades to `domain_allowlist` + both caches; agent pulls via the same key from Hub. |
| Delete semantics | DB `ON DELETE CASCADE` from `interception_path` → `interception_domain` | Avoids client-side orchestration; one DELETE statement is the whole operation. |
| IAM actions | `admin:ReadSettings` for GET, `admin:UpdateSettings` for writes | Mirrors `admin_hooks.go` / `admin_policies.go`; no new IAM actions minted. |
| Adapter picker | `GET /api/admin/traffic-adapters` → sorted IDs from `shared/traffic/adapters` | Single source of truth with data-plane registration; UI loads via React Query. |
| Row-shape contract | Extend `InterceptionDomainRow` in place | `json:",omitempty"` for admin-only fields keeps the enabled-only loader shape backward-compatible; nothing in tree serialises this struct to the wire. |

## 6. Implementation — task list

All three commits build and test green independently.

### C1 — Store + handler + OpenAPI + tests (committed)

Store (`packages/control-plane/internal/store/interception_domain.go`):
- Extend `InterceptionDomainRow` + `InterceptionPathRow` with
  `Description`, `Source`, `CreatedAt`, `UpdatedAt`, `CreatedBy`, and
  `DomainID` on the path. All additions use `omitempty` so the
  enabled-only loader output is backward-compatible.
- `ListEnabledInterceptionDomains(ctx)` — signature preserved.
- `ListInterceptionDomains(ctx, params) (*Result, error)` — paginated,
  includes disabled, searches name / host_pattern / adapter_id.
- `GetInterceptionDomain`, `CreateInterceptionDomain`,
  `UpdateInterceptionDomain` (COALESCE partial update),
  `DeleteInterceptionDomain`.
- `GetInterceptionPath`, `CreateInterceptionPath`,
  `UpdateInterceptionPath` (COALESCE partial update),
  `DeleteInterceptionPath`.
- `attachPaths` / `listPathsForDomain` helpers keep the enabled-only
  loader, the list endpoint, and the detail endpoint using the same
  query shape.

Handler (`packages/control-plane/internal/handler/admin_interception_domains.go`):
- 8 endpoints under `/api/admin/interception-domains*`.
- `validateDomainEnums` + `buildPathInputs` reject malformed payloads
  with a 400 before the DB round-trip.
- `invalidateInterceptionDomains` helper fires the single Hub push +
  increments the admin config version.
- Every 2xx write emits an audit entry with
  `EntityType ∈ {interceptionDomain, interceptionPath}`.

OpenAPI (`docs/users/api/openapi/admin/e25-s1-interception-domains.yaml`):
- 8 operations with examples (`api.openai.com` + `/v1/chat/completions`).
- Reuses the Prisma enums.

Tests:
- `store/interception_domain_test.go` — Create / Update / Delete
  round-trip + cascade, gated on `DATABASE_URL` so CI without Postgres
  still passes.
- `handler/admin_interception_domains_test.go` — enum validation, path
  input mapping, route registration, body-validation 400 path.
- `handler/admin_interception_domains_integration_test.go` —
  `DATABASE_URL`-gated create round-trip asserting Hub invalidation +
  audit enqueue (`TestCreateInterceptionDomain_InvalidateHubAndAudit`).
- `handler/admin_traffic_adapters_test.go` — catalog GET matches
  `adapters.BuiltinTrafficAdapterIDs()`.

### C2 — UI (committed)

- `src/api/services/interceptionDomains.ts` — typed client + catalog
  `listTrafficAdaptersCatalog()` for the adapter dropdown.
- `src/pages/compliance/InterceptionDomainsPage.tsx` — DataTable list
  with search + enabled filter + pagination + create modal.
- `src/pages/compliance/InterceptionDomainDetailPage.tsx` — summary
  card + nested paths sub-table + allowlist note banner.
- Shared `InterceptionDomainForm` and `InterceptionPathForm` reused
  from both list (create) and detail (edit + add path + edit path).
- Routes in `shellRouteConfig.tsx`; lazy pages in `lazyPages.tsx`.
- `nav.interceptionDomains` + `pages.interceptionDomains.*` i18n keys
  in all three locales (src + public).
- MSW handlers in `src/test/msw-handlers.ts` plus vitest coverage for
  the list page (load, create, delete, empty, allowlist note) and the
  detail page (summary, inline add path).

### C3 — Docs sync (this commit)

- `docs/developers/architecture/cross-cutting/foundation/service-call-framework.md §4.5` — note
  `interception_domains` is admin-managed via
  `/compliance/interception-domains`; annotate `domain_allowlist` as a
  derived projection.
- `docs/developers/specs/e3/e3-s5-config-sync-remediation.md` P2 — note E25-S1 has
  delivered the `interception_domains` + `domain_allowlist` admin
  surface.
- This file.

## 7. Acceptance criteria

- [x] Admin can create, list, update, and delete a domain via
  `/api/admin/interception-domains*`.
- [x] Admin can add, edit, and delete paths through the same URL
  hierarchy.
- [x] Every successful write fires one
  `InvalidateConfig("compliance-proxy", "interception_domains")` call
  and emits an audit entry.
- [x] Deleting a domain cascades to its paths.
- [x] The existing read-only `ListEnabledInterceptionDomains` still
  returns the same shape (verified by package build — Hub's Cat B
  loader is untouched).
- [x] UI list + detail + nested paths editor renders via MSW and
  passes vitest.
- [x] List and detail views expose a one-click enable/disable control for
  each domain (partial PUT `enabled`), in addition to the full edit form.
- [x] TypeScript typecheck green; all Go package builds green.
- [x] No new top-level dependencies added.

## 8. Rollout + rollback

- Rollout: CP restart picks up the new routes; no data migration
  needed (tables exist from earlier phases).
- Rollback: `git revert` the three commits. Seeded rows survive;
  the admin just loses the CRUD surface.

---
doc: tenancy-architecture
area: service
service: control-plane
tier: 1
---

# Tenancy & Organization Hierarchy Architecture

> **Tier 1 architecture doc.** Read this before touching organization, project, user, or any code path that uses `OrganizationID` / `OrganizationPath` to scope behaviour. IAM evaluation that uses these scopes lives in `iam-identity-architecture.md`. Quota rollup that uses these scopes lives in `quota-architecture.md`.

---

## 1. The tenancy tree

Nexus is organization-aware and supports multiple organisations on a single deployment. The unit of organisational scope is the **Organization**. Organisations form a **tree**: every org has zero or one parent and zero or more children.

```
nexus (root, system org)
├── acme-holdings
│   ├── acme-research
│   │   ├── project-aurora
│   │   └── project-bear
│   └── acme-marketing
│       └── project-cassia
└── widgets-inc
    └── project-default
```

A **Project** is a child of an organization. Projects do not have children. Resources (virtual keys, quotas, routing rules, IAM policies, credentials, hook configs) scope to either an org or a project.

## 2. Materialised ancestor path (`Organization.path`)

Each `Organization` row carries a `path` column — a **slash-delimited string** materialising the root-to-self chain. Schema: `tools/db-migrate/schema.prisma:295-326` (`Organization.path String @unique`, comment: "Materialized path for efficient subtree queries. Root orgs: `/{id}/`. Children: `{parent.path}{id}/`."). Example for `project-cassia`'s parent `acme-marketing` under `acme-holdings` under root `nexus`:

```
path = '/nexus/acme-holdings/acme-marketing/'
```

The path is the only column that materialises ancestry — there is **no separate `ancestor_path` array column**. The slash-delimited string supports subtree queries with a simple `LIKE '/nexus/acme-holdings/%'` index scan rather than a recursive CTE.

When an org moves in the tree:

1. Compute the new `path` from `parent.path + id + '/'`.
2. Update the org row.
3. Update every descendant's `path` in the same transaction (substring-replace the old ancestor prefix with the new one).
4. Emit an org-move event.

This is the only mutation path that touches the materialised path in bulk. Direct manual SQL updates would break the invariant `path == parent.path + id + '/'`.

## 3. Membership

The Control Plane data model **does not have a separate user-membership join table** (no `(user_id, org_id, role)` rows). Each `NexusUser` is bound to a **single Organization** via the `organizationId` FK:

```prisma
model NexusUser {
  ...
  organizationId  String    @default("default")
  organization    Organization @relation(fields: [organizationId], references: [id])
  @@unique([organizationId, email])
}
```

A user has access to exactly one home org. Cross-org access is expressed via IAM policies (attached to the user or to one of their IAM groups) whose `Resource` patterns target other orgs' NRN scopes — see `iam-identity-architecture.md` §8.

When a user is granted attach to the global `NexusSuperAdmin` policy (resource pattern `nrn:nexus:*:*:*/*`), that's the super-admin path. Below that, all cross-org access goes through explicit IAM policy `Resource` patterns rather than implicit ancestor inheritance.

IAM-group membership (the `IamGroupMembership` table) is a separate concept — groups bundle policies and users, but the user is still bound to exactly one home org.

## 4. Policy inheritance

**IAM policies attached to an ancestor org apply to its descendants** when the `Resource` pattern uses hierarchical scope. This is what makes "give the marketing team read access to all marketing projects" expressible without enumerating every child.

NRN resource patterns reflect this (5-segment grammar; see `iam-identity-architecture.md` §2):

- `nrn:nexus:gateway:org-acme-marketing:routing-rule/*` — all routing rules in acme-marketing.
- `nrn:nexus:gateway:org-acme-*:routing-rule/*` — all routing rules in any org whose ID starts with `org-acme-` (use with care; patterns are exact-prefix on each segment).

Scope matching is hierarchical at evaluation time: a pattern with scope `acme` matches both `acme` (exact) and `acme/marketing` (prefix-with-slash). When evaluating access to a routing rule in `project-cassia`, IAM walks the materialised `Organization.path` chain `/nexus/acme-holdings/acme-marketing/` and collects every applicable Allow / Deny along it.

Deny-overrides still applies: an explicit Deny anywhere in the ancestor chain wins.

## 5. Quota rollup

Quotas attach to org or project scope. The quota engine respects the **parent-org cap** invariant: a request that would succeed against the project-level quota still **fails** if it would exceed the parent org's quota.

Example: org `acme-marketing` has a 10 M token / day cap. Project `project-cassia` has a 4 M token / day allocation. If `project-cassia` is at 3 M but `acme-marketing` is already at 9.5 M because another project under acme-marketing used 6.5 M, then the next 1 M-token request still fails — the parent cap binds.

Computation walks the materialised path: for a request scoped to `project-cassia`, the quota engine increments counters for `project-cassia`, `acme-marketing`, `acme-holdings`, and `nexus` in one Redis `Pipeline()` round-trip (`packages/ai-gateway/internal/policy/quota/usage_cache.go:123` `IncrMulti`). Full details in `quota-architecture.md`.

## 6. RequestContext propagation

Every `/v1/*` request carries an evaluated tenant context. The AI Gateway router holds it in `VKContext` (`packages/ai-gateway/internal/routing/core/types.go`):

```go
type VKContext struct {
    ID               string
    Name             string
    OrganizationID   string
    OrganizationPath []string // root → self ancestors, sourced from Organization.path
    ProjectID        string
    SourceApp        string
    AllowedModels    []store.AllowedModelRef
}
```

`OrganizationPath` is the ancestor chain (parent → root) derived from the in-memory `orgParents` parent map at VK-hydration time (`packages/ai-gateway/internal/ingress/proxy/proxy.go:1573` `buildOrgPath`). The source of truth in Postgres is the single-column `Organization.path` materialised string described in §2; the slice form is a runtime convenience built by walking parents.

This context is built by AI Gateway's `vkauth` (virtual key resolution) and threaded through routing, hooks, quota, audit, and metrics. Every downstream record can answer "which tenant?" without re-fetching.

When emitting a `traffic_event` row, the audit pipeline stamps `org_id` and `org_name` columns (schema `tools/db-migrate/schema.prisma:1338-1339`). There is **no `org_ancestor_path` column** on `traffic_event` — analytics queries that need an ancestor scope JOIN through `Organization.path` at query time.

## 7. JIT user provisioning landing zone

When a user federates in via OIDC (SAML planned) for the first time, they need to land in an org. The IdP config row in the database carries a **default org** for JIT-provisioned users; per-claim-to-org mapping (e.g. `department=engineering` → `org-acme-research`) is **not yet implemented** in the IdP config schema — only the default-org binding is honoured today. See `docs/developers/roadmap.md` for the per-claim-mapping queue.

JIT creates the user, places them in the configured default org, and assigns the configured initial IAM-group memberships. Full federation flow in `idp-sso-architecture.md`.

## 8. Cross-tenant invariants

These invariants are binding:

- Virtual keys belong to exactly one project (application VKs) or exactly one user (personal VKs). Org is resolved via either chain — see `vk-org-resolution.md`.
- Provider credentials belong to exactly one org (cross-org credential sharing is by-design forbidden).
- Routing rules belong to one org or one project.
- IAM policies attach to one principal (user or IAM group); cross-org reach is via the `Resource` pattern, not via separate attachments per descendant.
- Quotas attach to one org or one project (and roll up per §5).
- Audit events attribute to one org (the org-at-emit-time, even if the org is later renamed/moved).
- A user's home org is fixed at JIT time; admins can change it via the IAM API but a user always has exactly one home org.

These invariants make the data model auditable: any record can answer "which tenant?" with a single column lookup.

## 9. Operational concerns

- **Org rename** — display name only; org ID is immutable.
- **Org move** — see §2 (atomic `path` update across the org + every descendant).
- **Org delete** — soft delete (`deleted_at`); resources are not cascaded automatically. Hard delete requires an explicit purge runbook.
- **Project move** — analogous to org move; rare in practice.
- **Cross-tenant resource transfer** — out of scope; today requires manual support process.

## 10. Sources

- `tools/db-migrate/schema.prisma:295-326` — `Organization` model + `path` materialised column.
- `tools/db-migrate/schema.prisma:184-220` — `NexusUser.organizationId` single-org binding.
- `tools/db-migrate/schema.prisma:1338-1339` — `traffic_event.org_id` + `org_name`.
- `packages/control-plane/internal/identity/users/handler/organizations.go` — org admin handlers.
- `packages/ai-gateway/internal/routing/core/types.go` — `VKContext.OrganizationID` / `OrganizationPath`.
- `packages/ai-gateway/internal/platform/store/virtualkey.go` — VK→org resolution (`vkSelectSQL`).
- `packages/ai-gateway/internal/policy/quota/chain.go` — hierarchical quota rollup walker.

## 11. Cross-references

- `iam-identity-architecture.md` — policy inheritance + NRN grammar.
- `quota-architecture.md` — quota rollup mechanics.
- `idp-sso-architecture.md` — JIT landing zone.
- `audit-pipeline-architecture.md` — tenant attribution on audit events.
- `vk-org-resolution.md` — the dual application/personal VK join chain.
- `credentials-architecture.md` — provider credentials are org-scoped.

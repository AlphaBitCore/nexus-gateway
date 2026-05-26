# Control Plane Multi Tenancy

*Audience: operators designing their tenant hierarchy and contributors touching org/project/VK code.*

Nexus Gateway is multi-tenant. The unit of tenancy is the Organization. Organizations form a tree, and every resource (virtual key, routing rule, quota, credential, IAM policy) belongs to exactly one org or project. Two independent join chains exist for virtual keys — one for application VKs, one for personal VKs — and both must be covered in any SQL that touches org resolution.

---

## Organization tree

Organizations form a parent-child tree; every org has zero or one parent:

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

A **Project** is a child of an organization. Projects do not have children. Resources scope to either an org or a project.

## Materialised ancestor path

Each `Organization` row carries a `path` column — a slash-delimited string that materialises the root-to-self chain. For `acme-marketing` under `acme-holdings` under root `nexus`:

```
path = '/nexus/acme-holdings/acme-marketing/'
```

This single column supports subtree queries with a `LIKE '/nexus/acme-holdings/%'` index scan, avoiding recursive CTEs. When an org moves in the tree, the org row and every descendant row update atomically — direct manual SQL edits would break the invariant.

## User membership

Each `NexusUser` is bound to a single organization via the `organizationId` FK. A user has access to exactly one home org. Cross-org access is granted through IAM policies (attached to the user or to an IAM group) whose `Resource` patterns target other orgs' NRN scopes — see [Control Plane IAM Model](Control-Plane-IAM-Model). IAM group membership is a separate concept; groups bundle policies and users, but a user still has exactly one home org.

## Policy inheritance

IAM policies attached to an ancestor org apply to its descendants when the `Resource` pattern uses hierarchical scope. This makes "give the security team read access across all child projects" expressible without enumerating every child:

```
nrn:nexus:gateway:org-acme-marketing:routing-rule/*
```

Deny-overrides applies: an explicit Deny anywhere in the ancestor chain wins.

## Quota rollup

Quotas attach to org or project scope. The quota engine enforces the parent-org cap invariant: a request that would succeed against the project quota still fails if it would exceed the parent org's quota. The engine walks the materialised `Organization.path` and increments counters for the project, its parent orgs, and the root in one Lua-scripted Redis multi-key operation.

## Virtual key org resolution — two join chains

A `VirtualKey` row resolves to its organization through two independent chains:

| VK type | Chain | Joining columns |
|---|---|---|
| `application` | `VirtualKey.projectId` → `Project` → `Organization` | Primary chain |
| `personal` | `VirtualKey.ownerId` → `NexusUser` → `Organization` | Fallback chain |

The `vkSelectSQL` constant in `packages/ai-gateway/internal/platform/store/virtualkey.go` JOINs both chains and `COALESCE`s them, preferring the application chain when present. The columns are mutually exclusive (a VK is either app-owned via Project or person-owned via Owner — never both).

Any SQL that touches org-derived columns for virtual keys must cover both chains. A single-chain JOIN silently drops one population. The canonical SQL pattern:

```sql
SELECT
  COALESCE(p."organizationId", u."organizationId") AS organization_id,
  COALESCE(org.name,           u_org.name)         AS organization_name
FROM "VirtualKey" vk
LEFT JOIN "Project"      p     ON vk."projectId" = p.id
LEFT JOIN "Organization" org   ON p."organizationId" = org.id
LEFT JOIN "NexusUser"    u     ON vk."ownerId" = u.id
LEFT JOIN "Organization" u_org ON u."organizationId" = u_org.id
```

Tests in `packages/ai-gateway/internal/platform/store/virtualkey_sql_test.go` pin the COALESCE and LEFT JOIN invariants to prevent accidental reversion.

## Tenant context propagation

Every `/v1/*` request carries an evaluated tenant context in `VKContext` (resolved at VK hydration time). This context is threaded through routing, hooks, quota, audit, and metrics — every downstream record can answer "which tenant?" without re-fetching. The `traffic_event` table stamps `org_id` and `org_name` at emit time. Analytics queries that need ancestor scope JOIN through `Organization.path` at query time.

## Operational notes

- **Org rename** — display name only; org ID is immutable.
- **Org delete** — soft delete (`deleted_at`); resources are not cascaded automatically.
- **JIT provisioning landing zone** — the IdP config row carries a default org for JIT users; per-claim-to-org mapping is not yet implemented.
- **Provider credentials** — are org-scoped; cross-org credential sharing is by-design forbidden.

---

## Canonical docs

- [`tenancy-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/control-plane/tenancy-architecture.md) — materialised path, membership model, policy inheritance, quota rollup, cross-tenant invariants
- [`vk-org-resolution.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/control-plane/vk-org-resolution.md) — dual join chain for application and personal VKs, SQL pattern, tests

**Adjacent wiki pages**: [Control Plane IAM Model](Control-Plane-IAM-Model) · [Control Plane SSO Okta AzureAD](Control-Plane-SSO-Okta-AzureAD) · [AI Gateway Virtual Keys Quotas](AI-Gateway-Virtual-Keys-Quotas) · [Control Plane Overview](Control-Plane-Overview)

---
doc: iam-identity-architecture
area: service
service: control-plane
tier: 1
---

# IAM & Identity Architecture

> **Tier 1 architecture doc.** Read this before touching any of: `packages/control-plane/internal/identity/iam/**`, `packages/shared/identity/iam/**`, any `iamMW(...)` wrapper, any `allowedActions` value in `packages/control-plane-ui/src/routes/shellRouteConfig.tsx`, the IAM seed block in `tools/db-migrate/seed/seed.ts` / `tools/db-migrate/seed/data/seed-baseline.sql`, or the canonical action catalog in `packages/shared/identity/iam/catalog.go` + `catalog_data.go`. External IdP federation lives in `idp-sso-architecture.md`. Organization hierarchy details live in `tenancy-architecture.md`.

---

## 1. Mental model

Nexus IAM is **AWS-IAM-shaped**: identities (User, Service Account), policies (Effect / Action / Resource / Condition), and resources addressed by NRN. Evaluation is **deny-overrides**: any explicit Deny wins over any Allow.

Three independent IAM dimensions:

1. **Action taxonomy (E43)** — which verbs exist (`admin:provider.read`, `admin:routing-rule.create`, …).
2. **Resource NRN** — which objects are addressable (`nrn:nexus:gateway:*:provider/openai`, …).
3. **Identity & policy attach** — which principals get which policies (direct attach + group membership; "role" in the UI is shorthand for an IAM group, see §6).

If a request is rejected with 403, one of those three is wrong. The canonical recovery pattern: confirm the action string matches between `shellRouteConfig.tsx` `allowedActions` and the handler's `iamMW(...)`; confirm the request-side NRN is derived via `iam.BuildRequestNRNForAction(action)` (never hand-built); confirm the principal has a matching policy attachment.

## 2. NRN — Nexus Resource Names

NRN format (binding, 5 segments with `/` between resourceType and id):

```
nrn:nexus:<service>:<scope>:<resourceType>/<resourceID>
```

- `<service>` — one of `gateway | iam | compliance | agent | platform`. Each product domain owns a slice of the resource catalog; `service` is the second segment of the NRN. (See `packages/shared/identity/iam/catalog.go` `Service` constants for the full mapping.)
- `<scope>` — `*` for global, `<org-id>` for org-scoped, `<org-id>/<project-id>` for project-scoped. Scope matching is hierarchical: `pattern=acme` matches both `acme` and `acme/marketing`.
- `<resourceType>` — kebab-case resource identifier from the catalog (e.g. `provider`, `routing-rule`, `virtual-key`, `iam-policy`, `iam-group`, `traffic-log`).
- `<resourceID>` — concrete identifier or `*` wildcard for policy patterns.

Examples:

- `nrn:nexus:gateway:*:provider/openai`
- `nrn:nexus:gateway:*:routing-rule/*`
- `nrn:nexus:iam:*:user/u-123`
- `nrn:nexus:iam:*:iam-policy/*`
- `nrn:nexus:compliance:*:hook/*`

Canonical builders live in `packages/shared/identity/iam/catalog.go` (`ResourceDef.NRN(scope, id)`) and `packages/control-plane/internal/identity/iam/nrn.go` (`BuildNRN`, `BuildRequestNRNForAction`, `MatchNRN`). **Always** use `iam.BuildRequestNRNForAction(action)` to derive the request-side NRN for an action — never hardcode the resource-type string in `iamMW(...)`. The canonical NRN builder (`iam.BuildRequestNRNForAction`) is load-bearing — calling it correctly is the only way to derive `resourceType` consistently.

## 3. Action taxonomy (E43)

Action format (binding, kebab-dot):

```
admin:<resource>.<verb>
```

The first segment names the API namespace (today: only `admin`). The second segment is the catalog resource name (kebab-case). The third segment is the verb (lowercase, kebab-case).

Examples:

- `admin:provider.read`
- `admin:provider.create`
- `admin:virtual-key.create`
- `admin:virtual-key.approve`
- `admin:routing-rule.update`
- `admin:routing-rule.simulate`
- `admin:traffic-log.read`
- `admin:audit-log.export`
- `admin:kill-switch.toggle`
- `admin:node.force-resync`

Verbs are a closed set defined in `packages/shared/identity/iam/catalog.go` (`Verb` constants). The CRUD verbs are `create | read | update | delete`. Per-resource verbs include `approve | reject | revoke | renew` (virtual-key lifecycle), `toggle` (kill-switch), `export` (audit-log), `simulate` (routing-rule what-if), `force-resync | write-override` (node), `write` (settings / observability deep writes), `acknowledge` (alert), `emergency-enable` (passthrough), `probe | rotate` (credential), `import` (rule-pack), `fulfill` (DSAR), `enroll` (device-enrollment), and `manage` (reserved for coarse-grained operations — avoid in new resources).

The full action catalog lives in `packages/shared/identity/iam/catalog.go` (carries the `Verb`, `Service`, `ResourceDef`, `Catalog` types plus the `Action()` and `NRN()` builders) and `packages/shared/identity/iam/catalog_data.go` (data rows). New actions go there. Granting an action to managed-policy seed rows happens **only** in the Prisma seed — see §7.

## 4. Policy document shape

```json
{
  "Version": "2026-05-12",
  "Statement": [
    {
      "Sid": "providers-read",
      "Effect": "Allow",
      "Action": ["admin:provider.read"],
      "Resource": ["nrn:nexus:gateway:*:provider/*"],
      "Condition": { "StringEquals": { "request.org": "org-acme" } }
    }
  ]
}
```

Evaluation:

1. Collect all applicable statements (from direct attach + group attach).
2. Match Action ∩ Resource ∩ Condition against the request.
3. If any matched statement has Effect `Deny`, request is **denied**.
4. Otherwise, if any matched statement has Effect `Allow`, request is **allowed**.
5. Otherwise, request is **denied** (default deny).

## 5. `iamMW(...)` middleware

Every admin route is wrapped:

```go
g.GET("/iam/policies", h.ListIAMPolicies, iamMW(iam.ResourceIamPolicy.Action(iam.VerbRead)))
```

The middleware:

1. Extracts the bearer token, resolves the principal (User / ServiceAccount).
2. Builds the request NRN: `nrn := iam.BuildRequestNRNForAction(action)`. **Critical:** the resourceType is derived from the action, not the URL.
3. Loads applicable policies (via the two-tier policy cache — in-process L1 then Redis L2 — cold path hits Postgres).
4. Evaluates per §4.
5. On allow, calls the handler. On deny, returns 403 with the action that failed.

The policy cache is **two-tier** (`packages/control-plane/internal/identity/iam/cache.go:12-17`): an in-process L1 `sync.RWMutex`-guarded map with a 10-second TTL (`defaultL1TTL = 10*time.Second`) on top of a Redis L2 with a 60-second TTL (`defaultL2TTL = 60*time.Second`); the Redis key prefix is `nexus:iam:policies:`. Invalidation on policy change is broadcast via the Hub WS change-signal pipeline — the Control Plane subscribes through `packages/shared/transport/thingclient/` (`OnConfigChanged` callback) and drops the affected cache keys at both tiers (NOT Redis pub/sub — Redis is cache-only).

## 6. UI ↔ backend symmetry (binding)

`packages/control-plane-ui/src/routes/shellRouteConfig.tsx` declares `allowedActions: ['admin:provider.read']` for each route. The same action MUST be checked by the backend handler's `iamMW(action)`. If the UI says one action and the backend checks another, users see the menu item and the link but get a silent 403 on click.

The Control Plane data model uses "IAM group" as the principal-grouping primitive (resource `iam-group`; managed policies attach to groups; users belong to groups via `IamGroupMembership`). The CP-UI sidebar labels this surface "Roles" for user friendliness — **role ≡ IAM group** at the data layer. Action strings reflect the data model: `admin:iam-group.read`, NOT `admin:role.read` (no such action exists).

When you add / rename / move a route:

1. Update `shellRouteConfig.tsx` `allowedActions`.
2. Update the corresponding `iamMW(...)` in the handler.
3. Sweep `packages/control-plane-ui/src/components/ui/Sidebar/Sidebar.tsx` icon mapping (dead `case` arms accumulate otherwise).
4. Run a positive test (super-admin can reach the route) AND a negative test (a role without the action gets 403).

This is the "IAM impact review" rule in CLAUDE.md; the 5-step checklist there is binding.

## 7. Managed policies + JIT

A set of well-known managed policies ships with every Nexus install, seeded by Prisma into the `IamPolicy` table (`type='managed'`). The canonical list:

| Policy | Grants |
|---|---|
| `NexusSuperAdmin` | `*` on every resource (`nrn:nexus:*:*:*/*`). Bootstrap super-admin. |
| `NexusAdminFullAccess` | `admin:*` on every resource. Platform administrator. |
| `NexusProviderAdminAccess` | AI Gateway provider / model / credential / VK / routing / quota / analytics + hook + rule-pack ownership + node ops on gateway. |
| `NexusViewerAccess` | Read-only across the platform (`admin:*.read` + `admin:routing-rule.simulate`). |
| `NexusSecurityAdminAccess` | Compliance & security: hook + rule-pack admin, exemption review, DSAR, kill-switch, AI-guard, alert admin, audit-log read+export, forensic reads. |

These policies are seeded from `tools/db-migrate/seed/seed.ts` (with the canonical contents materialised in `tools/db-migrate/seed/data/seed-baseline.sql`). **The Prisma seed is the canonical source for managed policies.** The Go-side file `packages/control-plane/internal/identity/iam/managed.go` was trimmed in E43 P3 to only the minimum needed by unit tests — today it carries `NexusSuperAdmin` and `NexusViewer` minimal fixtures plus the `NexusRegionalDeviceAdmin` worked example (E52-S3). Adding a new resource action to a managed policy is a **one-edit change** in the seed file; `managed.go` is **not** a parallel source.

**JIT user provisioning** — on first successful federation (OIDC; SAML planned), a Nexus user is created and assigned an initial set of group memberships based on IdP assertion claims. Full federation flow in `idp-sso-architecture.md`.

## 8. Organization + project scoping

Resources scope to organisations and projects via the NRN scope segment. Quotas, virtual keys, routing rules, policies — all carry an org or project scope. Policies can target `nrn:nexus:gateway:org-acme:*` to grant within one org, or `nrn:nexus:gateway:org-*:*` for cross-org grants. Hierarchical scope matching means `scope=org-acme` matches both `org-acme` and `org-acme/proj-aurora`.

Full hierarchy semantics (materialised path, parent-quota constraints, policy inheritance) live in `tenancy-architecture.md`.

## 9. IAM impact review (binding rule)

Whenever a change adds, moves, renames, or removes an **admin API endpoint, sidebar nav item, or route path** in `shellRouteConfig.tsx` or any `packages/control-plane/internal/**/handler/*` admin route registration, the PR MUST also:

1. Confirm the UI `allowedActions` and the handler `iamMW(...)` reference the same canonical `admin:<resource>.<verb>` action.
2. Decide whether the surface needs its own resource in `catalog_data.go` (carve out when granting it shouldn't imply granting unrelated settings) or can reuse an existing one (e.g., `settings`, `observability`).
3. If a new resource is added, update the **Prisma seed** so super-admin / viewer policies still grant access; update `managed.go` only if a test fixture needs the new action (it almost never does — keep the Go fixtures minimal).
4. Sweep `Sidebar.tsx` icon mappings / breadcrumb helpers so dead case arms don't accumulate.
5. Record the IAM decisions in the plan / commit message (e.g., "kept on `admin:settings.read`", "carved out as `prompt-cache`").

This rule is binding in CLAUDE.md. Skipping it requires explicit user approval in chat.

## 10. Caching, TTL, and invalidation

- **Per-principal policy cache** — two-tier (`packages/control-plane/internal/identity/iam/cache.go:12-17`): in-process L1 (`sync.RWMutex`-guarded map, 10-second TTL) backed by Redis L2 (60-second TTL); Redis key prefix `nexus:iam:policies:`.
- **Action catalog & managed-policy fixtures** — compiled into the binary (`packages/shared/identity/iam/catalog_data.go`, `packages/control-plane/internal/identity/iam/managed.go`); not cached at runtime because they do not change without a redeploy. The authoritative managed-policy rows in `IamPolicy` are reloaded via the same policy cache as user-attached policies.

Invalidation on policy change uses the Hub WS change-signal path delivered through `packages/shared/transport/thingclient/` `OnConfigChanged` callbacks (cross-ref `thing-config-sync-architecture.md`), NOT Redis pub/sub.

## 11. Sources

- `packages/control-plane/internal/identity/iam/` — runtime middleware, NRN builder, policy evaluator.
- `packages/shared/identity/iam/` — NRN builders, action catalog, policy types.
- `packages/shared/identity/iam/catalog.go` + `catalog_data.go` — canonical resource catalog (the `Catalog`, `ResourceDef`, `Verb`, `Service` types).
- `packages/control-plane/internal/identity/iam/managed.go` — minimal Go test fixtures only (NexusSuperAdmin + NexusViewer + NexusRegionalDeviceAdmin).
- `tools/db-migrate/seed/seed.ts` + `tools/db-migrate/seed/data/seed-baseline.sql` — canonical managed-policy seed.
- `packages/control-plane-ui/src/routes/shellRouteConfig.tsx` — UI `allowedActions`.
- `packages/control-plane-ui/src/components/ui/Sidebar/Sidebar.tsx` — sidebar icon mapping.

## 12. Cross-references

- `idp-sso-architecture.md` — external IdP federation, JIT user provisioning, OAuth+PKCE admin auth.
- `tenancy-architecture.md` — organisation hierarchy, materialised path, policy inheritance, quota rollup.
- `audit-pipeline-architecture.md` — every IAM denial + sensitive admin action emits an audit event.
- `service-call-framework.md` — CP ↔ Hub HTTP contracts the IAM-protected handlers proxy through.

# Control Plane IAM Model

*Audience: security reviewers and contributors adding admin endpoints or sidebar routes.*

The Control Plane IAM model is AWS-IAM-shaped: identities (users, service accounts), policies (Effect / Action / Resource / Condition), and resources addressed by Nexus Resource Names (NRNs). Evaluation is deny-overrides — any explicit Deny wins over any Allow. Every admin endpoint is gated by an `iamMW(action)` middleware that checks the caller's policies before any business logic runs.

---

## Nexus Resource Names (NRNs)

NRNs are the addressing scheme for all IAM-protected resources. Format (5 segments, binding):

```
nrn:nexus:<service>:<scope>:<resourceType>/<resourceID>
```

- `<service>` — one of `gateway | iam | compliance | agent | platform`.
- `<scope>` — `*` for global; `<org-id>` for org-scoped; `<org-id>/<project-id>` for project-scoped. Scope matching is hierarchical: a pattern with scope `org-acme` matches both `org-acme` and `org-acme/marketing`.
- `<resourceType>` — kebab-case identifier from the catalog (e.g. `provider`, `routing-rule`, `virtual-key`, `iam-policy`, `traffic-log`).
- `<resourceID>` — concrete ID or `*` wildcard for policy patterns.

Examples:

```
nrn:nexus:gateway:*:provider/openai
nrn:nexus:gateway:*:routing-rule/*
nrn:nexus:iam:*:user/u-123
nrn:nexus:compliance:*:hook/*
```

NRN builders live in `packages/shared/identity/iam/catalog.go` (`ResourceDef.NRN(scope, id)`) and `packages/control-plane/internal/identity/iam/nrn.go` (`BuildRequestNRNForAction`). Always use `iam.BuildRequestNRNForAction(action)` to derive the request-side NRN — never hardcode the resource-type string in `iamMW(...)`.

## Action taxonomy

Action format (binding, kebab-dot):

```
admin:<resource>.<verb>
```

The resource name is the catalog kebab-case identifier. Verbs are a closed set: `create | read | update | delete` for CRUD, plus per-resource verbs including `approve | reject | revoke | renew | toggle | export | simulate | force-resync | write-override | write | acknowledge | emergency-enable | probe | rotate | import | fulfill | enroll | manage`.

Selected examples:

| Action | Meaning |
|---|---|
| `admin:provider.read` | View provider list |
| `admin:virtual-key.create` | Create a virtual key |
| `admin:routing-rule.simulate` | Run a routing rule what-if |
| `admin:traffic-log.read` | Read traffic events |
| `admin:audit-log.export` | Export the admin audit log |
| `admin:kill-switch.toggle` | Toggle the kill switch |
| `admin:passthrough.emergency-enable` | Activate emergency passthrough |
| `admin:node.force-resync` | Force config resync on a node |

The full action catalog lives in `packages/shared/identity/iam/catalog_data.go`. New actions are added there.

## Policy document shape

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

Evaluation order:

1. Collect all applicable statements (from direct attach and IAM group attach).
2. Match Action ∩ Resource ∩ Condition against the request.
3. Any matched `Deny` → request denied.
4. Any matched `Allow` and no Deny → request allowed.
5. No match → request denied (default deny).

## iamMW middleware

Every admin route is wrapped by `iamMW`:

```go
g.GET("/iam/policies", h.ListIAMPolicies, iamMW(iam.ResourceIamPolicy.Action(iam.VerbRead)))
```

The middleware sequence:

1. Extract bearer token; resolve the principal.
2. Build the request NRN via `iam.BuildRequestNRNForAction(action)`.
3. Load applicable policies (via Redis cache; cold path hits Postgres).
4. Evaluate per the rules above.
5. On allow: call the handler. On deny: return 403 with the failing action.

The IAM policy cache key is `iam:policy:<principal>`. Invalidation uses the Hub WebSocket change-signal path via `thingclient.OnConfigChanged` — not Redis pub/sub.

## UI ↔ backend symmetry (binding)

`packages/control-plane-ui/src/routes/shellRouteConfig.tsx` declares `allowedActions` for each route. The backend `iamMW(action)` must check the same action string. Drift produces silent 403s.

The sidebar labels the IAM group surface "Roles" for user friendliness. At the data layer, the primitive is `iam-group`, so action strings are `admin:iam-group.read`, not `admin:role.read`.

## Managed policies

Five managed policies ship with every Nexus install (seeded from `tools/db-migrate/seed/data/seed-baseline.sql`):

| Policy | Grants |
|---|---|
| `NexusSuperAdmin` | Wildcard on every resource |
| `NexusAdminFullAccess` | `admin:*` everywhere |
| `NexusProviderAdminAccess` | AI Gateway operator scope |
| `NexusViewerAccess` | Read-only across the platform + routing-rule simulate |
| `NexusSecurityAdminAccess` | Compliance, security, hooks, kill switch, audit export |

The Prisma seed is the canonical source for managed policies. The Go fixture file `packages/control-plane/internal/identity/iam/managed.go` carries only the minimum needed by unit tests.

## IAM impact review (binding)

Any PR that adds, moves, renames, or removes an admin API endpoint, sidebar nav item, or route path must:

1. Confirm the UI `allowedActions` and the handler `iamMW(...)` reference the same canonical action.
2. Decide whether the surface needs its own resource in `catalog_data.go` or can reuse an existing one.
3. If a new resource is added, update the Prisma seed so super-admin and viewer policies still grant access.
4. Sweep `Sidebar.tsx` icon mappings to remove any dead `case` arms.
5. Record the IAM decision in the plan or commit message (e.g., "kept on `admin:settings.read`").

This rule is binding in CLAUDE.md. The canonical NRN-builder for `iamMW` is `iam.BuildRequestNRNForAction` — always use it, never hardcode the resource-type string.

---

## Canonical docs

- [`iam-identity-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/control-plane/iam-identity-architecture.md) — NRN grammar, action catalog, policy evaluation, iamMW, caching
- [`iam.md` (cp-ui features)](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/features/cp-ui/iam.md) — IAM section pages and managed-policy catalog
- [`.cursor/rules/iam-impact-review.mdc`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/.cursor/rules/iam-impact-review.mdc) — the 5-step IAM impact review checklist

**Adjacent wiki pages**: [Control Plane Overview](Control-Plane-Overview) · [Control Plane Authentication](Control-Plane-Authentication) · [Control Plane Multi Tenancy](Control-Plane-Multi-Tenancy) · [Recipe Adding An IAM Action](Recipe-Adding-An-IAM-Action) · [Trust Boundaries](Trust-Boundaries)

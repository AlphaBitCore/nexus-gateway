# Recipe Adding An IAM Action

*Audience: contributors adding a new admin API verb or resource to the IAM action catalog.*

Every admin API endpoint in Nexus Gateway is protected by an IAM action — a `admin:<resource>.<verb>` string that the middleware checks before allowing the request. Adding a new action means registering it in the catalog, wiring it into both the backend handler and the UI route config, updating the managed policy seed, and verifying the positive/negative IAM tests pass. The `iam-impact-review` skill (`Skill('iam-impact-review')`) runs the full 5-step audit automatically. The canonical reference is [`iam-identity-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/control-plane/iam-identity-architecture.md).

---

## IAM action format

```
admin:<resource>.<verb>
```

- `admin` — the API namespace (only `admin` today).
- `<resource>` — kebab-case resource name from the catalog (e.g. `provider`, `routing-rule`, `virtual-key`, `kill-switch`, `audit-log`).
- `<verb>` — a verb from the closed set in `packages/shared/identity/iam/catalog.go`: `create | read | update | delete | approve | reject | revoke | renew | toggle | export | simulate | force-resync | write | acknowledge | emergency-enable | probe | rotate | import | fulfill | enroll`.

Examples: `admin:provider.read`, `admin:kill-switch.toggle`, `admin:audit-log.export`, `admin:routing-rule.simulate`.

---

## Step 1 — Register the resource and action in the catalog

Open `packages/shared/identity/iam/catalog_data.go`. If the resource already exists, skip to adding the verb. If it is new, add a `ResourceDef` entry:

```go
var ResourceYourResource = ResourceDef{
    Service:      ServiceGateway,          // or ServiceIAM, ServiceCompliance, etc.
    ResourceType: "your-resource",
    Verbs: []Verb{VerbRead, VerbCreate, VerbUpdate, VerbDelete},
    // Add any extra verbs the resource needs.
}
```

Then add the resource to the `Catalog` slice in the same file. The catalog drives NRN building — `iam.BuildRequestNRNForAction(action)` derives the `resourceType` from the action via this catalog. Using `BuildRequestNRNForAction` everywhere (never hardcoding the resource type) is binding — the 2026-05-13 NRN-builder incident was caused by a hardcoded resource type string in `iamMW`.

## Step 2 — Wire iamMW into the handler

In the relevant handler file under `packages/control-plane/internal/<domain>/handler/`, wrap the route with `iamMW`:

```go
g.GET("/your-resource", h.ListYourResource,
    iamMW(iam.ResourceYourResource.Action(iam.VerbRead)))

g.POST("/your-resource", h.CreateYourResource,
    iamMW(iam.ResourceYourResource.Action(iam.VerbCreate)))
```

Use the catalog `Action()` builder — never pass a raw string literal to `iamMW`. The builder generates `admin:your-resource.read` from `ResourceYourResource.Action(VerbRead)`, keeping the action name in sync with the catalog.

## Step 3 — Wire allowedActions in the UI route config

In `packages/control-plane-ui/src/routes/shellRouteConfig.tsx`, set `allowedActions` to the same action string:

```tsx
{
  path: 'your-section/your-resource',
  LazyPage: L.Lazy<YourResourcePage>,
  allowedActions: ['admin:your-resource.read'],
  nav: {
    sectionKey: 'your-section',
    labelKey: 'yourResource.nav.label',
    to: '/your-section/your-resource',
    allowedActions: ['admin:your-resource.read'],
    order: <n>,
  },
},
```

The UI `allowedActions` controls which users see the menu item and link. If UI and backend carry different action strings, users see the link but receive a silent 403 on click.

## Step 4 — Update managed policies in the seed

In `tools/db-migrate/seed/seed.ts`, update the policy documents for the managed policies that should cover the new action:

- `NexusSuperAdmin` — `*` wildcard, no change needed.
- `NexusAdminFullAccess` — add `admin:your-resource.*` or individual verbs.
- `NexusViewerAccess` — add `admin:your-resource.read` so viewers can see the resource.
- `NexusSecurityAdminAccess`, `NexusProviderAdminAccess` — add if the resource belongs to their domain.

Missing a managed policy update means users assigned those policies silently lose access to the new resource. `NexusViewerAccess` is the most commonly forgotten.

If the new resource is a carve-out from an existing one (previously users accessed it via `admin:settings.read`), add the new action and remove the implicit coverage from the old resource.

## Step 5 — Run the IAM impact review

The `iam-impact-review` skill runs all five verification steps and emits a one-paragraph audit summary for the PR description:

```
Skill('iam-impact-review')
```

The five steps it checks:

1. UI `allowedActions` exactly matches handler `iamMW(action)`.
2. Resource carve-out decision is documented.
3. Managed policy seed updated for all affected roles.
4. Sidebar icon mapping and breadcrumb helpers swept for stale `case` arms.
5. Positive test (super-admin reaches the route) and negative test (viewer without the action gets 403).

If either the positive or negative test fails, stop and fix before merging.

## Step 6 — Verify using cp_curl

```bash
# Positive: super-admin can reach the new endpoint
cp_login
cp_curl /api/admin/your-resource

# Negative: viewer-level role gets 403 (diana@nexus.ai / viewer123 in local seed)
# Switch user and confirm the endpoint returns 403.

# Confirm action catalog includes the new action:
grep -n 'your-resource' packages/shared/identity/iam/catalog_data.go
grep -n 'your-resource' packages/control-plane-ui/src/routes/shellRouteConfig.tsx
grep -n 'your-resource' tools/db-migrate/seed/seed.ts
```

---

## Resource carve-out decision guide

Whether to carve out a new resource or reuse an existing one:

- **Carve out** when granting the resource shouldn't imply granting something unrelated. Example: `prompt-cache` was carved out from `settings` so a cache-admin role doesn't implicitly gain all settings access.
- **Reuse** when the resource is a small surface that naturally bundles with an existing one. Example: a new "AI Gateway statistics" surface can reuse `admin:observability.read`.

Document the decision in the PR description. This becomes load-bearing context for future maintainers deciding whether to split the resource further.

---

## What links break if you skip this

- **Skipping catalog registration**: `iam.BuildRequestNRNForAction(action)` cannot derive the NRN for the new action, causing 500 errors instead of 403s when the policy evaluator fails to build the request NRN.
- **Skipping managed policy seed update**: users assigned `NexusViewerAccess` see no menu item for the new resource; users assigned `NexusAdminFullAccess` may also lose access if the wildcard did not previously cover the new resource type.
- **Mismatching allowedActions and iamMW action strings**: the UI renders the menu item (the action passes the client-side check), but the backend middleware checks a different action and returns 403. The user sees the link but cannot use it — with no indication why.
- **Skipping the negative test**: a broad policy grant that accidentally covers the new action goes undetected until a role-boundary security review.

---

## Canonical docs

- [`iam-identity-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/control-plane/iam-identity-architecture.md) — NRN format, action taxonomy, iamMW middleware, managed policies, UI↔backend symmetry binding
- [`catalog_data.go`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/packages/shared/identity/iam/catalog_data.go) — canonical resource + action definitions
- [`seed.ts`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/tools/db-migrate/seed/seed.ts) — managed policy canonical source

**Adjacent wiki pages**: [Control Plane IAM Model](Control-Plane-IAM-Model) · [Recipe Adding A CP UI Section](Recipe-Adding-A-CP-UI-Section) · [Security Audit Forensics](Security-Audit-Forensics) · [Recipe Index](Recipe-Index)

# IAM section — CP-UI feature doc

> Audience: admins controlling who can do what. This section manages organisations, users, IAM groups (a.k.a. roles), policies, and external IdPs.

## Pages in this section

Action strings follow the canonical `admin:<resource>.<verb>` format (cross-ref `iam-identity-architecture.md` §3). The CP-UI sidebar labels the IAM-group surface as "Roles" for user friendliness — in the data model, an IAM group is the principal-grouping primitive that bundles policies and users (`iam-group` resource), so the action string is `admin:iam-group.read`, not `admin:role.read`. There is no `role` resource in the catalog.

| Page | Path | IAM action | Purpose |
|---|---|---|---|
| Organizations | `/iam/organizations` | `admin:organization.read` | Tenant tree: create, rename, move, soft-delete |
| Projects | `/iam/projects` | `admin:project.read` | Project CRUD within orgs |
| Users | `/iam/users` | `admin:user.read` | Local users + IdP-federated users; suspend |
| Roles (IAM groups) | `/iam/roles` | `admin:iam-group.read` | Groups bundle policies + users; surface labelled "Roles" in the sidebar |
| Policies | `/iam/policies` | `admin:iam-policy.read` | AWS-IAM-shape policy documents (Effect/Action/Resource/Condition) |
| Simulator | `/iam/simulator` | `admin:iam-policy.read` | Test "would this principal be allowed this action on this resource?" |
| Identity Providers | `/iam/identity-providers` | `admin:identity-provider.read` | External IdP configurations (Okta, Azure AD, OIDC; SAML planned) |

Route table source of truth: `packages/control-plane-ui/src/routes/shellRouteConfig.tsx:374-428`. The Identity Providers row above corresponds to the route declared at line 421-426 with `allowedActions: ['admin:identity-provider.read']`.

## Common workflows

- **Onboard a new org** — Organizations → new → set parent. Materialised path computed automatically (cross-ref `tenancy-architecture.md` §2).
- **Move an org under a different parent** — Organizations → select → "Move" → choose new parent. `Organization.path` of the org AND all descendants update atomically.
- **Federate logins from Okta** — Identity Providers → new OIDC → fill issuer + client id + claim mapping → save → test. Group membership for JIT-provisioned users comes from `IdpGroupMapping` rows (external IdP group → local `IamGroup`); unmapped externals are silently skipped. The OIDC handler at `packages/control-plane/internal/identity/authserver/login/oidc.go:195` calls `FederatedStore.JITProvisionUser` (`packages/control-plane/internal/identity/authserver/store/federated_store.go:146`), which creates the user with `canAccessControlPlane=false` and stamps memberships in a single transaction. There is no "default role" or "default org" field on the IdP row — provision the right `IdpGroupMapping` entries before flipping the IdP live.
- **Create a role for security/compliance officers** — Roles → new → attach the seeded `NexusSecurityAdminAccess` policy + any custom policy → save. Add users to the group.
- **Verify access** — Simulator → choose principal + action + resource NRN → "Evaluate" → see which statements matched / which Deny won.

## Seeded managed policies

The Prisma seed loads five managed policies; pick from these when assigning out-of-the-box access:

- `NexusSuperAdmin` — wildcard on every resource.
- `NexusAdminFullAccess` — `admin:*` everywhere.
- `NexusProviderAdminAccess` — AI Gateway operator (providers / models / VKs / routing / quotas / analytics + hook + rule-pack ownership).
- `NexusViewerAccess` — read-only auditor across the platform (`admin:*.read` + routing-rule simulate).
- `NexusSecurityAdminAccess` — compliance + security: hooks, rule packs, exemptions, DSAR, kill switch, AI guard, alerts, audit-log + revocation read+export.

See `tools/db-migrate/seed/data/seed-baseline.sql` for the canonical contents.

## Key API endpoints

```
/api/admin/organizations                       [GET/POST/PUT/DELETE]; POST /:id/move
/api/admin/projects                            [GET/POST/PUT/DELETE]
/api/admin/users                               [GET/POST/PUT/DELETE]; POST /:id/suspend
/api/admin/iam/policies                        [GET/POST/PUT/DELETE]; GET /:id/attachments
/api/admin/iam/groups                          [GET/POST/PUT/DELETE]
/api/admin/iam/groups/:id/members              [GET/POST]; DELETE /:membershipId
/api/admin/iam/groups/:id/policies             [POST]; DELETE /:attachmentId
/api/admin/iam/principals/:type/:id/policies   [GET/POST]; DELETE /:attachmentId
/api/admin/iam/simulate                        [POST]
/api/admin/iam/action-catalog                  [GET]
/api/admin/identity-providers                  [GET/POST/PUT/DELETE]; POST /:id/test
```

There is no `/api/admin/roles` route — the "Roles" UI surface targets `/api/admin/iam/groups`. Endpoint mount authority lives in `packages/control-plane/internal/identity/users/handler/iam.go` (`g.GET("/iam/policies", ...)`, etc.).

## Failure modes & gotchas

- **NRN-builder drift** — the canonical builder lives in `packages/shared/identity/iam/`; never hardcode NRN strings. Memory `project_iam_resource_nrn_bug` records a 2026-05-13 silent-403 caused by a builder mismatch.
- **Action catalog drift** — adding a new admin endpoint without updating the action catalog produces silent 403s (binding IAM-impact-review rule in CLAUDE.md).
- **Move-org cascades** — moving a non-trivial org updates many rows; UI surfaces "estimated rows affected" before commit.
- **JIT provisioning landing zone** — there is no IdP-level "default org / role" knob. A JIT user is created with `canAccessControlPlane=false` and **only** the IAM-group memberships derived from `IdpGroupMapping` for the external groups in the JWT `groups` claim (`store/federated_store.go:146-218`). If no mappings exist, the user lands with zero memberships and zero access — verify your mappings against the IdP claim shape via the Identity Providers test action before flipping live, and verify the resulting effective access in the Simulator.
- **Policy `Deny` overrides `Allow`** — common confusion: one ancestor org's broad Allow doesn't beat a child-level explicit Deny. Use Simulator to verify.

## Architecture references

- `docs/developers/architecture/services/control-plane/iam-identity-architecture.md` — NRN, action catalog, evaluation
- `docs/developers/architecture/services/control-plane/idp-sso-architecture.md` — external IdP federation, JIT
- `docs/developers/architecture/services/control-plane/tenancy-architecture.md` — org hierarchy, materialised path, policy inheritance
- `docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md` — IAM denials emit admin_audit

# E39-S1 — IAM Foundation: User/Org/Role Model

Status: draft
Epic: E39 — IAM & Identity Unification
Story: S1 — Unified User Model with Group-Based Roles

## User Story

As a platform administrator, I want all users (CP admins and agent employees)
to be managed in a single unified user model with group-based roles, so that
I can manage access control consistently.

---

## 1. Problem

The Nexus Gateway IAM layer has two overlapping user concepts that evolved
independently:

- **AdminUser** — the original Control Plane admin account; carries CP login
  credentials, RBAC via groups/policies, and API key ownership.
- **NexusUser** — introduced for agent enrollment; carries employee identity,
  `canAccessControlPlane`, and organization membership.

Because these two types were added at different times, `IamGroupMembership`
and `IamPolicyAttachment` contain `principalType = "admin_user"` throughout
the schema, API handlers, Go stores, and frontend code. The `AdminApiKey`
entity inherits permissions by ownership but that ownership path is not
explicitly documented in code.

The result is fragmented IAM: you cannot attach an IAM policy to a NexusUser
directly, group membership queries use the wrong principal type string, and
the frontend IAM pages show inconsistent principal labels.

---

## 2. Goal

Consolidate all user-facing IAM primitives onto `NexusUser` as the single
identity carrier:

1. Rename `principalType = "admin_user"` → `"nexus_user"` everywhere — DB
   rows, Go stores, Go handlers, and frontend components.
2. Declare the `NexusUser ↔ Organization` Prisma relation (FK already exists
   in the DB; the Prisma model just needs the `@relation` annotation on both
   sides).
3. Add `IamGroup.idpGroupName` for future IdP group sync (nullable, no
   behaviour change in this story).
4. Seed the five canonical system groups and five managed policies that every
   fresh installation must have.
5. Document — in code comments — that `AdminApiKey` permission inheritance
   flows through `ownerUserId → NexusUser → IamGroupMembership →
   IamPolicyAttachment`; no key-level policy rows are created.

---

## 3. Non-goals

- OIDC / SAML IdP integration and JIT group provisioning (separate epic).
- Removing the `AdminUser` Prisma model or its DB table — that model is still
  used by the CP auth-server session path; it is out of scope here.
- Any UI changes beyond the `principalType` label fix — UI improvements are
  in S4.
- Migration of production data — development-phase policy; a fresh seed is
  sufficient.

---

## 4. Design

### 4.1 Prisma Schema Changes (`tools/db-migrate/prisma/schema.prisma`)

#### 4.1.1 NexusUser ↔ Organization relation declaration

The foreign key column `NexusUser.organizationId` already exists in the DB.
Add the bidirectional `@relation` annotation so Prisma generates correct
include/select helpers:

```prisma
model NexusUser {
  // existing fields …
  organizationId String?
  organization   Organization? @relation(fields: [organizationId], references: [id])
  // …
}

model Organization {
  // existing fields …
  nexusUsers NexusUser[]
  // …
}
```

No DB migration is needed for this change — it is a Prisma-level declaration.

#### 4.1.2 IamGroup.idpGroupName field

```prisma
model IamGroup {
  // existing fields …
  idpGroupName String?   // external IdP group name for future JIT sync
  // …
}
```

Migration: `ALTER TABLE "IamGroup" ADD COLUMN "idpGroupName" TEXT;`

### 4.2 DB Migrations

#### 4.2.1 IamGroupMembership.principalType rename

```sql
UPDATE "IamGroupMembership"
SET "principalType" = 'nexus_user'
WHERE "principalType" = 'admin_user';
```

No schema column change is needed; the column is `TEXT`.

#### 4.2.2 IamPolicyAttachment.principalType rename

```sql
UPDATE "IamPolicyAttachment"
SET "principalType" = 'nexus_user'
WHERE "principalType" = 'admin_user';
```

### 4.3 Seed Data

Five system `IamGroup` rows (immutable, `isSystem = true`):

| name | displayName | description |
|---|---|---|
| `super-admins` | Super Administrators | Full platform access |
| `security-admins` | Security Administrators | Compliance and IAM access |
| `viewers` | Viewers | Read-only access to all resources |
| `developers` | Developers | AI Gateway invoke access |
| `members` | Members | Agent enrollment and basic access |

Five managed `IamPolicy` rows (`type = "managed"`, not editable):

| name | displayName | description |
|---|---|---|
| `NexusAdminFullAccess` | Nexus Admin Full Access | Grants full control over all Control Plane resources |
| `NexusComplianceAccess` | Nexus Compliance Access | Grants read/write access to compliance policies and traffic events |
| `NexusViewerAccess` | Nexus Viewer Access | Grants read-only access to all resources |
| `NexusGatewayInvokeAll` | Nexus Gateway Invoke All | Grants permission to invoke any model via AI Gateway |
| `NexusAgentAccess` | Nexus Agent Access | Grants agent enrollment and device management access |

Policy-to-group attachment (seeded as `IamGroupMembership` + group
`defaultPolicies`):

| Group | Attached Policy |
|---|---|
| `super-admins` | `NexusAdminFullAccess` |
| `security-admins` | `NexusComplianceAccess` |
| `viewers` | `NexusViewerAccess` |
| `developers` | `NexusGatewayInvokeAll` |
| `members` | `NexusAgentAccess` |

### 4.4 Go Store Changes (`packages/control-plane/internal/store/iam_crud.go`)

All hardcoded `"admin_user"` string literals in SQL `WHERE principalType =
'admin_user'` clauses and INSERT statements must be replaced with
`"nexus_user"`. Introduce a package-level constant:

```go
// principalTypeUser is the principalType string used for NexusUser principals
// in IamGroupMembership and IamPolicyAttachment rows.
const principalTypeUser = "nexus_user"
```

Replace every occurrence of the inline string with this constant. No SQL
query logic changes are needed beyond the string substitution.

### 4.5 Go Handler Changes

#### `packages/control-plane/internal/handler/admin_iam.go`

- Replace all `principalType == "admin_user"` guard checks with
  `principalType == "nexus_user"`.
- Update any error messages that contain `"admin_user"` to say `"nexus_user"`.

#### `packages/control-plane/internal/handler/admin_users.go`

- When creating `IamGroupMembership` or `IamPolicyAttachment` rows for a user
  (e.g. after user creation or bulk policy attach), pass `"nexus_user"` as
  `principalType`.

### 4.6 AdminApiKey Permission Inheritance Documentation

Add a block comment in `packages/control-plane/internal/store/iam_crud.go`
(or `admin_iam.go`) that makes the inheritance chain explicit:

```go
// AdminApiKey permission resolution:
//   An API key does NOT carry its own IamPolicyAttachment rows.
//   Access decisions for key-authenticated requests are resolved by:
//     1. Look up AdminApiKey.ownerUserId → NexusUser
//     2. Resolve NexusUser's effective policies via IamGroupMembership
//        (principalType="nexus_user") and direct IamPolicyAttachment rows.
//   This means revoking a user's group membership immediately affects all
//   API keys owned by that user.
```

### 4.7 Frontend Changes (`packages/control-plane-ui/src/`)

Four files contain the string `"admin_user"` in principalType comparisons or
API payloads. Change all occurrences to `"nexus_user"`:

- `src/pages/iam/IamPolicyDetail.tsx`
- `src/hooks/useIamUserDetail.ts`
- `src/pages/iam/IamSimulator.tsx`
- `src/pages/iam/IamRoleDetail.tsx`

Do **not** change `authPrincipalType` fields — those carry authentication
context (e.g. `"admin_user"` vs `"virtual_key"`) used by the CP auth-server
session lookup and must remain unchanged.

---

## 5. Tasks

- T1 — Prisma: add bidirectional `@relation` for `NexusUser ↔ Organization`
  (no migration needed, Prisma-only change).
- T2 — Prisma: add `IamGroup.idpGroupName String?` field and generate
  migration `ALTER TABLE "IamGroup" ADD COLUMN "idpGroupName" TEXT`.
- T3 — DB migration: `UPDATE "IamGroupMembership" SET principalType =
  'nexus_user' WHERE principalType = 'admin_user'`.
- T4 — DB migration: `UPDATE "IamPolicyAttachment" SET principalType =
  'nexus_user' WHERE principalType = 'admin_user'`.
- T5 — Seed: add 5 system `IamGroup` rows (`isSystem = true`).
- T6 — Seed: add 5 managed `IamPolicy` rows (`type = "managed"`) and attach
  each to its corresponding system group.
- T7 — Go store (`iam_crud.go`): introduce `principalTypeUser` constant;
  replace all `"admin_user"` SQL strings with the constant.
- T8 — Go handler (`admin_iam.go`): update principalType guard checks to
  `"nexus_user"`.
- T9 — Go handler (`admin_users.go`): update principalType in membership /
  attachment creation calls to `"nexus_user"`.
- T10 — Frontend: replace `"admin_user"` → `"nexus_user"` in
  `IamPolicyDetail.tsx`, `useIamUserDetail.ts`, `IamSimulator.tsx`,
  `IamRoleDetail.tsx`. Leave `authPrincipalType` untouched.
- T11 — Document `AdminApiKey` permission inheritance with block comment in
  Go source (see §4.6).

---

## 6. Acceptance Criteria

- AC1 — After migration and seed, `IamGroupMembership` contains zero rows
  with `principalType = 'admin_user'`; all user membership rows have
  `principalType = 'nexus_user'`.
- AC2 — After migration and seed, `IamPolicyAttachment` contains zero rows
  with `principalType = 'admin_user'` for user-type principals.
- AC3 — Five system `IamGroup` rows exist with `isSystem = true` after seed.
- AC4 — Five managed `IamPolicy` rows exist with `type = 'managed'` after
  seed.
- AC5 — `canAccessControlPlane` remains on `NexusUser`; the user management
  UI checkbox continues to toggle it correctly.
- AC6 — Existing IAM operations (policy attach/detach, group membership
  add/remove, IAM simulator evaluate) all return correct results after the
  rename.
- AC7 — `AdminApiKey`-authenticated requests resolve permissions through the
  owning `NexusUser`'s group memberships (verifiable via IAM simulator with
  key-as-principal).
- AC8 — `IamGroup.idpGroupName` column exists in the DB and accepts a null
  or string value; no functional IAM behaviour changes from this field.
- AC9 — Frontend IAM pages render without runtime errors after the
  `principalType` string change.

---

## 7. Risks

- **R1** — Existing sessions or cached tokens may carry `principalType =
  "admin_user"` in JWT claims or Redis session blobs. Mitigation: the
  principalType rename only affects DB membership/attachment rows, not session
  tokens; auth-server session lookup uses `authPrincipalType` which is
  unchanged.
- **R2** — Fresh seed may silently fail to attach policies to groups if the
  seed script runs before the `idpGroupName` migration. Mitigation: seed
  script must depend on the migration in execution order (Prisma seed runs
  after all migrations by design).
- **R3** — Frontend: a `"admin_user"` occurrence missed during the rename
  causes a 403 or empty policy list for a user-type principal. Mitigation:
  search the entire UI codebase with `grep -r '"admin_user"'` after T10 and
  verify zero results in non-auth files.

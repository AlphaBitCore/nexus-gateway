# E39-S4 — UI/UX Changes for IAM & Identity Unification

Status: draft
Epic: E39 — IAM & Identity Unification
Story: S4 — Control Plane UI: Unified User Model & Device Trust

## User Story

As a platform administrator, I want the Control Plane UI to reflect the
unified user model, group-based roles, and device trust information, so that
I can manage identity and access from a single interface.

---

## 1. Problem

The Control Plane UI was built before the unified user model was established.
Several display gaps remain:

- **Users list / detail** — No indication of which system IAM group a user
  belongs to (i.e. their effective "role"), and no quick way to see or toggle
  `canAccessControlPlane`.
- **IAM Groups page** — All groups look the same; system groups (immutable,
  seeded) are indistinguishable from custom groups. There is no `idpGroupName`
  display.
- **IAM Policies page** — Managed policies (seeded, not editable) show the
  same Edit / Delete actions as custom policies, leading to confusing errors
  when an admin tries to modify them.
- **Nodes page** — The `ThingAgent` list has no "who is logged in" column and
  no trust level indicator, so security admins cannot see device compliance
  at a glance.

---

## 2. Goal

Update four existing UI pages and one i18n layer to surface the new data
introduced by E39-S1 through S3:

1. **User List** — add "Groups" (role indicator) and "CP Access" columns.
2. **User Detail** — show org name, group memberships, `canAccessControlPlane`
   checkbox editable by super-admin.
3. **IAM Groups** — system group badge, `idpGroupName` display, read-only
   membership list for system groups.
4. **IAM Policies** — "Managed" badge, disable Edit/Delete for managed
   policies.
5. **Nodes** — "Current User" column and "Trust Level" badge column.
6. **i18n** — add all new string keys to `en`, `zh`, `es` locale files.

---

## 3. Non-goals

- Any new API endpoints — all data is available from existing endpoints
  updated in S1–S3 (group membership, policy type, ThingAgent with
  `currentAssignmentId` join).
- IdP group sync UI (the `idpGroupName` field is displayed only; setting it
  is a future story).
- Role-based page visibility changes beyond what is already configured in
  route permissions.
- Custom policy CRUD flows — only badge/disable changes for managed policies.

---

## 4. Design

### 4.1 User List Page

**File**: `src/pages/users/UserListPage.tsx` (or equivalent)

Add two columns to the user table:

| Column | Source | Display |
|---|---|---|
| Groups | `NexusUser.iamGroupMemberships[].group.name` | Comma-separated system group display names; if none, show "—" |
| CP Access | `NexusUser.canAccessControlPlane` | Green checkmark icon if `true`; grey dash if `false` |

The "Groups" column should show only system group names (those where
`isSystem = true`) as a concise role indicator. If a user belongs to
`super-admins`, show "Super Admin". Use a short-form mapping:

| System group name | Short label |
|---|---|
| `super-admins` | Super Admin |
| `security-admins` | Security Admin |
| `viewers` | Viewer |
| `developers` | Developer |
| `members` | Member |

If the user belongs to multiple system groups, show all short labels
separated by commas. Custom group names are omitted from this column
(shown on the detail page).

The existing `useApi` query key for the user list must include
`['admin', 'users', 'list', ...]`; if groups data requires a join, add
`?include=groups` to the existing list API call and update the queryKey
with a `'with-groups'` suffix to avoid cache collisions with callers that
fetch users without groups.

### 4.2 User Detail Page

**File**: `src/pages/users/UserDetailPage.tsx` (or equivalent)

Add the following sections to the existing user detail layout:

**Organization**
- Display `organization.name` (from `NexusUser.organization`).
- If `organizationId` is null, show "No organization assigned".

**Group Memberships**
- List all groups the user belongs to (system and custom).
- For each group: show `displayName`, a "System" badge if `isSystem = true`.
- For super-admins: show an "Add to group" / "Remove from group" action
  (calls existing group membership API).

**Control Plane Access**
- `canAccessControlPlane` checkbox (label: `t('iam:users.cpAccessLabel')`).
- Editable only if the current viewer has role `super-admins`.
- On change: call `PATCH /api/admin/users/{id}` with `{ canAccessControlPlane: boolean }`.
- Show a tooltip explaining what this flag controls.

### 4.3 IAM Groups Page

**File**: `src/pages/iam/IamGroupsPage.tsx` (or equivalent)

Changes:

1. **System group badge** — For groups where `isSystem = true`, render a
   lock icon with a "System" label badge next to the group name. This badge
   uses i18n key `t('iam:groups.systemBadge')`.

2. **idpGroupName display** — If `group.idpGroupName` is set, show it in
   a secondary line under the group name (label: `t('iam:groups.idpGroupLabel')`).
   If not set, show nothing (do not show "—").

3. **Membership list for system groups** — For system groups, the member
   list is read-only. Hide the "Add member" and "Remove" buttons. Show a
   caption: `t('iam:groups.systemGroupMembersNote')` explaining that system
   group membership is managed automatically via JIT provisioning.

4. **Custom groups** — Retain existing add/remove member functionality.

### 4.4 IAM Policies Page

**File**: `src/pages/iam/IamPoliciesPage.tsx` (or equivalent)

Changes:

1. **Managed badge** — For policies where `type = "managed"`, render a
   shield icon with a "Managed" label badge. I18n key:
   `t('iam:policies.managedBadge')`.

2. **Disable Edit/Delete for managed policies** — Managed policies must not
   be editable or deletable. Render the Edit and Delete actions as disabled
   (greyed out) for managed policies, with a tooltip:
   `t('iam:policies.managedEditDisabledTooltip')`.

3. **No behavior change for custom policies** — Custom policy CRUD remains
   unchanged.

### 4.5 Nodes Page (ThingAgent)

**File**: `src/pages/infrastructure/NodesPage.tsx` (or equivalent)

Add two columns to the agent node table:

#### Current User column

Source: `ThingAgent.currentAssignmentId → DeviceAssignment → NexusUser.displayName`

The Nodes list API response must include this join. If `currentAssignmentId`
is null or the assignment has no user, display "—".

`useApi` query key: `['admin', 'nodes', 'list', 'with-assignment', ...]`
(add `'with-assignment'` suffix to distinguish from other node list callers).

#### Trust Level column

Source: `ThingAgent.trustLevel`

Display as a status badge with four states:

| Value | Label | Badge color |
|---|---|---|
| 0 | Untrusted | Red |
| 1 | Enrolled | Grey |
| 2 | Identified | Yellow |
| 3 | Compliant | Green |

I18n keys: `t('nodes:trustLevel.0')`, `t('nodes:trustLevel.1')`, etc.

A tooltip on each badge explains what the level means
(`t('nodes:trustLevel.0_tooltip')`, etc.).

### 4.6 i18n Keys

All new strings use `t()` from `react-i18next`. New keys are added to
`src/i18n/locales/{en,zh,es}/pages.json` (and `nav.json` if needed).

Key structure (English values shown):

```json
{
  "iam": {
    "users": {
      "groupsColumn": "Groups",
      "cpAccessColumn": "CP Access",
      "cpAccessLabel": "Control Plane Access",
      "cpAccessTooltip": "Allows this user to log in to the Control Plane admin UI.",
      "noOrg": "No organization assigned"
    },
    "groups": {
      "systemBadge": "System",
      "idpGroupLabel": "IdP Group",
      "systemGroupMembersNote": "System group membership is managed automatically. Members are added via identity provider group sync."
    },
    "policies": {
      "managedBadge": "Managed",
      "managedEditDisabledTooltip": "Managed policies are built-in and cannot be edited or deleted."
    }
  },
  "nodes": {
    "currentUserColumn": "Current User",
    "trustLevelColumn": "Trust Level",
    "trustLevel": {
      "0": "Untrusted",
      "1": "Enrolled",
      "2": "Identified",
      "3": "Compliant",
      "0_tooltip": "Device certificate is invalid or expired.",
      "1_tooltip": "Device is online and enrolled but no user is logged in.",
      "2_tooltip": "A user is actively logged in on this device.",
      "3_tooltip": "User is logged in and the agent is running the minimum required version."
    }
  }
}
```

Chinese (`zh`) and Spanish (`es`) translations must provide all the same keys.
Technical terms (`IAM`, `IdP`, `CP`, `API`, `JIT`, `MDM`, `JWT`) stay in
English across all locales.

---

## 5. Tasks

- T1 — User List page: add "Groups" column (system group short labels) and
  "CP Access" column (`canAccessControlPlane` checkmark).
- T2 — User Detail page: add org name display, group membership list with
  system badge, and `canAccessControlPlane` checkbox (super-admin editable).
- T3 — IAM Groups page: add System badge for `isSystem = true` groups; show
  `idpGroupName` if set; render membership list as read-only for system groups
  with explanatory caption.
- T4 — IAM Policies page: add "Managed" badge for `type = "managed"` policies;
  disable Edit/Delete actions for managed policies with tooltip.
- T5 — Nodes page: add "Current User" column (from `DeviceAssignment →
  NexusUser.displayName`) and "Trust Level" badge column (0–3 with color and
  tooltip).
- T6 — i18n: add all new keys listed in §4.6 to
  `src/i18n/locales/{en,zh,es}/pages.json`; copy to
  `public/locales/{en,zh,es}/pages.json`; verify key counts match.

---

## 6. Acceptance Criteria

- AC1 — Nodes page shows "Current User" (display name of the logged-in user,
  or "—") and "Trust Level" badge for each agent device.
- AC2 — Trust Level badge colors match the table in §4.5; tooltips are
  present and i18n-keyed.
- AC3 — IAM Groups page shows a "System" badge for the 5 seeded system
  groups; their member lists are read-only (no Add/Remove buttons visible).
- AC4 — IAM Policies page shows a "Managed" badge for the 5 seeded managed
  policies; their Edit and Delete actions are disabled (not hidden) with a
  tooltip explaining why.
- AC5 — User detail page shows `canAccessControlPlane` checkbox; it is
  editable only when the viewer is in the `super-admins` group.
- AC6 — User list page shows the "Groups" column with system group short
  labels, and the "CP Access" column with a checkmark/dash.
- AC7 — All new UI strings use `t()` calls with valid i18n keys; no hardcoded
  English strings in JSX for the new content.
- AC8 — Key counts in `en/pages.json`, `zh/pages.json`, and `es/pages.json`
  are equal after the i18n additions.
- AC9 — `npm run build` (Vite production build) completes without TypeScript
  errors in the updated files.

---

## 7. Risks

- **R1** — The Nodes list API may not currently return
  `currentAssignmentId → DeviceAssignment → NexusUser.displayName` in a
  single response. Mitigation: confirm the CP admin API for nodes supports an
  `?include=assignment` query parameter or equivalent; if not, add a CP API
  change in the same PR (small handler update to join ThingAgent with
  DeviceAssignment and NexusUser).
- **R2** — The user list API may not return group memberships by default.
  Mitigation: add `?include=groups` support to the user list endpoint and
  update the queryKey with a `'with-groups'` suffix to prevent cache
  collisions.
- **R3** — Locale key-count divergence is easy to introduce when adding keys
  across three files. Mitigation: after T6, run a key-count diff (`jq
  '[paths(scalars)] | length' en/pages.json`) for all three locale files and
  confirm equal counts.
- **R4** — Disabling Edit/Delete via UI alone does not prevent a determined
  admin from calling the API directly. Mitigation: the CP API handler for
  policy update/delete must also check `type = "managed"` and return 403.
  This guard belongs in the S1 backend changes; the UI reflects the same
  constraint.

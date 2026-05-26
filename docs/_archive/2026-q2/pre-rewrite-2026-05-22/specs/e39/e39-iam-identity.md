# E39 — IAM & Identity Unification

> Epic: 39
> Status: Approved
> Date: 2026-05-09
> Architecture impact: `docs/users/product/architecture.md` § "IAM & Identity Unification (E39)"

---

## 1. Background

Nexus Gateway's IAM layer evolved incrementally across multiple epics,
resulting in two user identity models (`AdminUser` and `NexusUser`) that
coexist but are not unified. The fragmentation creates three concrete
problems:

1. **IAM policy assignment is broken for NexusUsers** — `IamGroupMembership`
   and `IamPolicyAttachment` contain `principalType = "admin_user"` throughout
   the DB, Go stores, and frontend. A NexusUser cannot be attached to a policy
   or group using the current principal type strings.

2. **Device identity is static** — The compliance proxy and Hub IdentityEnricher
   resolve the user behind a device using a `boundUserId` field set at
   enrollment time. When a different employee logs in to the same machine, the
   attribution is wrong. There is no concept of "who is currently using this
   device right now."

3. **AI Guard calls lack user attribution** — The AI Guard endpoint is
   authenticated with a VK (org-level identity), so every guard-path traffic
   event is attributed to the VK owner rather than the employee who triggered
   it.

E39 resolves all three problems through four stories:
- **S1**: Unify the user model; rename `principalType` strings; seed system
  groups and managed policies.
- **S2**: Introduce `DeviceAssignment` to track the currently logged-in user
  per device, with a Hub-computed trust level.
- **S3**: Update the traffic attribution pipeline to use `DeviceAssignment`
  for IP-based enrichment and JWT-based attribution for AI Guard calls.
- **S4**: Update the Control Plane UI to surface the new data.

---

## 2. Functional Requirements

### FR-1: Unified User Model (S1)

| ID | Requirement | Priority |
|---|---|---|
| FR-1.1 | All user-type IAM principals use `principalType = "nexus_user"`. No new `"admin_user"` rows are ever created. | Must |
| FR-1.2 | Existing `IamGroupMembership` and `IamPolicyAttachment` rows with `principalType = "admin_user"` are migrated to `"nexus_user"`. | Must |
| FR-1.3 | The Prisma schema declares the bidirectional `NexusUser ↔ Organization` relation. | Must |
| FR-1.4 | `IamGroup` has an `idpGroupName` nullable field for future IdP group sync. | Should |
| FR-1.5 | Five system `IamGroup` rows exist after seed: `super-admins`, `security-admins`, `viewers`, `developers`, `members`. These rows have `isSystem = true` and cannot be deleted via the API. | Must |
| FR-1.6 | Five managed `IamPolicy` rows exist after seed: `NexusAdminFullAccess`, `NexusComplianceAccess`, `NexusViewerAccess`, `NexusGatewayInvokeAll`, `NexusAgentAccess`. These rows have `type = "managed"` and cannot be edited or deleted via the API. | Must |
| FR-1.7 | `AdminApiKey` permission resolution flows through `ownerUserId → NexusUser → IamGroupMembership`. No key-level policy rows are created. This behavior is documented in code. | Must |
| FR-1.8 | `canAccessControlPlane` remains on `NexusUser`. Only users with `canAccessControlPlane = true` can log in to the Control Plane UI. | Must |

### FR-2: Device Identity & Trust (S2)

| ID | Requirement | Priority |
|---|---|---|
| FR-2.1 | `DeviceAssignment` tracks the currently active user session on each device. At most one active assignment (where `releasedAt IS NULL`) exists per device at any time, enforced by a DB partial unique index. | Must |
| FR-2.2 | On agent token issuance by the CP auth-server, a `DeviceAssignment` row is created with `source = "login"`, `loginMethod`, `tokenJti`, and `ipAddress`. Any previous active assignment for the same device is closed in the same transaction. | Must |
| FR-2.3 | `ThingAgent` has a `trustLevel` integer (0–3) updated by the Hub on every heartbeat. | Must |
| FR-2.4 | Trust level computation: 0 = cert invalid; 1 = online, no assignment; 2 = online, active assignment; 3 = level 2 + agent version ≥ minimum required. | Must |
| FR-2.5 | On agent logout (token revocation), the active `DeviceAssignment` is closed and `ThingAgent.trustLevel` drops to 1. | Must |
| FR-2.6 | The minimum required agent version for trust level 3 is configurable via `system_metadata` (key: `agent.min_trust_version`). Default: `"0.0.0"`. | Should |

### FR-3: Traffic Attribution (S3)

| ID | Requirement | Priority |
|---|---|---|
| FR-3.1 | `traffic_event` has `thing_id` and `thing_name` nullable columns. A partial index on `(thing_id, timestamp)` supports efficient device-level queries. | Must |
| FR-3.2 | Compliance-proxy event writer populates `thing_id` and `thing_name` at INSERT time when the source device is identifiable. | Must |
| FR-3.3 | Hub IdentityEnricher resolves `entity_id` (user) for compliance-proxy events via `DeviceAssignment` IP + time-window query. Falls back to metadata-based lookup if no assignment found. | Must |
| FR-3.4 | AI Guard calls from the agent are authenticated via a short-lived JWT (scope: `ai-guard:invoke`, TTL: 5 min) issued by the CP auth-server. | Must |
| FR-3.5 | The AI Gateway validates the AI Guard JWT using CP's JWKS (`/.well-known/jwks.json`), extracts `sub` (userId) and `device` (thingId), and writes them to `traffic_event`. | Must |
| FR-3.6 | AI Guard JWT JWKS is cached by the AI Gateway for 5 minutes. A 401 response triggers cache invalidation and one retry. | Must |
| FR-3.7 | In a correctly deployed enterprise environment (device enrolled, user logged in via CP agent token), zero `traffic_event` rows should have a null `entity_id` for agent-sourced or compliance-proxy-sourced events. | Should |

### FR-4: Control Plane UI (S4)

| ID | Requirement | Priority |
|---|---|---|
| FR-4.1 | User List page shows a "Groups" column (system group short labels) and a "CP Access" column. | Must |
| FR-4.2 | User Detail page shows org name, group memberships with system badge, and an editable `canAccessControlPlane` checkbox for super-admins. | Must |
| FR-4.3 | IAM Groups page shows a "System" badge for system groups; system group membership lists are read-only. | Must |
| FR-4.4 | IAM Policies page shows a "Managed" badge for managed policies; Edit/Delete are disabled for managed policies. | Must |
| FR-4.5 | Nodes page shows "Current User" (from active `DeviceAssignment`) and "Trust Level" (0–3 badge) columns for each agent device. | Must |
| FR-4.6 | All new UI strings are i18n-keyed in `en`, `zh`, and `es` locale files. | Must |

---

## 3. Non-Functional Requirements

| ID | Requirement |
|---|---|
| NFR-1 | `DeviceAssignment` creation (login flow) must complete within the existing CP auth-server token issuance latency budget (< 50 ms added overhead). |
| NFR-2 | Hub IdentityEnricher `DeviceAssignment` query must complete in < 100 ms p99 (simple indexed lookup by IP + timestamp). |
| NFR-3 | All new `traffic_event` columns are nullable; no existing INSERT statement fails due to missing values. |
| NFR-4 | JWKS fetch failure on the AI Gateway must not crash the process; affected requests return 503. |
| NFR-5 | The DB partial unique index on `DeviceAssignment(deviceId) WHERE releasedAt IS NULL` must be created in a transaction that first closes duplicate active rows, if any. |
| NFR-6 | Go tests: `go test -race -count=1` green for all changed packages. |
| NFR-7 | TypeScript strict mode; `npm run build` green after all UI changes. |

---

## 4. User Roles & Personas

| Role | Interaction |
|---|---|
| **Super Admin** | Manages system group memberships, toggles `canAccessControlPlane`, views the full IAM configuration. |
| **Security Admin** | Views device trust levels on the Nodes page, reviews compliance-proxy traffic events with user attribution. |
| **Platform Admin** | Manages IAM groups (custom), views managed policies (read-only), assigns users to custom groups. |
| **Compliance Officer** | Reviews traffic events; verifies that all events have `entity_id` populated (zero anonymous traffic). |
| **Employee (NexusUser)** | Logs in via the agent; their login creates a `DeviceAssignment`; their traffic is attributed to their user ID transparently. |
| **Developer (VK user)** | No direct interaction with IAM unification; benefits from correct policy resolution through the VK → NexusUser chain. |

---

## 5. Constraints & Assumptions

- The `AdminUser` Prisma model and its DB table are **not removed** in E39.
  The CP auth-server still uses it for session management. The unification is
  at the IAM principal type level only (`principalType` string in membership
  and attachment tables).
- Device IP attribution assumes either per-device IP assignment or a small
  enough time-window that IP collisions (shared NAT) are rare. Shared NAT
  environments are a known limitation documented in S3.
- The AI Guard JWT flow requires an agent software update to switch from VK
  to JWT authentication. Until the agent is updated, both VK and JWT paths
  are accepted by the gateway (transition period). VK removal is out of scope
  for E39.
- System group membership in E39 is managed manually by super-admins. JIT
  provisioning from an IdP (using `idpGroupName`) is a future epic.
- `agent.min_trust_version` defaults to `"0.0.0"` so all enrolled online
  agents with active assignments reach trust level 3 until an operator sets a
  minimum version requirement.
- Development-phase policy applies: no backward-compatibility layers, no
  migration of historical development data, fresh seed is acceptable.

---

## 6. Glossary

| Term | Definition |
|---|---|
| **NexusUser** | The unified user identity model carrying employee identity, org membership, and CP access flag. |
| **principalType** | The type discriminator in `IamGroupMembership` and `IamPolicyAttachment` rows. Valid values after E39: `"nexus_user"`, `"virtual_key"`, `"api_key"`. |
| **System group** | An `IamGroup` with `isSystem = true`. Seeded by the platform; cannot be deleted or renamed. Membership is managed by super-admins or JIT provisioning. |
| **Managed policy** | An `IamPolicy` with `type = "managed"`. Seeded by the platform; cannot be edited or deleted. |
| **DeviceAssignment** | A record linking a device (`ThingAgent`) to a user (`NexusUser`) for the duration of their login session. |
| **Trust level** | An integer (0–3) on `ThingAgent` reflecting the device's current compliance posture. |
| **IdentityEnricher** | Hub component that asynchronously resolves user identity for traffic events by matching source IP to `DeviceAssignment`. |
| **AI Guard** | The `/v1/guard` endpoint on the AI Gateway, called by the desktop agent to check prompt compliance before sending upstream. |
| **JWKS** | JSON Web Key Set; the CP's public key endpoint (`/.well-known/jwks.json`) used by the AI Gateway to verify AI Guard JWTs. |
| **JIT provisioning** | Just-In-Time group membership assignment driven by IdP group membership claims. Out of scope for E39. |

---

## 7. Priority (MoSCoW)

| Category | Items |
|---|---|
| **Must** | FR-1.1–1.3, FR-1.5–1.8, FR-2.1–2.5, FR-3.1–3.6, FR-4.1–4.6, all NFRs |
| **Should** | FR-1.4 (idpGroupName field), FR-2.6 (configurable min version), FR-3.7 (zero anonymous traffic SLO) |
| **Could** | Trust level enforcement in routing/policy evaluation (separate epic) |
| **Won't (this epic)** | IdP OIDC/SAML JIT group sync, `AdminUser` table removal, AI Guard VK path removal, semantic policy evaluation changes |

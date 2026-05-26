# E43-S2: IAM Service Taxonomy Redesign — 5 Services Aligned with Product Domains

**Status**: In progress
**Date**: 2026-05-13
**Predecessors**: e43-s1-iam-canonical-taxonomy.md (the 33-resource catalog),
                  commit `3fbff0ab` (AWS-style policy editor — scope-aware
                  dispatch + StringList canonical form)

## 1. Problem Statement

The original E43-S1 taxonomy tagged every catalog resource with one of three
`Service` values (`gateway` / `admin` / `compliance`). This worked as an
internal classifier but failed three product checks once IAM policies started
being authored by humans:

1. **Role overload on `admin`** — 16 of 33 resources were tagged
   `admin`, mixing IAM (users, orgs, policies), platform ops (nodes,
   observability, settings), and compliance emergency controls
   (kill-switch, ai-guard-config) into a single bucket. A role
   description like "security admin" could not be expressed by
   "give me admin: actions on the admin service" — too broad.

2. **Resources sitting in the wrong domain**:
   - `agent-device` / `device-group` / `device-assignment` were under
     `gateway` even though they belong to the Agent product line and
     are owned by security/compliance from a role perspective.
   - `kill-switch` / `ai-guard-config` were under `admin` even though
     they are compliance pipeline controls.
   - `node` / `observability` / `settings` were under `admin` even
     though they are platform-level ops concerns.

3. **UI Sidebar leak (the Frank/Bob case)** — when a managed policy
   granted `admin:*.read`, the wildcard matched every resource's
   read across all three services. The sidebar's `allowedActions`
   guards keyed off these actions, so a provider-admin saw every
   menu in the platform (IAM, Infrastructure, Devices, Identity
   Provider, Kill Switch) because the wildcard matched their reads
   even though those domains were outside their role intent.

## 2. Goals

- Carve the catalog by **product domain** so each Service corresponds
  to one user-recognisable capability the platform sells.
- Make every role expressible as a small set of "I own service X,
  I read service Y" statements without overreach.
- Preserve the AWS-style policy authoring surface introduced in
  `3fbff0ab` (per-resource Statements, `admin:<resource>.*` wildcard,
  StringList canonical form).
- Zero impact on SIEM event types, audit Entry construction, and the
  four CI consistency gates introduced in E43-S1 P6.

## 3. Design

### 3.1 Five Services

| Service      | Product feature                                                   | Persona ownership                |
|--------------|-------------------------------------------------------------------|----------------------------------|
| `gateway`    | AI Traffic Gateway (providers / models / VKs / routing / quotas)  | provider-admin (write), viewer (read) |
| `compliance` | Compliance Pipeline (hooks / rules / exemptions / DSAR / kill)    | security-admin (write), viewer (read) |
| `agent`      | Agent Fleet (devices, groups, assignments)                        | security-admin (manage), viewer (read) |
| `platform`   | Operations (nodes / observability / settings / alerts)            | super-admin + security + provider-admin (ops-scoped) |
| `iam`        | Identity & Access (users / orgs / policies / audit / sessions)    | super-admin (write), security (read for audit) |

Naming choices:
- `gateway` — kept (matches package + product name)
- `compliance` — kept
- `agent` — new, matches `packages/agent/` and product line
- `platform` — new, chosen over "ops" / "infra" because it covers both
  monitoring (alerts/observability) and infrastructure (nodes/settings)
- `iam` — new, chosen over "identity" to match AWS/GCP industry standard

### 3.2 Resource → Service Mapping (37 rows)

Resources marked with → indicate a service change from E43-S1.

```
gateway (11):
  provider, model, model-pricing,
  credential, virtual-key,
  routing-rule,
  quota-policy, quota-override, quota-analytics,
  analytics, traffic-log

compliance (9):
  hook, rule-pack,
  agent-exemption, compliance-exemption,
  compliance-report, interception-domain, dsar,
  ai-guard-config         → moved from `admin`
  kill-switch             → moved from `admin`

agent (3):
  agent-device            → moved from `gateway`
  device-group            → moved from `gateway`
  device-assignment       → moved from `gateway`

platform (5):
  node                    → moved from `admin`
  observability           → moved from `admin`
  settings                → moved from `admin`
  alert                   → moved from `admin`
  diagnostic-mode         → moved from `admin`

iam (9):
  user                    → moved from `admin`
  api-key                 → moved from `admin`
  organization            → moved from `admin`
  project                 → moved from `admin`
  iam-policy              → moved from `admin`
  iam-group               → moved from `admin`
  audit-log               → moved from `admin`
  revocation              → moved from `admin`
  nexus-session           → moved from `admin`
```

The `admin` service is fully drained — kept as a deprecated symbol in
the Go enum for any external caller that references it, but no
catalog row uses it post-migration.

### 3.3 NRN Format (unchanged)

```
nrn:nexus:<service>:<scope>:<resource>/<id>
```

Examples after the redesign:
```
nrn:nexus:gateway:*:provider/openai
nrn:nexus:compliance:*:hook/anthropic-pii-redactor
nrn:nexus:compliance:*:kill-switch/global
nrn:nexus:agent:*:agent-device/m-williams-macbook
nrn:nexus:platform:*:alert/quota-vk-expiring
nrn:nexus:iam:*:user/bob@nexus.ai
```

Scope is `*` for all entries in seeded managed policies. A future
multi-tenancy feature will populate scope with org/project IDs; the
policy structure does not need to change to support that.

### 3.4 Managed Policy Layout (post-redesign)

Each managed policy uses **one Statement per resource** (matching the
AWS-style editor's scope-aware dispatch from `3fbff0ab`):

```typescript
{
  Sid: 'hooks-admin',
  Effect: 'Allow',
  Action: ['admin:hook.*'],          // wildcard when role gets full lifecycle
  Resource: ['nrn:nexus:compliance:*:hook/*'],
}
```

When the role gets only a subset of the catalog verbs for a resource,
the Action lists specific verbs and the Sid carries a descriptive
suffix (`dsar-lifecycle`, `nodes-incident-response`, etc.).

The full policy structure per persona is laid out in `tools/db-migrate/
seed/seed.ts` block 15c.

### 3.5 Middleware + GetMePermissions

Both already derive the resource NRN's service from the action via
`iam.ServiceForAction(action)` (added in commit `8fe1e859`). With the
new service tags in catalog_data.go, the middleware automatically
evaluates against the correct NRN — no further code change in the
middleware or admin_extras handler.

### 3.6 UI Catalog Alignment — three-level hierarchy

Every UI that asks the user to pick an action surfaces the catalog as
a **three-level drill-down**:

```
Service          (top: 5 buckets — gateway / compliance / agent / platform / iam)
   └── Resource  (mid: the resources owned by that service)
          └── Action  (leaf: the verbs declared in the catalog for that resource)
```

This mirrors the way humans think about authorisation ("provider-admin
manages AI Gateway, specifically the credential resource, specifically
the rotate action") and matches the AWS-style hierarchy from
`3fbff0ab` (where each statement scopes to one resource type and an
action picker drills inside it).

The action-catalog endpoint
(`GET /api/admin/iam/action-catalog`) already returns
`{type, service, nrn, actions[]}` per resource, which carries enough
information; the UI layer is responsible for grouping by service at
the top.

UI surfaces that adopt the three-level hierarchy:

1. **Policy Editor** — `CatalogPicker.tsx` already renders resources
   in groups; the grouping key becomes the new 5-value `service` field.
   `ScopedActionsPicker.tsx` is unchanged (it already drills from
   resource → action). i18n adds the 5 service label keys.

2. **IAM Simulator** — when picking the action to test, the picker
   becomes service → resource → action instead of a flat list.

3. **SIEM Filter** — when choosing which event types to forward, the
   filter tree uses service → resource → event-type (the event-type
   is `<resource>.<verb>` per `SIEMEventType` so it maps 1:1 with the
   third level).

The i18n additions: 5 service label keys
(`iam:services.gateway`, `iam:services.compliance`, `iam:services.agent`,
`iam:services.platform`, `iam:services.iam`) plus a short description
per service for tooltip/help text.

### 3.7 Sidebar Nav Guards

Nav guard `allowedActions` strings reference the action name only
(e.g. `admin:provider.read`), not the service or NRN. So **no nav
guard changes are required** from the service retagging — the action
strings themselves are unchanged.

## 4. Risk Matrix

| Layer | Impact | Mitigation |
|---|---|---|
| Audit `EntryFor(c, ResourceX, VerbY)` emission | **None** — reads `.Name` only, not `.Service` | — |
| SIEM `SIEMEventType(resource, verb)` | **None** — does not include service in event type | E43-S1 P4 consistency test passes unchanged |
| iamMW middleware action gates | **None** — actions are unchanged, only Resource NRN segment moves | `ServiceForAction` looks up via catalog automatically |
| GetMePermissions per-action NRN | **None** — same `ServiceForAction` lookup | — |
| CI consistency gates (4 of them, E43-S1 P6) | **None** — gates check canonical action regex, not service | — |
| Existing user-authored IAM policies | **0 affected** — none exist yet (only 6 managed policies, all rewritten in this work) | — |
| Sidebar nav guard `allowedActions` | **None** — keys off action string, not service | — |
| UI catalog rendering | **Visible change** — CatalogPicker / Simulator / SIEM filter now show 5 buckets | Phase 5–7 rewrite + i18n |

## 5. Rollout Phases

| Phase | Scope | Exit criteria |
|---|---|---|
| 1 | This SDD | User-confirmed direction (recorded above) |
| 2 | Go catalog: add 3 service constants, retag 13 resources | `go build` clean, catalog_test passes |
| 3 | Zero-regression sweep: audit / SIEM / handler / CI gates | All four tests green |
| 4 | seed.ts policy rewrite: per-resource Statements, new NRN paths | `prisma db seed` reseeds 6 managed policies |
| 5 | UI Policy Editor catalog picker — group by 5 services | Vitest passes; manual verify catalog browser |
| 6 | UI IAM Simulator picker — same grouping | Vitest passes |
| 7 | UI SIEM filter UI — same grouping | Vitest passes |
| 8 | API ↔ UI permission audit | Spreadsheet of nav ↔ API action mismatch is empty |
| 9 | Reseed + restart CP + verify Bob/Carol/Diana | `/me/permissions` returns expected actions; sidebar matches role |

## 6. Out of Scope

- Multi-tenancy scope (org/project segment of NRN) — covered by a
  later epic; the structure here supports it without policy edits.
- Custom user-authored managed policies for additional roles —
  organisations are expected to clone the seeded policies and tune
  via the editor UI.
- Splitting `compliance` into `compliance-policy` + `compliance-runtime`
  — single `compliance` service is sufficient; revisit when AI Guard
  becomes a billable feature with its own permission boundary.

## 7. Open Questions

None at acceptance. Resolved during brainstorm:
- "Should agent and platform exist as separate services?" — yes
  (separate product lines + ops domains).
- "Should AI Guard live under compliance or admin?" — compliance.
- "Should kill-switch live under compliance or admin?" — compliance.
- "Should we use service name `ops` vs `platform`?" — `platform`
  (covers both monitoring and infrastructure).
- "Should we use `identity` vs `iam`?" — `iam` (industry standard).

# E43 — Story 1: IAM Canonical Action Taxonomy

> Epic: 43
> Status: Draft (pending review)
> Date: 2026-05-12
> Architecture impact: new `docs/users/product/architecture.md` § "IAM Canonical Action Taxonomy (E43)"

---

## 1. Context

Three layers of the platform speak about the same domain operations in
mutually inconsistent vocabularies:

| Domain concept | IAM action (current) | Audit `EntityType` (current) | SIEM `eventType` (current) | Frontend NRN picker |
|---|---|---|---|---|
| Virtual Key | `admin:CreateVirtualKey` | **`virtualKey` AND `virtual_key`** ⚠️ | `virtualKey.create` / `virtual_key.create` | `virtual-key` |
| Routing Rule | `admin:CreateRoutingRule` | `routingRule` | `routingRule.create` | `routing-rule` |
| Quota Override | `admin:CreateQuotaOverride` | `quota_override` | `quota_override.create` | `quota` (collapsed with policy) |
| User | `admin:CreateUser` | `nexusUser` | `nexusUser.create` | _(not in picker)_ |
| Audit Log | `admin:ExportAuditLog` | `adminAuditLog` | `adminAuditLog.export` | _(not in picker)_ |

Four casings (`CamelCase`, `camelCase`, `snake_case`, `kebab-case`) are mixed
across layers; two casings are mixed **within the audit layer alone**
(`virtualKey` and `virtual_key` are both emitted by different handlers
today). SIEM `eventType` is derived as `<EntityType>.<Action>`, so any
audit-side inconsistency propagates straight into SIEM filter strings that
operators have to remember literally.

The rot is not confined to frontend pickers. The Prisma seed at
`tools/db-migrate/seed/seed.ts` (block 13, `IAM Managed Policies`,
~L849–L908) references 10+ action identifiers that are **not in
`allAdminActions`** (`admin:CreatePolicy`, `admin:ReadRedaction`,
`admin:InvalidateCache`, `admin:RefreshRuntimeCache`, `admin:ReadQuota`,
`admin:ReadCache`, `admin:ReadHealth`, `admin:ReadConfig`, etc.). These
statements grant nothing — the IAM engine never evaluates them — so the
default admin roles installed into a fresh database are **silently
narrower than their declarations suggest**. Additionally,
`packages/control-plane/internal/iam/managed.go` carries a parallel set
of phantom-referencing policy definitions that no Go code actually reads
(zero `grep` hits for `iam.ManagedPolicies`); the file is dead code
mirroring an earlier version of the seed.

The product symptom is the inverse-audit finding fixed by commit `83713d91`:
frontend pickers and IAM tooling advertise action names that the policy
engine cannot resolve. The fix shipped in `83713d91` patched one of three
affected layers (frontend `ACTION_MAP`); this story addresses the root
cause.

## 2. User story

**As a** Nexus Gateway platform engineer (and downstream as an operator
writing IAM policies or SIEM filters),
**I want** a single canonical resource × verb taxonomy that the IAM engine,
audit log, SIEM bridge, and admin UI all derive from,
**so that** `virtualKey.create` means the same thing in every layer, default
managed policies grant what they appear to grant, and adding a new admin
operation cannot fall out of sync with any of its sibling layers without
failing the build.

## 3. Design decisions

### 3.1 Single source of truth: `packages/shared/security/iam.Catalog`

A new package `packages/shared/security/iam` exposes a compiled-in `Catalog`
variable that pairs every admin resource with its closed set of verbs and
NRN template. The package lives in `packages/shared/` because both Control
Plane and Nexus Hub consume it (Hub uses it indirectly via the SIEM
classifier, which today reads audit rows' free-form `EntityType`).

```go
// packages/shared/security/iam/catalog.go (new)
package iam

type Verb string

// Closed verb taxonomy. New verbs require explicit addition here.
const (
    VerbCreate      Verb = "create"
    VerbRead        Verb = "read"
    VerbUpdate      Verb = "update"
    VerbDelete      Verb = "delete"
    VerbApprove     Verb = "approve"
    VerbReject      Verb = "reject"
    VerbRevoke      Verb = "revoke"
    VerbRenew       Verb = "renew"
    VerbToggle      Verb = "toggle"
    VerbExport      Verb = "export"
    VerbSimulate    Verb = "simulate"
    VerbForceResync Verb = "force-resync"
    VerbWrite       Verb = "write"   // see §3.4 (write vs update)
    VerbManage      Verb = "manage"  // see §3.4 (manage vs CRUD)
)

type Service string

const (
    ServiceGateway    Service = "gateway"    // AI traffic plane
    ServiceAdmin      Service = "admin"      // IAM / audit / settings
    ServiceCompliance Service = "compliance" // hooks / rulepacks / exemptions
)

type ResourceDef struct {
    Name    string  // canonical kebab-case: "virtual-key"
    Service Service // routes the NRN template
    Verbs   []Verb  // closed set per resource
}

var Catalog = []ResourceDef{ /* full table — see §4 */ }
```

### 3.2 Action identifier format: `admin:<resource>.<verb>`

The IAM action surface stays a single namespace (`admin:`) because every
admin operation runs through one API and one audit pipeline.

The action body changes from `CamelCaseConcat` to dotted kebab-case:

```
old:  admin:CreateVirtualKey
new:  admin:virtual-key.create

old:  admin:ExportAuditLog
new:  admin:audit-log.export

old:  admin:SimulateRoutingRule
new:  admin:routing-rule.simulate
```

The IAM engine's `globMatch` (in `packages/control-plane/internal/iam/nrn.go`)
already treats `.` as a literal, so the existing wildcard semantics
(`admin:*`, `admin:Read*`) carry over (`admin:*` still matches everything;
the equivalent of `admin:Read*` becomes the more expressive
`admin:*.read`).

**Why the change is worth doing.** Three properties improve:

- **Direct SIEM alignment.** `admin:virtual-key.create` strips to
  `virtual-key.create`, which is identical to the SIEM `eventType` derived
  from audit (`<EntityType>.<Action>`). Operators write the same string in
  IAM policy and in SIEM filter.
- **2D wildcards.** `admin:virtual-key.*` (all verbs on one resource) and
  `admin:*.read` (read-any-resource) are first-class. The previous form
  required either CRUD-line enumeration or `admin:Create*`-style prefix
  globs that broke for domain verbs (`admin:Simulate*` matches only one
  resource accidentally).
- **Single casing rule.** kebab-case lowercase across the entire surface,
  matching HTTP paths, NRN resource segments, and the canonical
  `EntityType` we adopt in §3.3.

### 3.3 Audit invariants

`audit.Entry` fields `EntityType` and `Action` become **typed-by-convention**
values drawn from `Catalog`:

- `EntityType` ∈ `{ resource.Name | resource ∈ Catalog }`
- `Action` ∈ `{ string(verb) | verb ∈ resource.Verbs }` (the verb must be
  declared for that specific resource)

Enforced by replacing every literal `ae.Action = "create"` /
`ae.EntityType = "virtualKey"` call site with a constructor that takes
catalog identifiers:

```go
ae := audit.EntryFor(c, iam.ResourceVirtualKey, iam.VerbCreate)
ae.EntityID = vk.ID
ae.AfterState = vk
h.Audit.LogObserved(ctx, ae)
```

`audit.EntryFor` returns the same `audit.Entry`, with `EntityType` /
`Action` set from the catalog. Free-form assignment is eliminated as a
code path; §3.6 documents the CI lint that forbids regressions.

The `virtualKey`/`virtual_key` duplicate is resolved to canonical
`virtual-key`. The complete migration table is in §5.

### 3.4 Verb deduplication decisions

Two verb-level inconsistencies in the current code need resolution rather
than carry-over:

- **`Write` vs `Update` for `settings` / `observability` / `node`.** The
  semantic delta is unclear in the code; `admin:WriteSettings` and
  `admin:UpdateSettings` both appear. **Decision:** retain `write` only
  where the operation persists configuration with broader blast radius
  than a normal CRUD update (e.g., `node:write-override` — overriding a
  Thing's shadow config bypasses normal config flow). Default CRUD on
  settings uses `update`. P3 audits each site and pins the verb explicitly.
- **`Manage` for `alerts`.** `admin:ManageAlerts` today gates create /
  update / delete / acknowledge on alerts undifferentiated. **Decision:**
  split into the standard verbs (`alert.create`, `alert.update`,
  `alert.delete`, `alert.acknowledge`) during P3. The `manage` verb is
  retained in the catalog enum only as a transitional reservation for any
  future coarse-grained operation that legitimately spans multiple verbs.
- **`Hook` vs `Hooks`.** `admin:ReadHook` AND `admin:ReadHooks` both
  exist (same for Update). **Decision:** canonical resource is `hook`
  (singular, matches the table name); the plural variants are deleted
  during P3.
- **`RulePacks` (plural).** Canonical: `rule-pack` (singular).
- **`Revocations` (plural).** Canonical: `revocation` (singular).
- **`Analytics` and `QuotaAnalytics`.** Both kept as distinct resources
  (`analytics`, `quota-analytics`) because they map to disjoint
  read-paths; `Analytics` stays as a mass-noun-style resource name.

### 3.5 NRN derivation

Each `ResourceDef` carries a `Service`. The NRN template is mechanical:

```
nrn:nexus:<service>:<scope>:<resource>/<id>
```

`<service>` is the catalog `Service` field; `<resource>` is the catalog
`Name` field. The `<scope>` segment (orgs / projects / global) and `<id>`
are caller-provided. `(*ResourceDef).NRN(scope, id string) string`
constructs the full string.

The existing managed policies in `managed.go` already use
`nrn:nexus:gateway:*:*/*` and similar wildcards; those continue to work
unchanged because the wildcard semantics are unaffected.

### 3.6 CI consistency gates (locked in P6)

The taxonomy is enforced by **three** Go tests + one TypeScript test, all
required to be green for merge:

1. **`TestCatalogActionsMatchHandlerLiterals`** — greps every `iamMW("...")`
   string literal under `packages/control-plane/internal/handler/*.go`
   and asserts each one is the result of some `resource.Action(verb)`
   call (post-P3 there should be zero raw `admin:`-prefixed string
   literals at iamMW call sites).
2. **`TestSeededPoliciesMatchCatalog`** — validates seeded IAM policies
   against the catalog. Implementation in P6 picks whichever fits the
   existing test infra better: (a) regex/AST scan over
   `tools/db-migrate/seed/seed.ts` source for any `admin:...` string
   asserting each matches `^admin:[a-z][a-z0-9-]*\.[a-z][a-z-]*$` and
   resolves to a catalog entry, or (b) integration test that runs
   `prisma db seed` against an isolated test DB and queries
   `IamPolicy.document.Statement[].Action[]`. Either approach catches
   "managed-policy refers to action the engine can't resolve" — the
   class of bug that landed in production seed pre-E43.
3. **`TestAuditEntitiesAndActionsMatchCatalog`** — greps every
   `ae.EntityType = "..."` / `ae.Action = "..."` literal in
   `handler/*.go` (post-P4 there should be none — `audit.EntryFor` is
   the only allowed construction path) and fails build if any are
   found.
4. **`usePermission.coverage.test.ts`** (extended) — already verifies
   ACTION_MAP keys; P6 extends it to also assert each ACTION_MAP target
   string matches the new `^admin:[a-z][a-z0-9-]*\.[a-z][a-z-]*$` shape.

A `golangci-lint` rule (`forbidigo`) blocks future `Entry{Action: "..."}`
literal construction outside `audit.EntryFor`. This is preferred over a
runtime panic — the goal is to surface drift at compile time.

## 4. Canonical Catalog table

Each row is a `ResourceDef` in P2's `catalog.go`. The "Replaces" column
maps the old vocabulary to the new across all four layers.

| Resource (canonical) | Service | Verbs | Old IAM actions replaced | Old Audit EntityType replaced |
|---|---|---|---|---|
| `provider` | gateway | create, read, update, delete | `admin:{Create,Read,Update,Delete}Provider` | `provider` |
| `model` | gateway | create, read, update, delete | `admin:{C,R,U,D}Model` | `model` |
| `credential` | gateway | create, read, update, delete | `admin:{C,R,U,D}Credential` | `credential`, `credentialKey` |
| `virtual-key` | gateway | create, read, update, delete, approve, reject, revoke, renew | `admin:{C,R,U,D}VirtualKey` + `admin:{Approve,Reject,Revoke,Renew}VirtualKey` | `virtualKey`, `virtual_key` |
| `routing-rule` | gateway | create, read, update, delete, simulate | `admin:{C,R,U,D}RoutingRule` + `admin:SimulateRoutingRule` | `routingRule` |
| `hook` | compliance | create, read, update, delete | `admin:{C,R,U,D}Hook` + `admin:{Read,Update}Hooks` (deduplicated) | `hookConfig` |
| `rule-pack` | compliance | read, update | `admin:{Read,Update}RulePacks` | `rulePack`, `rulePackInstall`, `rulePackOverrides` |
| `quota-policy` | gateway | create, read, update, delete | `admin:{C,R,U,D}QuotaPolicy` | `quota_policy` |
| `quota-override` | gateway | create, read, update, delete | `admin:{C,R,U,D}QuotaOverride` | `quota_override` |
| `quota-analytics` | gateway | read | `admin:ReadQuotaAnalytics` | _(read-only — no audit row)_ |
| `analytics` | gateway | read | `admin:ReadAnalytics` | _(read-only)_ |
| `traffic-log` | gateway | read | `admin:ReadTrafficLog` | _(read-only)_ |
| `audit-log` | admin | read, export | `admin:{Read,Export}AuditLog` | `adminAuditLog` |
| `user` | admin | create, read, update, delete | `admin:{C,R,U,D}User` | `nexusUser` |
| `api-key` | admin | create, read, update, delete | `admin:{C,R,U,D}ApiKey` | `apiKey` |
| `organization` | admin | create, read, update, delete | `admin:{C,R,U,D}Organization` | `organization` |
| `project` | admin | create, read, update, delete | `admin:{C,R,U,D}Project` | `project` |
| `iam-policy` | admin | create, read, update, delete | `admin:{C,R,U,D}IamPolicy` | `iamPolicy`, `iamPolicyAttachment` |
| `iam-group` | admin | _(none in current code)_ | _(none — see out-of-scope)_ | `iamGroup`, `iamGroupMembership`, `iamGroupPolicyAttachment` |
| `agent-device` | gateway | create, read, update, delete | `admin:{C,R,U,D}AgentDevice` | `agentDevice` |
| `device-group` | gateway | create, read, update, delete | `admin:{C,R,U,D}DeviceGroup` | `deviceGroup`, `deviceGroupMembership`, `deviceGroupPolicy`, `deviceGroupPolicyRule` |
| `device-assignment` | gateway | _(read-only, internal)_ | _(none — internal)_ | `deviceAssignment` |
| `agent-exemption` | compliance | read, update, delete | `admin:{Read,Update,Delete}AgentExemption` | `agentExemption` |
| `compliance-exemption` | compliance | read, update, delete | `admin:{Read,Update,Delete}ComplianceExemption` | `complianceExemptionRequest`, `complianceActiveExemption`, `exemptionRequest` |
| `kill-switch` | admin | read, toggle | `admin:{Read,Toggle}KillSwitch` | `complianceKillswitch` |
| `ai-guard-config` | admin | read, update | `admin:{Read,Update}AIGuardConfig` | `aiGuardConfig` |
| `revocation` | admin | read | `admin:ReadRevocations` | _(read-only)_ |
| `alert` | admin | create, read, update, delete, acknowledge | `admin:ReadAlerts` + `admin:ManageAlerts` (split, see §3.4) | _(TBD — alerts audit not currently wired)_ |
| `observability` | admin | read, write | `admin:{Read,Write}Observability` | `observabilityConfig`, `observabilityRetention` |
| `settings` | admin | read, update, write | `admin:{Read,Update,Write}Settings` (see §3.4) | `settings`, `settings.credential_reliability`, `cache`, `agentSettings`, `agentShutdownWarning`, `cacheNormaliserConfig`, `deviceAuthSettings`, `geminiCacheConfig`, `payloadCaptureConfig`, `setupState`, `siemConfig`, `ssoConfig`, `streamingComplianceConfig` |
| `node` | admin | read, force-resync, write-override | `admin:ForceResyncNode`, `admin:WriteNodeOverride` | `node`, `thing`, `configSync`, `scheduledJob`, `enrollmentToken` |
| `dsar` | compliance | read | _(none in iamMW today — guarded by `admin:ReadAuditLog`)_ | `dsarRequest` |
| `compliance-report` | compliance | read | _(guarded by `admin:ReadAuditLog`)_ | `complianceReport` |
| `interception-domain` | compliance | read, update | _(guarded by `admin:ReadSettings`)_ | `interceptionDomain`, `interceptionPath` |
| `model-pricing` | gateway | _(read-only, via model)_ | _(guarded by `admin:ReadModel`)_ | `modelPricing` |
| `diagnostic-mode` | admin | read, update | _(guarded by `admin:ReadSettings`)_ | `diagnosticMode` |
| `nexus-session` | admin | _(internal)_ | _(none — internal)_ | `nexusSession` |

**Notes on the table.**

- Resources tagged _"read-only — no audit row"_ have no `Catalog.Verbs`
  entry beyond `read`; they correspond to GET-only endpoints whose access
  is gated but no mutation event is emitted.
- Several rows in the right column collapse multiple legacy `EntityType`
  values into a single canonical resource. Example: `settings` now covers
  the entire `system_metadata` key-value surface (the prior code split it
  into 10+ pseudo-EntityTypes per setting key). Audit rows after P4 will
  carry a `settingKey` field in `BeforeState`/`AfterState` to preserve the
  per-key dimensionality without polluting `EntityType`.
- `iam-group` resource exists in audit but has no `iamMW(...)` guards in
  current code; P3 keeps it as a catalog entry with the empty `Verbs` slice
  (read-only / internal) until §6.2 cleanup adds explicit guards.

## 5. Per-layer migration mapping

| Layer | Today | After E43 |
|---|---|---|
| **Catalog data** | `var allAdminActions = []string{...}` (flat) | `var Catalog = []ResourceDef{...}` in `shared/iam`; `allAdminActions` becomes `iam.AllActions()` |
| **Route guard** | `iamMW("admin:CreateVirtualKey")` | `iamMW(iam.ResourceVirtualKey.Action(iam.VerbCreate))` (computed const at init) |
| **Seeded managed policies** | `tools/db-migrate/seed/seed.ts` block 13 (legacy, with 10+ phantoms) + block 15c (E39, valid actions); `iam/managed.go` (dead code) | Block 13 deleted; block 15c rewritten to use `admin:<resource>.<verb>` strings; `managed.go` deleted |
| **Audit constructor** | `ae := auditFromContext(c); ae.Action = "create"; ae.EntityType = "virtualKey"` | `ae := audit.EntryFor(c, iam.ResourceVirtualKey, iam.VerbCreate)` |
| **SIEM derivation** | `entityType + "." + action` (unchanged code) | identical code; output now consistent because inputs are canonical |
| **Frontend ACTION_MAP** | `'virtual-key:create': 'admin:CreateVirtualKey'` | `'virtual-key:create': 'admin:virtual-key.create'` |
| **Frontend pickers** | hardcoded `RESOURCE_DEFS` / `COMMON_ACTION_GROUPS` | `iamApi.getActionCatalog()` → render |
| **OpenAPI** | (no spec for catalog) | new `docs/users/api/openapi/admin/e43-s1-iam-action-catalog.yaml` |

## 6. Phase plan

Phases are work order, not compatibility layers. Each phase ends in a
green build with no transitional shims.

### P1 — _this document + architecture doc update_ (no code)

Deliverables:

- `docs/developers/specs/e43/e43-s1-iam-canonical-taxonomy.md` (this file)
- New `## IAM Canonical Action Taxonomy (E43)` section in
  `docs/users/product/architecture.md`, anchored after the existing IAM section,
  with a backreference to this SDD.

Exit gate: user review + approval before P2 begins.

### P2 — Foundation: `packages/shared/security/iam` package + tests (no callers)

Deliverables:

- New `packages/shared/security/iam/catalog.go` with the §4 catalog table,
  `Verb` / `Service` / `ResourceDef` types, and helper methods
  (`MustFind`, `(r *ResourceDef).Action(verb)`, `.NRN(scope, id)`,
  `AllActions()`, `SIEMEventType(entityType, action)`).
- Per-resource convenience vars (`ResourceProvider`, `ResourceVirtualKey`,
  …) so handlers don't repeat `MustFind`.
- `catalog_test.go` covering: helper outputs, catalog self-consistency
  (every resource's verbs are unique; no duplicate resource names; all
  service values are members of the closed enum).
- Module: lives in the existing `packages/shared` Go module (no new
  go.mod). Vetted-dependency policy is unaffected (stdlib only).

Exit gate: `go test ./packages/shared/security/iam/...` green; build green; no
production caller is wired to the new package yet.

### P3 — IAM action layer migration (atomic)

Deliverables:

- `packages/control-plane/internal/handler/admin_extras.go` —
  `allAdminActions` replaced with `iam.AllActions()` call (drops the
  90-line literal). Phantom-only `GetMePermissions` behavior unchanged.
- All `iamMW("admin:CamelCase")` literals in `handler/*.go` (~322 call
  sites across ~95 unique actions) replaced with
  `iamMW(iam.ResourceX.Action(iam.VerbY))`. Migration is mechanical:
  exact mapping in §4 table.
- `packages/control-plane/internal/iam/managed.go` — **deleted outright**.
  This file is dead code today (no Go consumers; `iam.ManagedPolicies`
  has zero `grep` hits beyond the definition itself). The actual source
  of truth for seeded managed policies is
  `tools/db-migrate/seed/seed.ts`. Per CLAUDE.md Development-phase
  policy (no parallel "legacy" paths), `managed.go` and the related
  `ManagedPolicy` struct are removed in the same commit as the rest of
  P3.
- `tools/db-migrate/seed/seed.ts` — **two seed blocks** need updating:
  - **Block 13** (`IAM Managed Policies`, ~lines 849–908) — legacy
    policies referencing 10+ phantom actions (`admin:CreatePolicy`,
    `admin:ReadRedaction`, `admin:InvalidateCache`,
    `admin:RefreshRuntimeCache`, `admin:ReadQuota`, `admin:ReadCache`,
    `admin:ReadHealth`, `admin:ReadConfig`, etc.). Per CLAUDE.md
    Development-phase policy, this block is **deleted outright** rather
    than rewritten — its purpose is superseded by block 15c.
  - **Block 15c** (`Managed Policies (action names match iamMW())`,
    ~lines 979–1095) — the E39-era canonical block. Rewritten to use
    the new `admin:<resource>.<verb>` action format. Existing action
    strings here are valid (no phantoms in this block); the change is
    purely format.
- Dev DB re-seed via `cd tools/db-migrate && npx prisma migrate reset
  --force` then `npx prisma db seed`. The reset is the canonical
  Development-phase recovery for schema/seed changes.
- `packages/control-plane-ui/src/hooks/usePermission.ts` — `ACTION_MAP`
  target strings updated to new format (`admin:provider.create`, etc.).
  Keys unchanged; coverage test continues to pass.
- `tools/db-migrate/prisma/seed.ts` — re-seed any DB-persisted IAM policy
  rows that reference legacy action strings (none in shipped seeds; verify
  in dev DB and reset if needed).
- Updated Go unit tests covering the IAM engine + handler routes.

Exit gate: `go test ./packages/control-plane/...` green; CP starts
cleanly; admin auth + a representative iamMW-guarded route works
end-to-end (manual smoke via `tests/lib/auth.sh`).

### P4 — Audit layer migration

Deliverables:

- `packages/control-plane/internal/audit/writer.go` — add `EntryFor(c
  echo.Context, resource *iam.ResourceDef, verb iam.Verb) audit.Entry`
  constructor.
- Every `ae.Action = "..." / ae.EntityType = "..."` assignment in
  `packages/control-plane/internal/handler/*.go` rewritten to
  `audit.EntryFor`. Estimated ~120 call sites.
- The `virtualKey` / `virtual_key` divergence resolved (single canonical
  `virtual-key`).
- Settings-key dimensionality preserved via `BeforeState`/`AfterState`
  payload (§4 note) rather than `EntityType` proliferation; per-setting
  EntityTypes (e.g. `siemConfig`, `ssoConfig`, `agentSettings`) all
  collapse to `settings`.
- Audit unit + integration tests updated.
- New integration test `TestSIEMClassifyAdminEventCanonicalShape` in
  `packages/nexus-hub/internal/observability/siem/classify_test.go` (or the equivalent
  CP-side test if classify input is mocked there): issues each
  representative verb on each representative resource via the audit
  writer, drains the SIEM-formatted output, asserts `eventType` matches
  `^[a-z][a-z0-9-]*\.[a-z][a-z-]*$` and the resource segment is a member
  of `iam.Catalog`. This is the AC-3 verification, automated.
- **Heads-up for operators (acceptable per Development-phase policy, but
  flagged):** SIEM `eventTypes` whitelist strings configured before P4
  (e.g. `virtualKey.create`) stop matching after P4 because the bridge
  now emits `virtual-key.create`. Any existing dev `siem.config` entries
  must be manually re-entered in the new format until P5 ships the
  dropdown picker. No production traffic exists yet, so the only
  affected configurations are dev-local.

Exit gate: `go test ./packages/control-plane/... ./packages/nexus-hub/...`
green; `TestSIEMClassifyAdminEventCanonicalShape` covers AC-3.

### P5 — Catalog endpoint + frontend rewrite + SIEM filter UX

Deliverables:

- Backend: new handler `GET /api/admin/iam/action-catalog` in
  `admin_extras.go`. Auth: any admin user (no `iamMW` gate — this is
  metadata, matches the existing `/me/permissions` pattern). Response
  shape mirrors §4 (array of `{ type, nrn, actions: [{ name, verb }] }`).
- `docs/users/api/openapi/admin/e43-s1-iam-action-catalog.yaml` with paths,
  request/response schemas, error responses, examples.
- Frontend: `iamApi.getActionCatalog()` service + `ActionCatalog` /
  `ResourceCatalogEntry` types.
- Rewrite `packages/control-plane-ui/src/pages/iam/IamSimulator.tsx`:
  delete `RESOURCE_DEFS` constant; fetch catalog via `useApi`; render
  resource dropdown + filtered action dropdown from response.
- Rewrite `packages/control-plane-ui/src/pages/iam/IamPolicyEditorPage.tsx`:
  delete `useCommonActionGroups` hardcoded list; auto-generate two quick-
  add groups per resource (`<resource> full access` covering all verbs,
  `<resource> read-only` covering verbs containing `read`). Keep the
  wildcard "all admin (admin:*)" group as the only hand-written entry.
- SIEM `eventTypes` whitelist UI (admin settings page) — replace
  free-text input with multi-select pulling the same catalog (combined
  with `traffic.*` events from the existing classifier output).

Exit gate: `vitest run`, `go test`, and `tsc --noEmit` all green; manual
verification: open `/iam/simulator` and `/iam/policies/new`, confirm the
dropdowns now derive from the API and no phantom actions are present.

### P6 — CI consistency gate + lint

Deliverables:

- `packages/control-plane/internal/iam/catalog_consistency_test.go` —
  the three Go tests from §3.6 (handler literals, seeded policies, audit
  literals).
- `packages/control-plane/.golangci.yml` — add `forbidigo` rule blocking
  `audit.Entry{Action:` literal construction outside `audit.EntryFor`.
- `packages/control-plane-ui/src/hooks/usePermission.coverage.test.ts` —
  extend to also assert each ACTION_MAP target matches
  `/^admin:[a-z][a-z0-9-]*\.[a-z][a-z-]*$/`.

Exit gate: all four tests green; `golangci-lint run` produces no new
warnings on touched files; `git grep` for known phantom action strings
returns empty.

## 7. Acceptance criteria

- **AC-1:** A single `packages/shared/security/iam.Catalog` is the only place
  resource names, verbs, and NRN templates appear as data in this
  codebase. Every IAM action string in IAM policies, route guards, and
  audit logs is computed from this catalog at init time, not typed as a
  string literal.
- **AC-2:** Audit `EntityType` is a member of the canonical resource set;
  `virtualKey`/`virtual_key` duplication is removed.
- **AC-3:** SIEM `eventType` for a representative admin write is exactly
  `<resource>.<verb>` (e.g., `virtual-key.create`) — verified by an
  end-to-end test that issues a CP write and reads the SIEM-formatted
  output.
- **AC-4:** The IAM policy editor and IAM simulator UI present zero
  action strings that the IAM engine would not resolve; both pages
  source their dropdowns from `GET /api/admin/iam/action-catalog`.
- **AC-5:** Managed policies seeded into a fresh database grant exactly
  the actions claimed in their description; no statement references a
  phantom action.
- **AC-6:** Adding a new admin operation (e.g. `revoke` on a new
  resource) requires: (1) catalog edit, (2) route handler + iamMW call,
  (3) audit `EntryFor` call. Forgetting any of the three fails CI by
  P6's consistency tests.
- **AC-7:** Frontend `usePermission.coverage.test.ts` passes with all
  ACTION_MAP target strings matching the canonical regex.

## 8. Out of scope

- **Tenant-scoped resources (NRN `<scope>` segment).** The org / project
  scoping in NRN is preserved but not redesigned. A future story may
  introduce `Catalog.ScopeTemplate` if multi-tenant action shapes
  diverge.
- **`iam-group` resource verbs.** Catalog row exists but `Verbs` is
  empty; explicit guards on group CRUD remain a follow-up. The data
  layer is wired (audit emits `iamGroup` today).
- **Renaming user-visible field names** (e.g., the form-label "Action"
  in IAM Policy Editor). The UI strings stay i18n-localized; this story
  only changes the values, not the field labels.
- **Backward-compatible old-action support.** Pre-GA per CLAUDE.md
  Development-phase policy. Any dev DB carrying old-format policies is
  re-seeded; no shim layer.
- **Audit row backfill.** Pre-GA — historical audit rows in dev are
  discarded. Production carries no traffic yet.

## 9. Risks

- **Mechanical-but-large P3.** ~322 `iamMW` call sites + ~120 audit
  assignment sites = ~440 mechanical edits across the handler dir. Each
  is a string-replace, but the volume invites accidental typos. Mitigation:
  P6 CI gate catches any drift; P3 should run `gofmt`, `golangci-lint`,
  and the full handler test suite before commit.
- **`globMatch` semantics on `.`.** `.` is a literal char today; no
  hidden behavior changes. Verified by reading
  `packages/control-plane/internal/iam/nrn.go:112` — the implementation
  is a simple `strings.Split(pattern, "*")` walker. P2 includes a
  `catalog_test.go` test exercising `admin:virtual-key.*` and
  `admin:*.read` patterns against the engine.
- **Seeded-policy semantic drift.** The 10+ phantom action strings in
  `tools/db-migrate/seed/seed.ts` block 13 (`admin:CreatePolicy`,
  `admin:InvalidateCache`, `admin:RefreshRuntimeCache`,
  `admin:ReadRedaction`, etc.) currently grant nothing — so any role
  whose policy contains only phantoms ends up with a narrower effective
  permission set than the seed file appears to declare. Block 13 is
  superseded by block 15c (E39 canonical), but both are seeded today.
  **Decision:** delete block 13 outright in P3 (no semantic
  preservation — its declared grants were never actually in effect).
  Mitigation: P3 commit must include a per-role permission diff
  (super-admin / security-admin / compliance-admin / developer / viewer
  / member) showing exactly which actions each role _effectively_ has
  before and after P3, derived from `iam.Evaluate` against the seeded
  policy set. The user approves before merge.
- **`managed.go` deletion safety.** The file has zero Go consumers and
  the seed runs from `seed.ts`, so deletion should be a no-op at
  runtime. Mitigation: P3's CI run includes `go build ./...` after
  deletion to catch any unexpected import edges.
- **`Hook` plural deduplication.** `admin:ReadHook` is used by some
  callers and `admin:ReadHooks` by others. Mitigation: P3 audit phase
  reads each call site and confirms semantic equivalence before
  collapsing to `admin:hook.read`.

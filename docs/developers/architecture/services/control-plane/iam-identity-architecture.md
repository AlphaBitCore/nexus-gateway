# IAM & identity

IAM is the authorization layer for the Control Plane admin API. A canonical
resource/verb taxonomy feeds three consumers that all speak the same action
strings: the policy engine that decides allow or deny, the request-time
middleware that gates every admin route, and the admin UI that gates its nav
items and routes. Keeping those three in lockstep on one action string is what
makes a permission grant mean the same thing end to end.

## Canonical catalog

`packages/shared/identity/iam/` is the single source of truth for the resource
type set, the verb set permitted on each resource, the action-string format, the
NRN template, and the SIEM event-type format. A `ResourceDef` carries a
kebab-case `Name`, an owning `Service`, and a closed list of permitted `Verbs`.
Five services partition the catalog — `gateway`, `compliance`, `agent`,
`platform`, and `iam` — and become the second segment of every NRN. The verb set
is closed: standard CRUD plus lifecycle verbs such as `approve`, `revoke`,
`renew`, `toggle`, `simulate`, `export`, `emergency-enable`, `probe`, `rotate`,
`import`, `fulfill`, and `enroll`.

From a `ResourceDef` the catalog derives every identifier mechanically:
`Action(verb)` returns `admin:<resource>.<verb>` and panics at startup if the
verb is not declared on the resource; `NRN(scope, id)` returns
`nrn:nexus:<service>:<scope>:<resource>/<id>`; and `SIEMEventType` returns
`<resource>.<verb>` — the action body with the `admin:` prefix stripped, so an
operator sees the same string in an IAM policy and in a SIEM filter. Handlers
must construct identifiers through these helpers; hand-typed `admin:…` strings
and audit-entity literals are prohibited by CI consistency gates. `AllActions`
enumerates the full action set and feeds the per-principal permissions endpoint.

## NRN scheme

`packages/control-plane/internal/identity/iam/nrn.go` defines the Nexus Resource
Name: `nrn:nexus:<service>:<scope>:<resourceType>/<resourceID>`. A policy's
`Resource` patterns and a request's resource are both NRNs, matched segment by
segment. Service, resource type, and resource id match with glob wildcards (`*`
matches any segment, `gpt-*` matches `gpt-4o`). The scope segment matches
hierarchically: a pattern of `org-acme` matches both `org-acme` and
`org-acme/engineering`, so an organisation-scoped grant covers its descendant
projects — see [organization-hierarchy-architecture.md](organization-hierarchy-architecture.md).

`BuildRequestNRNForAction` derives the NRN a request is checked against from the
action alone: it parses the resource and looks up the owning service in the
catalog, producing `nrn:nexus:<service>:*:<resource>/*` so resource-scoped
policies evaluate correctly; a non-canonical action falls back to a
fully-wildcarded NRN. `BuildDeviceCandidateNRNs` expands a device action into the
unscoped resource plus one `group:<group-id>/<device-id>` candidate per group the
device belongs to.

## Policy model and evaluation

A policy is an AWS-style document — a `Version` and a list of `Statement`s, each
with an `Effect` of `Allow` or `Deny`, an `Action` list, a `Resource` list, and
an optional `Condition` block. Action and resource lists accept either a single
string or an array. The engine
(`packages/control-plane/internal/identity/iam/engine.go`) evaluates a statement
as a match when its action pattern globs the request action, any of its resource
patterns matches the request resource, and its conditions hold. The decision
order is explicit `Deny` over explicit `Allow` over default deny; a `Deny` that
matches any candidate resource denies the whole request, so a scoped `Deny` can
override a broader `Allow`.

`EvaluateMulti` is the candidate-list form: the same request can be authorised
by an unscoped grant or by a group-scoped grant, so the device-group candidates
and the unscoped resource are all scanned and any matching `Allow` authorises the
call. Conditions
(`packages/control-plane/internal/identity/iam/conditions.go`) support operators
including `StringEquals`, `StringLike`, `IpAddress`, the numeric comparators, and
the date comparators, evaluated against a request context that carries
`nexus:SourceIp` and `nexus:CurrentTime`.

Policies are loaded per principal through a `PolicyLoader`; the production loader
reads a principal's direct policy attachments unioned with the policies of every
group the principal belongs to (enabled policies only). Results are cached: an
in-process L1 cache always, and an optional Redis L2 cache when the engine is
built with the Redis option. Only non-empty policy sets are cached, and the
engine exposes per-principal and global cache invalidation. Each evaluation
reports whether it was a cache hit, which feeds the `cache` label on the
`iam.eval_total` metric.

### Cache-coherency bound on a multi-replica Control Plane

The L1 cache is in-process and the L2 cache is Redis. On every IAM mutation the
handler calls `InvalidateCache`, which clears the local L1 map and deletes the
matching Redis L2 keys. Consistent with the "Redis is a pure cache, no pub/sub"
model, this invalidation is **not broadcast to other Control Plane replicas**:
each replica's in-process L1 keeps serving the pre-change policy until its entry
expires (the L1 TTL, currently 10s). So on an HA deployment a privilege change
can take up to the L1 TTL to be reflected uniformly across all replicas — a
bounded, time-limited stale-**grant** window.

This is mitigated for the security-critical case (privilege *reduction*): the
same mutation handlers that change a policy or group also fan out a `scope=user`
token revocation over MQ to every affected principal, which rejects the existing
token cluster-wide on its next refresh regardless of L1 state. The residual
exposure is therefore limited to changes that do not invalidate the token (and
to deployments where the MQ revocation channel is wired). If immediate
cross-replica coherency is ever required, the correct mechanism is a short MQ
invalidation signal (not Redis pub/sub), keeping the cache pure.

## Principals, groups, and managed roles

IAM stores attachments and group memberships keyed on a principal type and id.
The principal type is `nexus_user` (a dashboard session's `admin_user` type is
mapped to it before any IAM lookup) or `api_key`. A principal receives policies
either by a direct attachment or by membership in a group that has policies
attached.

Built-in groups and managed policies are seeded — groups such as `super-admins`,
`provider-admins`, `security-admins`, and `viewers`, and managed policies such as
`NexusSuperAdmin` (every action on every resource), `NexusProviderAdminAccess`,
`NexusSecurityAdminAccess`, `NexusViewerAccess` (the read-only set across the
catalog), and `NexusIncidentResponse`. The catalog documents which service each
role owns — the provider-admin role owns the gateway service, the security-admin
role owns compliance and agent, and the viewer role reads across the catalog.

The `agent-device` resource (service: `agent`) exposes fleet-management verbs: `create`, `read`, `update`, `delete`, and `force-resync`.

### Grant ceiling (no privilege escalation)

Authoring an IAM policy or conferring one (attaching a policy to a principal or
group, or adding a principal to a group) is bounded by the **caller's own**
permissions: a principal may never grant a permission it does not itself hold.
Without this, a delegated "IAM operator" holding only `iam-policy.*` /
`iam-group.*` could author or attach an `admin:*` policy to itself (or a group it
belongs to) and silently become super-admin.

The boundary is enforced at the five privilege-conferring chokepoints —
`CreateIAMPolicy`, `UpdateIAMPolicy` (when the document changes),
`AttachPrincipalPolicy`, `AttachIAMGroupPolicy`, and `AddIAMGroupMember` (which
checks every policy attached to the joined group) — via
`Engine.PrincipalCoversDocument` (`internal/identity/iam/boundary.go`). For each
`Allow` statement in the candidate document, the caller must evaluate `Allow` for
every concrete catalog action the statement matches (so a wildcard like `admin:*`
expands across the catalog and each expanded action must be held) **and** each
literal action token (so a bare `*` or a non-catalog identifier is bounded
directly). Candidate `Deny` statements and `Condition`s are ignored — that is
conservative, it can only reject a grant, never permit one. A super-admin (whose
own policy `Allow`s every action on the universal NRN) covers any document and
passes with no special-casing. The check fails **closed**: a missing engine
(`503`) or an evaluation error (`500`) blocks the grant rather than skipping it,
and an uncovered permission returns `403` with code `PRIVILEGE_ESCALATION_BLOCKED`.

The conferring routes remain gated on the existing `iam-policy` / `iam-group`
verbs; the ceiling is an **independent** subset check layered on top, so it closes
the escalation regardless of which verb a delegated policy granted. There is no
dedicated higher "grant" verb — it would not close any attack surface the ceiling
does not already close, while adding catalog, seed, and UI surface (less-is-more).

The `assistant` resource (service: `iam`) exposes two actions: `admin:assistant.read`
(required to issue chat/models/sessions GET requests and open the SSE stream)
and `admin:assistant.write` (required to start chat turns, confirm/deny
dangerous-write confirmations, interrupt a turn, and delete sessions). All 9
assistant routes in `RegisterAssistantRoutes` are individually gated —
read verbs on `.read`, mutating verbs on `.write`. The widget itself renders
for every logged-in admin; a principal without the grant is refused by the
server-side IAM gate on its first call (inference cannot start, so the shared
system VK cannot be spent by an ungranted login).

## Request-time enforcement

`packages/control-plane/internal/platform/middleware/iamauth.go` is where a grant
becomes an allow or a 403. Handlers wrap each admin route with
`iamMW(action)`, which is `RequireIAMPermission` bound to the engine and a
catalog action. The middleware requires an authenticated admin (401 otherwise),
derives the request NRN via `BuildRequestNRNForAction` (or a caller-supplied
resource function), maps the session principal type to the IAM type, and
evaluates. On a deny it returns 403
with an `IAM_ACCESS_DENIED` body carrying the action, resource, and reason; every
evaluation increments `iam.eval_total` labelled by decision and cache outcome.

`RequireIAMPermissionForDevice` is the device-scope-aware variant for routes that
act on a single agent-device identified by a path parameter. It resolves the
device's group memberships through a `DeviceGroupLookup` and evaluates against the
candidate NRN list. The resolution fails closed: a nil lookup or a lookup error
leaves the device with no group memberships, so a scoped-policy admin is denied
while a fleet-wide grant still allows the call.

## UI and handler in lockstep

The admin UI gates the same way, against the same action strings. The route
configuration (`packages/control-plane-ui/src/routes/shellRouteConfig.tsx`)
declares an `allowedActions` list per route and per nav item, and the
`RequireRole` guard renders a route only when the signed-in principal's
permissions intersect that list. Those permissions come from the per-principal
permissions endpoint, which evaluates every catalog action for the caller.

Because the UI and the handler both key on the canonical action string, the two
must stay aligned: if a route's `allowedActions` and the handler's `iamMW(action)`
name different actions, the UI shows a page the API then denies — a silent 403.
Any change that adds, moves, renames, or removes an admin endpoint, a nav item,
or a route path therefore has to update both sides to the same action.

## References

- `packages/shared/identity/iam/` — canonical resource/verb/service catalog
- `packages/control-plane/internal/identity/iam/` — engine, NRN, conditions, cache, validator
- `packages/control-plane/internal/identity/iam/boundary.go` — grant ceiling (`PrincipalCoversDocument`, no privilege escalation)
- `packages/control-plane/internal/identity/users/handler/iam_grant_ceiling.go` — ceiling enforcement at the conferring handlers
- `packages/control-plane/internal/platform/middleware/iamauth.go` — request-time enforcement middleware
- `packages/control-plane/internal/identity/users/iamstore/` — policy / group / attachment storage + loader
- `packages/control-plane/internal/identity/users/handler/me.go` — per-principal permissions endpoint
- `packages/control-plane-ui/src/routes/shellRouteConfig.tsx` — UI route + nav `allowedActions`
- `packages/control-plane-ui/src/auth/guards/RequireRole.tsx` — UI permission guard
- `tools/db-migrate/seed/fixtures/IamPolicy.json` + `tools/db-migrate/seed/fixtures/IamGroup.json` (+ `IamGroupPolicyAttachment.json`) — seeded managed policies + built-in groups + their attachments

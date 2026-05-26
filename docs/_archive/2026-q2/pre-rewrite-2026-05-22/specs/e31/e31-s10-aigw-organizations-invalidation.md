# E31 S10 — AI Gateway organizations cache invalidation

**Epic:** 31
**Story:** 10
**Status:** Draft — 2026-04-27
**Requirements:** inline (gap C from e31-s7 introspection coverage matrix)

## User Story

As an admin renaming, re-parenting, or disabling an Organization in the
CP UI, I want the AI Gateway to immediately reflect the change in its
quota / org-tree calculations — instead of holding the stale
`OrgParents` map until the next ai-gateway restart.

## Background

`packages/ai-gateway/internal/pipeline/quota/policy_cache.go` builds an
`OrgParents` map at startup via `PolicyCache.Load`. That cache is read on
every quota-enforced request to walk the chain
project → org → parent-org → ... → root for aggregation. The map is
**only loaded at process start** — no thingclient `config_key` exists for
"the org tree changed".

CP UI's organization CRUD (`packages/control-plane/internal/handler/admin_organizations.go`)
already audits create/update/delete to admin_audit_log but does NOT
notify Hub. Hub does not broadcast anything for the `organizations`
key. ai-gateway's `OnConfigChanged` switch has no case for it.

Result: a rename, re-parent, or disable in CP UI is invisible to
ai-gateway runtime — including for org chains used in quota
enforcement — until the gateway is restarted.

Surfaced by e31-s7 introspection as **gap C**.

## Scope

### In

- New thingclient `config_key`: `organizations`. Pure invalidation
  signal — no payload state. (Same shape as `interception_domains`'s
  `null` payload pattern.)
- ai-gateway `OnConfigChanged` gains `case "organizations"` calling
  `policyCache.Load(ctx)` to rebuild the OrgParents map and the
  quota-policy list (Load already covers both — they share the same
  query). Existing `quota_policies` invalidation also calls Load, so
  the new `organizations` invalidation reuses the same code path.
- CP admin handlers `CreateOrganization`, `UpdateOrganization`,
  `DeleteOrganization` each emit
  `h.Hub.InvalidateConfig(ctx, "ai-gateway", "organizations")` after a
  successful DB write (mirrors how routing-rule and quota-policy CRUD
  fan out today).
- Remove the `"invalidation_note": "no auto-invalidation today"` marker
  from the `cache.policy_cache.org_parents` introspection source in
  `packages/ai-gateway/cmd/ai-gateway/main.go` since the gap is closed
  by this story.

### Out — projects audit

A side-question from the matrix: should `projects` get the same
treatment? Audit conclusion (recorded here so the gap is not re-opened
silently):

- ai-gateway does NOT cache project records. It uses `meta.ProjectID`
  via the VK-meta path (`internal/cachelayer`); when admin renames a
  project, the rename is invisible to ai-gateway because *nothing
  ai-gateway holds in memory references the project name*. Project ID
  is only used for quota chain construction (`internal/quota/chain.go:27`).
- The one edge case: re-parenting a project to a different
  organization. The project's owning VKs cache `vkMeta.OrganizationID`,
  which becomes stale after the move. **CP admin must invalidate the
  affected VKs after re-parenting** — that path already exists for
  every other VK-meta change. We add a brief comment to that effect in
  `admin_organizations.go::UpdateProject` rather than introducing a new
  `projects` config_key. If real-world friction shows up, promote this
  to its own follow-up SDD.

### Out — other

- No DB schema changes.
- No new IAM permissions.
- No CP UI changes (the existing admin pages already drive the CRUD
  endpoints).
- No agent involvement (agent has no quota / org tree).

## Tasks

### T1. CP admin: emit InvalidateConfig

In `packages/control-plane/internal/handler/admin_organizations.go`,
after a successful DB write in each of `CreateOrganization`,
`UpdateOrganization`, `DeleteOrganization`:

```go
if h.Hub != nil {
    h.Hub.InvalidateConfig(c.Request().Context(), "ai-gateway", "organizations")
}
```

The Hub side already supports `InvalidateConfig(thingType, configKey)`
without per-key allowlist (per `admin_extras.go::CacheFlush`'s pattern
of calling it for any string), so no Hub changes are needed.

### T2. ai-gateway: subscribe to `organizations`

In `packages/ai-gateway/cmd/ai-gateway/main.go::OnConfigChanged`, add:

```go
case "organizations":
    if policyCache != nil {
        reloadCtx, reloadCancel := context.WithTimeout(context.Background(), 10*time.Second)
        if err := policyCache.Load(reloadCtx); err != nil {
            logger.Warn("policy cache reload after organizations update failed", "error", err)
        }
        reloadCancel()
    }
    reported[key] = cs
```

(Mirrors the existing `case "quota_policies"` block.)

### T3. ai-gateway: drop gap C marker from introspection

In the runtime introspection block, the source
`cache.policy_cache.org_parents` currently returns:

```json
{
  "org_parents": {...},
  "invalidation_note": "no auto-invalidation today (gap C / e31-s10)"
}
```

Remove the `invalidation_note` key once T1 + T2 are wired and tested.

### T4. Tests

- New unit test in `admin_organizations_test.go` (or extend if exists):
  using a fake `HubNotifier`, assert that Create/Update/Delete each
  invokes `InvalidateConfig("ai-gateway", "organizations")` exactly
  once on the success path, and zero times on failure.
- ai-gateway: extend the existing `OnConfigChanged` integration test (if
  any) or add a unit test that calling the closure with the
  `organizations` key triggers a `policyCache.Load`. If wiring a fake
  PolicyCache is heavy, settle for a smoke test that the case is
  reachable (compiles + reported map gets the key).

### T5. Verify

- `go test -race -count=1 ./packages/control-plane/... ./packages/ai-gateway/...` PASS.
- Hand smoke (deferred to end-of-phase): edit an organization name in CP
  UI, then call `GET /api/admin/nodes/<ai-gateway-id>/runtime` and
  observe the `cache.policy_cache.org_parents` source reflect the
  updated parent map without restarting ai-gateway.

## Acceptance Criteria

1. Create / Update / Delete organization in CP UI → Hub log emits a
   `config_changed broadcast sent thing_type=ai-gateway
   config_key=organizations`.
2. ai-gateway log emits `applying config key config_key=organizations`
   followed by a successful policy cache reload within ~1s.
3. `OrgParents` map visible via `cache.policy_cache.org_parents`
   introspection source reflects the latest DB state, and the
   `invalidation_note` field is gone.
4. Project rename / re-parent paths are unchanged — documented in
   `admin_organizations.go::UpdateProject` comment, not implemented.
5. Tests in T4 pass; existing tests do not regress.

## Risks

- **Reload thrash on bulk ops.** A user importing 1000 organizations in
  bulk would trigger 1000 broadcasts. Mitigation: out of scope here;
  the CRUD does not currently support bulk import. If added later,
  collect the changes and emit one final invalidation rather than per-row.
- **Reload failure during update.** If `policyCache.Load` errors on
  reload, the cache keeps the previous (stale) data; the warn log
  surfaces it but the admin operation already succeeded. Acceptable —
  matches the existing `case "quota_policies"` behavior. Operators can
  re-trigger by editing any organization or by hitting
  `POST /api/admin/cache/flush`.
- **Hub broadcasts to compliance-proxy / agent.** The `organizations`
  key is targeted at ai-gateway only via the second arg to
  `InvalidateConfig`. Other thing types are not notified, so the WS
  push is not delivered to them — matches current behavior of
  `quota_policies`.

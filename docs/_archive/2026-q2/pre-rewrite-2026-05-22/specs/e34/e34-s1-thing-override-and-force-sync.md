# E34 S1 — Per-Thing Config Override + Force-Sync

## Story

As an admin operator I need to set, audit, and auto-expire per-Thing
configuration overrides (whole-key JSON replacement) on top of the existing
template tier, and to force-sync any Thing or single key on demand even when
the Thing reports it is already in sync, so I can run canary/regional/
capacity/break-glass/diagnostic scenarios case-by-case without having to
touch the global template.

## Scope

### Schema (DB)

- `tools/db-migrate/prisma/schema.prisma` — new `ThingConfigOverride` model + back-relation on `Thing`. New `IAMAction` rows (`admin:WriteThingOverride`, `admin:ForceResyncThing`) and `IAMRoleAction` mappings.
- `tools/db-migrate/migrations/<ts>_thing_config_override/migration.sql` — CREATE TABLE `thing_config_override` with PK `(thing_id, config_key)`, FK CASCADE to `thing(id)`, three indexes (`expires_at WHERE NOT NULL`, `thing_id`, `set_at DESC`), CHECK on `reason` length and `expires_at > set_at`.
- `tools/db-migrate/seed/seed.ts` — adds `admin:WriteThingOverride` and `admin:ForceResyncThing` to the `Action` arrays of `NexusComplianceAdmin` and `NexusProviderAdmin` policy documents (this repo's IAM is policy-document-based, not enumerator-table-based; `NexusSuperAdmin` covers via wildcard `Action: ['*']`).

### Hub backend (`packages/nexus-hub`)

- `internal/store/thing_config_override.go` (new) — `Get`, `Upsert`, `Delete`, `ListByThing`, `ListAll` (with filter + total + summary), `ListExpired`. List-by-thing and list-all JOIN `thing_config_template` to compute `stale = tct.version > tco.template_ver_at_set`.
- `internal/store/config_template.go` (extend) — `GetTemplate(ctx, type, configKey) (*ConfigTemplate, error)` single-row lookup.
- `internal/store/thing_registry.go` (extend) — `WriteDesiredAndBumpVer(tx, thingID, merged)` returns new `desired_ver`. List query LEFT JOIN aggregate to surface `override_count` + `override_stale_count` per Thing.
- `internal/thingmgr/override.go` (new) — `SetOverride`, `ClearOverride`. Single-tx flow: upsert/delete + recompute `thing.desired` (cascade template ⊕ override) + bump `desired_ver` + audit row + post-commit Hub `RePushConfigKey(force=true)`. `emergency_override` auto-set when `configKey == "killswitch"` or `reason` starts with `break-glass:`.
- `internal/thingmgr/drift.go` (extend) — `RePushAllKeys(thingID)` iterates `thing.Desired` and replays each key with Force=true; returns key count.
- `internal/handler/hub_api.go` (extend) — `ResyncThing` accepts empty body = whole-Thing replay returning `{ok, thingId, keyCount}`. New routes: `GET /api/hub/things/:id/overrides`, `PUT /api/hub/things/:id/overrides/:configKey`, `DELETE /api/hub/things/:id/overrides/:configKey`, `GET /api/hub/things/overrides`.
- `internal/jobs/override_expiry.go` (new) — 60 s tick scheduler job; calls `Manager.ClearOverride` for each row with `expires_at < NOW()`; actor = `system:override-expiry-job`.
- `internal/config/config.go` (extend) — `OverrideExpiryInterval time.Duration` with default `60 * time.Second`.
- `cmd/nexus-hub/main.go` (extend) — register `OverrideExpiry` job alongside the existing drift detector.

### Control Plane backend (`packages/control-plane`)

- `internal/handler/admin_thing_overrides.go` (new) — five admin routes:
  - `GET    /api/admin/things/:id/overrides`               (`admin:ReadSettings`)
  - `PUT    /api/admin/things/:id/overrides/:configKey`    (`admin:WriteThingOverride`)
  - `DELETE /api/admin/things/:id/overrides/:configKey`    (`admin:WriteThingOverride`)
  - `GET    /api/admin/things/overrides`                   (`admin:ReadSettings`)
  - `POST   /api/admin/things/:id/resync`                  (`admin:ForceResyncThing`)
  Validation: blacklist key check, `state` must be JSON object top-level, `reason` ≤ 500 chars, `expiresAt - NOW() ∈ [5 m, 30 d]`. Type-scope RBAC: `provider_admin` (group `provider-admins`) service-only, `compliance_admin` (group `compliance-team`) agent-only. **For override mutations** (PUT/DELETE), the `AdminAuditLog` row is written **by the Hub manager IN-TX**; CP must NOT also call `audit.Log` (would double-audit). **For force-sync** (POST `/resync`), CP writes the audit row (Hub does not audit redelivery).
- `internal/handler/admin_routes.go` (extend) — mount `RegisterAdminThingOverridesRoutes(g, iamMW)` next to the existing applied-config registration.
- `internal/handler/admin_things_applied_config.go` (extend) — response per entry adds `templateState`, `templateVer`, and (when an override exists) `override: { state, setBy, setAt, reason, expiresAt, templateVerAtSet, currentTemplateVer, stale, emergencyOverride }`. Single endpoint serves all data the new Configuration tab needs.

### Shared types (`packages/shared`)

- `configtypes/override_policy.go` (new) — `var NonOverridableConfigKeys = map[string]bool{"credentials": true, "virtual_keys": true}` + `func IsOverridable(configKey string) bool`. Used by CP admin handler to reject 400 BadRequest.

### Control Plane UI (`packages/control-plane-ui`)

- `src/api/services/hub.ts` (extend) — types: `ThingOverride`, `GlobalOverridesResponse`, `ResyncResponse`. Extend `AppliedConfigEntry` with `templateState`, `templateVer`, optional `override`. Five new client methods: `listOverrides`, `setOverride`, `clearOverride`, `listGlobalOverrides`, `resyncThing`.
- `src/pages/infrastructure/InfraNodesPage.tsx` (extend) — new `Overrides` column with count + stale chip; `Has overrides` filter chip in toolbar; queryKey extended.
- `src/pages/infrastructure/InfraNodeDetailPage.tsx` (extend) — drop `configSync` + `appliedConfig` tabs; add `configuration` tab; total 5 tabs (Overview, Configuration, Runtime, Metrics, Logs).
- `src/pages/infrastructure/ConfigurationTab.tsx` (new) — 4-column table (Key / Template default / Override / Applied) reading from extended applied-config endpoint; always-visible Force resync (per-key + whole-Thing) buttons; `+ Add override` and Edit/Clear actions per row; greys out blacklist keys; killswitch bypass red banner when applicable.
- `src/pages/infrastructure/OverrideEditorDrawer.tsx` (new) — right-side ~55% drawer, two-pane (read-only template / editable JSON), TTL preset picker, reason input, save/cancel. JSON validation client-side; server returns final ruling.
- `src/pages/infrastructure/InfraOverridesPage.tsx` (new) — `/infrastructure/overrides` page; aggregate counters; filter chips (type, hasTtl, stale, recent); table with 7 columns; per-row View / Force resync / Clear / Extend.
- Routes and nav — register `/infrastructure/overrides` and add the sidebar entry under Infrastructure.
- Files to DELETE: `src/pages/infrastructure/AppliedConfigTab.{tsx,module.css,test.tsx}` (subsumed by `ConfigurationTab`).
- i18n — extend `src/i18n/locales/{en,zh,es}/{pages,nav}.json` with `infrastructure.{overrides,configuration,editor}.*` keys + the `Overrides` nav label; mirror to `public/locales/`.

### OpenAPI

- `docs/users/api/openapi/admin/e34-s1-thing-override-and-force-sync.yaml` (new) — paths for the five admin routes + the extended `applied-config` shape; component schemas for `ThingOverride`, `SetOverrideBody`, `ListOverridesResponse`, `GlobalOverridesResponse`, `ResyncResponse`, and the extended `AppliedConfigEntry`.

## Tasks

1. **DB schema + IAM seed + Go blacklist** — Prisma model + migration SQL with IAM inserts; `configtypes/override_policy.go`. Run `prisma migrate dev`. Verify table + indexes via psql.
2. **Hub store layer** — implement and DB-test `thing_config_override.go` + the two helpers in existing files.
3. **Hub manager + RePushAllKeys** — implement and unit-test `SetOverride`, `ClearOverride`, and `RePushAllKeys`.
4. **Hub HTTP — /resync extension + override routes** — extend `ResyncThing`; add the 4 new override routes; wire into `routes.go`.
5. **Hub override-expiry job** — new job + register in `main.go`; verify TTL clears at next tick.
6. **CP admin overrides handler + applied-config extension** — five routes + validation + type-scope RBAC + audit; extend applied-config response with override fields and tests.
7. **UI hub.ts API + nodes list query enrichment** — types, client methods, list query LEFT JOIN.
8. **UI nodes list page** — `Overrides` column + `Has overrides` filter + tests.
9. **UI ConfigurationTab + detail page IA swap** — new tab component + delete `AppliedConfigTab.*` + reduce to 5 tabs.
10. **UI OverrideEditorDrawer** — drawer + JSON editor + TTL/reason; wired Save/Clear flows.
11. **UI Global override page + route + nav** — page + filter bar + summary counters + per-row actions.
12. **i18n** — keys in 3 locales + sync to `public/`.
13. **Verify** — full Go + Vitest sweep; restart Hub + CP; smoke flows (set, list, resync, TTL auto-expire, audit rows present).

## Acceptance Criteria

The 15 criteria from `docs/_archive/2026-q2/brainstorms/2026-04-28-node-per-thing-override-and-force-sync-design.md` §12 apply verbatim. Summary:

1. `thing_config_override` schema with all columns / indexes / constraints exists.
2. Blacklist enforced server + UI; setting `credentials` / `virtual_keys` returns 400.
3. After setting override, `BulkConfigPull(id=, type=)` returns the override state and the Thing's `desired_ver` reflects the bump.
4. Per-key force-sync on an in-sync Thing causes client `OnConfigChanged` re-run.
5. Whole-Thing force-sync replays every key with `force=true`.
6. TTL auto-expiry: a row with `expires_at = NOW() - 1 s` is cleared in ≤ 60 s; an `AdminAuditLog` row with `action='thing_override_cleared'` and `actorId='system:override-expiry-job'` exists. (Implementation reuses `Manager.ClearOverride` so the action name is the same as admin clears; the actor is the discriminator.)
7. Stale flag is `true` after the template version bumps post-override creation.
8. RBAC: provider_admin cannot override an agent Thing (403); compliance_officer cannot override a service Thing (403).
9. Every set / clear / auto-expire / force-sync writes exactly one `admin_audit_log` row.
10. Nodes list shows `Overrides` column + `Has overrides` filter narrows.
11. Configuration tab shows 4-column layout with override row styling and stale badge; force-resync buttons always visible.
12. Editor drawer pre-fills template on add and override state on edit; client + server validate JSON top-level + TTL range + reason length.
13. Global page shows all active overrides; per-row actions work; no bulk mutation.
14. Killswitch override during global engagement surfaces red banner on detail and red row on list.
15. All new strings have keys in `en/zh/es` locale files; technical terms remain English.

## Post-ship REV-S1 hardening (2026-04-27)

After the four-reviewer pass on the shipped E34-S1 work, the following
structural changes landed in the `REV-S1` commit chain. They preserve
external behaviour but tighten contracts on the Hub side; document them
here so the SDD reflects the as-built state.

### `store.OverrideState` newtype

`store.ThingConfigOverride.State` is no longer a bare `json.RawMessage`.
It is now a `store.OverrideState` value-type whose constructor
`NewOverrideState` enforces the spec invariant ("state MUST be a JSON
object at top level") at construction time. Empty input, invalid JSON,
arrays, scalars, and `null` are all rejected via three error sentinels:
`ErrEmptyState`, `ErrInvalidJSONState`, `ErrNonObjectState`. Callers read
the canonical bytes via `OverrideState.Bytes()`. The Hub HTTP override-set
handler maps the construction errors to 400; CP performs the same
validation up-front so well-formed admin traffic never sees these.

Scan paths use a package-internal `overrideStateFromDB` helper that
trusts the DB's `state jsonb NOT NULL` constraint and skips re-validation.

### `store.Thing` → `store.Thing` + `store.ThingWithOverrideAgg`

Bare `store.Thing` no longer carries `OverrideCount` /
`OverrideStaleCount` fields that `GetThing` could not populate (the
override counts are produced only by the `ListThings` JOIN). The list
path now returns `*ListThingsResult{Things: []ThingWithOverrideAgg}`
where `ThingWithOverrideAgg` embeds `Thing` plus the two aggregate
fields. Hub `/api/hub/things` JSON shape is byte-stable across this
refactor — embedded `Thing` carries the same field tags.

### `ErrNoDeliveryPath` sentinel

`thingmgr.RePushConfigKey` previously returned `nil` when neither the
local WebSocket pool nor MQ could deliver the force-push, and only
emitted a `slog.Warn` for visibility. The post-commit caller in
`Manager.SetOverride` / `Manager.ClearOverride` therefore reported
success even though the client never received the push. The sentinel
`thingmgr.ErrNoDeliveryPath` now flows up from `rePushConfigKeyForThing`
in that branch. The override-set/clear callers continue to treat push
failure as non-fatal (drift detection re-converges) but the warn line is
now unified under `event=override_push_failed` with an `operation=` tag,
so dashboards can alert on the audit-committed-but-not-delivered case.

### `audit.NewHashPayload` builder + sorted-key canonical chain

`audit.NewHashPayload(action, actorID, entityType, entityID)` validates
that `action` and `actorID` are non-empty (the chain key is meaningless
without them). Both writer paths build payloads through it.

`audit.NextHash` and `audit.VerifyChain` now hash a canonicalized
JSON-object encoding (sorted keys) of `HashPayload` instead of relying
on Go's struct-field-declaration order. A future struct refactor that
reorders fields is therefore safe — the same logical payload still
hashes to the same digest.

### `RePushAllResult`

`Manager.RePushAllKeys` returns `*RePushAllResult{Pushed, Failed}`
instead of `(int, error)`. Per-key failures accumulate into `Failed`
rather than aborting the loop; the Hub HTTP `ResyncThing` whole-Thing
response now emits `keyCount` (pushed) plus an optional `failed` array
when any key did not deliver.

### `configtypes.NonOverridableConfigKeys` unexported

The blacklist map is now unexported (`nonOverridableConfigKeys`) and
exposed through three predicates: `IsOverridable`, `IsBlacklisted`,
`BlacklistedKeys()`. External packages can no longer mutate the policy.

### Misc

- `recomputeDesiredTx` now `SELECT ... FOR SHARE` on
  `thing_config_template` so a concurrent template update on the same
  `(type, config_key)` cannot interleave between our read and the merge
  write.
- `override_agg` CTE in `ListThings` fixed to LEFT JOIN
  `thing_config_template` so an orphan override (template deleted out
  from under it) still contributes to `overrideCount`. The applied-config
  handler iterates orphan overrides after the templates loop and emits a
  synthesised entry with `templateState: null`, `templateVer: 0`, and the
  override block — without this, the orphan vanished from the
  Configuration tab.
- Hub `ListGlobalOverrides` now 400s on a non-bool `hasTtl` / `stale`
  query param instead of silently dropping the filter.
- Consumer `admin_audit` no longer silently drops `Marshal` failures on
  `BeforeState` / `AfterState`; the row still inserts (with NULL on the
  failed field) but a warn line + an `errorsTotal` increment surfaces
  the visibility gap.

# E29 S4 — Rule Pack Admin API CRUD + Install Management

## Story

As a compliance operator I need full CRUD on Rule Packs so I can
create, edit, and retire packs in-place instead of re-importing YAML
for every change. I also need to list installs per hook and
uninstall a pack binding without manual SQL.

## Scope

- `packages/control-plane/internal/handler/admin_rulepacks.go`
- `packages/shared/policy/rulepack/store.go`
- `docs/users/api/openapi/admin/e29-s4-rulepack-crud.yaml`

## New endpoints

- `POST   /api/admin/rule-packs`                          — create pack (JSON body, no YAML required)
- `PATCH  /api/admin/rule-packs/{id}`                     — update pack metadata + rules
- `DELETE /api/admin/rule-packs/{id}`                     — delete pack and cascade rules
- `GET    /api/admin/hooks/{hookId}/rule-packs`           — list installs for a hook
- `DELETE /api/admin/rule-pack-installs/{installId}`      — uninstall a pack from its hook
- `PATCH  /api/admin/rule-pack-installs/{installId}`      — enable/disable an install

## Tasks

1. Extend `rulepack.Store` with `Create`, `Update`, `Delete`,
   `ListInstallsByHook`, `UpdateInstall`, and `DeleteInstall`
   methods; each one writes through a single transaction.
2. Extend `admin_rulepacks` with handlers for the new endpoints and
   enforce the existing permission matrix
   (`admin:ReadRulePacks` / `admin:UpdateRulePacks`).
3. On `DELETE /rule-packs/{id}`, refuse if any install still points
   to the pack (HTTP 409). The operator must uninstall first.
4. On `PATCH /rule-pack-installs/{id}`, allow toggling `enabled`
   only; changing `pinVersion` requires uninstall + reinstall to
   keep audit trails clean.
5. Validate rule IDs inside a pack are unique; reject updates that
   would collide.
6. Emit an admin audit event (`nexus.event.admin-audit`) on the
   success path of every mutating endpoint (create, import, update,
   delete, install, patch-install, uninstall, upsert-overrides).
   Failure paths (validation, not-found, conflict, store error) MUST
   NOT emit an event. Use the shared `*audit.Writer` injected into
   `RulePackHandler` via its constructor. BeforeState/AfterState use
   `rulePackAuditSummary` (id, name, version, maintainer, rule count)
   so events stay bounded regardless of pack size.

## Acceptance criteria

- Handler tests cover all new endpoints, including 4xx/5xx paths.
- OpenAPI spec at `docs/users/api/openapi/admin/e29-s4-rulepack-crud.yaml` matches
  request/response shapes exactly.
- `admin:ReadRulePacks` allows list/get; `admin:UpdateRulePacks` is
  required for create/update/delete/install mutations.
- Every successful mutation publishes exactly one admin audit
  message on `nexus.event.admin-audit` (verified by the
  `TestAdminRulePacks_Audit_*` suites against an MQ producer spy);
  failure paths publish zero messages. Action/entity naming matches
  the coverage table in
  [`docs/developers/architecture/cross-cutting/observability/admin-audit-log-coverage.md`](../../../../docs/developers/architecture/cross-cutting/observability/admin-audit-log-coverage.md).

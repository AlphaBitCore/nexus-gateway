# E33 S6 — Admin API + UI for Streaming Policy

## Story

As an admin I need to edit the global streaming compliance policy and per-resource overrides (per `interception_domain` for compliance-proxy/agent, per `Provider` for ai-gateway) from the Settings UI without writing SQL, so the data plane picks up changes via the existing Hub shadow push.

## Scope

- `packages/control-plane/internal/handler/admin/streaming_compliance.go` (new) — `GET /api/admin/streaming-compliance` (read global default), `PUT /api/admin/streaming-compliance` (write global default to `system_metadata['streaming_compliance.config']`). Same pattern as the existing `payload_capture.config` admin handler.
- `packages/control-plane/internal/handler/admin/spill_store.go` (new) — `GET /api/admin/spill-store` (read backend config), `PUT /api/admin/spill-store` (write backend config to `system_metadata['spill_store.config']`).
- `packages/control-plane/internal/handler/admin/interception_domains.go` (existing — extend) — the existing PATCH/PUT for `/api/admin/interception-domains/:id` accepts the new override columns.
- `packages/control-plane/internal/handler/admin/providers.go` (existing — extend) — the existing PATCH/PUT for `/api/admin/providers/:id` accepts the new override columns.
- Hub HTTP API: `Hub` `NotifyConfigChange("streaming_compliance")` and `NotifyConfigChange("spill_store")` are already supported by the existing config-change router (Category A keys); add the dispatch entries.
- `docs/users/api/openapi/e33-s6-streaming-compliance.yaml` (new) — OpenAPI 3.1 for the two new admin endpoints + the column additions on `interception_domain` and `Provider`.
- `packages/control-plane-ui/src/pages/Settings/StreamingCompliance.tsx` (new) — global default form (mode dropdown, chunk_bytes, hook_timeout_ms, max_buffer_bytes, fail_behavior, capture toggles).
- `packages/control-plane-ui/src/pages/Settings/SpillStore.tsx` (new) — backend config form (backend select, root, max_size_gb, retention_days).
- `packages/control-plane-ui/src/pages/InterceptionDomains/Edit.tsx` (existing — extend) — add an "Streaming Override" panel with the seven nullable fields. Empty input = NULL = inherit.
- `packages/control-plane-ui/src/pages/Providers/Edit.tsx` (existing — extend) — same.
- i18n: add keys for the new labels into `en/zh/es` `pages.json` per project convention.
- Tests: BFF handler tests for the two new endpoints; UI component tests for the form panels (Vitest).

## Tasks

1. Implement the two new admin GET/PUT handlers; wire IAM check (`admin:WriteSettings` for PUT, `admin:ReadSettings` for GET).
2. Extend the existing interception-domain and provider PATCH handlers with the override fields. JSON schema validation rejects invalid mode strings and out-of-range `chunk_bytes`.
3. Hub `NotifyConfigChange` dispatch updates for `streaming_compliance` and `spill_store` keys.
4. Build the UI panels per project's existing patterns (`useApi` queryKey shape per CLAUDE.md binding rule).
5. Add i18n keys to all three locale files; copy into `public/locales/`.
6. Smoke the admin path: `curl -b cookie -X PUT http://localhost:3001/api/admin/streaming-compliance -d '{"default_mode":"chunked_async",...}'` ⇒ verify `system_metadata` row updated; verify Hub shadow event emitted.

## Acceptance criteria

- `PUT /api/admin/streaming-compliance` writes the JSON to `system_metadata` and triggers a Hub config-change push for the `streaming_compliance` key. Compliance-proxy / ai-gateway / agent all reload their `Policy` global default within 5 s.
- Editing an `interception_domain` row via the admin UI persists the override columns and Hub pushes `interception_domains` config-change to compliance-proxy + agent.
- Editing a `Provider` row via the admin UI persists the override columns and Hub pushes `providers` config-change to ai-gateway.
- All UI text uses `t('namespace:section.key')`; no hardcoded English in components.
- OpenAPI spec lives at `docs/users/api/openapi/e33-s6-streaming-compliance.yaml` and matches the implemented handlers.
- Browser verification of the UI is **out of scope** (per epic decision) — code-complete + Vitest passing is the bar.

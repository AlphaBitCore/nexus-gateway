# E30 — Provider `adapterType` (explicit adapter selection)

**Status:** Active — 2026-04-24
**Epic:** 30

## 1. Business goal

After E28 landed the composable `AdapterSpec` model, the AI Gateway still
picks an outbound wire adapter by **string-matching the operator-facing
`Provider.name` column** (`providers.FormatOfProvider(name)`). Any custom
provider name — e.g. `my-kimi-proxy` for an OpenAI-compatible upstream —
falls out of the switch and the gateway responds `400 no_compatible_provider`
even though the adapter is perfectly capable of serving the traffic.

E30 replaces that heuristic with an explicit `Provider.adapter_type`
column (required on create, mutable thereafter) that stores one of the
nine canonical `providers.Format` values. The AI Gateway reads the field
directly into `CallTarget.Format`; the legacy `FormatOfProvider` lookup
and the unused `Provider.type` column are removed. The Control Plane
admin API and Control Plane UI are updated so operators must pick the
adapter explicitly, mirroring the existing `InterceptionDomain.adapterId`
UX on the Compliance side.

The interception-domains page copy is updated in the same epic to
acknowledge that Desktop Agent and Compliance Proxy both consume the
snapshot.

Pre-GA: no backward compatibility — the `type` column is dropped in the
same migration that adds `adapter_type`.

## 2. User roles

| Role | Need |
|------|------|
| **Platform admin** | Pick the wire adapter explicitly when creating a provider so custom names never break routing; change it after the fact if the upstream actually changed wire format. |
| **Compliance officer** | Infer nothing about wire format from the operator-facing name; audit trail shows the exact adapter a provider is bound to. |
| **Application developer** | Keep the existing VK/ingress contract; behavior is unchanged as long as the adapter is set correctly. |
| **AI Gateway / smart router maintainer** | Read `CallTarget.Format` directly from the resolver; no name heuristic in executor, router, AI Guard, simulate, or provider-test paths. |
| **Desktop Agent / Compliance Proxy operator** | See UI copy that reflects both data planes consume the interception snapshot. |

## 3. Functional requirements

| ID | Requirement | MoSCoW |
|----|-------------|--------|
| FR-1 | Add a required `Provider.adapter_type` column (Prisma `adapterType String @map("adapter_type")`). Allowed values are the nine canonical `providers.Format` strings: `openai`, `anthropic`, `gemini`, `glm`, `deepseek`, `azure-openai`, `minimax`, `bedrock`, `vertex`. | Must |
| FR-2 | The `generic-jsonpath` traffic adapter is **not** a valid `adapter_type`. It stays traffic-side only. | Must |
| FR-3 | Drop the legacy `Provider.type` column outright in the same migration. No `@deprecated` shim, no dual-column phase. | Must |
| FR-4 | Migration backfills existing rows by mapping `lower(name)` through the same table the old `FormatOfProvider` switch used (`openai/moonshot/kimi/siliconflow → openai`, `deepseek → deepseek`, `anthropic/claude → anthropic`, `glm/zhipu → glm`, `gemini → gemini`, `azure-openai → azure-openai`, `minimax → minimax`, `bedrock → bedrock`, `vertex → vertex`). Unknown names default to `openai` so the migration cannot fail; operators fix up the value post-migration. | Must |
| FR-5 | DB-level CHECK constraint is **not** applied. Enum enforcement lives in the Control Plane handler (create + update) and is mirrored in the OpenAPI spec. AI Gateway treats an invalid value as a provider configuration error and surfaces a clear message without panicking. | Must |
| FR-6 | `adapter_type` is **mutable** after create. `PUT /api/admin/providers/:id` accepts the field and re-validates it against the nine values. No cascade — existing credentials, models, and routing rules continue to reference the provider by ID. Operator takes responsibility for coordinating an adapter switch with dependent data. | Must |
| FR-7 | AI Gateway `store.Provider` exposes `AdapterType` instead of the legacy `Type`. `GetProvider` selects `adapter_type`. | Must |
| FR-8 | `provtarget.ProviderRow` carries `AdapterType`; `PgResolver.Resolve` copies it into `CallTarget.Format` (as `providers.Format`). A missing/empty adapter type surfaces as a resolver error (`provtarget: provider %q: adapter_type empty`). | Must |
| FR-9 | Delete `packages/ai-gateway/internal/providers/lookup.go` in full (`FormatOfProvider`, `RegisterProviderFormat`, `UnregisterProviderFormat`, `testRegistry`). All six call sites (`executor`, smart router `strategy_smart`, AI Guard `backend_provider`, handler `cross_format`, handler `routing_simulate_endpoint`, handler `provider_test_endpoint`) switch to `target.Format`. Tests that called `RegisterProviderFormat` stop doing so and instead set the format on the CallTarget directly. | Must |
| FR-10 | Control Plane `POST /api/admin/providers` requires `adapterType` in the body and rejects any other value with `400 validation_error`. Legacy `type` is neither accepted nor echoed. | Must |
| FR-11 | Control Plane `PUT /api/admin/providers/:id` accepts `adapterType` and validates it against the nine values when present; omitting it leaves the field unchanged. | Must |
| FR-12 | `GET /api/admin/providers` list and `GET /api/admin/providers/:id` detail both include `adapterType` in the response and omit `type`. | Must |
| FR-13 | Seed data (`tools/db-migrate/seed/`) sets an explicit `adapterType` on every seeded provider and drops the legacy `type` key. | Must |
| FR-14 | Control Plane UI provider-template JSON files (`packages/control-plane-ui/public/provider-templates/*.json` + `index.json`) carry an `adapterType` field; picking a template pre-populates the dropdown. Legacy `type` is removed from the templates. | Must |
| FR-15 | Control Plane UI provider create wizard and edit form both show a required **Adapter** dropdown with nine options. The dropdown is editable on both create and edit (since FR-6 allows mutation), with a help tooltip explaining that switching adapter can break existing credentials/routes. | Must |
| FR-16 | Control Plane UI provider detail page displays the selected Adapter as a tag/field next to Region. | Must |
| FR-17 | UI field label is **"Adapter"** (not "Adapter Type", not "Wire Format") in all three locales (en/zh/es). Per `CLAUDE.md` i18n convention, technical option values (`openai`, `anthropic`, …) stay English across all locales. | Must |
| FR-18 | i18n keys live under the `providers` namespace (`providers.adapter.label`, `providers.adapter.help`, `providers.adapter.option.<key>`). Added to `src/i18n/locales/{en,zh,es}/pages.json` and copied to `public/locales/`; key counts match across locales. | Must |
| FR-19 | Interception-domains page copy is updated so `subtitle` and `allowlistNote` reflect that the snapshot is consumed by **both** Compliance Proxy **and** Desktop Agent. Mirror in zh/es and sync to `public/locales/`. | Must |
| FR-20 | OpenAPI fragment under `docs/users/api/openapi/ai-gateway/e30-s2-provider-adapter-type.yaml` documents the new field (enum, required on create, optional on update) and merges into `docs/users/api/openapi/gateway-api.yaml`. The nine values are listed letter-for-letter. | Must |

## 4. Non-functional requirements

| ID | Requirement |
|----|-------------|
| NFR-1 | Go 1.25+, idiomatic Go; no panics in library code; `log/slog` structured logging when emitting config errors. |
| NFR-2 | Adding a tenth wire format in the future requires adding the value in `providers/types.go`, the Control Plane validator, the OpenAPI enum, the UI dropdown options, and the i18n locales — no changes to resolver, executor, router, AI Guard, handler, or DB. |
| NFR-3 | No DB CHECK means validation is fully application-owned. The Control Plane validator is the authoritative gate; AI Gateway logs and rejects invalid rows with a clear error rather than silently defaulting. |
| NFR-4 | All changed paths covered by `go test -race -count=1 ./packages/ai-gateway/... ./packages/control-plane/...` and Vitest in `packages/control-plane-ui`. |
| NFR-5 | Grep gates pass: zero hits for `FormatOfProvider`, `RegisterProviderFormat`, `UnregisterProviderFormat` anywhere under `packages/**/*.go`; no residual `Provider.Type` production references in CP or AI Gateway. |
| NFR-6 | English-only for repo artifacts per `CLAUDE.md` (docs, comments, commit messages, Go code). UI strings use `t(...)` with entries in en/zh/es. |
| NFR-7 | Pre-GA development policy: no parallel `type`/`adapterType` paths, no feature flags, no `@deprecated` markers, no data-migration code for records created after the migration runs. |

## 5. Glossary

| Term | Meaning |
|------|---------|
| **Adapter** (UI label) | User-facing name for the wire format that the gateway uses to talk to this provider. Stored as `Provider.adapter_type`. |
| **`adapter_type`** (DB column) | Required `TEXT` column on `Provider`, one of the nine canonical `providers.Format` values. |
| **`Format`** (Go) | `providers.Format` type in `packages/ai-gateway/internal/providers/types.go`. Same nine values as `adapter_type`. |
| **`adapterId`** (Compliance side) | Separate column on `InterceptionDomain` that references `traffic.AdapterRegistry` (shares nine of ten IDs with `Format`, plus `generic-jsonpath`). Not the same column; no FK between the two. |
| **`CallTarget.Format`** | Field on the internal `providers.CallTarget` populated by `provtarget.PgResolver.Resolve` from `adapter_type`. |
| **`FormatOfProvider`** | **Deleted** name-based lookup; no longer used anywhere in production or tests. |

## 6. Constraints

- **Pre-GA, greenfield** per `CLAUDE.md`: drop the `type` column outright; do not keep parallel paths; do not stage the change across multiple releases.
- **Validation lives in the Control Plane handler and OpenAPI**, not in the DB.
- **Adapter is mutable**: the epic explicitly allows changing `adapter_type` on existing rows. Any cascade to credentials/models/routing rules is the operator's responsibility; surfaces like UI warn but do not block.
- **Parallel E29 overlap**: E29 is adding `Provider.region`. The shared-file collision points are `tools/db-migrate/schema.prisma`, `packages/ai-gateway/internal/store/provider.go`, `packages/control-plane/internal/store/provider.go`, `packages/control-plane/internal/handler/admin_providers.go`. This epic extends the same files without touching E29's additions.

## 7. Out of scope (this epic)

- Automatic validation of existing credentials / models / routing rules when the operator changes `adapter_type` (no cascade).
- A matching "adapter" column on any other table — scope is strictly `Provider`.
- Changing the `InterceptionDomain.adapterId` column naming or behavior (the sibling table already works correctly).
- Renaming the Go `providers.Format` type to `AdapterType` (keeping `Format` internal to AI Gateway code; only the DB column and the operator-facing field are called "Adapter / adapter_type").

# E30 — Story 2: Control Plane admin API, seeds, provider templates

## Context

Second story. Exposes `adapterType` through the admin provider CRUD, validates
it against the nine canonical values on both create and update (mutable per
FR-6), drops the legacy `type` field from request/response, updates seeds and
the UI provider-template JSON, and publishes the OpenAPI spec fragment.

Depends on s1 (the column and AI Gateway wiring must be in place).

## User Story

**As a** platform admin,
**I want** the admin provider API to require the Adapter on create and allow
changing it later,
**so that** custom provider names never surface as `400 no_compatible_provider`
at runtime and I can correct a misconfigured provider without deleting and
recreating it.

## Tasks

### 1. Control Plane store — `packages/control-plane/internal/store/provider.go`

- `Provider` struct: replace `Type string` with `AdapterType string`.
- `CreateProviderParams`: replace `Type` with `AdapterType` (required).
- `UpdateProviderParams`: replace any `Type *string` pointer with
  `AdapterType *string` (optional on update; `nil` leaves the column
  unchanged).
- `CreateProvider` INSERT: replace `"type"` column with `"adapter_type"`;
  bind `AdapterType`.
- `UpdateProvider`: dynamic SET builder must handle `adapter_type`.
- `ListProviders` / `GetProvider` SELECT: replace `"type"` with `"adapter_type"`;
  scan into `AdapterType`.
- `ProviderListItem` projection: replace `Type` field with `AdapterType`.

### 2. Control Plane handler — `packages/control-plane/internal/handler/admin_providers.go`

- Response shapes (`listItem`, `GetProvider` map): swap `type` key for
  `adapterType` in JSON; remove the legacy key entirely.
- `CreateProvider` request body:
  - Add `AdapterType string \`json:"adapterType"\``.
  - Remove the `Type string \`json:"type"\`` field and the `body.Type = "openai-compatible"` default at line ~160.
  - Validate `body.AdapterType != ""` and that it is one of the nine canonical values; return `400 validation_error` with a clear message ("adapterType must be one of openai, anthropic, gemini, glm, deepseek, azure-openai, minimax, bedrock, vertex") otherwise.
- `UpdateProvider` request body:
  - Add `AdapterType *string \`json:"adapterType"\``.
  - When non-nil, validate against the nine canonical values; reject with the same `400` message.
  - Pass through to `store.UpdateProviderParams.AdapterType`.
- Removes any other residual reads of `body.Type` or `p.Type`.

### 3. Canonical enum helper

Add a small helper in `packages/control-plane/internal/handler/` (e.g.
`provider_adapter_types.go`) that exports:

```go
// ValidAdapterTypes is the authoritative set of Provider.adapter_type
// values accepted by the admin API. It matches providers.Format in the
// AI Gateway; the two lists must be kept in lockstep.
var ValidAdapterTypes = []string{
    "openai",
    "anthropic",
    "gemini",
    "glm",
    "deepseek",
    "azure-openai",
    "minimax",
    "bedrock",
    "vertex",
}

func IsValidAdapterType(v string) bool { /* set lookup */ }
```

`CreateProvider` / `UpdateProvider` use `IsValidAdapterType`.

### 4. Seeds — `tools/db-migrate/seed/`

- Every seeded `Provider` row gets an explicit `adapterType` that matches
  its current behavior. Drop the legacy `type` field from seed payloads.
- Verify `npx prisma db seed` still succeeds after the migration.

### 5. Provider templates — `packages/control-plane-ui/public/provider-templates/`

- For every template JSON (openai, anthropic/claude, gemini, glm/zhipu,
  deepseek, moonshot/kimi, siliconflow, azure-openai, minimax, bedrock, vertex,
  etc.), add `"adapterType": "<canonical>"` and remove any `"type"` field.
- Update `index.json` entries to include `adapterType` alongside the existing
  metadata.

### 6. OpenAPI — `docs/users/api/openapi/ai-gateway/e30-s2-provider-adapter-type.yaml`

Document the request/response change:

- `CreateProviderRequest`: `adapterType` required, enum of nine strings;
  `type` removed.
- `UpdateProviderRequest`: `adapterType` optional, same enum when present.
- `Provider` (response): `adapterType` required; `type` removed.

Merge the matching bits into `docs/users/api/openapi/gateway-api.yaml` so the
consolidated admin API spec stays current.

## Acceptance criteria

- [ ] `POST /api/admin/providers` without `adapterType` returns `400 validation_error`.
- [ ] `POST /api/admin/providers` with `adapterType: "not-a-thing"` returns `400 validation_error`.
- [ ] `POST /api/admin/providers` with a valid `adapterType` persists the value and echoes it in the 201 body; response does not contain a `type` key.
- [ ] `PUT /api/admin/providers/:id` with a valid `adapterType` updates the row; same 400 for invalid value; omitting the field leaves the column unchanged.
- [ ] `GET /api/admin/providers` and `GET /api/admin/providers/:id` include `adapterType`; neither includes `type`.
- [ ] Seeds run clean with `npx prisma db seed`; every seeded provider has `adapterType` set.
- [ ] Every file in `public/provider-templates/` has `adapterType`; none has `type`.
- [ ] OpenAPI fragment exists under `docs/users/api/openapi/ai-gateway/e30-s2-provider-adapter-type.yaml`; `gateway-api.yaml` is merged.
- [ ] `go test -race -count=1 ./packages/control-plane/...` green.

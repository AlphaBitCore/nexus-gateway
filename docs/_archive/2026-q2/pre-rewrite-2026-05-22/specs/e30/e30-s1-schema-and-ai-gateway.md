# E30 — Story 1: Schema + AI Gateway adapter resolution

## Context

Ground-floor story for E30. Adds the `Provider.adapter_type` column, deletes
the legacy `Provider.type` column, rewrites AI Gateway target resolution to
carry `Format` from the DB, and deletes the name-based `FormatOfProvider`
lookup and its test helpers. Deliverables land together in one commit so the
AI Gateway never depends on both paths at the same time.

## User Story

**As an** AI Gateway maintainer,
**I want** the wire format for every provider to come from an explicit DB column,
**so that** custom provider names never fall out of a hardcoded switch and the six
internal callers read one field instead of re-deriving the adapter.

## Tasks

### 1. Prisma schema — `tools/db-migrate/schema.prisma`

- On `model Provider`:
  - Add `adapterType String @map("adapter_type")` (required, no default at the
    Prisma level — the migration handles backfill then drops the SQL default).
  - Remove the `type String @default("builtin")` line.
  - Keep `region String?` (E29 addition) untouched.

### 2. SQL migration — `tools/db-migrate/migrations/<ts>_provider_adapter_type/migration.sql`

Pattern: backfill existing rows, then drop the default, then drop the legacy column.

```sql
ALTER TABLE "Provider" ADD COLUMN "adapter_type" TEXT NOT NULL DEFAULT 'openai';

UPDATE "Provider"
   SET "adapter_type" = CASE
     WHEN lower(name) IN ('openai', 'moonshot', 'kimi', 'siliconflow', 'groq',
                          'together', 'perplexity', 'openrouter', 'ollama',
                          'lmstudio', 'fireworks', 'cerebras') THEN 'openai'
     WHEN lower(name) IN ('deepseek') THEN 'deepseek'
     WHEN lower(name) IN ('anthropic', 'claude') THEN 'anthropic'
     WHEN lower(name) IN ('gemini', 'google') THEN 'gemini'
     WHEN lower(name) IN ('glm', 'zhipu') THEN 'glm'
     WHEN lower(name) IN ('azure-openai', 'azure') THEN 'azure-openai'
     WHEN lower(name) IN ('minimax') THEN 'minimax'
     WHEN lower(name) IN ('bedrock', 'aws-bedrock') THEN 'bedrock'
     WHEN lower(name) IN ('vertex', 'vertex-ai', 'gcp-vertex') THEN 'vertex'
     ELSE 'openai'
   END;

ALTER TABLE "Provider" ALTER COLUMN "adapter_type" DROP DEFAULT;
ALTER TABLE "Provider" DROP COLUMN "type";
```

No CHECK constraint per FR-5. Verify `npx prisma migrate dev` runs clean
from an empty DB and from the current seeded DB.

### 3. AI Gateway store — `packages/ai-gateway/internal/store/provider.go`

- Replace `Type string // builtin | openai-compatible` with
  `AdapterType string // canonical providers.Format value`.
- Update the `Provider` comment block to describe the new field.
- Update `GetProvider` SELECT list and `Scan`:
  - Replace `type` with `adapter_type` in the column list.
  - Scan into `&p.AdapterType`.

### 4. AI Gateway resolver — `packages/ai-gateway/internal/providers/target/resolver.go`

- Add `AdapterType string` (plain string so the package does not depend on
  `providers.Format` alias; conversion happens at the read site).
- `PgResolver.Resolve` reads `pr.AdapterType`, validates non-empty, and sets
  `target.Format = providers.Format(pr.AdapterType)`. Empty value surfaces
  as `fmt.Errorf("provtarget: provider %q: adapter_type empty", providerID)`.

### 5. AI Gateway wiring adapter — `packages/ai-gateway/cmd/ai-gateway/wiring_provtarget.go`

- `providerStoreAdapter.GetProviderByID` copies `prov.AdapterType` into the
  returned `provtarget.ProviderRow{AdapterType: prov.AdapterType, ...}`.

### 6. Delete the name-based lookup

- Delete `packages/ai-gateway/internal/providers/lookup.go` in full.
- Delete any package-level helpers only `lookup.go` used.
- Remove all imports of the deleted symbols.

### 7. Call-site updates (six files)

For each file, drop the `FormatOfProvider(target.ProviderName)` call and
read `target.Format` directly. If `target.Format` is empty, surface the
existing "no compatible provider" / "unknown format" error.

| File | Change |
|------|--------|
| `packages/ai-gateway/internal/execution/executor/executor.go` | Line ~76 — use `target.Format`; return clear error when empty. |
| `packages/ai-gateway/internal/router/strategy_smart.go` | Line ~199 — use `routerTarget.Format`. |
| `packages/ai-gateway/internal/pipeline/aiguard/backend_provider.go` | Line ~48 — use `target.Format`. |
| `packages/ai-gateway/internal/handler/cross_format.go` | Line ~46 — use `t.Format`. |
| `packages/ai-gateway/internal/handler/routing_simulate_endpoint.go` | Line ~127 — use `t.Format`. |
| `packages/ai-gateway/internal/handler/provider_test_endpoint.go` | Line ~41 — use `target.Format`; drop name argument and `FormatOfProvider` call. |

### 8. Test updates

- `packages/ai-gateway/internal/router/strategy_smart_test.go`: replace
  `providers.RegisterProviderFormat("fake-anthropic", providers.FormatAnthropic)`
  with setting `Format` on the CallTarget fixture directly.
- Update any resolver / executor test that built a `Provider` / `ProviderRow`
  to populate `AdapterType`.
- Add a new resolver test that `PgResolver.Resolve` returns an error when
  `AdapterType` is empty.

### 9. Grep gates

After the change, these commands return zero hits inside `packages/`:

```bash
rg 'FormatOfProvider|RegisterProviderFormat|UnregisterProviderFormat' \
   packages/ -t go
rg '"type"\s*:|p\.Type\b|provider\.Type\b|Provider\.Type\b' \
   packages/ai-gateway -t go
```

(The `-A`/`-B` search above should find only unrelated `Type` fields such as
`IdpEntry.Type`; no `Provider.Type` references should remain.)

## Acceptance criteria

- [ ] Prisma schema has `adapterType` required and no `type` column on Provider.
- [ ] Migration applies cleanly to both empty DB and current dev DB; seeds still run.
- [ ] `store.Provider.AdapterType` exists; `Type` field removed; `GetProvider` selects `adapter_type`.
- [ ] `provtarget.ProviderRow.AdapterType` exists and is copied into `CallTarget.Format`.
- [ ] Empty `adapter_type` surfaces as an explicit resolver error.
- [ ] `lookup.go` is deleted; no production or test reference to `FormatOfProvider`.
- [ ] All six call sites use `target.Format`.
- [ ] `go test -race -count=1 ./packages/ai-gateway/...` green.
- [ ] Grep gates pass.

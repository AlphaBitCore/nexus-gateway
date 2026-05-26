# E87 — Endpoint Typology Unification

**Status:** Design — pending implementation (Phase 1 starting 2026-05-25)
**Date:** 2026-05-25
**Author:** nexus + Claude (deep investigation 2026-05-25 during A11 doc-backfill)
**Stories:** E87-S1 (Phase 1), E87-S2 (Phase 2), E87-S3 (Phase 3)
**Related, untouched:** routing-architecture, cost-estimation-architecture (consume the new types in Phase 2 callsite migration)

---

## 1. Goal

Replace the **three independent overlapping enum families** that today describe "what kind of API request is this" with a single canonical 3-axis taxonomy, so:

- Adding a new endpoint kind (e.g. realtime audio, video editing, structured-output) touches **one** package, not three.
- The audit / persistence layer stores a stable value that matches the routing + hook-filter layer's vocabulary, so no analytics SQL needs cross-table value translation.
- Compliance Proxy + Agent classifiers and AI Gateway dispatch use the **same** type names with **identical** semantics, so a "chat" request observed at the agent is the same `EndpointKind` value as a "chat" request observed at the gateway.

Architectural principle (binding for this epic): **endpoint typology has three orthogonal axes that must not be mashed into a single enum**:

1. **What** — `EndpointKind`: the semantic category of the request (chat, embedding, tts, image_generation, …).
2. **How** — `WireShape`: the request body / response body format (openai-chat, anthropic-messages, gemini-generate, …).
3. **Where** — `IngressPath`: the HTTP path the request was received on (`/v1/chat/completions`, `/v1/responses`, …). AIGW-internal only.

One request always has exactly one value on each axis. The same `EndpointKind` (e.g. chat) can be served over different `WireShape`s (openai-chat vs anthropic-messages vs gemini-generate); the same `WireShape` can be served over different `IngressPath`s (openai-chat over `/v1/chat/completions` vs `/v1/responses`). Mixing the axes into one enum is what produced today's drift.

## 2. Why

### 2.1 Current state: 11 partially-overlapping type definitions, 4 distinct taxonomies for "chat"

Today's tree has **eleven** type / function definitions that describe endpoint typology, and they conflict:

| # | Type / Function | Package | Axis it actually serves | Sample values | Problem |
|---|---|---|---|---|---|
| 1 | `provcore.Endpoint` | `ai-gateway/internal/providers/core/types.go` | **Mixes Axis 1+2+3** | `chat_completions`, `responses`, `completions_legacy`, `embeddings`, `models` | `chat_completions` and `responses` are both chat (Axis 1) over different wire shapes (Axis 2); enum can't separate them |
| 2 | `hookcore.EndpointType` | `shared/policy/hooks/core/types.go` | Pure Axis 1 (semantic) | `chat`, `embeddings`, `image_generation`, `tts`, `stt`, `video_generation`, `batch`, `job` | 8 values; missing `models`; the cleanest of the three Axis-1 enums |
| 3 | `audit.EndpointType` | `ai-gateway/internal/platform/audit/audit.go` | Axis 1, but named like Axis 3 | `chat/completions`, `embeddings`, `responses`, `completions`, `stt`, `tts`, `image_generation`, `batch` | Uses HTTP-path-shaped strings for a semantic-category column. Two constants (`Responses`, `Completions`) are dead — `EndpointTypeFromPath` never produces them |
| 4 | `audit.EndpointTypeFromPath` | same file | Axis 3 → Axis 1 (audit naming) | path in, `chat/completions` etc. out | One of two functions with this name; different output values from #5 |
| 5 | `hookcore.EndpointTypeFromPath` | `shared/policy/hooks/core/endpoint_classify.go` | Axis 3 → Axis 1 (hookcore naming) | path in, `chat` etc. out | Second `EndpointTypeFromPath`; same input → different output value from #4 |
| 6 | `classify.PathSegmentToEndpointType` | `shared/traffic/classify/path.go` | Axis 3 → Axis 1 (hookcore naming, via type alias) | path in, `chat` etc. out | Third path→type function; literally identical body to #5 |
| 7 | `RoutingContext.EndpointType` (untyped string) | `ai-gateway/internal/routing/core/types.go` | Axis 1, with audit-style naming | comment claims `chat/completions \| embeddings \| models` | Routing matcher reads this as untyped string |
| 8 | `Model.Type` | `ai-gateway/internal/platform/store/provider.go` | Axis 1, coarsest | `chat`, `embedding`, `image`, `audio` | Only 4 values; encodes "model capability category"; conflated with EndpointKind via routing capability filter |
| 9 | `Model.InputModalities` / `OutputModalities` | same file | Axis 1 finer (raw `[]string`) | `["text"]`, `["text","image"]`, `["embedding"]` | No enum; free-form strings; a fourth Axis-1 vocabulary |
| 10 | `Provider.adapter_type` (DB column) → `Model.ProviderAdapterType` (Go) | same file | Pure Axis 2 | `openai`, `anthropic` (DB) | Column name "adapter_type" is misleading — it is the wire shape; only correct Axis-2 source today |
| 11 | `classify.AdapterID` | `shared/traffic/classify/classify.go` | Pure Axis 2 (CP/Agent) | `openai-compat`, `azure-openai`, `cohere`, `gemini`, `vertex` | Same concept as #10 with different naming + different value enumeration |

**Concrete drift symptoms today**:

- A request to `POST /v1/chat/completions` produces `provcore.Endpoint = "chat_completions"`, `hookcore.EndpointType = "chat"`, `audit.EndpointType = "chat/completions"` — three different strings for one user intent.
- `traffic_event.endpoint_type` column stores `chat/completions`; routing rule `match.endpointType` SQL is keyed off `chat/completions`; hook pipeline filter is keyed off `chat`; reading these tables together requires cross-translation that has caused production bugs.
- Adding a new endpoint kind (the recent OpenAI Realtime API, the recent video generation endpoints) requires editing **at least four** files in three different packages with non-obvious dependency order.
- Two dead constants (`audit.EndpointTypeResponses`, `audit.EndpointTypeCompletions`) survive in the audit package because removing them feels risky — the actual code path collapses both into `chat/completions`.

### 2.2 Why a 3-axis taxonomy resolves it

The three axes are **independently necessary**:

- The hook pipeline must filter by **what** the request is doing (don't run a chat-text PII redactor on raw audio TTS bytes), regardless of what wire shape it arrives in.
- The codec layer must dispatch by **how** the request is shaped on the wire (parse OpenAI Chat vs Anthropic Messages), regardless of what endpoint kind it represents.
- The HTTP handler must dispatch by **where** the request landed (`/v1/responses` ≠ `/v1/chat/completions`), to know which adapter's request-shape decoder to call.

Mashing these into one enum forces every downstream consumer to either over-fit to one axis (e.g. `provcore.Endpoint` baking the HTTP path into the dispatch key) or duplicate the enum with a different axis bias (e.g. `audit.EndpointType` re-naming hookcore values to fit the persistence shape). The unification keeps each axis as its own typed value and provides a single `ClassifyPath(method, path)` function that returns all three.

## 3. The target architecture

### 3.1 Canonical types — single home

```
packages/shared/transport/typology/
  endpointkind.go      — EndpointKind enum (Axis 1)
  wireshape.go         — WireShape enum (Axis 2)
  classify.go          — ClassifyPath(method, path) → (EndpointKind, WireShape, ok)
  defaults.go          — built-in rule table (replaces shared/traffic/classify/defaults.go)
  typology_test.go
```

**`EndpointKind`** (typed string, Axis 1, single source of truth):

| Value | Covers |
|---|---|
| `chat` | `/v1/chat/completions`, `/v1/messages` (Anthropic), `/v1/responses`, `/v1/completions` (legacy), Gemini `generateContent`, Bedrock Converse |
| `embedding` | `/v1/embeddings`, Cohere `/v1/embed`, Gemini `embedContent`, Vertex `:predict` (embedding models), Voyage `/v1/embeddings` |
| `image_generation` | `/v1/images/generations` (OpenAI), `/v1/images/edits`, `/v1/images/variations`, provider-specific image endpoints |
| `tts` | `/v1/audio/speech` (OpenAI), provider-specific text-to-speech endpoints |
| `stt` | `/v1/audio/transcriptions`, `/v1/audio/translations` |
| `video_generation` | provider video-gen endpoints (placeholder until a provider lands one in prod) |
| `batch` | `/v1/batches` (OpenAI), provider async batch endpoints |
| `job` | provider long-running job endpoints (Bedrock `InvokeModelAsync`, Vertex jobs) |
| `models` | `/v1/models` (catalog read; never carries user content) |

**`WireShape`** (typed string, Axis 2, single source of truth):

| Value | Wire format |
|---|---|
| `openai-chat` | OpenAI Chat Completions JSON (`messages` array, `model`, `stream`, …) |
| `openai-responses` | OpenAI Responses API JSON (`input`, `instructions`, item-typed output) |
| `openai-completions-legacy` | OpenAI legacy text-completions JSON (`prompt` string) |
| `openai-embeddings` | OpenAI embeddings JSON |
| `openai-audio-speech` | OpenAI TTS JSON |
| `openai-audio-transcriptions` | OpenAI STT multipart |
| `openai-images` | OpenAI image-gen JSON |
| `anthropic-messages` | Anthropic Messages JSON |
| `gemini-generate-content` | Gemini `:generateContent` JSON |
| `gemini-embed-content` | Gemini `:embedContent` JSON |
| `bedrock-converse` | AWS Bedrock Converse JSON |
| `cohere-chat` | Cohere chat JSON |
| `cohere-embed` | Cohere embed JSON |
| `vertex-predict` | Vertex AI predict JSON (generic) |
| `voyage-embeddings` | Voyage embeddings JSON |
| (extensible; per-provider variants added as adapters land) |

The full WireShape list at E87-S1 commit time is the union of every value `Provider.adapter_type` could legitimately hold (matching the provider adapters under `packages/ai-gateway/internal/providers/specs/`).

**`IngressPath`** (typed string, **AIGW-internal only**, not exported via `shared/`):

The literal HTTP path the request hit, used purely for AIGW handler dispatch (`mux.Handle` registration). Not persisted, not seen by CP/Agent, not used by hooks. Stays as plain string in AIGW's internal `ingress/proxy/` package; no shared enum needed.

### 3.2 The single `ClassifyPath` function

```go
// In packages/shared/transport/typology/classify.go:
func ClassifyPath(method, path string) (EndpointKind, WireShape, bool) { … }
```

Replaces all three of #4, #5, #6 in the current-state table. Takes the raw HTTP method + path, returns the two canonical axis values. Returns `(_, _, false)` when nothing matches (caller defaults to "unclassified", which is the same backward-compatible semantics today's `classify.Classifier` provides).

AIGW dispatch, CP forward, Agent intercept, hook pipeline filter, audit persistence, routing rule matcher — **every consumer calls this one function** and reads the two typed values off the result.

### 3.3 Mapping: today → E87-final

| Today | E87-final | Note |
|---|---|---|
| `provcore.Endpoint = "chat_completions"` | `EndpointKind="chat"` + `WireShape="openai-chat"` | Split into two axes |
| `provcore.Endpoint = "responses"` | `EndpointKind="chat"` + `WireShape="openai-responses"` | Same as above with different wire |
| `provcore.Endpoint = "completions_legacy"` | `EndpointKind="chat"` + `WireShape="openai-completions-legacy"` | Same kind, legacy wire |
| `provcore.Endpoint = "embeddings"` | `EndpointKind="embedding"` + `WireShape="openai-embeddings"` (when openai-shape) | |
| `provcore.Endpoint = "models"` | `EndpointKind="models"` + `WireShape=""` (no body) | `models` carries no request body so wire shape is empty |
| `hookcore.EndpointTypeChat = "chat"` | `EndpointKind="chat"` | Direct rename (alias kept in Phase 2) |
| `hookcore.EndpointTypeEmbeddings = "embeddings"` | `EndpointKind="embedding"` | **Singular** — matches Model.Type convention; new value is `embedding` not `embeddings` |
| `audit.EndpointType = "chat/completions"` | `EndpointKind="chat"` | Phase 3 DB migration |
| `audit.EndpointType = "embeddings"` | `EndpointKind="embedding"` | Phase 3 DB migration; rename from plural |
| `Model.Type = "chat"` | derived from `Model.SupportedEndpointKinds` containing `chat` | Phase 3 — column removed |
| `Provider.adapter_type` (DB column) | `Provider.wire_shape` | Phase 3 DB rename |
| `classify.AdapterID = "openai-compat"` | `WireShape="openai-chat"` (or appropriate value) | classify.Classifier returns the WireShape directly |

### 3.4 Model capability layer

After E87, model capability is encoded by:

- `Model.SupportedEndpointKinds []EndpointKind` — the EndpointKinds this model can serve. Derived from a model row's existing `Type` + `InputModalities` + `OutputModalities` during Phase 3 migration. An omni model lists `[chat, image_generation, tts]`; a pure embedding model lists `[embedding]`; a STT-only model lists `[stt]`.
- `Model.Features []string` — orthogonal capability flags (`vision`, `function_calling`, `streaming`, `json_mode`, `thinking`, …). Unchanged from today.
- `Model.InputModalities` / `OutputModalities` — kept for fine-grained matching; routing capability filter reads both.

Routing rule capability pre-filter (today: "is `Model.Type` compatible with `RoutingContext.EndpointType`?") becomes: "does `Model.SupportedEndpointKinds` contain `RoutingContext.EndpointKind`?". Same idea, cleaner data model.

## 4. Per-phase plan

### Phase 1 — E87-S1: Add canonical typology package (no breaking change)

**Scope**:
- New `packages/shared/transport/typology/` package with `EndpointKind`, `WireShape`, `ClassifyPath` as described in §3.
- Built-in rule table populated from the union of today's `provcore.Endpoint` switch + `shared/traffic/classify/defaults.go` + `audit.EndpointTypeFromPath` switch.
- ≥95% unit coverage per coverage policy: tests for every supported (method, path) combination + unknown-path fallback + every (EndpointKind, WireShape) combination.
- Zero callsite changes anywhere else in the tree. Old enums unchanged.

**Acceptance**:
- `go test ./packages/shared/transport/typology/... -cover -count=1` reports ≥95%.
- `go vet ./packages/...` clean.
- `git grep -nE 'typology\.(EndpointKind|WireShape|ClassifyPath)' packages/` finds the type/function defs and the test file only (no production callers yet — that's Phase 2).
- A Go-level unit-test fixture demonstrates the (method, path) → (EndpointKind, WireShape) mapping for **every** path AIGW currently registers a handler on (chat completions, responses, completions legacy, embeddings, models, audio/transcriptions, audio/translations, audio/speech, images/generations, images/edits, images/variations, batches).

**Breaking-change ledger**: none. This phase is pure-add.

**Risks**: enum value bikeshedding. Mitigation: §3.1 fixes the value list at design time; Phase 1 implementation does not negotiate.

### Phase 2 — E87-S2: Migrate internal callsites (no breaking change, wire-format compat shim)

**Scope adjustment (2026-05-25, in-flight):** Phase 2 actual scope narrowed to "FromPath duplication eliminated + hookcore/classify type-aliased to typology". The full 9-row callsite table below originally targeted in Phase 2 — provcore.Endpoint, Record.EndpointType type change, Classifier rewrite, RoutingContext type change — is bundled into Phase 3 because Phase 3 deletes those legacy types entirely; migrating them in Phase 2 would be disposable work. The acceptance criterion is the FromPath/type-alias subset (see "Acceptance" below).

**Scope** (callsites to migrate):

| Callsite | Current type read/written | After E87-S2 |
|---|---|---|
| `packages/ai-gateway/internal/ingress/proxy/proxy.go` handler dispatch | `provcore.Endpoint` | `typology.EndpointKind` + `typology.WireShape` from `ClassifyPath` |
| `packages/ai-gateway/internal/ingress/proxy/proxy_cache.go` | same | same |
| `packages/ai-gateway/internal/routing/core/types.go` `RoutingContext.EndpointType` | untyped string | `typology.EndpointKind` typed field |
| `packages/ai-gateway/internal/routing/matcher/matcher.go` | reads `ctx.EndpointType` string | reads typed `EndpointKind` |
| `packages/shared/policy/hooks/core/types.go` `EndpointType` + `EndpointTypeFromPath` | own enum + own function | type alias to `typology.EndpointKind`; `EndpointTypeFromPath` delegates to `typology.ClassifyPath` |
| `packages/shared/traffic/classify/classify.go` + `path.go` + `defaults.go` | own `Rule` table + own `PathSegmentToEndpointType` | re-export `typology.EndpointKind` (type alias) + delegate; defaults table merges into `typology.defaults.go` |
| `packages/agent/internal/network/intercept/handler.go` `classifyEndpoint` | `hookscore.EndpointType` via `classify.Classifier` | `typology.EndpointKind` via `typology.ClassifyPath` |
| `packages/compliance-proxy/internal/proxy/forward/forward.go` + `server.go` `Classifier` field | `classify.Classifier` | `typology.ClassifyPath` direct call (no Classifier interface needed; the rule table is in `typology`) |
| `packages/ai-gateway/internal/platform/audit/audit.go` `Record.EndpointType` field + `EndpointTypeFromPath` | own enum + own function | `typology.EndpointKind` field; `EndpointTypeFromPath` delegates |

**Wire-format compat shim** (the trick that makes Phase 2 non-breaking on the wire):

The `TrafficEventMessage.EndpointType` JSON field on MQ wire today carries `chat/completions` / `embeddings` etc. (audit-style strings). Migrating to `EndpointKind` would change this to `chat` / `embedding`, which is a breaking change for any downstream consumer (Hub db-writer, SIEM forwarder, in-flight analytics) that's already deployed and reading the old values.

To keep Phase 2 wire-compatible:
- Add `legacyAuditString(EndpointKind) string` helper in `packages/shared/transport/typology/audit_legacy.go`.
- AI Gateway's audit writer calls this helper when marshalling `TrafficEventMessage.EndpointType` — wire continues to carry `chat/completions`.
- Hub's `traffic_event` writer reads the legacy string and continues to insert it.
- `traffic_event.endpoint_type` column continues to receive `chat/completions`.

The compat shim is **explicitly marked as Phase-3-removal-target** with a `// REMOVE IN E87-S3` comment.

**Acceptance** (Phase 2 narrowed scope per 2026-05-25 adjustment):
- The three `*EndpointTypeFromPath` switches across hookcore, classify, and audit are eliminated — all three now delegate to a single `typology.KindFromPathSegment` function.
- `hookcore.EndpointType` and `classify.EndpointType` are type aliases of `typology.EndpointKind`; the 8 `hookcore.EndpointType*` constants are aliases of `typology.EndpointKind*` constants (same underlying values, byte-identical wire format).
- The audit MQ wire format byte-identical with pre-E87 production state, achieved via the `typology.LegacyAuditEndpointString` compat shim (REMOVE IN E87-S3).
- (Bundled into Phase 3 per scope adjustment) Full migration of: provcore.Endpoint callsites, Record.EndpointType field type, Classifier interface rewrite, RoutingContext.EndpointType type change, agent + CP intercept direct `typology.ClassifyPath` calls.
- `go vet ./packages/...` clean.
- `go test` per-package green; coverage gate passes (`scripts/check-go-coverage.sh`).
- AIGW smoke (`tests/scripts/smoke-gateway.py --all-ingress`) passes — wire format byte-identical, traffic_event rows look unchanged.
- A migration-audit grep documented in the commit message confirms zero remaining old-enum references outside compat shims.

**Breaking-change ledger**: none on the wire / DB. Internal Go API breaks (every consumer's import path changes); intentional.

**Risks**:
- Routing-rule matcher reading the typed `EndpointKind` may produce different results from reading the untyped string if the strings drift (e.g. matcher expects "chat/completions" but typed value is `chat`). Mitigation: the compat shim layer also exposes a `legacyAuditString` for the matcher's old string-key path during Phase 2; matcher migration is one of the last migrations to land in this story.
- Hook `SupportsEndpoint(EndpointType)` methods read the type by value; the type alias `hookcore.EndpointType = typology.EndpointKind` preserves call shape without churn.

### Phase 3 — E87-S3: DB schema migration + remove legacy enums + bundled Phase 2 deferrals (breaking change, prod migration)

**Scope adjustment (2026-05-25, in-flight):** in addition to the original Phase 3 scope, Phase 3 absorbs the four bundled Phase 2 callsite migrations (provcore.Endpoint, Record.EndpointType, Classifier rewrite, RoutingContext type change, agent + CP direct ClassifyPath). The combined work is appropriate here because all four are legacy-enum-deletion concerns — they share the "remove and update callers in lockstep" pattern with the original Phase 3 enum removal.

**Sub-phase progress (2026-05-25):**

- **3a-1 Classifier delete** — DONE (commit `37769ce0b`): deleted shared/traffic/classify package (Classifier interface + Registry + Rule + 4 helper files + 2 test files); rewired CP/Agent/tlsbump to call typology.ClassifyPath directly; removed WithEndpointClassifier injection; -960 LOC.
- **3a-2 RoutingContext retype** — DONE (commit `924b98e22`): retyped routing/core.RoutingContext.EndpointType from string to typology.EndpointKind; matcher.go translates back via typology.LegacyAuditEndpointString at the rule-condition boundary (compat hop, removed in 3c when rule data migrates).
- **3a-3 audit.Record.EndpointType retype** — COLLAPSED INTO 3b: per architecture brainstorm 2026-05-25, retyping audit.EndpointType from `= string` to `= typology.EndpointKind` without changing wire values is purely cosmetic. The real audit unification only happens when wire values change (3b). 3a-3 absorbed into 3b's "Wire format change" step.
- **3a-4 provcore.Endpoint deletion** — PARTIALLY LANDED (cache slice only): the original "101-site migration" estimate undercounts; sub-agent scoping found 976 touch sites total because provcore.Endpoint is in the public AdapterSpec interface (Transport.BuildURL, SchemaCodec.EncodeRequest/DecodeResponse, StreamDecoder.Open) cascading into ~20 adapter packages. provcore.Endpoint conflates EndpointKind (semantic, Axis 1) and WireShape (request body format, Axis 2). 5 enum values map as: ChatCompletions→(Chat, OpenAIChat), ResponsesAPI→(Chat, OpenAIResponses), CompletionsLegacy→(Chat, OpenAICompletionsLegacy), Embeddings→(Embeddings, OpenAIEmbeddings), Models→(Models, ∅). Every callsite chooses one axis based on its concern (cache key = WireShape; routing dispatch = EndpointKind; audit emit = legacy string via LegacyAuditEndpointString).
- **3a-cache (carve-out of 3a-4, DONE):** commit `4f4aa5711` collapsed the cache layer's redundant two-field tagging (`OriginEndpoint provcore.Endpoint` + `OriginBodyFormat provcore.Format`) into a single `OriginWireShape typology.WireShape` field across 15 files. `CanonicalBridge.ResponseAcrossFormats` signature collapsed in lockstep: `(fromFormat, fromEndpoint, toFormat) → (from, to typology.WireShape)`. Added `(Ingress).WireShape()` resolver + exported `LegacyEndpointAndFormatToWireShape` helper. Cached Redis entries with old shape don't match new lookups (acceptable — cache regenerates on TTL). Live AIGW smoke deferred to the post-3a-4 PR per the scoped-retest-only feedback memory; unit + cross-ingress regression tests cover the typed-field collapse behavior.
- **3a-4 remaining (provcore.Endpoint enum delete + AdapterSpec interface):** queued for a dedicated worktree. Touches ~20 adapter packages (transport.go/codec.go/stream.go per openai/anthropic/azure/bedrock/cohere/gemini/glm/minimax/replicate/vertex/voyage/etc.) + Ingress.Endpoint field + the 95% coverage gate per package + the full AIGW `--all-ingress` smoke. Honest estimate: 4-6 hours focused work, dedicated session + PR per docs-backfill code-anchored protocol.

**Scope**:

1. **DB schema migration** (`tools/db-migrate/migrations/<ts>_e87_endpoint_typology_unification/migration.sql`):
   - `ALTER TABLE "Provider" RENAME COLUMN "adapter_type" TO "wire_shape";` (cleanest name match)
   - `ALTER TABLE "traffic_event" ADD COLUMN wire_shape TEXT;` (new column)
   - Drop and recreate `traffic_event.endpoint_type` CHECK constraint to accept canonical EndpointKind values
   - Backfill: `UPDATE "traffic_event" SET endpoint_type = CASE endpoint_type WHEN 'chat/completions' THEN 'chat' WHEN 'embeddings' THEN 'embedding' ELSE endpoint_type END;` (one-shot scan; ~10M rows in prod; runs ~5 min)
   - Backfill: `UPDATE "traffic_event" SET wire_shape = …` derived from `provider_id` + historical endpoint_type at backfill time
   - Drop `Model.Type` column after deriving `SupportedEndpointKinds` JSONB column

2. **Wire format change**: `TrafficEventMessage.EndpointType` JSON field now carries canonical EndpointKind strings (`chat`, `embedding`, …). Compat shim from Phase 2 deleted. **Downstream MQ consumers (Hub db-writer, SIEM forwarder) updated in lockstep within this commit** — that's the breaking change.

3. **Remove legacy types**:
   - Delete `provcore.Endpoint` enum + `EndpointTypeString` helper
   - Delete `audit.EndpointType` constants (already aliased in Phase 2; remove the aliases)
   - Delete `Model.Type` field from Go struct + DB column
   - Delete the dead `audit.EndpointTypeResponses` + `audit.EndpointTypeCompletions` constants
   - Delete `classify.AdapterID` (replaced by `WireShape`)
   - Delete `Classifier` interface in `shared/traffic/classify` (CP/Agent call `typology.ClassifyPath` directly)

4. **Analytics + dashboard updates**:
   - Sweep `tools/db-migrate/seed/` for any seed SQL referencing `chat/completions` literally
   - Sweep `packages/control-plane/internal/observability/analytics/**` for SQL or Prisma queries reading `endpoint_type`
   - Update Control Plane UI `traffic-event` filter dropdowns (`packages/control-plane-ui/src/api/services/**`) to use new EndpointKind values
   - Document the value-rename in the prod-deploy runbook (`docs/operators/ops/runbooks/prod-deploy-data-changes.md`)

5. **Scenario test sweep**: `tests/scenarios/*_test.go` — any test asserting on `endpoint_type` literal `chat/completions` updates to `chat`.

6. **L5 scenarios**: at least one new scenario test exercises the value-migration end-to-end (POST `/v1/chat/completions`, read traffic_event, assert `endpoint_type = 'chat'`). Per CLAUDE.md L5 binding, the new scenario must have a live `--- PASS` run output in the PR.

**Acceptance**:
- `npm run check:doc-lockstep`, `scripts/check-go-coverage.sh`, all other CI gates green.
- `go vet ./packages/...` clean; `npm test` for CP UI green.
- AIGW smoke `--all-ingress` green.
- Live L5 scenario `--- PASS` evidenced in PR description (CLAUDE.md L5 binding).
- prod-deploy-data-changes runbook updated and read by user before merge.
- A11 doc (`docs/developers/architecture/cross-cutting/foundation/endpoint-typology-architecture.md`) written **after** the code lands, reflecting the final state. No legacy section.

**Breaking-change ledger**:
- **Wire format**: `nexus.event.{ai-traffic,compliance,agent}` `TrafficEventMessage.EndpointType` value vocabulary changes from `chat/completions` to `chat` (and similar). Any in-flight messages enqueued before the deploy carry old values; the new consumer must accept both for the 6-hour `NEXUS_EVENTS` stream MaxAge after deploy (post-MaxAge, only new values exist).
- **DB schema**: `Provider.adapter_type` renamed; `Model.Type` dropped; `traffic_event.endpoint_type` value vocabulary changed; `traffic_event.wire_shape` added.
- **Analytics SQL**: any operator query reading `endpoint_type = 'chat/completions'` breaks. Sweep in this commit covers Nexus-owned SQL; external dashboards (Grafana, etc.) are operator-owned and warned via runbook.
- **CP UI**: filter dropdowns change values; backwards-incompatible API contract change on the admin analytics endpoints. CP-UI updated in lockstep.

**Risks**:
- In-flight MQ messages straddling the deploy. Mitigation: new consumer accepts both old and new values for one stream MaxAge cycle (~6 h) post-deploy; then a follow-up commit removes the dual-read.
- Long backfill on large `traffic_event`. Mitigation: backfill runs in a single UPDATE with no `WHERE` filter — Postgres handles 10M rows in ~5 min on the prod hardware; not blocking enough to need batching, but a `prod-deploy-data-changes.md` entry documents the expected runtime.
- Operator-owned external dashboards may break silently. Mitigation: runbook + a Hub `system_metadata` marker `e87_endpoint_kind_migrated_at` so operators can grep for "rows before this timestamp use legacy strings" in their own tooling.

## 5. Non-goals

This epic does **not**:

- Introduce a generic "request taxonomy" framework for arbitrary HTTP APIs. The 3 axes are specific to LLM-style traffic.
- Add new EndpointKind values beyond what today's enum unions cover (no speculative `realtime` or `web_search` unless one of the supported providers ships such an endpoint).
- Refactor the routing rule grammar. Routing rules continue to match on the same fields; only the value-format of the `endpointKind` match field changes.
- Touch the hook pipeline structure, the cost-estimator pricing formula, the cache layer, or any non-typology concerns. Each consumer reads the new types; their logic is unchanged.
- Migrate historical `traffic_event` rows older than the migration date in dev/test environments. Phase 3's backfill is prod-only; dev environments re-seed.

## 6. Per-phase 2-round review gate (CLAUDE.md binding)

Each phase commit requires:

1. **Round 1 self-audit** (CLAUDE.md mandatory rule 2.5, 4 questions):
   - Q1: every sub-task done?
   - Q2: zero `TODO`/`FIXME`/`unimplemented`/`stub` in production code?
   - Q3: every changed path tested or explicitly acknowledged untested?
   - Q4: no "we'll fix later" claims unless user pre-approved?
2. **Round 2** verifies the fixes from Round 1.
3. User-presented Chinese summary of the phase's diff (per `feedback_docs_backfill_code_anchored_protocol` Step 9 binding).
4. User approval → commit.
5. Push + PR + (user-driven merge).

The phase plan in this doc is the anti-drift anchor; every phase commit message references back to its §4 sub-section by anchor.

## 7. Post-E87 contributor guide

After E87-S3 lands, adding a new endpoint kind (e.g. realtime audio) takes:

1. Add the value to `packages/shared/transport/typology/endpointkind.go`.
2. Add any new WireShape(s) the endpoint uses to `wireshape.go`.
3. Add the (method, path) → (kind, shape) rule to `typology/defaults.go`.
4. If a provider adapter is needed, add `packages/ai-gateway/internal/providers/specs/<name>/`.
5. Update `Model.SupportedEndpointKinds` seed data for models that serve the new kind.
6. Update the hook `SupportsEndpoint` helpers if a new "ChatOnly"-style preset is needed.

Nothing else. No second `EndpointType` enum to mirror, no `EndpointTypeFromPath` function to copy-paste, no audit-vs-routing translation table to update.

## References

- `packages/ai-gateway/internal/providers/core/types.go` — current `provcore.Endpoint` enum being retired
- `packages/shared/policy/hooks/core/types.go` — current `hookcore.EndpointType` enum becoming the canonical EndpointKind
- `packages/ai-gateway/internal/platform/audit/audit.go` — current `audit.EndpointType` enum being retired
- `packages/shared/traffic/classify/` — current `Classifier` infrastructure being absorbed into `typology/`
- `packages/ai-gateway/internal/routing/core/types.go` — `RoutingContext.EndpointType` typed-up in Phase 2
- `packages/ai-gateway/internal/platform/store/provider.go` — `Model.Type` column being dropped
- `tools/db-migrate/schema.prisma` — `Provider.adapter_type` → `wire_shape` rename in Phase 3

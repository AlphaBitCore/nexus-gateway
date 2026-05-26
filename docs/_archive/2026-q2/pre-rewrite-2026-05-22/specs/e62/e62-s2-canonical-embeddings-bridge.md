# E62-S2 — Canonical Embeddings Types, Canonical Bridge, SchemaCodec Interface Extension, Model Capability Matrix

> Story: e62-s2
> Epic: 62
> Status: In-progress
> Date: 2026-05-19
> Requirements: `docs/developers/specs/e62/e62-cross-adapter-embeddings.md` §FR-2, §FR-6, §FR-7, §FR-8
> Architecture: `docs/developers/architecture/cross-cutting/foundation/endpoint-typology-architecture.md` §2, §3, §4, §5; `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md` §3a Rules 1-7 (generalised per typology doc §4.4); `docs/developers/architecture/services/ai-gateway/normalization-architecture.md` (E58-S0 decode delegation contract)
> Memory: `project_e62_cross_adapter_embeddings`, cross-ref `feedback_token_field_handler_sweep` (token field stamp sweep rule applies to embedding usage too)
> Blocked by: none
> Blocks: S3 (codecs depend on canonical types + capability matrix), S4 (audit references endpoint_type + capability), S5 (smoke depends on bridge + codecs)

---

## User Story

As a **gateway implementer adding embeddings + future multimodal endpoints**, I want canonical embedding types defined in `providers/core/types.go`, a working `IngressEmbeddingsToCanonical` bridge mirroring `IngressChatToCanonical`, an extended `SchemaCodec` interface that supports binary content and async artefacts ahead of time, and a `Model` capability matrix that the routing engine consults for the reject-asymmetry rule — so that S3 codecs plug into a complete framework and so that future epics (E63 audio, E64 image, E65 batch, E66 video) need zero interface churn.

---

## Tasks

### T1 — Canonical embedding types

- T1.1 In `packages/ai-gateway/internal/providers/core/types.go`, add:
  ```go
  type EmbeddingsRequest struct {
      Model           string             `json:"model"`
      Input           EmbeddingsInput    `json:"input"`           // string | []string | []int
      Dimensions      *int               `json:"dimensions,omitempty"`
      EncodingFormat  *string            `json:"encoding_format,omitempty"` // "float" | "base64"
      User            *string            `json:"user,omitempty"`
      // Extension fields ride via canonicalext under nexus.ext.<provider>.*
  }

  type EmbeddingsInput struct {
      // exactly one of:
      String  *string
      Strings []string
      Tokens  [][]int
  }
  func (e *EmbeddingsInput) UnmarshalJSON([]byte) error { /* discriminator */ }
  func (e EmbeddingsInput) MarshalJSON() ([]byte, error) { /* discriminator */ }

  type EmbeddingsResponse struct {
      Object  string                  `json:"object"` // "list"
      Data    []EmbeddingDataItem     `json:"data"`
      Model   string                  `json:"model"`
      Usage   EmbeddingsUsage         `json:"usage"`
  }

  type EmbeddingDataItem struct {
      Object    string    `json:"object"`           // "embedding"
      Embedding []float32 `json:"embedding"`         // empty if EncodingFormat=base64
      Base64    string    `json:"-"`                 // populated when EncodingFormat=base64
      Index     int       `json:"index"`
  }

  type EmbeddingsUsage struct {
      PromptTokens int `json:"prompt_tokens"`
      TotalTokens  int `json:"total_tokens"`
  }
  ```
- T1.2 Add `Endpoint` constant `EndpointEmbeddings = "embeddings"` if not already present. Verify against existing code (the SDD references show `EndpointEmbeddings` is already defined).
- T1.3 Add `Usage` struct field `Usage.EmbeddingTokens int` IF the existing `Usage` struct does not already carry an analog. Decision: reuse `Usage.PromptTokens` (semantically equivalent: input tokens), `Usage.TotalTokens`; set `Usage.CompletionTokens=0` explicitly. No new Usage field needed.
- T1.4 Unit tests for `EmbeddingsInput` discriminator marshalling: round-trip for string / []string / []int variants. Plus an explicit error on mixed shapes.

### T2 — Canonical bridge

- T2.1 In `packages/ai-gateway/internal/execution/canonicalbridge/bridge.go` (or a sibling `embeddings.go`), add:
  ```go
  func (b *Bridge) IngressEmbeddingsToCanonical(format Format, body []byte, target CallTarget) ([]byte, error)
  func (b *Bridge) ResponseCanonicalToIngressEmbeddings(format Format, canonical []byte) ([]byte, error)
  ```
  Symmetric with chat. Both consult the per-format codec in `b.codecs`.
- T2.2 `EndpointRoutable` is updated for `EndpointEmbeddings`:
  ```go
  case EndpointEmbeddings:
      _, ingressOK := b.codecs[ingress]
      _, targetOK  := b.codecs[target]
      if !ingressOK || !targetOK { return false }
      return true   // capability pre-filter (T5) handles the asymmetry check separately
  ```
  Remove the old `return ingress == target` short-circuit.
- T2.3 Stream variants: N/A for embeddings (FR-2.5). Document in source comment. Future-proofing note: if a provider ever ships streaming embeddings, add `StreamEmbeddingsSession` mirroring `StreamSession` — no canonical bridge change needed.
- T2.4 `canonicalext` helpers used unchanged for `nexus.ext.<provider>.*` on the embedding canonical body. Cohere `input_type`, Gemini `taskType`, etc.

### T3 — SchemaCodec interface extension (structured results)

- T3.1 In `packages/ai-gateway/internal/providers/core/spec.go`, change `SchemaCodec` to use structured result types:
  ```go
  type EncodeResult struct {
      Body        []byte        // wire body
      ContentType string        // "application/json" default; varies for multipart / binary
      Headers     http.Header   // per-request headers (multipart boundary, x-goog-api-key, etc.)
      URLOverride string        // optional URL-path override (e.g. Gemini :embedContent vs :batchEmbedContents); empty = default
      Rewrites    []string      // human-readable transform list (stamped on x-nexus-coerced)
  }
  type DecodeResult struct {
      CanonicalBody []byte
      Usage         Usage
      Artifacts     []ArtifactRef
  }
  type SchemaCodec interface {
      EncodeRequest(endpoint Endpoint, canonicalBody []byte, target CallTarget) (EncodeResult, error)
      DecodeResponse(endpoint Endpoint, nativeBody []byte, contentType string) (DecodeResult, error)
  }
  ```
  Rationale: returning a struct rather than positional values lets future per-request controls (signed trailers, multipart parts, idempotency tokens) extend the struct without breaking the interface again. See `endpoint-typology-architecture.md` §4.1.
- T3.2 Add `ArtifactRef`, `ArtifactKind`, `JobRef`, `JobStatus` to `packages/ai-gateway/internal/providers/core/types.go`:
  ```go
  type ArtifactKind string
  const (
      ArtifactKindImage ArtifactKind = "image"
      ArtifactKindAudio ArtifactKind = "audio"
      ArtifactKindVideo ArtifactKind = "video"
      ArtifactKindJob   ArtifactKind = "job"
  )
  type ArtifactRef struct {
      Kind      ArtifactKind
      MIMEType  string
      URL       string
      Bytes     []byte
      Base64    string
      JobID     string
      Width     int
      Height    int
      DurationS float64
      SizeBytes int64
  }
  type JobRef struct { ProviderID, JobID, InternalID string; SubmittedAt time.Time }
  type JobStatus string
  const ( JobStatusQueued = "queued"; JobStatusRunning = "running"; JobStatusSucceeded = "succeeded"; JobStatusFailed = "failed"; JobStatusCanceled = "canceled" )
  ```
- T3.3 `AsyncAdapter` interface signature is **NOT declared in E62**. Only `JobRef`, `JobStatus`, and `ArtifactKind=job` (referenced by `ArtifactRef`) ship in E62. The concrete `SubmitJob` / `PollJob` / `CancelJob` interface shape lands in **E65's orchestrator SDD**, validated against the orchestrator implementation. Pre-declaring an interface against unknown requirements violates the "interface widens only once" commitment we make in §T3.1.
- T3.4 Migrate all existing chat / responses codecs to the new structured result. Mechanical: every `EncodeRequest` returns `EncodeResult{Body: prev_body, ContentType: "application/json"}`; every `DecodeResponse` returns `DecodeResult{CanonicalBody: prev_canonical, Usage: prev_usage}`. Existing tests pass unchanged.
- T3.5 The `PassthroughRewrite` callback signature in `AdapterSpec` is **not** affected. No change.

### T4 — Model capability matrix migration

- T4.1 Prisma migration `tools/db-migrate/migrations/<ts>_e62_model_capability.sql`:
  ```sql
  ALTER TABLE "Model"
      ADD COLUMN "inputModalities"  TEXT[] NOT NULL DEFAULT ARRAY['text'],
      ADD COLUMN "outputModalities" TEXT[] NOT NULL DEFAULT ARRAY['text'],
      ADD COLUMN "lifecycle"        TEXT   NOT NULL DEFAULT 'sync',
      ADD COLUMN "capabilityJson"   JSONB;
  ```
- T4.2 `tools/db-migrate/schema.prisma` Model entity gains the four fields per FR-8.1.
- T4.3 Codegen regenerates Go types in `packages/shared/storage/<gen>/` (or wherever Prisma-Go codegen lands today).
- T4.4 Seed file (`tools/db-migrate/seed/seed.ts`) — chat models keep defaults (`text` / `text` / `sync` / NULL). New embedding model rows set explicitly:
  - `text-embedding-3-small`: `outputModalities=["embedding"]`, `lifecycle="sync"`, `capabilityJson={"embeddings":{"max_input_tokens":8191,"supported_dimensions":[512,1024,1536],"default_dimension":1536,"max_batch_size":2048}}`. (Verify max_batch_size against latest OpenAI docs at impl time.)
  - `text-embedding-3-large`: similar but dimensions up to 3072 and `default_dimension=3072`.
  - `text-embedding-ada-002`: `supported_dimensions` omitted (model rejects `dimensions` field — comment cites observed 400). `default_dimension=1536`.
  - Azure equivalents: same `capabilityJson` per model; URL path templated per deployment.
  - `embed-multilingual-v3`: `outputModalities=["embedding"]`, `capabilityJson={"embeddings":{"max_input_tokens":512,"supported_dimensions":[1024],"default_dimension":1024,"max_batch_size":96,"supported_input_types":["search_document","search_query","classification","clustering"]}}`.
  - `embed-english-v3`: same.
  - `text-embedding-004` (Gemini): `outputModalities=["embedding"]`, `capabilityJson={"embeddings":{"max_input_tokens":2048,"supported_dimensions":[768],"default_dimension":768,"max_batch_size":100,"supported_task_types":["RETRIEVAL_QUERY","RETRIEVAL_DOCUMENT","SEMANTIC_SIMILARITY","CLASSIFICATION","CLUSTERING","QUESTION_ANSWERING","FACT_VERIFICATION"]}}`.
- T4.5 Each capability value cited in seed gets a SQL `-- comment` referencing the upstream doc URL + date observed. Per CLAUDE.md and `provider-adapter-architecture.md` §3a Rule 7 (empirical evidence).
- T4.6 Migration is forward-only (CLAUDE.md no-back-compat). No down-migration script.
- T4.7 **Hot-path caching (binding).** The routing engine consults `capabilityJson` on every request; raw JSONB parse per request is unacceptable. A typed Go struct `ModelCapabilitySnapshot` is parsed from `capabilityJson` once at routing-snapshot construction and on shadow-pushed config changes (mirroring how `Model.inputPricePerMillion` and other fields are cached today). Snapshot rotation uses the existing `atomic.Pointer` swap pattern under `packages/ai-gateway/internal/routing/`. Pre-filter (T5) consults the snapshot, not the JSONB column.

### T5 — Routing pre-filter (capability-based asymmetry reject)

- T5.1 In `packages/ai-gateway/internal/routing/` (the engine), add a candidate-target pre-filter step that runs BEFORE scoring:
  ```go
  func (r *Router) preFilterCandidates(req CanonicalRequest, candidates []Target) []Target {
      kept := []Target{}
      for _, t := range candidates {
          if !r.capabilityCompatible(req, t.Model) { continue }
          kept = append(kept, t)
      }
      return kept
  }
  ```
- T5.2 `capabilityCompatible` implements the rules from `endpoint-typology-architecture.md` §5.3:
  - Endpoint type: `req.EndpointType == target.Model.outputModalities` → embedding endpoint expects `outputModalities` containing `"embedding"`, chat endpoint expects `"text"`.
  - Input modalities: `req.InputModalities ⊆ target.Model.inputModalities`.
  - Lifecycle: `req.Lifecycle == target.Model.lifecycle`.
  - Endpoint-specific (embeddings): if `req.Dimensions != nil`, then `*req.Dimensions ∈ target.Model.capabilityJson.embeddings.supported_dimensions`. If `req.BatchSize > 1`, then `req.BatchSize <= max_batch_size`. If `req.EncodingFormat != ""`, then `req.EncodingFormat ∈ supported_encoding_formats` (defaulting to `["float","base64"]` when omitted).
- T5.3 **A single target failing the filter is NOT a client error** — it is dropped from the candidate pool; routing proceeds with surviving candidates (the priority / weighting / health-check engine then picks one). HTTP 400 fires **only when every candidate fails the pre-filter** (candidate pool empty). The error envelope adapts to ingress format AND includes an `available_capabilities` array enumerating the considered targets so admin can self-debug:
  - OpenAI / Azure: `{"error":{"code":"no_compatible_provider","message":"No routing target supports dimensions=N","param":"dimensions","type":"invalid_request_error","available_capabilities":[{"provider":"openai","model":"text-embedding-3-small","supported_dimensions":[512,1024,1536]},...]}}`
  - Cohere: `{"message":"No routing target supports dimensions=N","code":"no_compatible_provider","available_capabilities":[...]}`
  - Gemini: Google error shape extended with `available_capabilities` in `error.details[0]`.
- T5.4 The capability pre-filter is endpoint-agnostic in design. Chat-specific filters (today none — chat models accept all chat shapes) plug in via the same rule machinery. Future epics (image-gen size, video duration) add their own clauses without rearchitecting.
- T5.5 Codec safety-net (FR-6.3): if a request bypasses the pre-filter due to admin config drift, the codec returns `400 invalid_request` rather than silently mutating user input. Concretely: each codec's `EncodeRequest` validates against the codec's own `Model.capabilityJson` snapshot and rejects on mismatch.
- T5.6 Per-route policy `on_capability_mismatch: reject | warn-and-continue` (default `reject`) on the routing rule. When `warn-and-continue` is set AND a target's required extension is missing (e.g. Cohere v3 needs `input_type`), the routing engine fills from a per-route default-extensions map (FR-6.6) and stamps `x-nexus-coerced: ext.cohere.input_type=auto-filled` on the response. Default-strict behaviour preserved if the admin does not opt in.

### T6 — Tests

- T6.1 `EmbeddingsRequest` / `EmbeddingsResponse` JSON marshalling round-trip — table-driven over all field combos.
- T6.2 `IngressEmbeddingsToCanonical` for each (ingress format, target format) pair in {OpenAI, Azure, Cohere, Gemini}^2. Cohere ingress + OpenAI target → canonical OpenAI shape; OpenAI ingress + Gemini target → canonical OpenAI shape (Gemini-specific extensions ride via `nexus.ext.gemini.*`).
- T6.3 `EndpointRoutable` for `EndpointEmbeddings` returns true for compatible pairs, false for unregistered formats.
- T6.4 `capabilityCompatible` truth-table tests: every endpoint-specific constraint exercised against representative model fixtures.
- T6.5 Routing pre-filter integration test: a request with `dimensions=1024` routes through OpenAI (supports) but rejects when Cohere is the only candidate (doesn't support 1024 — Cohere is 1024 fixed only? — verify against seed).
- T6.6 SchemaCodec interface migration test: every existing chat codec's `EncodeRequest` returns `contentType="application/json"`; every existing `DecodeResponse` returns `artifacts=nil`. Behaviour parity for chat is preserved.
- T6.7 Prisma migration smoke: apply migration to a clean dev DB, verify existing chat Model rows have safe defaults, new embedding seed rows have populated `capabilityJson`.
- T6.8 Coverage ≥95% on `providers/core/`, `execution/canonicalbridge/`, `routing/`.

### T7 — Documentation

- T7.1 Update `provider-adapter-architecture.md` §3a if any Rule wording needs to extend to `EndpointEmbeddings` explicitly (currently the text says "OpenAI chat-completions shape" — the scope note added in E62 requirements drafting frames this, but Rule 1's wording may still benefit from a parenthetical "(per-endpoint canonical; see endpoint-typology-architecture.md §2)").
- T7.2 No new files; updates are inline.

---

## Acceptance Criteria

- A1: Canonical `EmbeddingsRequest`, `EmbeddingsResponse`, `EmbeddingsUsage`, `EmbeddingsInput`, `EmbeddingDataItem` types exist in `providers/core/types.go` with correct JSON marshalling.
- A2: `canonicalbridge.IngressEmbeddingsToCanonical` and `ResponseCanonicalToIngressEmbeddings` exist and pass round-trip tests for all four in-scope provider formats.
- A3: `EndpointRoutable(EndpointEmbeddings, ingress, target)` returns true for cross-format pairs when both formats are registered.
- A4: `SchemaCodec` interface signature carries `(body, contentType, rewrites, err)` and `(canonical, usage, artifacts, err)`. All existing chat codecs migrated successfully; chat smoke green.
- A5: `ArtifactRef`, `ArtifactKind`, `AsyncAdapter`, `JobRef`, `JobStatus` declared (no impl).
- A6: `Model` table gains `inputModalities`, `outputModalities`, `lifecycle`, `capabilityJson`. Migration applies cleanly. Seed populates capability for in-scope embedding models with empirical citations.
- A7: Routing engine pre-filters candidates by capability. Empty candidate list → HTTP 400 `no_compatible_provider` shaped per ingress format.
- A8: Codec safety-net rejects mismatched fields with `400 invalid_request` rather than mutating input.
- A9: All chat smoke phases (P3, P3R, P3A, P3G) continue passing — no regression.
- A10: Coverage ≥95% on every modified package.

---

## Out of Scope (S2)

- Concrete codec implementations for OpenAI / Azure / Cohere / Gemini embeddings — that is S3.
- Audit row endpoint_type stamping logic — that is S4.
- Smoke phase P3E — that is S5.
- AsyncAdapter implementations — that is E65 (orchestrator).
- Image / audio / video canonical types — those are E63 / E64 / E66.
- Admin UI for capability matrix display / edit — out of E62 (FR-8.4 deferred).
- GLM embedding codec — out of E62 (FR-3.6 deferred).

---

## Implementation Notes

- The `EmbeddingsInput` discriminator pattern is the trickiest part of T1. JSON union types in Go usually use either (a) custom Marshal/Unmarshal with a discriminator field, or (b) `json.RawMessage` + lazy parsing. Choose (a) — explicit discriminator makes downstream consumers' lives easier.
- The capability pre-filter (T5) is a routing concern; the rule data (`capabilityJson`) is a Model concern. Don't let routing engine code mutate Model rows; routing reads a cached snapshot.
- The codec safety-net (T5.5) duplicates a check that the pre-filter already did. This is intentional — pre-filter is the fast path; codec is the safety net for config drift. Codec rejection produces audit row with `pipeline_skipped_reason="codec_capability_safety_net"`.
- The `Usage` struct's `EmbeddingTokens` decision (T1.3) — reuse `PromptTokens` because OpenAI's embedding response uses `prompt_tokens`. This is consistent with the rest of the alias-extraction logic (`shared/normalize/codecs/openai_chat.go` and friends handle the `prompt_tokens` alias).
- Migration order: capability matrix first (T4), then types (T1), then bridge (T2), then routing pre-filter (T5). The reverse order causes test failures because tests reference each other's outputs.
- The interface change (T3) is the breaking change. All adapter packages need their codec methods touched. Single PR for the change keeps build coherent.

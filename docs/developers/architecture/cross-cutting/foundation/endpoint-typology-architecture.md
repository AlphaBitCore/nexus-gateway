# Endpoint typology architecture

The `packages/shared/transport/typology/` package is the single source of truth for how Nexus classifies every HTTP traffic event it handles. One Go package; six source files; two typed-string enumerations; one path-classification function. Every service that asks "what kind of API call is this?" or "what wire format does it use?" calls into this one package — there is no parallel taxonomy elsewhere in the tree, and there is no upstream-protocol library leaking these concepts into service code.

Nexus classifies every traffic event along three orthogonal axes: **what kind of API call it is** (semantic), **what wire shape its body uses** (protocol), and **which ingress path delivered it** (route). The first two are platform-wide canonical types — `typology.EndpointKind` and `typology.WireShape`. The third is AI Gateway-internal — the route table is the only place that knows the literal HTTP path. A single `ClassifyPath(method, path)` function maps every observed `(method, path)` pair to the matching `(EndpointKind, WireShape)` pair, and the rest of the stack reasons about typology values from that point forward.

Anchor packages:

- `packages/shared/transport/typology/` — the **single source of truth**: `EndpointKind` + `WireShape` constants, the `Rule` struct, the 28-rule built-in table, `ClassifyPath(method, path)`, and the `KindFromPathSegment(segment)` path-segment lookup used by the audit pipeline.
- `packages/shared/policy/hooks/core/types.go` — `hookcore.EndpointType = typology.EndpointKind` (type alias) plus the eight `EndpointType*` re-export constants. Hook configs match against `EndpointType`; the hook pipeline reads the canonical kind via this alias.
- `packages/ai-gateway/internal/routing/core/types.go` — `RoutingContext.EndpointType typology.EndpointKind`. The routing matcher reads it via `resolveField("endpointType")`.
- `packages/ai-gateway/internal/execution/canonicalbridge/api.go` — `CanonicalBridge.ResponseAcrossFormats(from, to typology.WireShape, body []byte)` is the cache-HIT cross-format reshape boundary. `WireShape` on both sides; the bridge decodes via the `from` shape and re-encodes via the `to` shape.
- `packages/ai-gateway/internal/ingress/proxy/ingress.go` — `Ingress.WireShape typology.WireShape` is the route-table field that carries the canonical wire shape for cache tagging + codec dispatch. Every route in `cmd/ai-gateway/wiring/routes.go` declares the WireShape directly (e.g. `WireShape: typology.WireShapeAnthropicMessages` for `/v1/messages`).

## 1. The 3-axis classification

Every HTTP traffic event Nexus handles carries three independent classifications:

| Axis | Type | Carries | Lives in |
|---|---|---|---|
| **1. Semantic** | `typology.EndpointKind` | What the user is asking the model to do — chat, embeddings, image generation, … | `packages/shared/transport/typology/endpointkind.go` |
| **2. Wire shape** | `typology.WireShape` | Which request body protocol the upstream expects — `openai-chat`, `anthropic-messages`, `gemini-generate-content`, … | `packages/shared/transport/typology/wireshape.go` |
| **3. Ingress path** | `string` | The literal HTTP path that delivered the request (`/v1/chat/completions`, `/openai/deployments/<dep>/chat/completions`, `/api/paas/v4/chat/completions`) | AI Gateway route table only |

The axes are independent:

- **One `EndpointKind` is served over many `WireShape` values.** `EndpointKindChat` covers `WireShapeOpenAIChat`, `WireShapeOpenAIResponses`, `WireShapeOpenAICompletionsLegacy`, `WireShapeAnthropicMessages`, `WireShapeGeminiGenerateContent`, `WireShapeVertexGenerateContent`, `WireShapeBedrockConverse`, `WireShapeCohereChat` — every wire format that asks a model to produce a response to a prompt.
- **One `WireShape` rides over many ingress paths.** `WireShapeOpenAIChat` is served at `/v1/chat/completions` (canonical OpenAI), `/openai/deployments/*/chat/completions` (Azure), and `/api/paas/v4/chat/completions` (Zhipu GLM) — same wire format, different URL conventions.
- **Axis 3 is route-table state, not a typology constant.** The AI Gateway route table is the only place that knows the literal HTTP path; Compliance Proxy + Agent observe the path on intercepted traffic and immediately reduce it to `(Kind, Shape)` via `ClassifyPath`. There is no `typology.IngressPath` type because no cross-service consumer needs the path string after classification.

## 2. EndpointKind (Axis 1)

`EndpointKind` is a typed string. Nine values cover every supported semantic category:

| Constant | Value | Covers |
|---|---|---|
| `EndpointKindChat` | `chat` | `/v1/chat/completions`, `/v1/responses`, `/v1/completions` (legacy), `/v1/messages` (Anthropic), Gemini `:generateContent`, Vertex `:generateContent`, Bedrock Converse |
| `EndpointKindEmbeddings` | `embeddings` | `/v1/embeddings` (OpenAI / Azure / GLM), `/v1/embed` + `/v2/embed` (Cohere), Gemini `:embedContent` / `:batchEmbedContents`, Vertex `:embedContent` / `:batchEmbedContents`, Voyage embeddings |
| `EndpointKindImageGeneration` | `image_generation` | `/v1/images/generations`, `/v1/images/edits`, `/v1/images/variations` |
| `EndpointKindTTS` | `tts` | `/v1/audio/speech` and provider-specific text-to-speech |
| `EndpointKindSTT` | `stt` | `/v1/audio/transcriptions`, `/v1/audio/translations`, and provider-specific speech-to-text |
| `EndpointKindVideoGeneration` | `video_generation` | Reserved for provider video-generation endpoints; no rules match yet |
| `EndpointKindBatch` | `batch` | `/v1/batches` and provider-specific async batch ingest |
| `EndpointKindJob` | `job` | Long-running provider job endpoints (Bedrock InvokeModelAsync, Vertex prediction jobs) |
| `EndpointKindModels` | `models` | `/v1/models`, `/v1/models/{model}` — never carries user content; hook pipeline and cost layer ignore |

The closed enumeration is exported as `AllEndpointKinds` for exhaustiveness tests + UI population. `(EndpointKind).IsValid()` reports membership; the empty `EndpointKind` is treated as **unclassified** (no rule matched) and falls through every kind-aware filter — no special-casing required.

The string values are the canonical wire format. They appear verbatim in the `traffic_event.endpoint_type` column, on the `TrafficEventMessage.EndpointType` MQ field, in the `ai-gateway` Prometheus `endpoint` label, and as the cost-formula registry key. Renaming a constant value is a coordinated change across DB columns, Prometheus labels, MQ wire formats, and downstream analytics SQL.

## 3. WireShape (Axis 2)

`WireShape` is a typed string identifying the request/response body protocol. The naming convention is `<vendor>-<shape>` in kebab-case, where the vendor prefix is the body's protocol family (not the upstream brand — every OpenAI-compatible provider rides `openai-*` shapes).

19 named values + the `WireShapeNone` sentinel cover every adapter currently shipped:

| Family | Constants |
|---|---|
| OpenAI family | `openai-chat`, `openai-responses`, `openai-completions-legacy`, `openai-embeddings`, `openai-audio-speech`, `openai-audio-transcriptions`, `openai-images`, `openai-batches` |
| Anthropic | `anthropic-messages` |
| Google Gemini (AI Studio) | `gemini-generate-content`, `gemini-embed-content` |
| Google Vertex AI | `vertex-generate-content`, `vertex-embed-content` |
| AWS Bedrock | `bedrock-converse`, `bedrock-invoke`, `bedrock-embeddings` |
| Cohere | `cohere-chat`, `cohere-embed` |
| Voyage AI | `voyage-embeddings` |
| Sentinel | `WireShapeNone` (empty string) — endpoints with no request body, e.g. `GET /v1/models` |

The closed enumeration is exported as `AllWireShapes` (excluding the sentinel). `(WireShape).IsValid()` mirrors `EndpointKind.IsValid()`. Callers that need "is there a body to parse?" check against `WireShapeNone` rather than the empty string literally.

The OpenAI family prefix `openai-*` covers every OpenAI-shape-compatible provider — that is the architectural choice. A request to vLLM or Together AI riding the `POST /v1/chat/completions` shape carries `WireShape = openai-chat`, even though the upstream brand is not OpenAI. The wire shape says **what the body looks like**, not **whose server it goes to**.

## 4. IngressPath (Axis 3 — AI Gateway-internal only)

The literal HTTP path that delivered a request is meaningful only inside the AI Gateway route table (`packages/ai-gateway/cmd/ai-gateway/wiring/routes.go`), where the path → handler binding lives. The route table entry carries `(path, WireShape, BodyFormat)` — `WireShape` is the per-request typology shape (Axis 2) and `BodyFormat` is the adapter-family identifier (`provcore.Format`; see §10).

Downstream of dispatch, no consumer needs the path string. The Compliance Proxy and Agent see the path on intercepted traffic, call `ClassifyPath` once, and discard the path — they reason about `(Kind, Shape)` from that point forward. The hook pipeline, the routing matcher, the cost estimator, the audit writer, the cache tagger, the Prometheus labeler — none of them know or care about the path that delivered the request. The typology package therefore does not export an `IngressPath` type: there would be no cross-service consumer.

## 5. The typology package

`packages/shared/transport/typology/` is one Go package, no subdirectories. Six source files + their tests:

| File | Contents |
|---|---|
| `typology.go` | Package doc — the 3-axis principle and the package's "single source of truth" role |
| `endpointkind.go` | `EndpointKind` type, 9 constants, `AllEndpointKinds`, `IsValid()`, `String()` |
| `wireshape.go` | `WireShape` type, 19 constants + `WireShapeNone`, `AllWireShapes`, `IsValid()`, `String()`, `KindFromWireShape` |
| `classify.go` | `Rule` struct, `ClassifyPath(method, path)`, the internal `matchPath` / `globMatch` / `equalFold` helpers |
| `defaults.go` | The 28-rule built-in table consumed by `ClassifyPath` |
| `path_segment.go` | `KindFromPathSegment(segment)` for path-segment lookup used by audit + hook pipelines |

The package has zero `packages/shared/` dependencies and zero AI Gateway / Compliance Proxy / Agent / Hub dependencies — every other package can import it freely. Tests cover the rule table, the glob matcher, the legacy mappings, and the validity predicates at 96%+ statement coverage.

## 6. ClassifyPath

`ClassifyPath(method, path string) (EndpointKind, WireShape, bool)` is the only function that maps a path to its typology. Every consumer — AI Gateway dispatch, Compliance Proxy forward handler, Agent intercept handler, hook pipeline filter, audit persistence, routing rule matcher — calls this one function.

**Semantics.** Method comparison is case-insensitive. Path matching uses single-segment glob where `*` matches any run of non-slash characters within the same path segment (`**` is not supported). Rules are evaluated in registration order and the first match wins. When no rule matches the function returns `("", WireShapeNone, false)` — callers treat unclassified as "kind-aware filters pass through".

**The built-in table** (`packages/shared/transport/typology/defaults.go`) has 28 rules covering:

- Every path AI Gateway's HTTP mux registers (chat, responses, messages, embeddings, models, Gemini `generateContent` / `embedContent` / `batchEmbedContents`, Azure deployment-suffixed paths, GLM PAAS v4 paths).
- Every upstream-provider path Compliance Proxy + Agent intercept transparently (OpenAI, Azure OpenAI, Cohere, Gemini AI Studio, Vertex AI for embeddings).
- Every canonical OpenAI endpoint that may be intercepted (audio transcriptions, audio speech, image generation, batch ingest, legacy completions).

**Precedence pitfall: Vertex before Gemini.** Vertex AI paths look like `/v1*/projects/<proj>/locations/<loc>/publishers/<pub>/models/<model>:generateContent`. The Gemini AI Studio pattern `/v1*/models/*:generateContent` can match these via the `*` star expanding across slashes when bracketed by `/`. Because first-match-wins is the precedence rule, **Vertex rules are registered before Gemini rules** so the more specific path classifies first. New rules that could subset an existing pattern must be placed before it.

## 7. Path-segment helper

`KindFromPathSegment(segment string) EndpointKind` (`packages/shared/transport/typology/path_segment.go`) maps the AI Gateway's internal path-segment string (the trimmed suffix after the API-version prefix) to its canonical `EndpointKind`. It is the single source of truth shared by `shared/policy/hooks/core.EndpointTypeFromPath` and `ai-gateway/internal/platform/audit.EndpointTypeFromPath`.

| Input segment | Resolves to |
|---|---|
| `chat/completions`, `completions`, `responses` | `EndpointKindChat` |
| `embeddings` | `EndpointKindEmbeddings` |
| `audio/transcriptions`, `audio/translations` | `EndpointKindSTT` |
| `audio/speech` | `EndpointKindTTS` |
| `images/generations`, `images/edits`, `images/variations` | `EndpointKindImageGeneration` |
| `batches` | `EndpointKindBatch` |
| anything else | `""` (unclassified — kind-aware filters fall through) |

Adding a new path-segment form is a one-line change in this switch. The matching `ClassifyPath` rule must be added in `defaults.go` in the same commit.

The wire vocabulary downstream of the helper is the canonical `EndpointKind` string verbatim: `audit.Record.EndpointType`, `TrafficEventMessage.EndpointType`, `traffic_event.endpoint_type`, the cost-formula registry key in `packages/ai-gateway/internal/execution/estimator/cost_formula_registry.go`, and the Prometheus `endpoint` label all carry the canonical strings (`chat`, `embeddings`, `stt`, `tts`, `image_generation`, `batch`). No translation hop exists between the helper and persistence.

The routing matcher (`packages/ai-gateway/internal/routing/matcher/matcher.go: resolveField("endpointType")`) returns the canonical `EndpointKind` value directly; routing-rule conditions that filter on `endpointType` compare against the canonical kind strings.

## 8. Cache integration

Cache entries — both response cache (`packages/ai-gateway/internal/cache/semantic/`) and stream cache (`packages/ai-gateway/internal/cache/stream/`) — tag every entry with a single `OriginWireShape typology.WireShape` field. The cache-HIT comparison is one equality test:

```go
// packages/ai-gateway/internal/ingress/proxy/proxy_cache.go
sameShape := entry.OriginWireShape == ingress.WireShape
```

When the shapes differ the cache layer calls `CanonicalBridge.ResponseAcrossFormats(entry.OriginWireShape, ingress.WireShape, respBody)`. The bridge:

1. Decodes `respBody` using the `from` shape's codec into the canonical chat-completions representation.
2. Re-encodes the canonical form using the `to` shape's codec.
3. Returns the reshaped bytes.

Codec selection lives entirely inside `Bridge.responseToCanonical` and the matching forward encoder; the cache layer holds no codec logic. A `WireShape` value the bridge does not know returns an error, which the cache HIT path treats as "fall back to verbatim bytes" — clients receive the original cached body and a warning is logged.

**The `Ingress.WireShape` field** (`packages/ai-gateway/internal/ingress/proxy/ingress.go`) carries the canonical wire shape directly — no derivation needed. The route table in `cmd/ai-gateway/wiring/routes.go` declares each route's wire shape literally (e.g. `WireShape: typology.WireShapeOpenAIChat` for `/v1/chat/completions`, `WireShape: typology.WireShapeAnthropicMessages` for `/v1/messages`, `WireShape: typology.WireShapeGeminiGenerateContent` for `/v1beta/models/{model}:generateContent`).

## 9. Routing integration

`RoutingContext.EndpointType` is `typology.EndpointKind`. The proxy handler derives the canonical kind once per request from the resolved ingress wire shape (`endpointType := string(typology.KindFromWireShape(resolved.WireShape))`) and stamps it onto both `audit.Record.EndpointType` and the routing context — no string round-trip through a path-segment form.

Downstream of construction:

- **The capability pre-filter** (`packages/ai-gateway/internal/routing/resolver.go`) compares `rctx.EndpointType == typology.EndpointKindEmbeddings` to decide whether to apply embedding-specific capability rules.
- **The matcher's field extractor** (`packages/ai-gateway/internal/routing/matcher/matcher.go: resolveField("endpointType")`) returns the canonical `EndpointKind` value (`chat`, `embeddings`, …) directly. Routing rule conditions that filter on `endpointType` compare against canonical kind strings.

## 10. Format vs WireShape

`provcore.Format` and `typology.WireShape` look related but answer different questions and both are needed:

- **`provcore.Format`** — adapter-family identifier. One Format per registered provider adapter (`FormatOpenAI`, `FormatAnthropic`, `FormatGemini`, …). Acts as: codec registry key (`Bridge.codecs map[Format]SchemaCodec`), routing dispatch key (one provider has one Format), CallTarget.Format field, SSE encoder family selector (NewStreamTranscoder takes Format because OpenAI-family wire shapes share the OpenAI SSE grammar).
- **`typology.WireShape`** — per-request wire shape identifier. Identifies the specific JSON body shape on a single request (`openai-chat`, `openai-responses`, `anthropic-messages`, `gemini-generate-content`, …). One adapter (= one Format) can serve multiple wire shapes (OpenAI serves `openai-chat` + `openai-responses` + `openai-embeddings` + `openai-completions-legacy`). Acts as: codec dispatch parameter (each codec switches on its supported shapes), cache entry tag, Ingress field, routes table value.

The structural projection (Format → adapter's native chat/embeddings WireShape) lives in `canonicalbridge/bridge.go` as `chatWireShapeForFormat` + `embeddingsWireShapeForFormat`. Both are exhaustively unit-tested as lockstep gates: adding a new adapter Format requires adding its native WireShape entry to those helpers (or accepting the OpenAI-family default).

## 11. Per-service surface

| Service | Imports typology for |
|---|---|
| **AI Gateway** (`packages/ai-gateway/`) | Cache `OriginWireShape` tagging; `CanonicalBridge.ResponseAcrossFormats` cross-shape reshape; `RoutingContext.EndpointType` + capability pre-filter; matcher's `resolveField("endpointType")`; audit emit's `EndpointTypeFromPath` (canonical kind verbatim); cost-formula registry key |
| **Compliance Proxy** (`packages/compliance-proxy/`) | `ClassifyPath` inside the TLS-bump forward handler (`shared/transport/tlsbump/forward_handler.go`) to derive `EndpointKind` for hook-pipeline filtering on intercepted traffic |
| **Agent** (`packages/agent/`) | `ClassifyPath` inside the intercept handler (`internal/network/intercept/handler.go`) for the same hook-pipeline filtering on locally-intercepted traffic |
| **Nexus Hub** (`packages/nexus-hub/`) | Persists `TrafficEventMessage.EndpointType` into `traffic_event.endpoint_type` byte-for-byte; the wire field carries the canonical kind string already, no translation needed |
| **Control Plane** (`packages/control-plane/`) | Reads the canonical kind strings from `traffic_event` for admin UI display and routing-rule conditions |

Only AI Gateway, Compliance Proxy, and Agent import `packages/shared/transport/typology` directly. Hub and Control Plane see the same canonical kind strings on the wire and on the DB column.

## 12. Adding a new endpoint kind

To add a new `EndpointKind` value (e.g. a hypothetical `EndpointKindFineTune`):

1. **`packages/shared/transport/typology/endpointkind.go`** — add the constant + its doc comment. Append it to `AllEndpointKinds`.
2. **`packages/shared/transport/typology/wireshape.go`** — if the new kind is served over a new wire format, add the `WireShape*` constant + append to `AllWireShapes`.
3. **`packages/shared/transport/typology/defaults.go`** — register the `(method, path) → (Kind, Shape)` rule. Place it in priority order; if it could subset an existing pattern, place it first.
4. **`packages/shared/transport/typology/path_segment.go`** — if the AI Gateway uses a distinct path-segment form for the endpoint (e.g. multiple URL suffixes collapsing to the same kind), add the case to `KindFromPathSegment`.
5. **`packages/shared/policy/hooks/core/types.go`** — add the matching `EndpointType*` re-export constant if hooks need to filter on the new kind.
6. **Tests** — `packages/shared/transport/typology/*_test.go` covers the new rule + legacy mapping. `packages/shared/policy/hooks/core/endpoint_classify_test.go` covers the hook-side filtering.

The 95% package coverage gate (per CLAUDE.md binding) enforces that every new rule and every new legacy mapping carries test assertions.

---
doc: endpoint-typology-architecture
area: cross-cutting
service: foundation
tier: 1
updated: 2026-05-20
---

# Endpoint Typology & Multimodal Gateway Architecture

> **Tier 1 architecture doc.** Read this before adding any new endpoint to the AI Gateway (embeddings, image generation, TTS / STT, video generation, batch / async jobs), before extending the `SchemaCodec` interface, before adding modality-specific hooks, before extending the `Model` capability matrix, or before adding a new phase to the `/smoke-gateway` harness. This doc is the **frame** that `provider-adapter-architecture.md` (chat), the embeddings work (E62), and all subsequent multimodal epics (E63 TTS+STT, E64 image-gen, E65 async jobs, E66 video-gen, E67 modality hooks) fit into. It does **not** replace `provider-adapter-architecture.md`; it generalises it.

Nexus Gateway started as a chat-completions gateway. The chat architecture (`provider-adapter-architecture.md` §3a Rules 1-7) defines a single **canonical bus** (OpenAI chat-completions shape) that every chat provider adapter wires itself into. The translation count collapses from N×M (N ingresses × M providers) to N+M (each side translates to/from canonical).

This pattern is correct for chat. It does **not** generalise verbatim to embeddings / image / audio / video / async jobs — those have their own grammars, their own lifecycle, and (for video) no dominant industry spec. The right generalisation is **per-endpoint canonical, shared backbone**: each endpoint picks its own canonical spec, but every endpoint shares the same routing engine, hook framework, audit lifecycle, codec interface, and smoke harness.

This doc defines that frame.

---

## 1. Endpoint typology — five wire classes

Every AI endpoint Nexus serves falls into one of five typologies. Typology is a **wire-protocol classification**, not a business-functional one — `chat` and `embeddings` are both Typology A even though they do different things, because their HTTP shape is the same (JSON in, JSON out, sync).

| Code | Typology | Examples (today + future) | Request shape | Response shape | Lifecycle |
|---|---|---|---|---|---|
| **A** | Sync JSON I/O | chat completions, embeddings, image-gen JSON-mode, judge calls | JSON | JSON | request → response |
| **B** | Stream JSON delta | chat-stream, responses-api stream | JSON | SSE deltas | request → SSE stream |
| **C** | Sync binary I/O | TTS (text → audio bytes), image-gen binary-mode, STT (audio multipart → JSON) | JSON or multipart | bytes / URL / JSON | request → response (mixed content-type) |
| **D** | Stream binary | TTS-stream (audio frames), image progressive (rare) | JSON | binary frames | request → binary stream |
| **E** | Async job | video-gen, image-batch, long-running fine-tune, OpenAI Batches | JSON | `job_id` envelope | submit → poll / webhook → fetch artifact |

**Hard rule:** every new endpoint **must** be classified into exactly one typology before any other design work begins. Typology drives codec interface choice, lifecycle service wiring, hook applicability, and smoke phase. **Cross-typology endpoints (e.g. "sometimes sync, sometimes async") do not exist** — split them into two endpoint definitions instead.

### Why typology, not provider, is the unit

Provider-centric thinking ("the OpenAI epic", "the Google epic") breaks down once a provider serves multiple typologies. OpenAI serves A (chat, embeddings), B (chat-stream), C (TTS, STT, images), and E (Batches). Google Veo is purely E. Cohere is mostly A. A capability matrix indexed by typology composes cleanly; one indexed by provider does not.

---

## 2. Per-endpoint canonical — pick the industry spec, don't invent

Within each typology, we still need to pick a **canonical body shape** so that N ingresses + M providers translate through one hub. The chat decision was: "OpenAI chat-completions is the industry default; canonical = OpenAI shape." We apply the same principle to every other endpoint — **pick the strongest existing spec, don't invent**.

| Endpoint | Typology | Canonical = | Rationale | Provider-specific extensions go to |
|---|---|---|---|---|
| `chat.completions` | A / B | **OpenAI `/v1/chat/completions`** ✅ already shipped | Industry default; Anthropic, Gemini, DeepSeek, Kimi, GLM all converge to this shape | `nexus.ext.<provider>.<key>` (already used for Anthropic `thinking`, Gemini `thinkingConfig`) |
| `responses` | A / B | **OpenAI `/v1/responses`** ✅ already shipped (E56) | Treated as a separate ingress *format* under the same provider, per `provider-adapter-architecture.md` §3a "Ingress format ≠ canonical: the responses-api case" | `nexus.ext.openai.<key>` for `previous_response_id`, `store`, built-in tools |
| `embeddings` | A | **OpenAI `/v1/embeddings`** (E62) | Cohere `/v1/embed`, Voyage `/v1/embeddings`, Gemini `:embedContent` are all near-supersets of OpenAI's fields | `nexus.ext.cohere.{input_type, embedding_types, truncate}`, `nexus.ext.gemini.{taskType, title, outputDimensionality}` |
| `images.generations` (sync) | A or C | **OpenAI `/v1/images/generations`** (E64) | Minimal field set (`model`, `prompt`, `size`, `n`, `quality`, `response_format`, `style`); Stability / Bedrock richer params (cfg_scale, steps, seed, negative_prompt, controlnet) ride as extensions | `nexus.ext.stability.{cfg_scale, steps, sampler, init_image}`, `nexus.ext.bedrock.<key>` |
| `audio.speech` (TTS) | C / D | **OpenAI `/v1/audio/speech`** (E63) | Industry standard (`model`, `input`, `voice`, `response_format`, `speed`); ElevenLabs `voice_settings`, Google Cloud TTS `audioConfig` ride as extensions | `nexus.ext.elevenlabs.{voice_settings, optimize_streaming_latency}`, `nexus.ext.gcp.audioConfig` |
| `audio.transcriptions` (STT) | C | **OpenAI `/v1/audio/transcriptions`** (E63) | multipart + JSON response is the industry default; Deepgram, AssemblyAI, Whisper all offer OpenAI-compat | `nexus.ext.deepgram.<key>`, `nexus.ext.aai.<key>` |
| `batches` (chat / embed batch) | E | **OpenAI `/v1/batches`** (E65) | OpenAI shipped a clean spec; Anthropic follows the same shape with `/v1/messages/batches` | `nexus.ext.anthropic.<key>` if Anthropic adds non-OpenAI fields |
| `videos.generations` | E | **Nexus-defined minimum canonical** (`prompt`, `duration_seconds`, `aspect_ratio`, `seed`, `n`) + `nexus.ext.<provider>.<key>` (E66) | No industry spec is dominant today (Veo public, Sora preview, Runway / Pika proprietary). Locking to any single vendor's shape today creates migration debt later. We define the minimum subset every provider supports, push everything else to extensions. When (or if) one spec becomes dominant in 2027+, we MAY promote its richer fields into canonical via an arch-doc PR. | `nexus.ext.veo.{personGeneration, enhancePrompt}`, `nexus.ext.sora.{n_seconds, size, quality}`, `nexus.ext.runway.{mode, motion, prompt_image}`, `nexus.ext.pika.<key>`, `nexus.ext.kling.<key>` |
| **async-job lifecycle envelope** (all Typology E) | meta | **Replicate Predictions envelope** for single-job E endpoints; **OpenAI Batches envelope** for batch E endpoints (E65) | Replicate is the industry de-facto for video / image-async (`id`, `status`, `urls.{get,cancel}`, `output`, `error`, `webhook`); OpenAI Batches has its own shape for batch chat/embed that we keep compatible | both forms share a Go-layer `JobRef` struct; wire serialisation differs |

### Rules for picking a canonical (when adding a new endpoint)

1. **Prefer an OpenAI spec when one exists.** OpenAI's API designs are the gravity centre — half the industry already speaks them. Don't invent if OpenAI has a clean spec.
2. **If no OpenAI spec, pick the strongest public spec from a major provider.** Document the choice + rationale in this table.
3. **If no spec is dominant yet (video today), pick one and explicitly list it.** Avoid "union of all providers" — that becomes a maintenance graveyard. Specific provider quirks ride on `nexus.ext.<provider>.*`.
4. **Never invent a universal multi-endpoint canonical** (e.g. `{prompt, modality, params}`). Each endpoint has its own grammar; the abstraction always leaks.
5. **Streaming variant uses the same canonical body**, just wrapped in SSE / chunked transfer — never a separate canonical spec.

### Why `nexus.ext.<provider>.*` is the load-bearing trick

The chat canonical works because Anthropic's `cache_control`, Gemini's `thinkingConfig`, Anthropic's `cache_creation_input_tokens` — fields that have no OpenAI mapping — all ride inside the canonical body under `nexus.ext.<provider>.*`. The same pattern carries every other endpoint:

- Cohere embedding's `input_type=search_query` → `nexus.ext.cohere.input_type` on canonical OpenAI embedding shape.
- Stability image's `cfg_scale=7` → `nexus.ext.stability.cfg_scale` on canonical OpenAI images shape.
- Sora video's `n_seconds=10` → `nexus.ext.sora.n_seconds` on canonical Veo shape.

This is what makes "1-1 translation" possible. Without `nexus.ext.*`, the canonical would have to either absorb every provider's quirks (bloat) or drop them (data loss). Helpers: `packages/ai-gateway/internal/providers/canonicalext/`.

---

## 3. Layered architecture — what is shared, what is per-endpoint

```
┌────────────────────────────────────────────────────────────────────┐
│ L8  Smoke harness (phase fan-out: P3 chat / P3E embed / P3I image /│
│                    P3A audio / P3V video; per-phase assertions)    │ ← shared
├────────────────────────────────────────────────────────────────────┤
│ L7  Routing engine + capability matrix                             │
│     ingress.capability ⊆ target.capability   (reject + pre-filter) │ ← shared engine; per-endpoint capability fields
├────────────────────────────────────────────────────────────────────┤
│ L6  Audit & cost (traffic_event endpoint_type discriminator +      │
│                  modality-specific JSONB metadata; cost calc       │
│                  dispatches on endpoint_type)                      │ ← shared writer; per-endpoint cost formula
├────────────────────────────────────────────────────────────────────┤
│ L5  Hook framework — endpoint-aware + modality-aware               │
│     HookInput.{EndpointType, InputModality, OutputModality}        │
│     Hook.SupportsEndpoint() / SupportsModality()                   │
│     Class-A (content scan, per modality) / Class-B (metadata)      │ ← shared pipeline; per-hook applicability
├────────────────────────────────────────────────────────────────────┤
│ L4  Lifecycle services                                             │
│     (a) Async job orchestrator: Job table + NATS fan-out + webhook │
│     (b) Artifact relay: passthrough URL by default, spillstore opt-in│ ← Typology E only; new in E65
├────────────────────────────────────────────────────────────────────┤
│ L3  SchemaCodec (extended interface)                               │
│     EncodeRequest → (body, contentType, rewrites, err)             │
│     DecodeResponse → (canonical, usage, artifacts, err)            │
│     AsyncAdapter (Submit/Poll/Cancel) — Typology E only            │ ← shared interface; per-provider impl
├────────────────────────────────────────────────────────────────────┤
│ L2  Per-endpoint canonical body shape (the §2 table)               │ ← per-endpoint
├────────────────────────────────────────────────────────────────────┤
│ L1  Endpoint typology (A / B / C / D / E)                          │ ← classification only
└────────────────────────────────────────────────────────────────────┘
```

**The architectural commitment is: layers L3 through L8 are reused unchanged across endpoints.** L1 + L2 are per-endpoint. The chat architecture is the specialisation of this stack for `chat.completions` + `responses`; the multimodal architecture is the same stack with additional L2 hubs filled in.

---

## 4. SchemaCodec — extended interface (binding for all new adapters)

The chat SchemaCodec assumes JSON-in, JSON-out, sync. Once C, D, and E typologies land, this assumption breaks. The interface is extended **once, in E62**, so every subsequent epic plugs in without further interface churn.

### 4.1 Extended SchemaCodec signature

The codec needs to set per-request **headers** (multipart boundary, Gemini `x-goog-api-key`, custom auth schemes), **content type**, and occasionally override the **URL template** (Gemini chooses between `:embedContent` vs `:batchEmbedContents` based on input shape). Returning each as a positional value bloats the signature and locks future extensions. **The codec returns a structured `EncodeResult` / `DecodeResult` so future per-request controls (e.g. trailers, multipart parts, signed headers) can extend the struct without breaking the interface again.**

```go
// packages/ai-gateway/internal/providers/core/spec.go

type EncodeResult struct {
    Body        []byte          // wire body (JSON, multipart, binary)
    ContentType string          // "application/json" by default; varies for multipart / binary
    Headers     http.Header     // per-request headers (multipart boundary, x-goog-api-key, etc.) — merged with transport-level headers (auth, user-agent, trace)
    URLOverride string          // optional URL-path override (Gemini :embedContent vs :batchEmbedContents); empty = use adapter's default template
    Rewrites    []string        // human-readable transform list (stamped on x-nexus-coerced)
}

type DecodeResult struct {
    CanonicalBody []byte
    Usage         Usage           // token / unit accounting (extended via BillableUnits — see §7)
    Artifacts     []ArtifactRef   // binary or URL-typed artefacts (image bytes, audio bytes, video URLs, async job refs). Empty for chat / embeddings.
}

type SchemaCodec interface {
    EncodeRequest(endpoint Endpoint, canonicalBody []byte, target CallTarget) (EncodeResult, error)
    DecodeResponse(endpoint Endpoint, nativeBody []byte, contentType string) (DecodeResult, error)
}
```

Chat / embedding adapters that exist today set `ContentType="application/json"`, `Headers=nil`, `URLOverride=""`, `Artifacts=nil`. **Zero behaviour change** during migration. Future binary / multipart adapters populate the relevant fields without further interface churn.

// ArtifactRef describes a non-JSON response payload component.
type ArtifactRef struct {
    Kind      ArtifactKind // image | audio | video | job
    MIMEType  string       // "image/png", "audio/mpeg", "application/json" (for job ref), ...
    URL       string       // provider-hosted URL, OR nexus-hosted signed URL after spillstore relay
    Bytes     []byte       // inline bytes when MIMEType is small and not URL-served (uncommon)
    Base64    string       // base64 form for OpenAI-shape image responses (`b64_json` field)
    JobID     string       // for Typology E, the provider's job id; canonical envelope reshapes into Replicate predictions form
    Width     int          // image / video dimension (informational)
    Height    int          // image / video dimension (informational)
    DurationS float64      // audio / video duration in seconds (cost basis)
    SizeBytes int64        // byte size (informational, for spillstore decisions)
}
```

**Future adapters (E63 onwards)**: TTS adapter populates `Artifacts[0]` with audio bytes / URL; image-gen adapter populates `Artifacts` with per-image refs; video-gen adapter returns one artifact with `Kind=ArtifactKindJob` and `JobID` set.

### 4.2 Async extension (Typology E) — interface defined in E65, not here

Sync codec interfaces cannot express "submit and wait" semantics cleanly. Typology E adapters need an **additional** interface alongside `SchemaCodec`. The interface SHAPE (`SubmitJob` / `PollJob` / `CancelJob`, plus `JobRef`, `JobStatus`, webhook signing, idempotency key handling, intermediate-artifact streaming) is **defined in the E65 orchestrator SDD**, not pre-declared here.

The rationale: we do not have a concrete async adapter implementation to validate the interface against in E62. Pre-declaring an interface and then breaking it in E65 violates the "interface widening only once" commitment. We instead **commit only to the layered architecture position**: AsyncAdapter is a separate interface from SchemaCodec, lives under `packages/ai-gateway/internal/providers/core/`, and is consumed by the orchestrator service (E65) — and lock the exact signature when E65 has a working implementation to validate against.

`JobRef`, `JobStatus`, and `ArtifactKind=job` are declared in E62 because they appear in `ArtifactRef` (the codec return type), but the `AsyncAdapter` interface itself waits.

### 4.3 Streaming sessions — unchanged from chat

`StreamSession` / `StreamDecoder` from `provider-adapter-architecture.md` §4 carry over verbatim for Typology B and D. The only addition for D (stream binary): the session emits `ArtifactChunkRef` (small piece of bytes / frame) instead of `CanonicalChunk`. This is a thin variant added in E63, not a rewrite.

### 4.4 Conformance rules — extended to all endpoints

Rules 1-7 from `provider-adapter-architecture.md` §3a apply to **every endpoint**, not just chat. Specifically:

| Rule | Generalised statement |
|---|---|
| 1 | Each endpoint has exactly one canonical body shape, chosen from §2 above. New canonical fields require an architecture-doc PR — adapters do not add them unilaterally. |
| 2 | Each non-canonical-shape adapter owns its full bidirectional translation. The canonical-shape adapter (e.g. OpenAI for chat / embed / image / TTS / STT) stays identity. |
| 3 | Per-model wire quirks live in the adapter that talks to that wire — never in `spec_adapter.go` or shared helpers. Generalises to embedding's "ada-002 rejects `dimensions`", image's "dall-e-2 rejects `quality`", etc. |
| 4 | `nexus.ext.<provider>.<key>` for any field with no clean canonical mapping. |
| 5 | Cross-format callers canonicalize **before** the codec runs. For embeddings: `IngressEmbeddingsToCanonical`. For images: `IngressImagesToCanonical`. New helper per endpoint, same pattern. |
| 6 | Streaming and non-streaming parity — N/A for embeddings (no stream), in scope for TTS-stream and chat-stream. |
| 7 | Every model prefix-rule needs an observed-400 citation in the source comment. |

Rule 8 (`shared/normalize` for decode) generalises to every endpoint's response normalisation; new normaliser packages land under `packages/shared/transport/normalize/codecs/` as new wire-formats are added.

---

## 5. Capability matrix — Model table extension

Routing decisions and "reject before codec" gating both need to know **what each (provider, model) tuple can do**. Today the gateway implicitly knows "chat models support chat-completions". With embeddings + multimodal, "implicit" no longer works.

### 5.1 New columns on `Model`

```prisma
model Model {
  // ... existing fields ...

  // Multimodal capability matrix (added in E62).
  inputModalities  String[] @default(["text"])    // ["text"] | ["text","image"] | ["audio"] | ["image"] | ["video"]
  outputModalities String[] @default(["text"])    // ["text"] | ["embedding"] | ["image"] | ["audio"] | ["video"]
  lifecycle        String   @default("sync")      // "sync" | "stream" | "async"
  capabilityJson   Json?                          // free-form per-modality capability bag (see §5.2)
}
```

**Migration policy (E62):** add the four columns with safe defaults that mirror today's chat semantics. No backfill needed for existing chat models — defaults match. Embedding seed rows ship with `outputModalities=["embedding"]`.

### 5.2 `capabilityJson` shape — per-endpoint sub-objects

```jsonc
// Example: text-embedding-3-large
{
  "embeddings": {
    "max_input_tokens": 8191,
    "supported_dimensions": [256, 512, 1024, 1536, 2048, 3072],  // empty = single fixed dim
    "default_dimension": 3072,
    "max_batch_size": 2048
  }
}

// Example: dall-e-3
{
  "images": {
    "supported_sizes": ["1024x1024", "1792x1024", "1024x1792"],
    "supported_qualities": ["standard", "hd"],
    "supported_styles": ["vivid", "natural"],
    "max_n": 1,
    "response_formats": ["url", "b64_json"]
  }
}

// Example: tts-1
{
  "audio_speech": {
    "supported_voices": ["alloy","echo","fable","onyx","nova","shimmer"],
    "supported_formats": ["mp3","opus","aac","flac","wav","pcm"],
    "max_input_chars": 4096
  }
}

// Example: google-veo-1.0
{
  "videos": {
    "supported_aspect_ratios": ["16:9","9:16","1:1"],
    "supported_durations_s": [5, 10, 15, 30],
    "max_n": 1,
    "person_generation_modes": ["allow_adult","dont_allow"]
  }
}
```

Adding a new model = update the seed with the matching `capabilityJson`. Provider-adapter PR + seed PR in lockstep.

**Hot-path performance note (binding).** The routing engine consults `capabilityJson` on **every request**; a raw JSONB parse per request is unacceptable. The routing snapshot cache (today's mechanism for `Model.inputPricePerMillion` and friends) pre-parses `capabilityJson` once into a typed Go struct (`ModelCapabilitySnapshot`) at startup and on shadow-pushed config changes. The pre-filter consults the snapshot, not the JSONB column. Shadow change → atomic snapshot swap (mirrors how chat routing handles `Model` field updates today). Implementation references the existing `atomic.Pointer` swap pattern under `packages/ai-gateway/internal/routing/`.

### 5.3 Routing pre-filter — the asymmetry rule (binding)

When an ingress request would route across providers (e.g. OpenAI ingress → Cohere target), the routing engine **filters candidate targets up front** by checking whether the request's stated requirements fit the target's capability:

```
filter(targets, ingress_req):
    for target in targets:
        target.capability = capabilityJson[endpoint]
        keep iff target satisfies:
            - input_modalities ⊇ ingress_req.input_modalities
            - output_modalities ⊇ ingress_req.output_modalities
            - lifecycle compatible (sync/stream/async match)
            - per-endpoint constraints:
                embeddings:  ingress_req.dimensions in target.supported_dimensions
                             ingress_req.batch_size <= target.max_batch_size
                images:      ingress_req.size in target.supported_sizes
                             ingress_req.quality in target.supported_qualities
                             ingress_req.n <= target.max_n
                audio:       ingress_req.voice in target.supported_voices
                             ingress_req.format in target.supported_formats
                videos:      ingress_req.aspect_ratio in target.supported_aspect_ratios
                             ingress_req.duration_s in target.supported_durations_s
```

**Fallback chain (binding clarification).** Routing pre-filter runs over the **candidate list** that the routing rule's match step produced. A single target failing capability check **drops that target from the candidate pool — it does NOT return 4xx**. As long as ≥1 candidate survives, routing proceeds with the surviving candidates (the priority / weighting / health-check engine then picks one). The HTTP 400 fires only when **every candidate fails the pre-filter**. This mirrors today's chat behaviour where unhealthy / over-quota targets are dropped without surfacing 5xx to the client unless all targets fail.

**Error envelope on global reject.** When every candidate fails, return `400 no_compatible_provider` shaped per ingress format. The envelope MUST include enough detail for admin self-debug:

```jsonc
{
  "error": {
    "code": "no_compatible_provider",
    "message": "No routing target supports dimensions=2048 for embedding requests",
    "param": "dimensions",
    "type": "invalid_request_error",
    "available_capabilities": [
      { "provider": "openai", "model": "text-embedding-3-small", "supported_dimensions": [512, 1024, 1536] },
      { "provider": "openai", "model": "text-embedding-3-large", "supported_dimensions": [256, 1024, 3072] },
      { "provider": "cohere",  "model": "embed-english-v3",      "supported_dimensions": [1024] }
    ]
  }
}
```

The `available_capabilities` list shows the candidates that were considered and what they would have accepted. Admin sees at a glance why their request failed and what to ask for. **No back-channel debugging required.**

**Never silently re-shape user input** (don't truncate dimensions, don't down-project vectors, don't split batches without explicit policy). Explicit failure beats silent corruption. **However**, admins MAY opt into best-effort routing via a per-route policy flag (`on_capability_mismatch: reject | warn-and-continue`, default `reject`) — this lifts the failure into a warning header `x-nexus-coerced` when the next candidate has compatible defaults that approximate the request. The opt-in toggle preserves the default-strict behaviour while giving operators an escape hatch for non-strict workloads.

When at least one target survives → the codec for the chosen target runs. If a codec encounters an unexpected mismatch (e.g. an admin pushed an inconsistent `capabilityJson` and pre-filter let it through), it returns a structured `400 invalid_request` with a `reason` field — the routing pre-filter is the **fast path**; the codec is the **safety net**.

This rule generalises the embedding-only "reject + pre-filter" decision (E62) to every multimodal endpoint. **It is the architectural answer to "what happens when the ingress and the target disagree on a parameter".**

---

## 6. Hook framework — endpoint + modality awareness

Today's hook framework (`hook-architecture.md`) is endpoint-agnostic and modality-agnostic. Embeddings exposed this: a `PII Detector` hook runs on an embedding response (a float array), finds no text, and `Abstain`s — wasted setup cost + audit noise. Video / image will be worse — content-scanning hooks need to know **what kind of content** they are looking at.

The framework grows two new axes: **EndpointType** (already locked in for E62 via the option-2 decision) and **Modality** (locked in alongside, even though the first content-scanning modality hook is not until E64).

### 6.1 Extended `HookInput`

```go
// packages/shared/policy/hooks/core/types.go
type HookInput struct {
    // Existing fields
    Normalized *normalize.NormalizedPayload
    Model      string
    FinishReason string
    TokenCount int
    Stage      string  // "request" | "response" | "streaming"
    // ... etc

    // New: endpoint + modality awareness
    EndpointType    EndpointType   // chat | embeddings | image_generation | tts | stt | video_generation | batch | job
    InputModality   []Modality     // text | image | audio | video — modalities present in the request
    OutputModality  []Modality     // modalities present in the response
}

type EndpointType string
const (
    EndpointTypeChat            EndpointType = "chat"
    EndpointTypeEmbeddings      EndpointType = "embeddings"
    EndpointTypeImageGeneration EndpointType = "image_generation"
    EndpointTypeTTS             EndpointType = "tts"
    EndpointTypeSTT             EndpointType = "stt"
    EndpointTypeVideoGeneration EndpointType = "video_generation"
    EndpointTypeBatch           EndpointType = "batch"
    EndpointTypeJob             EndpointType = "job"  // poll / cancel
)

type Modality string
const (
    ModalityText  Modality = "text"
    ModalityImage Modality = "image"
    ModalityAudio Modality = "audio"
    ModalityVideo Modality = "video"
)
```

### 6.2 Extended `Hook` interface

```go
type Hook interface {
    Name() string
    Decide(ctx context.Context, tx *InterceptedTransaction) Decision

    // New: applicability gates. Pipeline.BuildPipeline filters hooks by these BEFORE constructing the dispatcher.
    SupportsEndpoint(EndpointType) bool
    SupportsModality(Modality) bool
}
```

Default helpers for existing chat-only hooks: `SupportsEndpoint(t) → t == EndpointTypeChat`. As hooks are extended (e.g. PII Detector for STT transcript), they declare additional endpoints.

### 6.3 Class A vs Class B hooks

Hooks split into two classes by applicability:

**Class A — content-scanning hooks (operate on the body payload).**

| Hook | Modalities | Applies to |
|---|---|---|
| PII Detector | text | chat (req/resp), STT (resp transcript), embedding request (text input) |
| Keyword Filter | text | same as above |
| Content Safety | text | same as above |
| AI Guard | text | chat (req), embedding (req) |
| Image NSFW Classifier | image | image_generation (resp), chat (resp when vision model attaches image), STT (N/A) |
| Logo / Face Detector | image | image_generation (resp), chat (resp) |
| Audio Voice-Clone Safety | audio | TTS (resp), STT (req) |
| Video Frame Scan (compose image + audio) | video | video_generation (resp, post-completion) |
| Rule-Pack (text rules) | text | text-bearing endpoints |

**Class B — metadata / control hooks (operate on transaction metadata, not body).**

| Hook | Applies to |
|---|---|
| Rate Limiter | every endpoint |
| Request Size Validator | every endpoint |
| IP Access Filter | every endpoint |
| Audit Emitter (always) | every endpoint |
| Cost Stamper (always) | every endpoint |
| Quota Counter | every endpoint |
| Rule-Pack (metadata rules: provider, region, model, status) | every endpoint |

**Class-label semantics note.** The `Class A` / `Class B` labels classify **applicability** (which endpoints + modalities does this hook run on), not **operation** (does this hook inspect, modify, or annotate the body). A Class-A hook MAY redact / rewrite the body during its decide step — PII Detector is the canonical example (inspect-and-redact). Future hooks that are purely transformational (e.g. an STT-transcript translation hook) still fit the Class-A label provided they target a content-bearing endpoint + modality. The split is about "should this hook be CONSTRUCTED for this endpoint", not "what does the hook do".

**Pipeline-build rule:** Pipeline filters at build time, not at decide time.

```
hooks_for(endpoint, in_mod, out_mod):
    return [h for h in registry if
        h.SupportsEndpoint(endpoint) AND
        ANY (h.SupportsModality(m) for m in (in_mod ∪ out_mod) ∪ {""})]
```

This means: for an embedding response (`endpoint=embeddings`, `out_modality=[]` of text-segments because vectors don't extract as text), no Class-A text hook is even constructed — saves ~0.5ms setup + emits an explicit audit field `pipeline_skipped_reason=endpoint_embeddings_no_text` instead of misleading `decision=APPROVE`.

### 6.4 Audit & cost are NOT hooks

Cost stamping, traffic_event writing, Prometheus emission, and quota counters run **unconditionally** in handler main-path code, not inside `Pipeline`. This is unchanged from today's chat path; it generalises directly.

### 6.5 Cost — `BillableUnits` abstraction (avoids endpoint-switch sprawl)

Per-endpoint cost formulas accumulate quickly: embeddings price per token, image-gen prices per image and per size, TTS prices per second, video prices per second, batch may price per row. Hand-rolling an `if endpointType == X { ... }` switch in `proxy.go` for every new endpoint creates a single point of accumulation that resists refactoring.

**E62 introduces `BillableUnits` and `CostFormula`** so the dispatcher in `proxy.go` consults a single per-endpoint formula registered alongside the adapter, not a centralised switch:

```go
// packages/ai-gateway/internal/execution/estimator/units.go
type BillableUnits struct {
    PromptTokens     int     // chat / embeddings input; STT output (transcript)
    CompletionTokens int     // chat output; reasoning tokens roll up here per existing rule
    ReasoningTokens  int     // separately metered for reasoning-priced models
    CachedTokens     int     // prompt-cache read tokens
    Images           int     // image-gen output count
    AudioSeconds     float64 // TTS output / STT input duration
    VideoSeconds     float64 // video-gen output duration
    Requests         int     // batch row count, or 1 for single-shot
}

type CostFormula func(units BillableUnits, model *Model) decimal.Decimal

// Registry per endpoint:
//   EndpointTypeChat       → chatCostFormula     (existing)
//   EndpointTypeEmbeddings → embeddingCostFormula (E62-S4: PromptTokens * inputPrice/M)
//   EndpointTypeTTS        → ttsCostFormula      (E63)
//   EndpointTypeImage      → imageCostFormula    (E64)
//   EndpointTypeVideo      → videoCostFormula    (E66)
```

The dispatcher in `proxy.go` becomes:
```go
formula := costRegistry.Lookup(endpoint)
cost := formula(units, model)
```

Each new endpoint epic adds its own formula registration; the dispatcher does not grow. The `Model` row carries the relevant per-unit pricing fields (`inputPricePerMillion`, `imagePricePer1k`, `audioPricePerMinute`, `videoPricePerSecond`) — these are added as needed in their respective epic, not all at once in E62.

---

## 7. Audit lifecycle — `traffic_event` polymorphism

The MQ envelope (`packages/shared/transport/mq/messages.go`) already carries `EndpointType`, and the audit code stamps it on every emission — we **discriminate, not extend**. Per the live schema in `tools/db-migrate/schema.prisma` `model traffic_event` there is no dedicated `endpoint_type` column yet; the discriminator currently rides on the in-flight MQ envelope and is consumed by cost / dashboard pipelines via `EndpointType` on `HookInput`. Persisting it to a dedicated `traffic_event.endpoint_type` column is tracked under the embeddings/E62 series; the migration MUST be checked into `tools/db-migrate/migrations/` in the same PR. Modality-specific facts ride in JSONB columns (`details`, `latency_breakdown`) — no new top-level columns per endpoint.

| `endpoint_type` value | Required fields (column / JSONB key) | Optional metadata JSONB |
|---|---|---|
| `chat` | `prompt_tokens`, `completion_tokens`, `total_tokens`, `cache_read_tokens`, `cache_creation_tokens`, `reasoning_tokens`, `estimated_cost_usd` | reasoning model flag |
| `embeddings` | `prompt_tokens`, `total_tokens`, `estimated_cost_usd` | `metadata.embedding.{dimension, batch_size, encoding_format}` |
| `image_generation` | `estimated_cost_usd` | `metadata.image.{n, size, quality, style, response_format}` |
| `tts` | `prompt_tokens` (input chars), `estimated_cost_usd` | `metadata.audio.{voice, format, duration_s, sample_rate}` |
| `stt` | `completion_tokens` (output token-equivalent), `estimated_cost_usd` | `metadata.audio.{input_duration_s, model_used}` |
| `video_generation` | `estimated_cost_usd` (final, after job completion) | `metadata.video.{aspect_ratio, duration_s, resolution, person_generation}`, `metadata.job.{provider_job_id, internal_id, submitted_at, completed_at}` |
| `batch` | rolled up from per-line results at batch completion | `metadata.batch.{total_requests, succeeded, failed, output_file_id}` |

**Rule:** never silently overload a column for a different semantic (e.g. don't store image-count in `prompt_tokens` because "it's a token-like field"). If a fact doesn't fit an existing column, put it in `metadata` JSONB and let dashboards extract via jsonpath. Schema migration only when a fact needs an index (e.g. cost reporting).

---

## 8. Async job orchestrator (Typology E) — E65 specifics, framed here

Typology E endpoints (video, image-batch, long fine-tune, OpenAI Batches) require state that outlives the HTTP request that submitted them. The orchestrator is a new package introduced in E65; this section locks in its contract.

### 8.1 Storage

**Job state lives in Postgres**, not NATS. Durability is more important than latency for jobs that take minutes to hours.

```prisma
model Job {
  id              String   @id @default(uuid())   // nexus internal id (returned to client)
  organizationId  String
  providerId      String
  providerJobId   String                          // upstream's job id
  endpointType    String                          // "video_generation" | "image_batch" | ...
  modelId         String
  status          String                          // queued | running | succeeded | failed | canceled
  canonicalReq    Json                            // canonical body that was submitted
  capabilityJson  Json?                           // snapshot of model's capability at submit time (for replay)
  artifactRefs    Json?                           // []ArtifactRef when status ∈ {succeeded, failed}
  errorDetail     Json?                           // populated when status = failed
  costUsd         Decimal? @db.Decimal(20,8)      // final cost once known
  submittedAt     DateTime @default(now())
  startedAt       DateTime?
  completedAt     DateTime?
  pollNextAt      DateTime?                       // next scheduled poll (NULL = no auto-poll)
  webhookUrl      String?                         // client-supplied webhook for completion
  webhookSig      String?                         // HMAC signing key (gateway-issued)
  trafficEventId  String?                         // foreign key to the originating traffic_event row

  @@index([organizationId, status])
  @@index([pollNextAt])
  @@index([providerId, providerJobId])
}
```

### 8.2 Event fan-out

**NATS is for events, not state.** Subjects:

- `nexus.jobs.<endpoint_type>.submitted` — fired after `Job` row inserts.
- `nexus.jobs.<endpoint_type>.completed` — fired after `Job.status` transitions to `succeeded` or `failed`.
- `nexus.jobs.<endpoint_type>.canceled` — fired after `Cancel` confirmed.

Subscribers:
- Webhook deliverer (separate worker; signs payload with `webhookSig`).
- Audit-row finaliser (stamps final cost, artifact URIs, completion time on the originating `traffic_event`).
- Smoke test harness (subscribes during E62-S5 / E65 tests).

### 8.3 Polling strategy

Each adapter's `PollJob` returns a recommended next-poll interval based on upstream's expected latency:

```go
type JobStatus struct {
    State        JobStateEnum
    Progress     float64       // 0..1
    NextPollHint time.Duration // adapter's recommendation; orchestrator may override based on load
}
```

Orchestrator's poll worker:
- Reads `Job` rows with `pollNextAt <= now()` and `status in {queued, running}`.
- Calls `adapter.PollJob(JobRef)`.
- Updates `status`, `pollNextAt = now() + NextPollHint`, `artifactRefs` if final.
- On terminal state, fires the completion event.

### 8.4 Cancel semantics

Adapter declares cancel support via `Manifest.SupportsJobCancel`. If false, `POST /v1/jobs/{id}/cancel` returns `400 not_cancellable`. If true, orchestrator calls `adapter.CancelJob` and the upstream's idempotency is honoured (a second cancel returns `409 already_canceled`).

### 8.5 Wire envelope — Replicate Predictions

The client-facing wire envelope for **video / image-async** jobs uses the Replicate Predictions shape:

```json
{
  "id": "n_job_01H...XYZ",                                     // nexus-issued
  "provider": "google-veo",
  "model": "veo-1.0",
  "status": "queued",
  "input": { /* canonical body the client submitted */ },
  "output": null,                                              // populated on succeeded
  "error": null,                                               // populated on failed
  "urls": {
    "get":    "https://nexus/v1/jobs/n_job_01H...XYZ",
    "cancel": "https://nexus/v1/jobs/n_job_01H...XYZ/cancel"
  },
  "webhook": "https://client.example.com/hooks/veo-done",      // echoed if provided
  "created_at": "2026-05-19T08:00:00Z",
  "started_at": null,
  "completed_at": null,
  "metrics": null                                              // populated on completion (duration_s, cost_usd, etc.)
}
```

For **chat / embed batches**, the OpenAI Batches envelope is preserved (`id`, `object: "batch"`, `status`, `request_counts`, `output_file_id`, ...). Same `Job` table backs both; only the wire serialisation differs.

### 8.6 Artifact relay policy

Default behaviour: gateway returns the upstream's artifact URL verbatim. Upstream signs the URL; nexus does not re-sign.

Opt-in behaviour (per-org policy, set in admin UI): gateway downloads the artifact, uploads to nexus spillstore (S3), and returns a nexus-signed URL with TTL ≤ 24h. Used when compliance requires the artifact to be served from a trusted domain.

`traffic_event.metadata.video.artifact_uri` records whichever URL was returned to the client. The original upstream URL (when relayed) is stamped in `metadata.video.upstream_uri` for forensic purposes.

---

## 8.7 Three-traffic-path consistency — AI Gateway, Compliance Proxy, Agent

The Nexus stack runs **the same Go forwarder pipeline in three different paths**, each with its own ingress mechanic but sharing the hook framework, the audit lifecycle, and the `shared/traffic/adapters/` + `shared/transport/normalize/` content-extraction layer:

| Path | Ingress mechanic | Detail doc | Content extraction layer | Hook framework |
|---|---|---|---|---|
| **AI Gateway** | `POST /v1/...` from corporate app → gateway routes → upstream | `provider-adapter-architecture.md` + this doc | `providers/specs/<x>/codec.go` (gateway-internal codec) | `packages/shared/policy/hooks/` |
| **Compliance Proxy** | CONNECT from corporate network → MITM TLS bump → forward to provider | `compliance-pipeline-architecture.md` | `packages/shared/traffic/adapters/api/<provider>/` `Normalize` + Tier-1 normalizers (`shared/transport/normalize/codecs/<wire>`) | `packages/shared/policy/hooks/` (identical) |
| **Agent** | OS-level intercept (macOS NE / Linux pf / Windows WinDivert) → forward to provider | `agent-forwarder-architecture.md` + `agent-ne-fail-open-architecture.md` | same as Compliance Proxy | `packages/shared/policy/hooks/` (identical) |

The 5 typologies and 8 per-endpoint canonicals defined in §1 + §2 above apply uniformly to **all three paths**. Adding a new endpoint to Nexus is therefore work across all three:

```
                         ┌─────────────────────────┐
                         │  shared/transport/      │
                         │  normalize/codecs/      │ ← single parse per wire format
                         │  (Tier-1 normalizer)    │
                         └──────────┬──────────────┘
                                    │ produces NormalizedPayload {Kind, ...}
              ┌─────────────────────┼─────────────────────┐
              │                     │                     │
              ▼                     ▼                     ▼
    ┌──────────────────┐  ┌──────────────────┐  ┌──────────────────┐
    │  AI Gateway      │  │ Compliance Proxy │  │  Agent           │
    │  (codec.Decode)  │  │  (forward path)  │  │  (forward path)  │
    └─────────┬────────┘  └─────────┬────────┘  └─────────┬────────┘
              │                     │                     │
              └─────────────────────┼─────────────────────┘
                                    ▼
                       ┌─────────────────────────┐
                       │  shared/policy/hooks/   │ ← endpoint-aware filter at build
                       │  Pipeline               │   (§6 above)
                       └────────────┬────────────┘
                                    ▼
                       ┌─────────────────────────┐
                       │  traffic_event + audit  │ ← endpoint_type discriminator
                       │  (§7 above)             │
                       └─────────────────────────┘
```

### 8.7.1 `NormalizedPayload.Kind` extension — the typology kicker

`NormalizedPayload.Kind` is the entry point that hooks + UI use to identify what kind of AI content a traffic event carries. Today: `ai-chat`, `http-text`, `http-json`, `http-form`, `http-multipart`, `http-binary`. As each typology lands, a new `Kind` value joins the enum:

| `Kind` | Typology | Lands in | Carries (in `NormalizedPayload`) |
|---|---|---|---|
| `ai-chat` | A / B | existing | `Messages[]`, `Model`, `FinishReason`, `Tools[]`, `Usage` |
| **`ai-embeddings`** | A | **E62** | `Inputs []string` (request text), `Model`, `Usage`, **no vectors stored** (storage cost; vectors aren't useful for hook scanning) |
| `ai-image-generation` | A or C | E64 | `Prompt`, `Model`, `Artifacts []BinaryRef` (URL or `spillKey` for image bytes), `Metadata.Image.{n,size,quality,style}` |
| `ai-tts` | C / D | E63 | `Input` (text), `Model`, `Artifacts []BinaryRef` (audio bytes/URL), `Metadata.Audio.{voice,format,duration_s}` |
| `ai-stt` | C | E63 | `Input` (`BinaryRef` to audio multipart), `Model`, `Transcript` (response text), `Metadata.Audio.{input_duration_s}` |
| `ai-video-generation` | E | E66 | `Prompt`, `Model`, `Job` (`{provider_job_id, internal_id, status, artifact_uri}`), `Metadata.Video.{aspect_ratio,duration_s}` |
| `ai-batch` | E | E65 | `BatchRef` (provider batch id), `RequestCount`, `Status`, links to per-line `traffic_event` rows when batch completes |
| `ai-job-status` | E (poll) | E65 | `JobRef`, `Status`, `Progress`, latest artefacts |

**Vector / audio-byte / video-byte storage rule (binding).** Inline storage of multi-MB binary payloads in `traffic_event_normalized` is forbidden. The bet documented in `normalization-architecture.md` § "Storage strategy" (normalised JSON ≤ 256 KiB) breaks for image / audio / video. New typologies that include binary artefacts MUST:

1. Reference artefacts as `BinaryRef {size, sha256, content_type, spillKey}`, not inline bytes.
2. Upload artefacts to `shared/storage/spillstore` (S3 in prod, local-FS in dev) when capture is requested.
3. Default capture posture is `metadata-only` — full-byte capture is per-org opt-in, governed by `hook_config` retention policy.
4. Embeddings are a special case: response vectors are **never** stored (high volume, low forensic value). `ai-embeddings` Kind carries no `Artifacts`; only request text + usage metadata. Re-derivable on demand by replaying the embedding call.

### 8.7.2 Per-path responsibility matrix per typology

The architecture commitment is that adding a new endpoint requires lockstep work across all three paths. The matrix below lists what each path delivers for each typology:

| Typology / endpoint | AI Gateway | Compliance Proxy | Agent (Linux/Windows, pf intercept) | Agent (macOS, NE intercept) |
|---|---|---|---|---|
| **chat / responses** ✅ | codec, canonical, hook | MITM + extract + hook + audit | full extract + hook + audit | metadata-only (no TLS bump on NE) |
| **embeddings (E62)** | codec + bridge + capability + cost | URL-pattern classifier → `ai-embeddings` Kind → request-text extract → hook | same as CP, local | metadata-only (host + size + Provider attribution; no content) |
| **image-gen (E64)** | codec + image artifact handling | `ai-image-generation` extract; opt-in spillstore upload of generated image; image NSFW hook (E67) | same as CP, local | metadata-only |
| **TTS (E63)** | codec + binary streaming response handling | `ai-tts` extract; opt-in spillstore upload of audio; voice-clone safety hook (E67) | same as CP, local | metadata-only |
| **STT (E63)** | codec + multipart request handling | `ai-stt` extract; pre-redacted-transcript hook scan; spillstore for original audio | same as CP, local | metadata-only |
| **video-gen (E66)** | codec + Veo canonical + AsyncAdapter wiring | `ai-video-generation` extract; job-completion artifact relay; video frame scan hook (E67, runs in async job orchestrator, NOT in MITM hot path) | same as CP, local | metadata-only |
| **batch / async-job (E65)** | AsyncAdapter + Job table + webhook deliverer | `ai-batch` / `ai-job-status` extract; lifecycle event upload | same as CP, local | metadata-only |

**macOS Agent caveat:** Today's `NETransparentProxyProvider` does not TLS-bump (see `agent-ne-fail-open-architecture.md` + CLAUDE.md "macOS NE proxy must fail-open"). Content-aware hooks therefore do not run on macOS for any endpoint — chat included. The macOS agent path captures metadata only (host, IP, port, process attribution, size, byte counts) and emits the corresponding `http-*` `Kind`. Promoting macOS to TLS-bumped intercept is tracked separately (pf-based replacement); it is not blocked by E62-E66.

> **Credibility-level limitation, not a permanent design choice.** Every new endpoint typology that lands (embeddings now, audio next, image/video after) widens this gap. The product promise is "unified compliance for AI traffic on every endpoint"; on macOS, that promise currently degrades to "metadata only". **The pf-intercept replacement deserves explicit prioritisation alongside the E63-E67 typology epics, not indefinite tracking-without-scheduling.** It is numbered as **E74** in `docs/developers/roadmap.md` (was proposed as E68 in early E62 drafts; E68 was claimed by the cache-feedback work in `roadmap.md` before E74 was assigned).

### 8.7.3 Endpoint classification — URL pattern + method + content-type

The CP and Agent paths need to classify the endpoint **before** picking an extractor. AI Gateway already knows the endpoint from its own routing table; CP/Agent must infer from the wire request.

A new shared package `packages/shared/traffic/classify/` (lands in E62) exposes:

```go
// EndpointClassifier maps (host, method, path, content-type) to (EndpointType, AdapterID).
type EndpointClassifier interface {
    Classify(host, method, path, contentType string) (EndpointType, AdapterID, bool)
}
```

Registry-style: each provider's adapter contributes rules at `init()` time. Examples:

```
api.openai.com   POST  /v1/chat/completions          application/json    → (chat,              openai)
api.openai.com   POST  /v1/embeddings                application/json    → (embeddings,        openai)
api.openai.com   POST  /v1/images/generations        application/json    → (image_generation,  openai)
api.openai.com   POST  /v1/audio/speech              application/json    → (tts,               openai)
api.openai.com   POST  /v1/audio/transcriptions      multipart/form-data → (stt,               openai)
api.openai.com   POST  /v1/batches                   application/json    → (batch,             openai)
api.anthropic.com POST /v1/messages                  application/json    → (chat,              anthropic)
api.anthropic.com POST /v1/messages/batches          application/json    → (batch,             anthropic)
api.cohere.com   POST  /v1/embed                     application/json    → (embeddings,        cohere)
*.googleapis.com POST  /v1*/models/*:embedContent    application/json    → (embeddings,        gemini)
*.googleapis.com POST  /v1*/models/*:batchEmbedContents application/json → (embeddings,        gemini)
```

E62 ships embedding rules + the classifier framework. Subsequent epics append their endpoints. **The classifier is NOT a hook** — it runs before the hook pipeline, sets `EndpointType` + `Modality` fields on `HookInput`, and lets the endpoint-aware pipeline filter cut the right Class-A hooks for the right traffic.

### 8.7.4 Test coverage across the three paths

Per CLAUDE.md AI-Gateway smoke binding, the smoke harness is the load-bearing verification gate. Each path has its own smoke harness:

| Path | Smoke harness | Phase added by E62 |
|---|---|---|
| AI Gateway | `/smoke-gateway` (`tests/scripts/smoke-gateway.py`) | **P3E** (per §9 below) |
| Compliance Proxy | `/test-compliance-proxy` (`.claude/skills/test-compliance-proxy/`) | Embedding through-MITM smoke arm |
| Agent | per-platform synthetic tests (`/test-cursor-adapter`, `/test-geminiweb-adapter`, …) + macOS NE manual verification | No new arm in E62 (agent macOS is metadata-only; Linux/Windows pf-intercept rollout adds coverage in later epic) |

The CP arm for embeddings (E62-S6) sends a real `POST /v1/embeddings` to api.openai.com through the production CP, verifies (a) MITM bump succeeds, (b) extract produces `Kind=ai-embeddings` + `Inputs[0]` populated, (c) hook pipeline runs **with Class-A text hooks active on request** (PII Detector valid on the input text) **and Class-A text hooks skipped on response** (no text to scan), (d) `traffic_event_normalized` row has `Kind=ai-embeddings` and `request_normalized.Inputs` populated, response has empty `data`, (e) Prometheus deltas on `nexus_traffic_events_total{source="compliance-proxy",endpoint="embeddings"}`.

### 8.7.5 Cross-path drift detection (future safeguard)

The three-source consistency invariant (§8.7.4 + S6 T6) is strong at build time but fragile at runtime: any path's codec patch that lands without parallel updates to the other two surfaces silently as divergent `NormalizedPayload` rows in production. Fixture-based unit tests can only cover known shapes; novel upstream responses arrive in prod first.

**Recommended (E65+ scope):** a background job samples recent (~1h) overlap between paths — pairs of `traffic_event` rows where the AI Gateway processed a request that an Agent or Compliance Proxy also forwarded for the same VK / model / request hash. The job hashes the resulting `NormalizedPayload` and alerts when a hash mismatch exceeds a threshold (e.g. >1% of sampled overlap). This catches codec drift in production before it accumulates into audit-corrupting volumes.

The detector is **NOT E62 scope** — it requires E65's job-orchestrator infrastructure. Documented here so E65's design accounts for it as a first-class background job.

---

## 9. Smoke harness — phase fan-out

`/smoke-gateway` (today P1–P3G + P4–P7) gets one new phase per endpoint typology as endpoints land.

| Phase | Typology | Endpoint coverage | First epic |
|---|---|---|---|
| P3 | A / B (chat) | `/v1/chat/completions` non-stream + SSE + cache + dry-run | already exists |
| P3R | A / B (responses) | `/v1/responses` non-stream + SSE + cross-format | already exists (E56) |
| P3A | A / B (messages) | `/v1/messages` (Anthropic ingress) non-stream + SSE | already exists |
| P3G | A / B (gemini) | Gemini `:generateContent` non-stream + SSE | already exists |
| **P3E** | A (embeddings) | `/v1/embeddings` + Azure + Cohere `/v1/embed` + Gemini `:embedContent`; native + cross-format | E62-S5 |
| **P3I** | A / C (image-gen sync) | `/v1/images/generations` + Stability + Bedrock; native + cross-format; URL + b64 modes | E64 |
| **P3T** | C / D (TTS) | `/v1/audio/speech` non-stream + stream binary; native + cross-format | E63 |
| **P3S** | C (STT) | `/v1/audio/transcriptions` multipart upload | E63 |
| **P3B** | E (batch) | `/v1/batches` submit + poll + fetch | E65 |
| **P3V** | E (video-gen) | submit + poll + webhook + fetch artifact; cancel | E66 |

**Per-phase assertions (binding template — every new phase must check these):**

1. **HTTP response shape** matches the canonical shape (per §2).
2. **traffic_event row** exists with `endpoint_type` set correctly + modality-specific metadata populated (per §7).
3. **Prometheus delta** — `nexus_traffic_events_total{endpoint="<endpoint_type>"}` incremented.
4. **Cost stamping** — `estimated_cost_usd` is non-zero on a real upstream call (zero on dry-run, miss, error).
5. **Cross-ingress consistency matrix** — every pair (ingress, target) of distinct wire formats verifies that response reshaping is correct (e.g. Cohere ingress + OpenAI target returns Cohere shape).
6. **Reject-asymmetry test** — at least one negative test in the phase confirms an incompatible request returns 400 `no_compatible_provider` (per §5.3).
7. **Endpoint-irrelevant arms are skipped**, not silently passed (e.g. no "2-turn prompt cache" arm on P3E embeddings — embeddings have no prompt-cache semantic).

**Embedding-specific (P3E) details land in E62-S5 SDD; the template above is the contract every phase honours.**

### 9.1 Smoke upstream cost policy

Smoke arms calling real upstream cost real money. P3E embeddings is cheap (~$0.0002 per smoke run). P3I image-gen and P3V video are expensive ($0.04/image, ~$10/video). Running the full matrix on every CI invocation is not financially viable.

**Policy (lands per typology as each phase ships):**

| Phase | Default mode | `--all-upstream` flag effect |
|---|---|---|
| P3 / P3R / P3A / P3G (chat) | Real upstream (cost negligible) | unchanged |
| **P3E (embeddings)** | Real upstream (cost negligible) | unchanged |
| P3T (TTS), P3S (STT) | Real upstream for short fixtures; fixture-based for long-input arms | Run all arms against real upstream |
| P3I (image-gen) | **Fixture-based by default**; one real-upstream smoke per provider in nightly | `--all-upstream` runs full matrix |
| P3V (video-gen) | **Fixture-based by default**; one real-upstream smoke per provider in weekly | `--all-upstream` runs full matrix |
| P3B (batch) | Submit-only on real upstream; poll via fixture | `--all-upstream` runs poll-to-completion on real upstream |

`smoke-gateway.py` carries a `_cost_policy.json` table mapping phase → mode. The `--all-upstream` flag overrides defaults for the run. Smoke output report's header summary stamps "mode: default / all-upstream" so cost auditors can correlate spend.

**E62 scope:** P3E ships in real-upstream mode (negligible cost). Cost-policy framework lands as a hook for E63+, but the JSON map is created in E62-S5 with the P3E entry.

---

## 10. Roadmap — how this stack fills in

| Epic | Scope | Stack layers introduced |
|---|---|---|
| **E62** Cross-Adapter Embeddings | OpenAI + Azure + Cohere + Gemini embedding adapters; canonical embedding shape; cross-format routing for embeddings; **endpoint-aware hook framework**; **SchemaCodec interface extension (contentType + artifacts)**; **Model capability matrix migration**; P3E smoke phase | L1 typology classification, L2 canonical for embeddings, **L3 extended SchemaCodec**, **L5 endpoint-aware hooks**, **L7 capability matrix**, L8 phase fan-out |
| **E63** Audio (TTS + STT) | OpenAI + ElevenLabs + Deepgram + Whisper TTS / STT; binary I/O codec contract; audio cost formula; streaming binary path; P3T + P3S phases | Typology C + D, audio modality in capability matrix |
| **E64** Image Generation (sync) | OpenAI + Stability + Bedrock image-gen; **first content-scanning hook for image modality**; **artifact relay (passthrough URL default)**; P3I phase | First Class-A image hooks, L4 artifact relay v1 |
| **E65** Async Job Orchestrator | `Job` table, NATS event fan-out, webhook deliverer, poll worker; Replicate Predictions wire envelope; OpenAI Batches compatibility; P3B phase | **L4 lifecycle services**, Typology E base infrastructure |
| **E66** Video Generation | Google Veo + Sora + Runway adapters; video canonical (Veo + `nexus.ext.*`); video-specific capability fields; P3V phase | Video modality, builds on E65 |
| **E67** Modality-Aware Hooks Expansion | Image NSFW classifier, logo / face detector, audio voice-clone safety, video frame scan (compose); per-modality `applicableIngress` + `applicableModalities` filtering | Class-A modality hooks land in full |

Each epic is self-contained per dependency: E63 / E64 can ship in parallel (different typologies, independent infrastructure). E65 must precede E66. E67 can ship after E64 or E66 — modality hooks are additive.

**Roadmap order is reorderable by customer demand.** The numbering reflects an architectural dependency ordering (typology + lifecycle complexity), not customer demand ranking. In practice, image-generation has 2+ years of mainstream production use (DALL-E, Stability, Midjourney customers already exist); video-gen is hype-driven. If customer signals support it, **E64 (image) can ship before E63 (audio)** without architectural rework — both build on the same E62 foundation. Suggested when re-ordering: keep E65 (async-job orchestrator) before E66 (video) because video needs the orchestrator; keep E67 (modality hooks) last because hooks are additive.

**E62 is the load-bearing epic — it establishes L3, L5, and L7 generality that every subsequent epic relies on. Bugs / shortcuts in E62 propagate to E66.**

**Open item flagged in §8.7.2:** macOS pf-intercept replacement of NETransparentProxyProvider has been numbered **E74** in `docs/developers/roadmap.md` (the cross-epic canonical tracker). The original E62-drafting proposal said E68 but that slot was claimed by negative-feedback cache work before E74 was assigned.

---

## 11. Explicit NOT-DOs

These are choices we have considered and **reject**. Recording them here so they don't get re-litigated mid-implementation.

1. **No universal multimodal canonical** (e.g. `{prompt, modality, params}`). Each endpoint has its own grammar; abstraction always leaks; provider quirks always end up in extension fields anyway. Per-endpoint canonical wins.
2. **No silent down-projection or input mutation** (don't truncate dimensions, don't split batches without policy, don't reformat aspect ratios). Routing pre-filter rejects with a clear `400` instead. Explicit failure beats silent corruption.
3. **No content-scanning hooks running on bodies with no extractable content** (PII on embeddings, keyword on raw audio). Endpoint-aware hook framework filters at pipeline-build, not at decide-time. Saves cost; cleaner audit.
4. **No hooks running on video frames inside the request-handler hot path.** Video frame scanning happens **inside the async job orchestrator** post-completion, with its own timeout / fail-open semantics. Putting it in the request hot path would block HTTP for minutes.
5. **No async semantics bolted onto the sync `SchemaCodec` interface.** `AsyncAdapter` is a separate interface; sync-only adapters do not implement it.
6. **No artefact persistence beyond opt-in spillstore.** Gateway is not an artifact CDN. Default = passthrough provider URL. Long-term storage is the customer's S3.
7. **No NATS-backed job state.** Job durability requires SQL. NATS is for fan-out only.
8. **No provider-keyed epics.** Epic scope follows endpoint typology, not provider names. Adding OpenAI image generation is part of E64 (image-gen), not "the OpenAI epic".
9. **No new `Adapter` Go interface for embeddings / audio / image / video.** The existing `Adapter` (+ extended `SchemaCodec`, + optional `AsyncAdapter` for Typology E) is sufficient. Parallel abstractions get rejected — this is the reason `EmbeddingProvider` interface was explicitly rejected in E61-S5.
10. **No `// TODO` markers in shipped code claiming future features.** If a feature is not yet supported, return a structured error (`501 not_implemented`) with `feature` field set, and link to the SDD section that will land it.

---

## 12. Sources

- `packages/ai-gateway/internal/providers/core/spec.go` — `SchemaCodec`, `StreamDecoder`, `AdapterSpec`. **Will be extended in E62.**
- `packages/ai-gateway/internal/providers/core/types.go` — `Endpoint`, `Format`, canonical types. **Will be extended in E62 (`EmbeddingsRequest/Response`).**
- `packages/ai-gateway/internal/providers/dispatch/spec_adapter.go` — generic dispatcher.
- `packages/ai-gateway/internal/execution/canonicalbridge/bridge.go` — request canonicalisation hub. **`IngressEmbeddingsToCanonical` lands here in E62.**
- `packages/ai-gateway/internal/execution/canonicalbridge/stream.go` — stream transcoders.
- `packages/shared/policy/hooks/core/types.go` — `Hook`, `HookInput`, `Decision`. **Extended in E62 with `EndpointType` + `Modality`.**
- `packages/shared/policy/pipeline/pipeline.go` — pipeline builder. **Extended in E62 to filter by endpoint at build time.**
- `packages/shared/transport/normalize/codecs/` — Tier-1 normalisers per wire format.
- `tools/db-migrate/schema.prisma` — `Model`, `Provider`, `TrafficEvent`, **`Job` (new in E65)**.
- `tests/scripts/smoke-gateway.py` — smoke harness. **`P3E` lands in E62-S5.**

---

## 13. Cross-references

- `provider-adapter-architecture.md` — the chat-specific specialisation of this frame. Rules 1-7 generalise to every endpoint per §4.4 above.
- `hook-architecture.md` — extended in E62-S1 with endpoint + modality awareness.
- `routing-architecture.md` — capability-matrix pre-filter rule (§5.3 above) is implemented in the routing engine.
- `cost-estimation-architecture.md` — per-endpoint cost formula dispatch.
- `response-cache-architecture.md` — response cache currently scopes to chat; embedding cacheability is decided per-endpoint (E62 says no; future endpoints decide individually).
- `cache-multi-tier-architecture.md` — cache tiers; embeddings are referenced as a *tool* for semantic cache (E61), not as a *cached* endpoint themselves.
- `normalization-architecture.md` — `shared/normalize` codec delegation; new endpoint normalisers land here.
- `prompt-cache-architecture.md` — provider-side prompt cache is a chat-only concept; not applicable to embeddings / image / audio.
- `audit-pipeline-architecture.md` — `traffic_event` schema authority; consult before adding any new field.
- E62 docs: `docs/developers/specs/e62/e62-cross-adapter-embeddings.md`, `docs/developers/specs/e62/e62-s1-hook-endpoint-gating.md`, `docs/developers/specs/e62/e62-s2-canonical-embeddings-bridge.md`, `docs/developers/specs/e62/e62-s3-provider-codecs.md`, `docs/developers/specs/e62/e62-s4-traffic-event-embeddings.md`, `docs/developers/specs/e62/e62-s5-smoke-embeddings.md`, `docs/users/api/openapi/ai-gateway/e62-s2-embeddings.yaml`.

# Normalization architecture

The normalize layer turns raw provider/wire bytes into a canonical `NormalizedPayload` — readable text, token usage, and request/response structure — independent of which provider or wire format produced them. It lives in `packages/shared/transport/normalize` and is shared by the AI Gateway, Compliance Proxy, Agent, and Hub audit pipeline, so the same bytes yield the same normalized result everywhere.

## 1. The NormalizedPayload contract

A `Normalizer` (`packages/shared/transport/normalize/core`) implements:

```go
type Normalizer interface {
    ID() string
    Normalize(ctx context.Context, raw []byte, meta Meta) (NormalizedPayload, error)
}
```

`Meta` carries the call context: `AdapterType` (the wire key), `Model`, `ContentType`, `Direction` (`DirectionRequest` / `DirectionResponse`), `EndpointPath`, and `Stream`. `NormalizedPayload` is the canonical output — `Kind` (`ai-chat` / `ai-embedding` / `http-json` / …), `Protocol`, `Model`, `Stream`, `Messages[]`, `Tools[]`, `Params`, `Usage`, `FinishReason`, `Inputs[]` (embeddings), `Confidence`, and `DetectedSpec`. A normalizer that does not recognize the bytes returns `ErrUnsupported`, which the coordinator uses to fall through to the next candidate.

## 2. The tiered dispatch model

`core.Registry` is the coordinator. `BuildRegistry` (`packages/shared/transport/normalize/buildregistry.go`) assembles it once per service and freezes it. `Registry.Normalize` dispatches in tiers:

- **Tier 1 — keyed per-wire normalizers** (`normalize/codecs`, registered by `RegisterDefaultAIBuiltins`, plus per-host traffic adapters via `RegisterTier1AdapterNormalizers`). Selected by `AdapterType` and `AdapterType::EndpointPath` keys — JSON wires with a known shape.
- **Tier 1.5 — the sniff pass.** When every keyed candidate missed or declined, the registry offers the body to codecs enrolled via `RegisterSniffer` (anthropic, openai-chat, openai-responses, gemini — in that precision order). Each implements the optional `core.Sniffer` capability: `LooksLike(raw, meta)` probes a bounded prefix for protocol-distinctive markers in BOTH directions. Response markers are probed unconditionally (the Anthropic `message_start` SSE frame, the OpenAI `chatcmpl` / `"object":"chat.completion` discriminators, the Gemini `"candidates"` key plus a corroborating Gemini key); request markers are probed only when `meta.Direction` is request or unset — anthropic matches `"messages"` + `"max_tokens"` (the pair is protocol-required on `/v1/messages`), openai-chat matches `"messages"` + `"model"` with an `"author"` exclusion (chatgpt-web requests carry the same pair but wrap the role in an `author` object and belong to the Tier-2 chatgpt-web spec), gemini matches `"contents"` plus one of `"generationConfig"` / `"systemInstruction"` / `"safetySettings"`. An Anthropic request body satisfies both the anthropic and openai request probes (`messages`+`model`+`max_tokens` is a superset); registration order resolves the ambiguity — anthropic probes first, so the stricter marker set wins. A match runs the same claim contract as Tier 1. This is how key-missed capture traffic — whose `AdapterType` carries a host or tool name rather than a wire key, and whose path resolves nothing — still lands on the full-fidelity codec instead of the pattern probe or the verbatim dump, in both directions. Probe precision is pinned by the cross-corpus sniffer matrix test (`codecs/sniffer_test.go`): no sniffer outside a case's allowed set may probe-match its corpus wire, and the request-direction walk-order discrimination is pinned end-to-end by `TestRequestSniffOrderDiscrimination` plus the `*-req-keymissed` corpus goldens.
- **Tier 2 — consumer-web pattern probe + NonJSONDetector framework** (`normalize/extract`, wired by `WireTier2`). The JSON probe recognises only consumer-web shapes the codecs do not own — the ChatGPT-web request/JSON-Patch-SSE pair, the claude.ai single-prompt request, and the flat-prompt legacy completions shape. Standard-API wires (OpenAI Chat, Anthropic Messages, Gemini, OpenAI Responses) are deliberately NOT patterned here: the Tier-1 codecs decode them by key, path, or sniff, and a duplicate Tier-2 spec would only produce a lower-fidelity second answer. For wires that are not plain JSON the detector chain runs: a protobuf Connect-RPC envelope (`ConnectRPCProtobufDetector`) or a Google `batchexecute` form post (`BatchExecuteDetector`). Each detector implements `ID()` / `LooksLike(raw)` / `Decode(raw, direction)`.
- **Tier 3 — generic HTTP** (`GenericHTTPNormalizer`). The catch-all that records non-AI HTTP structure when no AI wire matches.

A tier's result is accepted only when its `Confidence` clears the registry threshold (default 0.70); otherwise the coordinator continues to the next candidate.

**Confidence semantics — one meaning per input shape.** Confidence always answers "what fraction of this input did the decoder recognize", with the denominator defined by the input's shape:

| Input shape | Formula | Where |
|---|---|---|
| Stream (SSE fold) | recognized frames / total data frames (`[DONE]` and blank data lines are sentinels, counted in neither; an unparseable or alien-shape frame counts toward the total only — the decodable prefix folds instead of erroring; a scanner abort on an oversized line weighs as one lost frame). Recognition is envelope-key presence (openai: id/object/created/model/choices/usage; gemini: candidates/usageMetadata/modelVersion; anthropic: the typed frame vocabulary), value-empty or not, so filter-prologue and heartbeat frames cannot demote a real stream below the claim threshold | `anthropic_stream.go`, `openai_chat.go` stream fold, `gemini_stream.go` — each fold sets `Confidence` itself; the codec's `Normalize` wrapper stamps only when the fold left it unset. Known incompleteness: `openai_responses.go` folds its stream into a synthesized document and field-shape-scores that, and the Cohere SSE codec has no fold at all (its streams field-shape-score the first chunk) — frame-coverage semantics for both is ledgered future work |
| Single JSON document | weighted field coverage: 0.50 baseline + 0.40 × required-key ratio + capped optional bonus − bounded unknown-key penalty, range [0.40, 1.00] | `core.ScoreTier1Confidence` (`core/confidence.go` carries the full rubric and the rationale for each weight) |
| Consumer-web patch stream | applied patch frames / patch-candidate frames, gated on a signature key (`conversation_id` / `message_id`) appearing in at least one raw frame — probed at the frame's top level and nested one hop under the patch value (`v.conversation_id`, the delta-encoding stream variant's only placement) | Tier-2 chatgpt-web response probe |
| Non-JSON detector (protobuf / batchexecute) | weighted signal coverage: 0.60 baseline + 0.30 × required-signal ratio + bounded bonus − unknown penalty, range [0.50, 1.00] | `extract/detector.go` |
| Fallback projection | constant 1.0 — full confidence in the PROJECTION (see §4.1); makes no AI-semantics claim | `generic_http.go` |

One deliberate exception: **host selection evidence**. `extract.NormalizeForAdapter` (entered from a per-host adapter that already resolved the producer by host evidence) stamps `SelectionEvidence=host` on its payload and KEEPS the honest decode coverage (single-prompt consumer-web specs like claude-web cap near 0.6 — they extract the prompt and nothing else). The Registry's `tryClaim` accepts a `SelectionEvidence=host` payload over the 0.70 threshold on the strength of that host match, not the coverage number — the host IS the source of truth for "this is adapter X traffic", so dropping a correctly-attributed row to the generic-http fallback merely because its coverage is honestly low would be a strictly worse operator outcome. Because coverage and selection-evidence are different scales, the UI renders a "host-matched" label in place of the numeral for these rows (mirroring the Structural badge's numeral suppression). The earlier design floored confidence to 0.95 instead; that floored number is retired — it made an honest 0.6 read as more trusted than a real sniffed decode. Adapter-keyed callers likewise satisfy the chatgpt-web signature gate by host evidence alone; key-missed Tier-2 traffic keeps the strict gate so coverage alone never claims a foreign patch stream.

## 3. Canonical usage normalization

Every normalizer maps the upstream's native token counts onto one canonical `Usage` (`normalize/core`):

```
PromptTokens · CompletionTokens · TotalTokens · CacheReadTokens · CacheCreationTokens · ReasoningTokens   (all *int)
```

The convention is OpenAI's, so cost and analytics never branch on provider:

- **Anthropic** — wire `input_tokens` counts uncached input only, so the normalizer sets `PromptTokens = input_tokens + cache_read_input_tokens + cache_creation_input_tokens` and `CompletionTokens = output_tokens` (`codecs/anthropic_messages.go`).
- **Gemini** — `PromptTokens = promptTokenCount`, `CompletionTokens = candidatesTokenCount + thoughtsTokenCount`, `CacheReadTokens = cachedContentTokenCount`, `ReasoningTokens = thoughtsTokenCount` (`codecs/gemini_generate.go`).
- **OpenAI-compatible family** — `codecs/openai_chat.go` resolves the cached-token alias chain across vendors (DeepSeek `prompt_cache_hit_tokens`, Moonshot `prompt_cache_tokens`); the Responses-API top-level `input_tokens` / `output_tokens` mapping lives in `codecs/openai_responses.go`.

This is the contract `core.ExtractUsage` in the AI Gateway depends on — see [provider-adapter-architecture.md](provider-adapter-architecture.md) §5.

## 4. Text and structure extraction

Beyond usage, a normalizer reconstructs the conversation into `Messages[]`, each a `core.Message` with a role and a `[]ContentBlock`. Content blocks carry typed payloads — `text`, `tool_use`, `tool_result`, and `reasoning` — so the audit pipeline stores readable normalized text and preserves tool and chain-of-thought content rather than dropping it. Embedding wires populate `Inputs[]` and `Usage` instead of messages. Streamed responses are folded frame by frame (`normalize/extract/accumulator.go`) into a single final payload.

A Tier-1 codec whose surface is captured as a self-contained SSE stream folds that stream itself before decoding. `codecs/openai_responses.go` is one case: a streamed `/v1/responses` egress is stored as the raw Responses-API event stream the client received (`event: response.output_text.delta` … terminated by `response.completed`, which carries the complete response object). The codec collapses that stream to the terminal response object — falling back to the accumulated `output_text` deltas when the capture is truncated before the terminal event — so a streamed row normalizes to the same `Messages[]` + `Usage` as a non-streamed one instead of failing the JSON decode on the leading `event:` framing and emitting an empty payload. `codecs/anthropic_stream.go` folds the Anthropic `/v1/messages` event stream the same way: content blocks are reassembled in stream order (`text_delta` runs → a `text` block, `thinking_delta` runs → a `reasoning` block, `input_json_delta` runs → a `tool_use` block with the accumulated JSON decoded into the input), the model arrives on `message_start`, the stop reason and output-side usage on `message_delta`, and `LooksLikeAnthropicSSE` routes the body to the fold even when the capture lost the stream flag and Content-Type. Shared SSE line-splitting lives in `codecs/sse_fold.go` (`walkSSEFrames`, which also carries each dispatch's `event:` name to the callback), used by the OpenAI Chat and Anthropic stream folds and the generic-http SSE projection.

### 4.1 Fallback projections (generic-http)

Traffic no AI tier claims still gets a TYPED structural projection, never a blind dump. `GenericHTTPNormalizer` (`codecs/generic_http.go`) routes by byte sniff first — capture-side Content-Type headers are routinely missing or mis-stamped, so the bytes outrank the header — then by declared Content-Type:

| Wire shape (sniff/CT) | Kind | Body view | Notes |
|---|---|---|---|
| SSE framing (first non-comment line opens `event:` / `data:` — leading `:` keep-alive comment lines are skipped within a 256-byte probe window; a declared `text/event-stream` Content-Type routes here even when a longer preamble defeats the sniff) | `http-sse` | `sseFrames[]` — one frame per `data:` line: `event` name + `data` (decoded JSON tree) or `dataText` (verbatim string, at most one set — an empty data line carries neither) | Capped at 2000 frames AND 1 MiB cumulative frame data + `sseTruncated:true` (together they bound the persisted row; the raw bytes remain in the Raw view, which is also why the frame list never duplicates the verbatim text) |
| NDJSON (two+ independently complete JSON lines) | `http-json` | `json` — array of the decoded lines | One bad line invalidates the NDJSON assumption → text projection |
| Valid JSON document (first non-ws byte `{`/`[` + whole-body `json.Valid`) | `http-json` | `json` — decoded tree | Claims regardless of declared Content-Type; invalid JSON never enters (a declared-JSON body that fails the decode keeps the explicit error path: text projection for UTF-8 prose, binary digest + surfaced error otherwise) |
| `application/x-www-form-urlencoded` | `http-form` | `form` — key→value map (multi-valued keys newline-joined) | |
| `multipart/*` | `http-multipart` | `form` — field-by-field text; file parts decay to a `<file len=N>` marker | Missing boundary → binary digest |
| `text/*`, or no Content-Type with UTF-8-looking bytes | `http-text` | `text` — verbatim | |
| Everything else | `http-binary` | `binaryRef` — size + sha256 only, bytes never inlined | |

**Provenance semantic.** Every payload this normalizer emits — all branches above plus decode-error partials — stamps `detectedSpec:"generic-http"` and `confidence:1.0`. The 1.0 means full confidence in the projection itself: a structural projection is always a faithful rendering of what it claims to be. It makes zero claim about AI semantics — "no AI spec identified" is what the `generic-http` value says, never a lowered score. The UI renders the provenance chip on fallback rows from these two fields (as a neutral "Structural" badge that suppresses the confidence numeral — printing 1.00 next to a Tier-1 decode's 0.95 would read as more trusted than the real decode).

**Hook scannability.** `NormalizedPayload.TextProjection()` (`core/projection.go`) is the contract that keeps every typed projection visible to content-scanning hooks (keyword, PII): `http-sse` projects one entry per frame (verbatim `dataText`, or the re-marshaled `data` tree), and an `http-json` tree projects as its compact re-marshaled document. A new fallback Kind MUST extend `httpTextProjection` in the same change — a Kind the projection cannot read is content a configured `http`/`all` hook silently stops scanning.

## 5. Reuse across services

`BuildRegistry` wires every tier — `codecs.RegisterDefaultAIBuiltins` (which also enrolls the Tier-1.5 sniffers), `adapters.RegisterTier1AdapterNormalizers`, `extract.WireTier2` — and freezes the registry. Each service builds it once at startup and calls `Registry.Normalize`:

- **AI Gateway** — `core.ExtractUsage` (`packages/ai-gateway/internal/providers/core/usage_extractor.go`) is the entry point; each codec's `DecodeResponse` delegates there.
- **Compliance Proxy** — wires the registry to normalize intercepted request/response bodies (`packages/compliance-proxy/cmd/compliance-proxy/wiring/normalize.go`).
- **Agent** — normalizes a client's outbound traffic on the forward path.
- **Hub** — normalizes at audit time when ingesting agent audit uploads.

Because all four build from the same assembly, the same upstream bytes produce byte-identical canonical output regardless of where they were captured. A new provider's usage or text mapping is added once, in the shared layer, and every service inherits it. The interception-side detail (per-host adapters, Tier-2 detectors for consumer surfaces) is in [compliance-pipeline-architecture.md](../compliance-proxy/compliance-pipeline-architecture.md).

### 5.1 AI Gateway audit key selection — always the ingress format

The AI Gateway audit emitter (`internal/platform/audit`) feeds captured request/response bodies through this registry via `core.BuildAuditFn`. The routing key it supplies is the **ingress wire format** for **both** directions — never the routed upstream adapter — because every byte buffer the gateway captures is in the client (ingress) wire shape:

- **Request** bytes are captured at handler dispatch in the client's wire shape. The codec translates `A → canonical → B` only for the bytes sent upstream, which are never the captured `RequestBody`.
- **Response** bytes are always re-encoded back to the ingress shape (`B → canonical → A`) before they touch `rec.ResponseBody`. Every capture site does this: non-stream responses are stored after `egressReshapeNonStream`; the streaming tee wraps the client `ResponseWriter` so it buffers the per-chunk-reshaped SSE the client received; error bodies are `EncodeErrorEnvelopeForIngress` output. There is no path where the captured response bytes are in the upstream provider's wire shape.

So a non-Gemini model (e.g. OpenAI `o1`) served over the Gemini `:generateContent` ingress records its response in Gemini `candidates[]` shape and is keyed on `gemini` → the Gemini normalizer claims it at Tier 1 (`kind=ai-chat`). Cross-format ingresses whose format has no adapter-only registration (`/v1/responses`, where the ingress format is `openai-responses`) resolve through the registry's path-keyed fallback (`::/v1/responses`); SSE that no Tier-1 key folds is claimed by the matching codec through the Tier-1.5 sniff pass, and only shapes no sniffer recognizes reach the Tier-2 SSE walker. The single key source is `audit.normalizeAdapterType`, which returns `rec.IngressFormat` lower-cased.

### 5.2 AI Gateway lazy canonical + audit reuse (compute-at-most-once)

The request-path canonical (`buildRequestContext` → `Registry.Normalize`) and the
async audit normalize are the **same** computation over the **same** ingress
bytes. To avoid doing it twice, the gateway computes it at most once:

- **Lazy compute.** `buildRequestContext` runs `Normalize` only when a synchronous
  consumer needs the canonical — the response cache (freshness detector + L2
  semantic write-back) or a `smart` routing rule (the only strategy that reads
  `rctx.Request.Messages`). The gate (`Handler.needCanonical`) is a fail-safe
  superset: `cacheEnabled OR anyEnabledRuleHasSmartStrategy` (recursive over the
  strategy tree), computing the canonical on any uncertainty. When no consumer
  needs it, the request path leaves it nil and the async audit writer derives the
  normalized payload from the raw bytes as before.
- **Audit reuse.** When the canonical *was* computed, the request goroutine
  marshals it once (`finalizeAudit` → `RequestNormalizedReuse`) and the audit
  writer reuses those bytes for `request_normalized` instead of re-`Normalize`-ing.
  The request-path `Meta` is aligned to the audit `Meta` (`StripContentTypeParams`
  + lower-cased `AdapterType`) so the reused bytes are **byte-identical** to a
  fresh re-`Normalize` — `traffic_event_normalized` is unchanged. The reuse path
  re-emits the same `normalize_total{direction="request",status="ok"}` /
  `payload_bytes` series the bridge would.

Both behaviors are behind `NEXUS_LAZY_CANONICAL` (default off, kill-switch on:
always compute + always re-`Normalize` = legacy path). The gate decision is
consumer-derived and independent of the ingress↔upstream spec relationship.

## 6. Adding a normalizer or detector

- **Tier-1 normalizer** — implement `Normalizer` in `normalize/codecs`, register it in `codecs/register.go` under its `AdapterType` (and `AdapterType::EndpointPath`) keys, and stamp `Confidence` via `ScoreTier1Confidence` so a low-confidence parse can fall through to Tier 2. If the wire has a cheap protocol-distinctive prefix marker, also implement `core.Sniffer` and enroll it via `RegisterSniffer` so key-missed capture traffic reaches the codec; every new probe must keep the cross-corpus sniffer matrix (`codecs/sniffer_test.go`) clean.
- **Tier-2 detector** — implement `NonJSONDetector` and append it to `NonJSONDetectors` in `normalize/extract/detector.go`; `WireTier2` picks it up automatically.

**Sharp edge:** the standard-API vendor adapters (`packages/shared/traffic/adapters/api/*`) carry no `Normalize` method — the shared codecs own their wire-format keys (`anthropic`, `openai-compat`, `gemini`, …) via `RegisterDefaultAIBuiltins`. Per-host `Normalize` methods exist only on consumer/IDE surfaces, and those whose wire IS a standard API delegate to the shared codec singletons (`codecs.SharedOpenAIChat()` et al.), re-stamping `DetectedSpec` with their adapter ID for provenance. Adding a `Normalize` method to a vendor adapter whose ID a codec already owns panics Hub startup with a duplicate registration — that panic is the lock-step guard, mechanized by a registration test (`adapters/register_builtins_test.go`).

## References

- `packages/shared/transport/normalize/core/` — Normalizer interface, NormalizedPayload, Usage, Registry coordinator
- `packages/shared/transport/normalize/codecs/` — Tier-1 per-wire normalizers + registration
- `packages/shared/transport/normalize/extract/` — Tier-2 NonJSONDetector framework + spec probe + SSE accumulation
- `packages/shared/transport/normalize/buildregistry.go` — tiered registry assembly
- `packages/shared/traffic/adapters/builtins.go` — per-host Tier-1 adapter registration
- `packages/ai-gateway/internal/providers/core/usage_extractor.go` — AI Gateway entry into the shared layer
- `packages/compliance-proxy/cmd/compliance-proxy/wiring/normalize.go` — compliance-proxy registry wiring

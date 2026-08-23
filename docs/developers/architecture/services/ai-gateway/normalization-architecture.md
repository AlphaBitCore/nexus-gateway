# Normalization architecture

The normalize layer turns raw provider/wire bytes into a canonical `NormalizedPayload` — readable text, token usage, and request/response structure — independent of which provider or wire format produced them. It lives in `packages/shared/transport/normalize` and is shared by the AI Gateway, Compliance Proxy, and Agent (request-path enforcement) and the Control Plane (view-time recompute), so the same bytes yield the same normalized result everywhere. The canonical projection is **not persisted on the audit write path**; the Control Plane recomputes it on demand from the stored (already-redacted) body when an operator opens the Traffic drawer — see §5.2 and [audit-pipeline-architecture.md](../../cross-cutting/observability/audit-pipeline-architecture.md).

## 1. The NormalizedPayload contract

A `Normalizer` (`packages/shared/transport/normalize/core`) implements:

```go
type Normalizer interface {
    ID() string
    Normalize(ctx context.Context, raw []byte, meta Meta) (NormalizedPayload, error)
}
```

`Meta` carries the call context: `AdapterType` (the wire key), `Model`, `ContentType`, `Direction` (`DirectionRequest` / `DirectionResponse`), `EndpointPath`, and `Stream`. `NormalizedPayload` is the canonical output — `Kind` (`ai-chat` / `ai-embedding` / `http-json` / …), `Protocol`, `Model`, `Stream`, `Messages[]`, `Tools[]`, `Params`, `Usage`, `FinishReason`, `Inputs[]` (embeddings), `Confidence`, and `DetectedSpec`. A normalizer that does not recognize the bytes returns `ErrUnsupported`, which the coordinator uses to fall through to the next candidate.

`Params` records what the caller asked for about SAMPLING — temperature, top-p, top-k, max tokens, stop sequences — and about REASONING. The three wires express reasoning three ways and they are not losslessly interconvertible: OpenAI takes a level (`reasoning_effort`), Anthropic a budget (`thinking.budget_tokens`), Gemini a budget plus a visibility flag (`generationConfig.thinkingConfig`). `Params.Reasoning` records whichever the caller used, in their own terms, and never derives the other — converting a level to a budget is a judgement about one wire's model, so it belongs to that wire's codec under the split `provider-adapter-architecture.md` §3a already applies to every cross-vendor difference. Two decoding rules follow from that: Anthropic's `{"type":"disabled"}` carries no budget and a nil budget cannot be told from silence, so it lands as an effort of `none`; Gemini's `thinkingBudget: -1` means "you decide", a value no other wire has, and is preserved rather than normalised away. `Reasoning.Asked()` is the predicate consumers use — an allocated struct with nothing in it is silence, not a request to reason.

The response side settled the same concept earlier: thinking blocks, thought parts and reasoning items all decode to `ContentReasoning`, and the counts unify as `Usage.ReasoningTokens`.

A `TransformSpan` describes one byte-level modification against a `NormalizedPayload` (a hook redaction, AI-Guard suggestion, cache-normaliser strip, or cache_control inject); `ApplySpans` reconstructs the wire body from the payload + its spans. A span's `ContentAddress` indexes the modified content (e.g. `messages.<i>.content.<j>`, `http.bodyView`). The reserved address `AddressAuditOnlySentinel` (`"webhook.flat"`) marks an **audit-only** span: it records *what* a subsystem flagged but addresses a flat projection the gateway never reconstructs, so `ApplySpans` drops it — it lands in the audit record only and never mutates delivered or stored bytes. `IsAuditOnlySentinelAddress` is the single recogniser; compliance consumers use its negation as a denylist to tell an applicable redaction from an advisory audit-only span (see `hook-architecture.md` §5, `CarriesRedaction`).

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
- **Gemini request system prompt** — the codec reads both `systemInstruction` and `system_instruction`. Google's JSON mapping accepts the proto field name alongside the camelCase one, and the compliance adapter already reads both, so accepting only the camelCase spelling meant a snake_case caller's system prompt was scanned but never normalised: the audit row showed a conversation missing the instruction that shaped it. The camelCase field wins when both are present (`codecs/gemini_generate.go`).

- **Gemini** — `PromptTokens = promptTokenCount`, `CompletionTokens = candidatesTokenCount + thoughtsTokenCount`, `CacheReadTokens = cachedContentTokenCount`, `ReasoningTokens = thoughtsTokenCount` (`codecs/gemini_generate.go`).
- **OpenAI-compatible family** — `codecs/openai_chat.go` resolves the cached-token alias chain across vendors (DeepSeek `prompt_cache_hit_tokens`, Moonshot `prompt_cache_tokens`); the Responses-API top-level `input_tokens` / `output_tokens` mapping lives in `codecs/openai_responses.go`.

This is the contract `core.ExtractUsage` in the AI Gateway depends on — see [provider-adapter-architecture.md](provider-adapter-architecture.md) §5.

## 4. Text and structure extraction

Beyond usage, a normalizer reconstructs the conversation into `Messages[]`, each a `core.Message` with a role and a `[]ContentBlock`. Content blocks carry typed payloads — `text`, `tool_use`, `tool_result`, and `reasoning` — so the audit pipeline stores readable normalized text and preserves tool and chain-of-thought content rather than dropping it. Embedding wires populate `Inputs[]` and `Usage` instead of messages. Streamed responses are folded frame by frame (`normalize/extract/accumulator.go`) into a single final payload.

A Tier-1 codec whose surface is captured as a self-contained SSE stream folds that stream itself before decoding. `codecs/openai_responses.go` is one case: a streamed `/v1/responses` egress is stored as the raw Responses-API event stream the client received (`event: response.output_text.delta` … terminated by `response.completed`, which carries the complete response object). The codec collapses that stream to the terminal response object — falling back to the accumulated `output_text` deltas when the capture is truncated before the terminal event — so a streamed row normalizes to the same `Messages[]` + `Usage` as a non-streamed one instead of failing the JSON decode on the leading `event:` framing and emitting an empty payload. `codecs/anthropic_stream.go` folds the Anthropic `/v1/messages` event stream the same way: content blocks are reassembled in stream order (`text_delta` runs → a `text` block, `thinking_delta` runs → a `reasoning` block, `input_json_delta` runs → a `tool_use` block with the accumulated JSON decoded into the input), the model arrives on `message_start`, the stop reason and output-side usage on `message_delta`, and `LooksLikeAnthropicSSE` routes the body to the fold even when the capture lost the stream flag and Content-Type. Shared SSE **frame** splitting lives in `codecs/sse_fold.go` (`walkSSEFrames`), used by the OpenAI Chat and Anthropic stream folds and the generic-http SSE projection. It follows SSE dispatch semantics rather than splitting per line: fields are collected until the blank separator, **multiple `data:` lines in one frame are joined with `\n`**, the `event:` name applies to the whole frame regardless of field order, comments and `id:`/`retry:` are ignored, and a trailing frame with no blank line after it is still dispatched at EOF (a truncated capture is the normal shape for an aborted stream, so dropping it would lose that stream's last frame). Dispatching per `data:` line instead — which it did previously — split a W3C-legal multi-line frame into fragments that individually failed to parse, so the frame lost its Tier-1 decode and the body fell through to Tier 3 verbatim. The trade is explicit: consecutive `data:` lines with **no** blank separator, each a complete JSON object, are not spec-legal SSE and now collect into one unparseable frame that falls to Tier 3 — a malformed shape degrading along the fail-open path, in exchange for a legal shape decoding correctly.

### 4.0 Multimodal text surfaces

Three Tier-1 codecs project the multimodal JSON surfaces so the Traffic drawer shows their text the way it shows chat, instead of the generic-http fallback dumping a multi-MB base64 image inline or misdetecting a binary audio body: `codecs/openai_images.go` (`/v1/images/generations` — request `prompt` string/array → user text, response `revised_prompt` → assistant text, each artifact a `MediaRef` — see §4.2), `codecs/openai_audio_speech.go` (`/v1/audio/speech` — request `input` + `instructions` → user text, `kind=ai-tts`), and `codecs/openai_audio_transcriptions.go` (`/v1/audio/{transcriptions,translations}` — response-only, the `json`/`verbose_json`/`text` transcript → assistant text, `kind=ai-stt`). They register under the `<fmt>::<path>` + path-only keys like the embeddings codec, so the path-keyed entry preempts the adapter-only chat codec.

**Terminal-claim contract (load-bearing).** A codec keyed to a binary or non-JSON wire MUST claim its result terminally — return a summary payload with a valid `Kind`, never `core.ErrUnsupported`. A decline hands the walk to the next candidate, the adapter-only chat codec, whose hard unmarshal error on the non-JSON bytes stops the tier walk before Tier-2/3 and mislabels the row `partial`. The TTS response (binary audio) is the concrete case: `openai_audio_speech.go` claims it as an `http-binary` summary (size + content-type), which is exactly the fix for the pre-codec misdetect. There is deliberately **no multipart request codec** — the boundary is stripped from `Meta.ContentType` before any codec runs (`auditbridge.go` cuts at `;`), and the STT/video multipart request bytes are not captured (audio fingerprint-only), so the transcriptions codec is response-only and no images-edits/videos-submit request codec exists.

Video and rerank get no codec — their bodies render acceptably as `http-json` and a dedicated `ai-video`/`ai-rerank` codec would render an empty shell for a text-less job/ranking response. STT/video **response** bodies (transcript/job JSON, small) are captured by the parallel handlers under the payload-capture toggle with `ResponseAction=ActionApprove` so the view-time recompute has bytes to normalize.

**Decode leniency (load-bearing).** Every Tier-1 codec's whole-struct decode
goes through `codecs/lenient.go` `decodeLenient`: when the full-struct
unmarshal fails on a type-level mismatch, it retries field-by-field via
`map[string]json.RawMessage`, dropping only the mistyped field and keeping
everything decodable. The bytes these codecs read are CAPTURED third-party
traffic — an arbitrary app talking to a provider, not something the gateway
validated — so one mistyped optional scalar (`"temperature":"0.7"` as a
string) must not erase the message content from the audit record. Before this
seam, such a body produced a `partial` normalized row with zero messages;
compliance scanning was rescued by the gjson flat-text fallback, but the
agent / control-plane audit consumers were not. Input that is not a JSON
object at all (or where no field recovers) still errors and falls through to
the fallback projections, unchanged.

### 4.1 Fallback projections (generic-http)

Traffic no AI tier claims still gets a TYPED structural projection, never a blind dump. `GenericHTTPNormalizer` (`codecs/generic_http.go`) routes by byte sniff first — capture-side Content-Type headers are routinely missing or mis-stamped, so the bytes outrank the header — then by declared Content-Type:

| Wire shape (sniff/CT) | Kind | Body view | Notes |
|---|---|---|---|
| SSE framing (first non-comment line opens `event:` / `data:` — leading `:` keep-alive comment lines are skipped within a 256-byte probe window; a declared `text/event-stream` Content-Type routes here even when a longer preamble defeats the sniff) | `http-sse` | `sseFrames[]` — one entry per dispatched SSE **frame** (not per `data:` line; a frame's multiple `data:` lines are joined with `\n` before projection): `event` name + `data` (decoded JSON tree) or `dataText` (verbatim string, at most one set — a frame carrying no data line is not projected at all) | Capped at 2000 frames AND 1 MiB cumulative frame data + `sseTruncated:true` (together they bound the persisted row; the raw bytes remain in the Raw view, which is also why the frame list never duplicates the verbatim text) |
| NDJSON (two+ independently complete JSON lines) | `http-json` | `json` — array of the decoded lines | One bad line invalidates the NDJSON assumption → text projection |
| Valid JSON document (first non-ws byte `{`/`[` + whole-body `json.Valid`) | `http-json` | `json` — decoded tree | Claims regardless of declared Content-Type; invalid JSON never enters (a declared-JSON body that fails the decode keeps the explicit error path: text projection for UTF-8 prose, binary digest + surfaced error otherwise) |
| `application/x-www-form-urlencoded` | `http-form` | `form` — key→value map (multi-valued keys newline-joined) | |
| `multipart/*` | `http-multipart` | `form` — field-by-field text; file parts decay to a `<file len=N>` marker | Missing boundary → binary digest |
| `text/*`, or no Content-Type with UTF-8-looking bytes | `http-text` | `text` — verbatim | |
| Everything else | `http-binary` | `binaryRef` — size + sha256 only, bytes never inlined | |

**Provenance semantic.** Every payload this normalizer emits — all branches above plus decode-error partials — stamps `detectedSpec:"generic-http"` and `confidence:1.0`. The 1.0 means full confidence in the projection itself: a structural projection is always a faithful rendering of what it claims to be. It makes zero claim about AI semantics — "no AI spec identified" is what the `generic-http` value says, never a lowered score. The UI renders the provenance chip on fallback rows from these two fields (as a neutral "Structural" badge that suppresses the confidence numeral — printing 1.00 next to a Tier-1 decode's 0.95 would read as more trusted than the real decode).

**Hook scannability.** `NormalizedPayload.TextProjection()` (`core/projection.go`) is the contract that keeps every typed projection visible to content-scanning hooks (keyword, PII): `http-sse` projects one entry per frame (verbatim `dataText`, or the re-marshaled `data` tree), and an `http-json` tree projects as its compact re-marshaled document. A new fallback Kind MUST extend `httpTextProjection` in the same change — a Kind the projection cannot read is content a configured `http`/`all` hook silently stops scanning.

### 4.2 Media: one shape, one grammar, one resolver

Every binary a request or response carries is represented by exactly one
type, `core.MediaRef`, whatever the modality and whatever the provider spells
it. It never carries content bytes in any encoding. It carries what the bytes
ARE (modality, mime, size), what proves which bytes they were (`sha256`), and
— when they are recoverable — a `locator` that says where to find them.

This replaced two `BinaryRef` families, six divergent per-codec behaviours,
and a parallel `image_ref` summary. Those were not accidents: each was the
fastest locally-correct move at the time, and the result was a system where
"is there an image here" had six different answers.

#### The locator grammar

`transport/normalize/locator` owns the grammar, the predicates that decide
whether bytes are addressable, and the resolver that reads them back. All
three live together because a locator is a promise, and a promise made in one
package and kept in another is a promise that drifts. Before this package
existed the codecs decided a download was available and the admin artifact
endpoint decided separately whether it was — so they could, and repeatedly
did, disagree. A card offering a control that then fails is the single
failure this area exists to prevent.

Five containers, each resolvable from the stored bytes ALONE — no database,
no session, no provider call:

| container | addresses |
|---|---|
| `body` | the whole stored body IS the artifact |
| `json:<path>` | plain base64 at a gjson path |
| `datauri:<path>` | a full data URI at a gjson path |
| `sse:<frame>:<path>` | a gjson path inside the Nth data-bearing SSE frame |
| `multipart:<part>` | a named part of a multipart body |

Source-independence is what makes a locator portable: the same string means
the same bytes in the Control Plane, in the Agent Dashboard, and in a
browser. It is why the Agent needed no new API to render media.

`sse:` frames are counted in arrival order over data-bearing frames only, by
the same walk the stream folds use, so the index a codec emits and the index
the resolver counts cannot drift. The container exists because streamed media
is whole within its frame — Gemini sends a complete `inlineData` part per
chunk, OpenAI's `partial_image` events each carry a renderable image — and no
measured producer splits one payload across frames.

#### Custody

Six states, and every one has a producer. A seventh, `redacted`, was defined
with them and nothing ever emitted it (compliance redacts text, not media);
a custody value nothing can reach is a distinction the system claims to make
and does not, so it was removed rather than left as decoration.

| state | meaning | producer |
|---|---|---|
| `captured` | bytes are in the stored body at `locator` | the codecs |
| `external` | a remote URL the gateway never fetched | the codecs |
| `provider-ref` | the provider holds it under its own id | the codecs |
| `fingerprint` | read and forwarded, not retained; digest survives | CP view-time, from `artifact_refs` |
| `aged-out` | captured once, removed by retention | CP sidecar tier |
| `absent` | never there | the codecs |

The predicate that decides whether a UI offers a control is
`captured && !truncated` — captured-and-truncated is a real third state
meaning "the bytes ARE at that locator and are not fully recoverable", which
the card says out loud with its `cause` instead of pretending nothing was
there.

`aged-out` is produced where the Control Plane serves a stored sidecar after
establishing that no body is recoverable. The reference in that sidecar still
says `captured` because it described the world on the day it was written;
returning it unchanged puts a Download on the card for bytes that provably no
longer exist. The digest is kept — it is the last thing that still proves
which file this was — and the locator is cleared, because anything that
trusts "has a locator" would otherwise rebuild the same broken control.

#### Serving

`GET /api/admin/traffic/:id/artifact?locator=<locator>&direction=request|response`
resolves through the same package that built the locator. The served
Content-Type comes from SNIFFING the bytes, never from what the wire
declared — a declared type is written by whoever sent the payload. Anything
not on the frozen inline-renderable list still serves; it downloads instead
of rendering, which is why that list can stay short without anything becoming
unreachable. Responses carry `nosniff`, a `default-src 'none'; sandbox` CSP,
`Cross-Origin-Resource-Policy: same-origin`, `Cache-Control: private,
no-store`, an ETag over the bytes, and a filename built from OUR id and the
sniffed type.

Omitting `locator` keeps the endpoint's original behaviour for image
generations and TTS. Those are not a second implementation: they derive the
locator the caller did not send and go down the same path.

#### Large binary inputs

STT audio and video input references are captured by HANDING OVER the bytes
the request already holds, not by copying them. Measured at the 26 MiB STT
ceiling: the pooled-copy path costs 1.36 ms and 27 MB across 4 allocations
(and a buffer that large never returns to the pool, whose cap is 2 MiB);
the handover costs 3.3 ns and zero allocations. That difference is the whole
reason these can be captured at all without a request-path regression — the
size lands off-heap on the async audit side, via the same spill the inline
ceiling already governs.

Ownership passes to the audit record; the caller must not write to the slice
afterwards. Reading it is fine, which is what lets the same bytes still be
forwarded upstream: capture must never change what goes on the wire, and both
the STT and video paths pin that with a test that re-emits the forward AFTER
capture has taken its reference and asserts the caller's bytes survive
verbatim.

When capture is off, the fingerprint alone still records which file was sent,
and the Control Plane synthesises a `fingerprint` media element from it. The
discriminator is "we stored no request body yet recorded a fingerprint" — not
a list of endpoint kinds — so TTS and image generation, whose fingerprints
describe a RESPONSE and whose JSON request body IS stored, are unaffected and
their artifacts stay in the one place they already are.

#### The compliance boundary

A media element contributes NOTHING to `TextProjection`, the list the
content hooks scan. The bytes are binary, a regex over base64 matches
nothing useful, and projecting them would cost every scan the size of every
payload. The sibling text still projects — the caption next to an image is
often exactly what a rule needs to match — and both halves are pinned in
`core/media_never_scanned_test.go`, because this boundary has been broken
more than once by a codec serialising wire structure into a text block.
## 5. Reuse across services

`BuildRegistry` wires every tier — `codecs.RegisterDefaultAIBuiltins` (which also enrolls the Tier-1.5 sniffers), `adapters.RegisterTier1AdapterNormalizers`, `extract.WireTier2` — and freezes the registry. Each service builds it once at startup and calls `Registry.Normalize`:

- **AI Gateway** — `core.ExtractUsage` (`packages/ai-gateway/internal/providers/core/usage_extractor.go`) is the entry point; each codec's `DecodeResponse` delegates there.
- **Compliance Proxy** — wires the registry to normalize intercepted request/response bodies (`packages/compliance-proxy/cmd/compliance-proxy/wiring/normalize.go`).
- **Agent** — normalizes a client's outbound traffic on the forward path (enforcement / hook input).
- **Control Plane** — recomputes the normalized projection at view time, on demand, from the stored (already-redacted) request/response body when an operator opens the Traffic drawer (`GET /traffic/:id/normalized`). No producer persists the projection on the audit write path, and the Hub neither self-derives nor backfills it — see [audit-pipeline-architecture.md](../../cross-cutting/observability/audit-pipeline-architecture.md) §10.2.

Because all four build from the same assembly, the same upstream bytes produce byte-identical canonical output regardless of where they were captured. A new provider's usage or text mapping is added once, in the shared layer, and every service inherits it. The interception-side detail (per-host adapters, Tier-2 detectors for consumer surfaces) is in [compliance-pipeline-architecture.md](../compliance-proxy/compliance-pipeline-architecture.md).

### 5.1 AI Gateway audit key selection — always the ingress format

The AI Gateway captures request/response bodies in the client (ingress) wire shape and persists `rec.IngressFormat` — the **authoritative ingress wire-format key** — onto `traffic_event.ingress_format` so the Control Plane's view-time recompute selects the right codec. The key is the ingress wire format for **both** directions — never the routed upstream adapter — because every byte buffer the gateway captures is in the client (ingress) wire shape:

- **Request** bytes are captured at handler dispatch in the client's wire shape. The codec translates `A → canonical → B` only for the bytes sent upstream, which are never the captured `RequestBody`.
- **Response** bytes are always re-encoded back to the ingress shape (`B → canonical → A`) before they touch `rec.ResponseBody`. Every capture site does this: non-stream responses are stored after `egressReshapeNonStream`; the streaming tee wraps the client `ResponseWriter` so it buffers the per-chunk-reshaped SSE the client received; error bodies are `EncodeErrorEnvelopeForIngress` output. There is no path where the captured response bytes are in the upstream provider's wire shape.

So a non-Gemini model (e.g. OpenAI `o1`) served over the Gemini `:generateContent` ingress records its response in Gemini `candidates[]` shape and is keyed on `gemini` → the Gemini normalizer claims it at Tier 1 (`kind=ai-chat`). Cross-format ingresses whose format has no adapter-only registration (`/v1/responses`, where the ingress format is `openai-responses`) resolve through the registry's path-keyed fallback (`::/v1/responses`); SSE that no Tier-1 key folds is claimed by the matching codec through the Tier-1.5 sniff pass, and only shapes no sniffer recognizes reach the Tier-2 SSE walker. The persisted key is `traffic_event.ingress_format` (stamped from `rec.IngressFormat`), consumed by the Control Plane view-time recompute (`GetTrafficEventForNormalize` reads it, lower-cased; an empty value — agent / compliance-proxy rows that carry the domain-matched adapter id, or older rows — falls through to the path-and-sniff resolution). See [traffic-capture-storage-normalize-design.md](traffic-capture-storage-normalize-design.md) for the ingress-frame invariant and the cross-protocol rationale.

### 5.2 AI Gateway lazy canonical (compute-on-demand)

The request-path canonical (`buildRequestContext` → `Registry.Normalize`) is
computed only when a synchronous consumer needs it:

- **Lazy compute.** `buildRequestContext` installs a lazy normalize seam and
  materializes the canonical only when a synchronous consumer pulls it — the
  response cache stage (when `cacheEnabled`) or a `smart` routing rule (the only
  strategy that reads `rctx.Request.Messages`, gated by
  `Handler.smartRouteNeedsCanonical` → `Resolver.RequestNeedsCanonical`, recursive
  over the strategy tree via `core.ConfigReadsContent` / `StrategyTreeReadsContent`).
  Each consumer is fail-safe — it pulls the canonical on any uncertainty. When no
  consumer needs it, the request path leaves it nil.

`NEXUS_LAZY_CANONICAL` gates this (default **on**; set `NEXUS_LAZY_CANONICAL=0`
to force always-compute as a kill-switch). The gate decision is consumer-derived
and independent of the ingress↔upstream spec relationship.

**The audit path runs no normalize.** The gateway does not stamp
`request_normalized` / `response_normalized` on the audit row, and the request
canonical is not reused for audit — the audit path runs no `Normalize` and no
`json.Marshal` of the canonical on the request goroutine. The normalized
projection is recomputed at view time by the Control Plane from the stored
(already-redacted) body, so it is neither produced on the latency path nor
persisted on the write path. See §5 and
[audit-pipeline-architecture.md](../../cross-cutting/observability/audit-pipeline-architecture.md) §10.2.

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

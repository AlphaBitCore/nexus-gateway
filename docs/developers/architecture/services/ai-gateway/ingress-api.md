# AI Gateway ingress API

This is the caller's guide to the AI Gateway's HTTP API: how to authenticate, which endpoints exist, and how the gateway lets you keep your existing SDK while routing to any provider. Request and response bodies follow the upstream provider shapes (OpenAI, Anthropic, Gemini) — this page documents the gateway-specific surface, not a re-specification of those bodies; for per-provider body detail see [provider-adapter-architecture.md](provider-adapter-architecture.md) and [provider-coverage.md](provider-coverage.md).

## 1. Base URL and authentication

Point your SDK's base URL at the gateway and use a Nexus **virtual key** as the credential. Every route accepts the virtual key in either carrier:

- `Authorization: Bearer <virtual-key>` — the standard bearer convention, honored on all routes.
- `X-Nexus-Virtual-Key: <virtual-key>` — an explicit header alternative.

Gateway-issued virtual keys are prefixed `nvk_`. The caller never sends the upstream provider's own API key; the gateway holds provider credentials and attaches them when it dispatches upstream.

## 2. Supported endpoints

| Endpoint | Shape | Streaming |
|---|---|---|
| `POST /v1/chat/completions` | OpenAI Chat Completions | yes (`stream: true`) |
| `POST /v1/messages` | Anthropic Messages | yes |
| `POST /v1/responses` | OpenAI Responses | yes |
| `POST /v1/embeddings` | OpenAI Embeddings | no |
| `POST /v1/images/generations` | OpenAI Images (generation) | no |
| `POST /v1/audio/speech` | OpenAI TTS | no |
| `POST /v1/audio/transcriptions`, `/v1/audio/translations` | OpenAI STT (multipart) — parallel streaming-proxy path | no |
| `POST /v1/rerank` | Cohere Rerank (`/v2/rerank`) — the canonical rerank shape (OpenAI ships no rerank API) | no |
| `POST /v1/guardrail` | Standalone compliance verdict (allow/block/redact) over caller text — parallel handler, no upstream relay | no |
| `POST /v1/videos` · `GET /v1/videos/{id}` · `GET /v1/videos/{id}/content` · `DELETE /v1/videos/{id}` | OpenAI Videos (async generation) — multipart submit + poll / download / delete follow-ups, parallel handler + gateway-owned job store; a Gemini target takes the Veo cross-shape leg | no (poll-driven) |
| `GET /v1/realtime` (WebSocket upgrade) | OpenAI Realtime voice — server-side WebSocket relay, parallel handler; **dark launch** (entitlement-gated) | no |
| `POST /api/paas/v4/chat/completions`, `/api/paas/v4/embeddings` | GLM-native | chat: yes |
| `POST /openai/deployments/{deployment}/chat/completions`, `/embeddings` | Azure OpenAI-native | chat: yes |
| `POST /v1beta/models/{model}:generateContent`, `:streamGenerateContent` | Gemini-native | via the stream variant |
| `GET /v1/models`, `GET /v1/models/{model}` | model catalog | no |
| `POST /v1/estimate` | cost preview (no upstream call) | no |
| `POST /v1/ai-guard/classify`, `/v1/ai-guard/compliance-webhook` | AI-Guard classifier | no |

The canonical `/v1/*` routes are the primary surface. The provider-native shims mirror a provider's own path and body so an SDK already pointed at GLM, Azure, or Gemini works against the gateway with only a base-URL and credential change. `/v1/estimate` returns the projected cost of a request without calling upstream (see [cost-estimation-architecture.md](cost-estimation-architecture.md)); the AI-Guard routes are covered in [aiguard-architecture.md](aiguard-architecture.md).

The multimodal routes (`/v1/images/generations`, `/v1/audio/speech`) are OpenAI-shape **native passthrough**: the raw body is forwarded unchanged (modulo the alias → provider-model rewrite; `model` sits at the JSON root exactly as on chat) to an OpenAI-compatible upstream, and the response cache is endpoint-skipped (`gateway_cache_skip_reason = modality_endpoint` — generative variety is the product; no per-modality cache knob exists). The image multipart siblings — `/v1/images/edits`, `/v1/images/variations` — are not yet registered: they need multipart model extraction and ingress-path preservation, and ship together with that work (a route that cannot resolve its model would 5xx, which is worse than 404).

**Routing is modality-scoped across every endpoint.** A request is only ever dispatched to a model whose catalog modality can serve its endpoint kind — enforced uniformly by the routing modality guard for all strategies and by the requested-model passthrough path (see [routing-architecture.md](routing-architecture.md) §4). A chat model named explicitly on `/v1/images/generations`, or auto-selected onto an audio endpoint, is rejected with `400 MODEL_MODALITY_MISMATCH` rather than forwarded to the provider for an opaque 502; when a strategy's whole candidate set is cross-modality the request surfaces `no_compatible_provider`. `model: auto` is modality-aware — on a non-chat endpoint it deterministically picks the cheapest enabled model of that modality (no router LLM), so an image `auto` never lands on a chat model.

**STT (`/v1/audio/transcriptions`, `/v1/audio/translations`)** is served by a **parallel streaming-proxy handler** (`ServeSTT`), NOT the small-JSON `ServeProxy` pipeline: its request is a large binary multipart stream — one-shot, un-re-readable — that the byte-slice executor, response cache, text-scanning hook pipeline, and canonical/codec bridge cannot serve without polluting the hot core (see [e88-s5](../../../specs/e88-s5-stt-transcription.md)). The handler is a straight line — VK auth → per-VK RPM → per-VK generative-caps concurrency → bounded multipart parse → single-target resolve → native multipart forward → meter → audit — reusing the SAME cross-cutting subset via the shared `Handler` methods, and touching none of `ServeProxy`'s internals. Both routes share one wire shape; the handler forwards the ingress path (`transcriptions` vs `translations`) verbatim to the upstream. The request-stage pipeline scans the `prompt` form field — the one request-side text leaf (audio stays fingerprint-only, unscannable pre-transcription): block → 403, redact → the sanitized prompt replaces the original before `ReEmit` so the unredacted text never reaches the provider (redact-re-emit — unlike the video multipart, the parsed-field rebuild CAN carry a modified value), approve → forward unchanged. `compliance_coverage` is honest: `prompt-only` when a content hook cleanly scanned the prompt; `none` when there is no prompt, no content hook, or a hook errored. The transcript (output side) still forwards unredacted — transcript redaction is the v1b differentiator. Bounds: `http.MaxBytesReader` caps the upload mid-stream at the STT generative-caps ceiling (~26 MiB → 413 before full drain), part-count / single-file-part / per-field bounds reject multipart bombs, and a duplicated governance field (`model` / `response_format`) is rejected 400. Only `json` / `verbose_json` / `text` response formats are served; `srt` / `vtt` / `streaming` return an explicit 400 (deferred). Metering derives `AudioSeconds` from the response `duration` (verbose_json) or provider usage tokens; neither present prices $0 with a deduped WARN (never priced on the audio byte-count). Failover is absent in v1a — a deliberate simplification, not a hard limit: v1a bounded-buffers the audio (capped by the upload ceiling), so the body is re-readable and failover is reachable, but it is deferred because a wedged-credential retry also wants the circuit-breaker feedback the executor owns. An upstream failure returns the error to the caller.

**Video (`/v1/videos` + follow-ups)** is the FIRST async capability (see [e88-s6](../../../specs/e88-s6-video-async.md)): a multipart `POST /v1/videos` submits a generation job and returns a JOB object (not a completion), then `GET /v1/videos/{id}` polls, `GET /v1/videos/{id}/content` downloads the artifact, and `DELETE /v1/videos/{id}` cancels. Like STT and guardrail these are **parallel handlers** (`ServeVideo*`), NOT `ServeProxy` — the submit is a large multipart upload and the follow-ups are governed passthroughs keyed by a **gateway-owned correlation store** (`gateway_async_job`, the gateway's first runtime-writable table). Correctness rests on read-your-write (a client may poll immediately after submit): the row binds the provider job id → VK → submit-time credential, so every follow-up is authz'd on the row (`unknown / foreign id → 404 non-disclosure`, never forwarded upstream) and resolves the SAME provider account that owns the job (credential pinned). **Cost (single cost-bearing row):** the submit row stamps the requested-seconds × per-second-price estimate (estimate-as-floor); the poll that first observes completion reconciles the live quota with the same seconds×price value (never a provider-reported figure); poll/content/delete rows stamp $0. **Render bound:** admission counts the VK's non-terminal job rows (built-in cap, not the HTTP-concurrency cap). **Content relay:** streamed through a sha256/size fingerprint tee with a 1 GiB ceiling (declared oversize → 502, mid-stream overflow → connection abort — never a silent short file), a Content-Type allowlist (`video/mp4`, `image/jpeg|png|webp`), and `nosniff` + `attachment`; `coverage = none` (the artifact is not content-scanned — R-1). **Cross-shape:** a Gemini-format target takes the **Veo leg** — the codec translates OpenAI `/v1/videos` ↔ `:predictLongRunning` + long-running operations (allow-list-only, lossy size → aspect+resolution with `X-Nexus-Coerced`, provider errors normalized to the OpenAI envelope), the canonical job id is `veo_`+base64url(operation name), and the download dereferences the provider artifact URI under an SSRF + host-allow-list guard (the one provider-URL-deref in the product). Per-leg capability differences (Veo: `video`-variant only, best-effort local delete that does NOT stop the still-billed render, honest `size`/`completed_at`/`prompt` omissions) are in the e88-s6 §6 matrix. `GET /v1/videos` (list) and `remix` / `edits` / `extensions` / `characters` are deliberately unserved with an explicit OpenAI-shaped 404 envelope (list is account-wide → would leak other VKs' jobs; remix/edits render additional paid seconds and need their own admission-cost design). SDK poll loops ride the VK RPM budget.

**Guardrail (`/v1/guardrail`)** is the standalone compliance-verdict endpoint (see [e90-s1](../../../specs/e90-s1-guardrail-endpoint.md)): a caller submits text and receives an `allow` / `block` / `redact` verdict from the **same** hook pipeline the inline path runs (rule-pack + PII redaction + the AI-Guard judge), **without** relaying an LLM completion — the ApplyGuardrail / Content-Safety category, backed by the deployment's already-configured policy rather than a generic classifier (same policy, two entry points, one audit trail). Like STT it is a **parallel handler** (`ServeGuardrail`), not `ServeProxy`: it reuses the shared admission subset (VK auth → per-VK RPM → per-VK generative-caps concurrency → body cap) and the SAME `resolver.BuildPipeline` + `Pipeline.Execute` the inline path uses, then maps the aggregate `CompliancePipelineResult` to a JSON verdict — touching none of `ServeProxy`'s routing/executor/codec/cache. The request is `{stage: input|output, content | messages[], include_redacted_content?}` (exactly one of content/messages; `stage` defaults to `input`). The endpoint **always returns 200** with the verdict — a `block`/`redact` disposition is data in `action`, not an HTTP error. Load-bearing behaviours: `coverage` (`full`/`degraded`/`none`) is the fail-open honesty signal — a judge that fails open leaves an `allow` indistinguishable from a clean scan without it, so `degraded` marks "not fully scanned" and `none` marks "no hooks bound for this stage"; `assessments[]` is the per-policy breakdown projected from the per-hook results; `redactions[]` are rule-pack/PII spans as offsets into the joined evaluated text (AI-Guard judge spans are audit-only, not returned, and not reflected in `redacted_content` — consistent with inline); `blocking` carries category/severity/labels only (pack/version/rule IDs are withheld to avoid an evasion oracle). The raw evaluated text is **never persisted** to the payload store (the input is the caller's sensitive material — allow, block, and redact all store only the verdict/tags/coverage). v1 bounds judge-budget abuse with per-VK concurrency + RPM + a 1 MiB body cap; a hard per-VK spend ceiling and per-call cost in the response are a fast-follow (both need judge cost surfaced through the hook boundary — the per-call judge cost meanwhile rides the joinable internal `ai-guard` audit row).

**Realtime voice (`GET /v1/realtime`, WebSocket)** is the gateway's FIRST WebSocket surface (see [e88-s7](../../../specs/e88-s7-realtime-voice-relay.md)): a server-side relay of the OpenAI Realtime API. The upgrade request runs the shared admission chain on plain HTTP (every refusal is an ordinary HTTP error), the gateway dials the provider `wss://…/v1/realtime?model=<resolved>` FIRST (provider key injected, client credential/headers/subprotocols never echoed upstream) and only then hijacks the client — a 101-then-immediate-close would break every SDK. Then two goroutines relay text frames **verbatim** (one per direction, sequential Read→Write, no queue; binary → 1003, >16 MiB frame → 1009). This is a **P1 dark launch**: the realtime model is reachable only by a VK whose `AllowedModels` explicitly names it — an EMPTY list ("unrestricted" everywhere else) is **NOT** entitled here (an unbounded voice session is the most expensive billable surface), and a non-entitled or unroutable model gets one 404 `NO_COMPATIBLE_PROVIDER` non-disclosure envelope. Operators entitle a **dedicated realtime VK** (adding the model to a busy unrestricted VK would restrict all of that VK's other traffic). Built-in caps (not admin knobs): a per-VK concurrent-session cap (default 2 — raise via env for production fan-out) and a per-WS-frame ceiling; a 65-minute hard session guard; a 60-second by-hash VK-status recheck severs a revoked key mid-session (fail-open on a transient backend blip). **Metering:** one `traffic_event` row per in-band `response.done` (the protocol re-bills context per response, so a row is one exchange — fresh id, six-component text+audio+cached cost, real created→done latency) plus one $0 session row at close; per response the gateway runs a fresh quota Check → Reconcile → post-settle sever (reject or downgrade cap crossed → close 1008, bounding overshoot to ≤ one response). `compliance_coverage = none` on every row — **P1 does no content scanning in either direction** (transcript-level compliance is the P3 differentiator). Per-minute-billed models, browser/ephemeral-token clients, and Azure/Gemini realtime are out of P1 scope.

Cross-shape translation is live for the **image** modality only (proposal D9 / P2.5): a self-built OpenAI-images↔`:generateContent` codec so a caller uses one canonical image contract across providers whose image model has no dedicated endpoint (Gemini Nano Banana). Image routing opens exactly two target legs in v1: the literal OpenAI provider (native passthrough, byte-unchanged) and Gemini (translated); every other provider — including OpenAI-wire-compatible siblings such as Azure — is its own demand-gated leg, not yet routable for images. For **audio and TTS** routes, cross-shape does not apply: the resolved provider must speak the OpenAI shape.

### Image request parameters, per leg (the caller contract)

When routing resolves an image request to a **native OpenAI-shape** provider, the body forwards unchanged (P1 passthrough; the provider applies its own parameter rules). When it resolves to the **Gemini leg**, the canonical body is translated field-by-field under a **closed allow-list** — every parameter is mapped, coerced with a recorded marker on the `X-Nexus-Coerced` response header, or rejected 400; nothing is silently dropped or forwarded. Because default SDK clients do not surface response headers, **this table is the primary contract** an engineer programs against:

| Parameter | Native OpenAI leg | Gemini leg |
|---|---|---|
| `prompt` | forwarded | → `contents[0].parts[0].text`; must be a non-empty string (array/object forms 400 — the scanner-alignment rule) |
| `n` | forwarded (OpenAI enforces its own `n ≤ 10`) | must be **1** (omitted on the wire — the upstream default; live-verified 2026-07-15: Gemini rejects `candidateCount>1` for image models with 400 "Multiple candidates is not enabled"). Anything else (0, 2+, fractional, string) 400 |
| `size` | forwarded | → `imageConfig.aspectRatio` via a closed lossy map over the documented OpenAI sizes (`256x256`/`512x512`/`1024x1024`→`1:1`, `1792x1024`→`16:9`, `1024x1792`→`9:16`, `1536x1024`→`3:2`, `1024x1536`→`2:3`); marker recorded. `auto`, empty, or absent → field omitted (provider default, no marker); any other value 400. **Pixel dimensions are not honored — output resolution is provider-decided**; the marker communicates the ratio, not a resolution guarantee |
| `quality`, `style`, `user` | forwarded | dropped with a value-free marker (`quality→dropped`, …) — no Gemini equivalent |
| `response_format` | forwarded (dall-e default is `url`) | closed value set. `b64_json` → native. `url` → **coerced to `b64_json`** + marker (the gateway does not yet serve artifact URLs). **Absent → `b64_json` + marker `response_format:default→b64_json`** — the OpenAI default would have been `url`, so an SDK caller reading `.url` gets nothing; read `.b64_json` on this leg. Unknown values 400 |
| `stream` | ignored (all multimodal is forced non-stream) | same |
| anything else (`background`, `output_format`, `tools`, `systemInstruction`, `safetySettings`, `nexus.*`, unknown keys) | forwarded (OpenAI validates; `nexus` is stripped) | **400, never forwarded** — the closed-set guarantee that keeps tool-augmented generation, provider-safety relaxation, and prompt-scanner bypass off the wire. The 400 names the field and the resolved provider/model |

**Built-in generative caps (both legs).** Expensive generative endpoints carry
built-in per-VK caps — no admin configuration — so a leaked or abusive virtual
key cannot turn a per-call-priced endpoint into an unbounded billing-DoS. For
`image_generation`: at most **4 concurrent** requests per VK (a 5th returns
**429 `GENERATIVE_CONCURRENCY_LIMIT`** with `Retry-After` until one completes),
and a request body over **256 KiB** returns 413 (tighter than the global 10 MiB
cap — image bodies are small JSON prompts). TTS and video carry their own
defaults (8 / 2 concurrent). The caps are code defaults, env-overridable
(`AI_GATEWAY_GENERATIVE_CAP_<KIND>_CONCURRENCY` / `_MAX_BYTES`) for operators who
must tune them. Non-generative traffic (chat / embeddings) is never affected.
The override is **global, not per-VK** — on the single-tenant deployment,
raising the concurrency cap to serve one high-volume image VK also raises the
billing-DoS ceiling for every other VK (an informed org-level tradeoff; there is
deliberately no per-VK knob).

Response on the Gemini leg is reshaped to the OpenAI images envelope — **`created` + `data[].b64_json` only: no `usage` object, no `data[].revised_prompt`** (callers migrating from `gpt-image-1`/`dall-e-3` should expect those fields to be absent on this leg; token accounting still lands in the traffic event). Interleaved model text parts are dropped (the images envelope has no text slot — a documented loss). A provider-safety block surfaces as an OpenAI-shaped content-policy **400 that never retries or fails over to another target**; an image-less upstream reply surfaces as 502 — never a 200 with an empty `data[]`.

### Multimodal compliance coverage (capability matrix)

What the gateway scans and enforces, per modality and per artifact mode — stated exactly, never more:

| Modality | Input (prompt/text) | Output | Artifact accounting |
|---|---|---|---|
| Image generation | **Scanned**; a content-rule match **hard-blocks (403 `GENERATIVE_PROMPT_BLOCKED`)** even when the matching hook is configured observe-only — the binary output is uninspectable, so the prompt is the only control point | **NOT scanned** (binary artifact; no output-side content review) | `b64_json` mode: streaming `{sha256, sizeBytes, mime}` fingerprint; `url` mode: reference stored, **never dereferenced** — no fingerprint |
| TTS (`/v1/audio/speech`) | **Scanned**; keeps the operator-configured posture (no hard-block escalation); a redact demand fails closed (403) because the wire cannot carry a rewritten prompt | **NOT scanned** (audio waveform) | Whole audio body fingerprinted with the real response `Content-Type` |
| STT (`/v1/audio/transcriptions`, `/v1/audio/translations`) | **Prompt scanned** — the `prompt` form field (the one request-side text leaf) runs the request-stage pipeline: block → 403; redact → the sanitized prompt replaces the original before `ReEmit` (redact-re-emit — the provider never sees the raw text); `compliance_coverage = prompt-only` on a clean content-hook scan, `none` when there is no prompt / no content hook / a hook errored. The audio input itself is a waveform, unscannable pre-transcription | **NOT scanned** — the transcript forwards unredacted; output transcript redaction (`coverage = output`) is the v1b differentiator, gated on transcript-PII demand | Input **audio** fingerprinted `{sha256, sizeBytes, mime}` (reference only — the audio bytes never enter the audit body pool); the transcript is not an artifact |
| Rerank (`/v1/rerank`) | **Fully scanned — request-side**: the `query` **and every element of `documents[]`** are extracted as separate text parts and scanned. Unlike the binary modalities, a redact policy **rewrites the offending text in place** in the outgoing body (real redaction, not block-only) so the provider never sees the PII while relevance ranking still works; a hard-block policy blocks. Documents are the enterprise's retrieved corpus and the bulk-PII carrier, so each is scanned individually | scores/order only (no provider-generated free text to scan) | Bounded/truncated document projection captured on a hook match so redaction is auditable — full document text stays behind the raw-payload sidecar (off by default) to bound row size |
| Video (`/v1/videos`) | **Scanned**; a content-rule match **hard-blocks (403 `GENERATIVE_PROMPT_BLOCKED`)** even observe-only — the video output is uninspectable, so the prompt is the only control point (same posture as image); a redact demand fails closed (403). The `input_reference` image is NOT scanned (binary) | **NOT scanned** — the generated video is an uninspectable artifact (R-1 stands, owner+legal-gated); the download's fingerprint tee is R-1's named remediation mount point | Submit `input_reference` fingerprinted `{sha256, sizeBytes, mime}` (reference only); the downloaded artifact fingerprinted through the streaming tee on a COMPLETE relay (empty `artifact_refs` on an aborted/incomplete transfer is the honest incompleteness signal). `compliance_coverage = none` on poll/content rows |
| Realtime voice (`GET /v1/realtime`) | **NOT scanned (P1)** — the relay forwards frames verbatim in both directions; `compliance_coverage = none` on every row. Transcript-level compliance (the industry gap) is the P3 differentiator; audio frames are never scanned or buffered into the audit pool (mirrors STT's R-7) | **NOT scanned (P1)** — output enforcement (detect→cancel with a bounded already-heard leak, never in-flight audio redaction) is P3 | No artifact — session token metering only; audio bytes never enter the audit body pool |
| Chat / embeddings | Scanned and enforced per the configured hook policy (unchanged by the multimodal work) | Chat responses scanned per policy; embedding vectors carry no scannable text | — |

Every multimodal traffic event carries a `compliance_coverage` value (`prompt-only` / `none`) recording what actually ran — `prompt-only` is stamped only when a content-scanning hook really evaluated the prompt; a hooks-off pipeline, an unscannable prompt slot, or an emergency bypass records `none`. Rerank is the one non-chat kind whose coverage is a **full request-side scan** (query + all documents), not prompt-only; its coverage is stamped truthfully from what the extractor actually ran and never over-claims.

### Model catalog

`GET /v1/models` and `GET /v1/models/{model}` return the catalog of models the gateway serves. Both require a valid virtual key (parity with every upstream provider's `/v1/models`); an unauthenticated call is rejected with `401`. The result is scoped to the key: a virtual key restricted to specific models sees only those in the list, and requesting the detail of a model outside that scope returns `404` (the model is hidden rather than revealed).

The response shape follows the caller's SDK. Send an `anthropic-version` header to get Anthropic's native `/v1/models` shape (`data[].{type:"model", id, display_name, created_at, max_input_tokens, max_tokens}` plus top-level `first_id`/`last_id`/`has_more`); otherwise the OpenAI-style `{object:"list", data:[…]}`. `GET /v1/models/{model}` returns a single entry in the same shape.

Each entry carries Nexus extension fields so a client can choose a model locally without a second round-trip: `aliases` (alternate request strings that resolve to the model), `features` (capability flags such as `vision`, `function_calling`, `json_mode`, `thinking`), the context window (`maxContextTokens`/`maxOutputTokens`, carried as the native `max_input_tokens`/`max_tokens` in the Anthropic shape), `type` plus `inputModalities`/`outputModalities`, `lifecycle` (`ga`/`preview`/`deprecated`), `capabilityJson` (embedding dimensions, batch limits), and `pricing` (configured USD-per-million-token input/output rates plus cached-input read/write rates). SDKs ignore the extension keys; any field is omitted when unset.

## 3. Cross-format translation

You do not have to match your API shape to the target provider. The gateway accepts your request in whichever supported shape your SDK speaks, translates it to whatever provider and model the routing rule selects, and translates the response back into your shape. A request to `/v1/chat/completions` can be served by an Anthropic or Gemini model, and the client still receives an OpenAI Chat Completions response.

This works through a canonical pivot. The ingress codec decodes your body into a canonical (OpenAI-shaped) representation via `CanonicalBridge.IngressChatToCanonical` (or the embeddings counterpart); routing selects the target; the target provider's adapter translates the canonical request into that provider's wire format; and on the way back the upstream response is normalized to canonical and re-encoded to your ingress shape via `ResponseCanonicalToIngress`. The translation contract — canonical equals the OpenAI shape, and each non-OpenAI adapter owns its own canonical↔wire mapping — is detailed in [provider-adapter-architecture.md](provider-adapter-architecture.md) §3a, and the normalization layer in [normalization-architecture.md](normalization-architecture.md).

Two guardrails bound this:

- **Compatibility filter** — the gateway only routes to targets the ingress shape can be translated to; an incompatible target is filtered out of the candidate set rather than producing a broken call.
- **Responses-API guard** — a `/v1/responses` request whose resolved target is not natively a Responses provider is rejected with a Responses-shaped `400`, because stateful Responses fields and OpenAI built-in tools cannot be honored over a non-Responses wire.

## 4. Choosing the model

The request's `model` field drives routing (see [routing-architecture.md](routing-architecture.md)). Send a concrete model and the gateway resolves it to a provider+model target through the active routing rules; send the `auto` sentinel to hand model selection to the LLM-dispatch smart router (see [smart-routing-architecture.md](smart-routing-architecture.md)).

Provider-specific parameters that have no clean OpenAI equivalent (for example Anthropic's `thinking` or Gemini's `thinkingConfig`) travel in the `nexus.ext.<provider>.<key>` namespace on the request body, so a single canonical request can still carry vendor extensions — see [provider-adapter-architecture.md](provider-adapter-architecture.md) §Rule 4.

## 5. Streaming

Set `stream: true` (or use a provider-native streaming path such as Gemini's `:streamGenerateContent`) to receive a Server-Sent Events stream. The stream is emitted in the event grammar of the API shape you called, regardless of which provider served it — a streamed `/v1/chat/completions` always yields Chat Completions SSE frames even when an Anthropic or Gemini model produced them.

## 6. Response and control headers

On every response the gateway stamps headers that report what happened:

- `X-Nexus-Routed-Model` / `X-Nexus-Routed-Provider` — the model code and provider the request was actually served by (useful when the requested model was `auto` or substituted by routing).
- `X-Nexus-Attempts` — how many upstream attempts were made (retry/fallback).
- `X-Nexus-Cache` — the cache outcome for the request.
- `X-Nexus-Quota-Used` / `X-Nexus-Quota-Limit` / `X-Nexus-Quota-Warning` / `X-Nexus-Quota-Downgrade` / `X-Nexus-Quota-Original-Model` — quota accounting and any quota-driven model downgrade.
- `X-Nexus-Hook` / `X-Nexus-Coerced` / `X-Nexus-Mode` — compliance-hook and request-handling annotations.

On the request side, send `X-Nexus-No-Cache: 1` to bypass the response cache for that call (the request still executes upstream; see [response-cache-architecture.md](response-cache-architecture.md)). The older `X-Nexus-Aigw-No-Cache` spelling is deprecated but still honoured; move to `X-Nexus-No-Cache`, which will be the only accepted name in a future release.

To attribute traffic to your own users and conversations, tag requests with
`X-Nexus-End-User-Id` and `X-Nexus-Session-Id`. Both are opaque to the
gateway and land on the persisted traffic row (`traffic_event.end_user_id` /
`.session_id`, each indexed with `timestamp`), so your billing or debugging
system can join gateway traffic per user and group it per conversation. The
headers are the only source: a provider's own end-user field (the OpenAI
`user` / `safety_identifier`, the Anthropic `metadata.user_id`) identifies
your end user to that provider and is left alone, so the tag you send here
is the one that lands on the row. Values are trimmed and capped at 256
bytes; they never affect quota, routing, or IAM. The full
header registry is [nexus-headers.md](../../cross-cutting/foundation/nexus-headers.md).

## 7. Errors

Errors are returned in the envelope of the API shape you called, with the HTTP status preserved: an OpenAI-shape route returns `{"error": {"message", "type", "code", "param"}}` and an Anthropic-shape route returns `{"type": "error", "error": {"type", "message"}}`. So an SDK's native error handling continues to work against the gateway.

## References

- `packages/ai-gateway/internal/auth/vkauth/vkauth.go` — virtual-key carriers and `nvk_` prefix
- `packages/ai-gateway/cmd/ai-gateway/wiring/routes.go` — ingress route registration
- `packages/ai-gateway/internal/ingress/models/models.go` — model-catalog endpoints (`/v1/models`, `/v1/models/{model}`): vk enforcement, per-key filtering, response shapes
- `packages/ai-gateway/internal/execution/canonicalbridge/` — ingress↔canonical translation
- `packages/ai-gateway/internal/ingress/proxy/proxy.go` — cross-format filter, Responses guard, response headers, no-cache handling
- `packages/ai-gateway/internal/ingress/envelope/error_envelope.go` — per-ingress error envelopes
- `packages/ai-gateway/internal/providers/canonicalext/` — `nexus.ext.<provider>.<key>` extension helpers

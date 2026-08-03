# Proposal: Full-modality gateway (image / TTS / STT / video)

> **Status:** Draft v2.1 — reworked after a 5-expert adversarial review (architect / security / business / performance / product, 2 rounds, code-grounded), then re-verified by a second 5-expert pass. The first review's 1 critical + 8 high findings are all **closed**; the second pass rated the rework **minor-fixes** (no needs-rework, no new critical), and those minor fixes are folded in here: request-time vs view-time boundary made explicit, URL-mode-vs-byte-bearing artifact fingerprint honesty, `compliance_coverage` as a versioned request-time stamp, `ext` coverage upgraded to a structural gate, single-tenant wording. This revision **resolves the decisions** rather than deferring them to the owner. **Type:** Architecture proposal (not yet ratified) — ready for brainstorm / SDD.
>
> **Goal:** Extend the gateway from `chat` + `embeddings` to image generation, TTS, STT, and (later) video — reusing the existing typology foundation, treating normalization as **read-only, view-time projection** (no normalized-sidecar persistence, no lossless re-serialization), and letting compliance degrade gracefully but *visibly*. The full-modality claim is scoped honestly: where we cannot inspect (binary output), we say so on the record.

---

## 0. Decisions resolved (the calls this proposal makes)

The review surfaced eight owner-decisions (D1–D8). Rather than punt them, this proposal makes each call on best-architecture / best-product grounds. Owner can override, but the plan is decisive by default. A ninth decision (D9) was added in a later revision — see the note under the table.

| # | Decision | **Resolution** |
|---|---|---|
| D1 | Where the descriptor lives | **Descriptor assembly is view-time from the stored payload; the `traffic_event_normalized` sidecar is not resurrected.** The redacted payload already carries the content; the descriptor (units + non-text metadata) is assembled at view-time, like the current normalized view-time recompute. **But the request path is not a no-op** (see §3.0): it MUST run text-leaf extraction so hooks scan the prompt in real time and produce redaction spans — otherwise P2 prompt hard-block never fires and unscanned prompt text is retained at rest. So the split is: **request-time = text-leaf extraction (scan + redact) + two non-PII stamps; view-time = descriptor assembly.** Two request-time **non-PII** stamps land on `traffic_event` (not the sidecar): the binary artifact fingerprint (D4) and the `compliance_coverage` enum (D5/§6). Both are versioned `traffic_event` contract changes with CHANGELOG notes (same rigor as §4), both GDPR-neutral (row-level retention/DSAR). The AI-Gateway no longer stamps the normalized sidecar (that write is skipped); the Hub-side write + the table still exist and are pending the GDPR Art.17 DROP. (View-time direction confirmed by owner.) |
| D2 | Whether to build at all | **Demand-gated at P0.** No sync-modality plumbing is built without a real prospect. Reserved typology enums stay dormant at zero cost until then. "Demand-gated" now gates *whether*, not just *when*. |
| D3 | First value to ship | **Cost + audit visibility, decoupled from compliance.** It is the highest-ROI, no-text-compliance-needed slice; it ships across all three sync modalities first (P1). |
| D4 | Artifact custody | **Streaming integrity fingerprint (byte-bearing modes only) + reference on `traffic_event`; never server-side dereference the URL (SSRF guard).** URL-return mode is honestly reference-only (no artifact-body fingerprint). Proxy-and-store is a deployment-level opt-in for a legal-custody requirement, off by default. (Single-tenant product — this is a per-deployment switch, not multi-tenant isolation.) |
| D5 | Generative prompt-side compliance posture | **Hard-block-on-match by default** for image/video generation — it is the only control point for illegal-content prevention. Not observe-only. |
| D6 | Cost fields | **Re-add the deleted `BillableUnits` units + per-kind formulas as a versioned migration, shipped in the same batch as any modality route.** No route lands before its cost formula. |
| D7 | `ext` governance | **`ext` is never a compliance or cost blind path.** Any user-intent text → promoted to core text-leaves (scanned + redacted). Any billing dimension → promoted to core. `ext` holds only non-text, non-billing provider knobs. |
| D8 | Video in v1 | **Removed from v1.** No video schema is designed until the async-job capability (P4) exists and a hard requirement arrives. One-line deferral only. |
| D9 | Cross-shape image translation (revision) | **Self-built image codec, owner-directed, still per-leg demand-gated.** The confirmed demand is "generate images/videos" with the provider **unspecified** — so "OpenAI-shape only" was our scoping, not the requirement, and the owner has directed that cross-shape be built. That owner directive is the demand signal (per D2 "whether"); the market trend (Google deprecating Imagen `:predict` on 2026-08-17 and pushing to the chat-shaped Nano Banana `:generateContent`) is supporting context for *why image needs a codec at all*, not a substitute for demand. Mechanism: canonical = OpenAI images shape; the gateway owns the OpenAI-images↔provider-wire translation, **self-built** (not delegated to a provider's OpenAI-compat shim). **Each non-OpenAI image leg is independently whether-gated** (a named provider demand): v1 opens exactly two — OpenAI images (native passthrough) + Gemini Nano Banana. **Native passthrough stays the default** for any image provider that already speaks the OpenAI images shape, so the codec set stays small, not a per-provider-sprawl template. **Allow-list-only + strict §3a** (see §7): the OpenAI-images canonical is a closed field set; every param is explicitly mapped / dropped-with-a-caller-visible-signal / 400 (never a silent drop), and any field outside the canonical schema — `tools`, `toolConfig`, `systemInstruction`, `safetySettings`, the whole `nexus.ext.*` channel — is **rejected (400), never forwarded** to `:generateContent` (R-8). This narrows §11's cross-shape non-goal and moves image cross-shape out of P5 into the image phase (§9). Reverses two binding statements in `provider-adapter-architecture.md` (§1/§3, "multimodal routes are native passthrough, no cross-shape") — those must change in lockstep (§3a contract change). Field-level mapping in the SDD; the *mapping* is not greenfield (Google's OpenAI-compat layer and LiteLLM both implement it), but the gateway's cross-format *bridge* is (today `canonicalbridge` carries only chat + embeddings — image needs net-new `IngressImagesToCanonical` / `ImagesWireShapeForTarget` / image response reshaper). |

> **D9 revision note.** Owner-directed after prior-art review of LiteLLM / Portkey / Google's OpenAI-compat layer. Supersedes the original "OpenAI-shape only in v1" scoping and the "cross-shape deferred to P5" non-goal **for the image modality only** — audio/video cross-shape and full chat cross-shape stay deferred (§9/§11). Accepted limitations, each surfaced honestly at runtime, not just in this doc: (a) OpenAI `size` (pixel dims) → Gemini `aspectRatio` is inherently lossy (a coarse ratio + `imageSize` tier, not arbitrary pixels) — so **image is carved out of the §3a round-trip-identity lossless standard** (it cannot round-trip pixel size); the coercion is reported to the caller via `X-Nexus-Coerced`, not silently swallowed. (b) `response_format:"url"` is honored only once the artifact store (D4) is wired, and its served URLs must be authorized to the originating principal/VK, unguessable, expiring, and audited (R-9) — until D4 is wired a `url` request degrades explicitly (`X-Nexus-Coerced` / 400), never a silent b64 substitution. **Conversational (multi-turn) image editing is out of scope for the one-shot images canonical** — it is reached via the chat `:generateContent` route and governed as chat (R-3), not through P2.5.

---

## 1. Current foundation — what exists, what does not

**Already shipped (`packages/shared/transport/typology/`) — verified in review:**

- `EndpointKind` reserves `image_generation`, `tts`, `stt`, `video_generation`, `batch`, `job`.
- `WireShape` defines `openai-images`, `openai-audio-speech`, `openai-audio-transcriptions`, `openai-batches`.
- `ClassifyPath` maps — with passing tests — `/v1/images/*`, `/v1/audio/speech`, `/v1/audio/transcriptions|translations`, `/v1/batches` to their `(Kind, WireShape)` pairs.
- Audit already classifies these `endpoint_type` values.

**Explicitly NOT built (the real work — do not overstate it):**

- **No ingress routes** for `/v1/images/*`, `/v1/audio/*` (requests 404 today).
- **Cost does NOT support these.** `BillableUnits` was reduced to `PromptTokens` / `CompletionTokens`; `Images` / `AudioSeconds` / `VideoSeconds` were deleted. Unknown-kind falls back to chat pricing with a one-time WARN. Any modality traffic is silently mis-priced until §4 lands. **This is a versioned DB/registry contract change, not additive.**
- **No per-modality normalization.** The normalize layer produces the chat/embedding projection only.
- **`video_generation` has a Kind but no WireShape / classify rule** — no industry-canonical video shape exists.

Reserved enums are zero-cost when dormant; they are **not** evidence that the work is half-done.

---

## 2. Design principle — normalization is read-only, view-time observation

The chat canonical-adapter model (canonical = OpenAI shape; adapters own lossless canonical↔wire translation) pays off only when the industry converged on one shape, transport is sync/SSE, and the payload is text. For the new modalities those conditions degrade, so we drop two assumptions:

- **No universal canonical body shape across modalities.** Forcing one causes perpetual churn.
- **Native-shape passthrough is the default; image is the one modality that also gets a self-built cross-shape codec (D9).** v1 forwards each provider on the wire shape the customer already sends (**native-shape passthrough**, raw body unchanged) wherever the customer and provider already share a shape. The exception is **image generation**: because best-in-class image models increasingly have no dedicated endpoint (Google's Nano Banana is chat-shaped `:generateContent`), the gateway exposes a single canonical image contract (OpenAI images shape) and **owns the OpenAI-images↔provider-wire translation** for the non-native legs (Gemini `:generateContent`), strict-§3a. Cross-shape translation for the *other* modalities (audio, video) and full chat stays deferred (§9, §11).

Normalization therefore stops being *translation* and becomes *observation* — and, per D1, that observation happens at **view-time from the redacted stored body**, never on the write path.

---

## 3. Normalization — the corrected model

### 3.0 The request-time / view-time split (load-bearing)

This is the single most important clarification of the "read-only, view-time" framing: it applies to **descriptor assembly**, not to compliance execution. Two distinct jobs run at two distinct times.

| Runs at | Job | Why it cannot move |
|---|---|---|
| **Request time** | Text-leaf extraction → feed hooks (real-time scan + P2 prompt hard-block) → emit redaction spans applied to the stored body | Hooks must decide *before* the request is forwarded; redaction spans must be produced *before* the body is persisted, or unscanned prompt text is retained at rest |
| **View time** | Descriptor assembly (units, non-text metadata) from the already-redacted stored body | Nothing to enforce; pure presentation — reuses the existing normalized view-time recompute |

The request-time text-leaf extraction is what makes "hooks: no change" true *and* keeps prompts scanned. It closes the native-passthrough blind path: a provider-specific text slot the extractor has **not** modeled would silently skip both real-time scanning and redaction — so extractor text-slot coverage is a **structural delivery gate**, not a documentation wish (see §3.4 rule 5).

### 3.1 Descriptor assembly — view-time, no sidecar resurrection (D1)

The descriptor (units + non-text metadata) is assembled *when a traffic event is viewed*, from the already-redacted body — the same view-time recompute the normalized view already uses. Consequences:

- The `traffic_event_normalized` sidecar is **not resurrected** — the AI-Gateway no longer stamps it (that write is skipped); the Hub-side write + the table still exist and are pending the GDPR Art.17 DROP. This proposal adds nothing to it.
- Text leaves (prompt, transcript) inherit redaction automatically — they are read from the already-redacted body at view time; the redaction itself was applied at request time (§3.0).
- **Two request-time write additions, both non-PII, both versioned `traffic_event` contract changes** (same rigor as §4, CHANGELOG note, GDPR-neutral row-level fields): (1) the binary artifact fingerprint (§5), because binary bytes are gone by view-time; (2) the `compliance_coverage` enum (§6), because it records what actually ran at request time and cannot be trustworthily recomputed from config later. Neither touches the normalized sidecar, so the GDPR trajectory is unaffected.

### 3.2 The descriptor as a NormalizedPayload projection (contract, not a third structure)

The descriptor is **not** a new parallel structure that bypasses the hook-scannability contract. It maps into the existing `NormalizedPayload`:

- **Text leaves** (`prompt`, `negativePrompt`, `inputText`, `transcriptText`) become text `ContentBlock`s so `TextProjection()` covers them → the rule-pack / hook engine scans them **with no change to the hook layer**. This is what makes "hooks: no change" actually true.
- **Non-text metadata** (model, size, voice, output reference) rides a typed sidecar on the payload, not the text projection.

### 3.3 Descriptor core (view-time shape)

| Modality | Text leaves (scanned) | Non-text sidecar |
|---|---|---|
| **image** | `prompt`, `negativePrompt` | `model, size/aspectRatio, n, seed?, quality?, outputRef[]` |
| **tts** | `inputText` | `voice, model, format, outputRef` |
| **stt** | `transcriptText` (output) | `audioRef (input), model, language?, durationRef` |

`units` is **not** a descriptor field — it is a *derived* value and belongs to the cost layer (§4), keeping the descriptor pure observation.

### 3.4 `ext` governance — never a blind path (D7)

Provider-specific params that do not fit the core ride an `ext` sub-object **in the view-time projection** (populated by the extractor reading native wire top-level fields). This view-time `ext` is distinct from `canonicalext`, which operates on the canonical *request* body inside a codec. For the native-passthrough modalities no such codec runs, so the distinction is clean. The one exception is the image cross-shape codec (D9/P2.5): it *does* run `canonicalext` on the request path — but note the **caller-facing `nexus.ext.*` channel is allow-list-rejected on the image ingress (R-8, §7)**, so a caller cannot use it to inject wire fields; the codec's own internal use of `canonicalext.Get` for provider-required wire knobs (e.g. `responseModalities`) is code the gateway controls, not caller input. Binding rules:

1. **Any field carrying user-intent text** (prompt / negative / system in a provider-specific slot) MUST be promoted to a **core text-leaf** so it is scanned and redacted. It never hides in `ext`.
2. **Any field that is a billing dimension** (e.g. a provider whose price varies by sampler-steps or by resolution) MUST be promoted to **core** and fed to the cost formula.
3. `ext` therefore holds **only** non-text, non-billing, no-product-consumption provider knobs (sampler, cfg_scale, steps). It is preserved for forwarding + audit fidelity, not interpreted.
4. Fields whose wire expression diverges across providers but are cross-provider *semantic* (size vs width/height vs aspect-ratio) get a small **per-provider normalization step** in the extractor when promoted to core — we acknowledge the extractor is not free; it is thin but not zero.
5. **Rules 1–2 are enforced by a structural gate, not a documentation wish.** Each provider adapter ships an **extractor text-slot coverage assertion** (a conformance test asserting every user-text-bearing wire field is mapped to a core text-leaf). Native-shape passthrough is enabled for a provider **only** when its coverage assertion passes; an un-modeled provider shape either fails the gate or is registered in the §6 residual-risk register with the capability matrix honestly reflecting "prompt scanning limited to modeled fields." This upgrades D7's "MUST promote" from a written constraint to a mechanical delivery gate.

### 3.5 Binary payloads — reference only, and honest about which mode has a fingerprint

Binary output/input is never inspected or stored as bytes. Whether the gateway can compute an **artifact-body fingerprint** depends on whether the bytes flow through it — and the proposal must be honest about this (an overclaim here is exactly the false-assurance §6 exists to prevent):

- **Byte-bearing modes** — inline base64 (`response_format=b64_json`, OpenAI images' default), a binary response body, TTS audio, or opt-in proxy-and-store — the bytes pass through the gateway, so it computes `{ sha256, sizeBytes, mime }` via a streaming `io.TeeReader`. **Implementation constraint:** the tee-to-hash sink is placed **before** any audit truncation/spill, or a truncated body would hash to a fingerprint that is not the full artifact's.
- **URL-return mode** — the provider returns only a URL; the artifact bytes never traverse the gateway, and D4 forbids dereferencing it. Here there is **no artifact-body fingerprint** — only the URL reference and the response-envelope hash. The capability matrix (§6) states this per mode; we never claim an artifact fingerprint we do not have.
- **STT input audio** — the fingerprinted artifact is the *inbound* multipart audio, a different code path from the response tee: hash it inbound as it is received (`sha256`/size), keep the file part as a `<file len=N>` marker, and never buffer it into the text audit budget (§9, P3).

This reconciles with the in-tree `core.ArtifactRef` (which a codec may materialize with decoded bytes for its own use): that struct is a transient codec output; the audit fingerprint is computed on the streaming path and codecs must not materialize artifact bytes into the audit body. See §5 for custody.

---

## 4. Cost — a real migration, shipped with the route (D6)

The claim "cost already supports these" is false and must be fixed as follows, **in the same batch as any modality route**:

1. Re-add the deleted units to `BillableUnits` (`AudioSeconds`, `Images`, `VideoSeconds` as needed) — a versioned schema/registry change with a `CHANGELOG` migration note.
2. Register per-kind cost formulas keyed by `(endpoint_type, model)`: image → images × size/quality tier; tts → input characters; stt → audio seconds; (video → output seconds, later).
3. **Units derivation order:** prefer the provider response `usage` block (same pattern as embeddings usage fallback); only decode the audio container for duration when the response omits it; on an undecodable format, fall back to byte-count and WARN — never silently mis-attribute.
4. No modality route reaches production billing before its formula is wired. This closes the "silently priced as chat" gap.

---

## 5. Artifact custody (D4)

- **Byte-bearing responses:** gateway computes and persists the artifact-body fingerprint + size + mime on the `traffic_event` row (streaming, §3.5). Signed provider URLs expire, so the *fingerprint*, not the URL, is what makes the audit chain defensible.
- **URL-return responses:** the provider URL is stored **but never dereferenced server-side** (SSRF guard; egress allowlist if ever fetched). There is **no artifact-body fingerprint** in this mode — only the URL reference + envelope hash. This is stated plainly in the capability matrix; we do not pretend the audit chain is byte-verifiable when it is not.
- **Opt-in proxy-and-store** (deployment-level config, off by default): a deployment with a legal custody requirement can enable full artifact storage — accepting the bandwidth/storage cost for reproducible retrieval, and gaining a fingerprint even for otherwise-URL-return providers.
- **Gateway-served artifact URLs are an authenticated inbound surface, not just an SSRF-outbound concern (R-9).** The SSRF guard above governs the *outbound* fetch of a provider URL. When the gateway itself serves generated bytes to satisfy `response_format:"url"` (D9), it mints a gateway-hosted URL — a new *inbound* fetch endpoint. That URL MUST be authorized to the originating principal/VK (or be an unguessable, expiring, single-use capability token), served over the authenticated data plane, and audited as a read like any other data-plane access — never an unauthenticated or enumerable `GET`. Single-tenant removes cross-org isolation but not per-principal/per-VK confidentiality: a generated image is exactly the sensitive content routing-through-a-compliance-gateway exists to protect. The artifact-serving authz model is designed **once**, shared with the async webhook/artifact surface (§8).
- This gives a defensible audit chain for byte-bearing traffic without an infrastructure explosion, and resolves the §3.3-vs-§9 hash contradiction honestly: byte-bearing modes always have a streaming fingerprint; URL mode is explicitly scoped as reference-only.

---

## 6. Compliance — graceful but visible, with legal sign-off

- **Prompt-side is hard-block by default for generative image/video** (D5). It is the only control point; observe-only would mean zero prevention of illegal-content generation.
- **STT output** (`transcriptText`) is scanned like any text leaf; enforcement depth is scoped in P3 (§9) because rewriting non-JSON / streaming transcripts is real work, not free.
- **Coverage is visible, never silent.** Every traffic event carries a `compliance_coverage` signal (`prompt-only` / `output` / `none`), **stamped at request time** (it records what actually ran, and config can change later — a view-time recompute from current config would misreport history). It is a non-PII enum on `traffic_event` (row-level retention/DSAR, GDPR-neutral, versioned per §4). The UI renders a per-modality coverage badge; operators are never left assuming a modality is scanned when only its prompt is.
- **Residual-risk register (new section, owner/legal sign-off required):** binary output content review (CSAM/NSFW/voice-clone) is **not** performed; input audio is not inspectable; artifact bytes are referenced not scanned. Each is listed with its remediation path and a v1-accept flag. No generative binary modality ships without sign-off.
- **External capability matrix:** the doc states, per modality *and per artifact mode*, exactly what is covered — input-prompt scanned? output scanned? artifact-body fingerprinted (byte-bearing) vs URL-reference-only? — the single source of truth for sales copy, feature docs, and contract annexes. It never claims an artifact fingerprint for URL-return mode or output scanning for binary. We market "cost + audit visibility for image/audio", not "full-modality compliance".
- **Generation is governed by the declared endpoint (kind/path), not by inferred model capability.** A caller declares generative intent by hitting the gateway's generative image/video contract; the hard-block, coverage stamp, and per-modality cost attach to that declared kind. Capability-based governance was considered and rejected on its own merits: it over-blocks dual-modality models on pure-text requests (a Gemini flash model resolves for both text and image, decided per request by `responseModalities`, so gating on "this model can emit images" would hard-block ordinary text chat), and it would make a routing-metadata field (`outputModalities`, Prisma-defaulted to `["text"]`) compliance-load-bearing with a silent fail-open on any mis-tagged model. (Endpoint-based governance is also the observed norm among governance gateways — a dated competitive comparison lives in a separate analysis doc, not here, so this design doc does not carry point-in-time competitor claims that would age unguarded.)
- **Inline image in a general chat request = governed as chat, and disclosed as an owner-accepted residual (R-3), not a silent hole.** An image emitted inline from a chat/multimodal call (a dual-modality model with `responseModalities:["IMAGE"]` on the chat endpoint) is governed as chat: its prompt text is already scanned by the standard chat hooks, and its output is not content-scanned (same as all binary output). The *only* thing the generative hard-block adds over chat compliance is escalating an **observe-configured** prompt match to a hard block. That delta closes under **enforce** mode (the shared pipeline blocks on the chat path regardless of endpoint) — **but under observe mode it is a real, adversary-selectable evasion**: a VK holder can route generative-image intent through the chat endpoint to dodge the escalation, precisely for the observe-configured operator D5 exists to protect. This is not "moot"; it is an owner-accepted residual, carried as **R-3** with that framing, and stated in the external capability matrix (a caller who wants the generative hard-block calls the generative endpoint; operators who need it on every path run the rule in enforce mode).

---

## 7. Abuse controls (non-functional requirement)

Expensive modalities cost far more per call than chat, and this branch is doing open-proxy hardening — so:

- New routes **reuse the full VK auth + per-VK quota + rate-limit + kill-switch middleware chain** (verified at route registration, not assumed).
- Generative endpoints get **stricter per-VK rate + concurrency caps** and **request/artifact size caps**, shipped as **built-in sensible defaults** (adapter/runtime-filled), not admin-facing per-modality config knobs — less-is-more. A leaked VK against an un-throttled generative endpoint would be a billing-DoS / expensive open proxy — explicitly designed out.
- **The cross-shape image codec is allow-list-only (R-8).** The OpenAI-images canonical is a **closed field set**. The codec's job (§3a Rule 7) covers not just mapping the known params but rejecting everything else: any field outside the canonical schema — `tools`, `toolConfig`, `systemInstruction`, `safetySettings`, and the entire `nexus.ext.*` channel — is **rejected with 400, never forwarded** to `:generateContent`. This is a departure from the chat codec's warn-and-forward default (`canonicalext.ScanUnsupported` is observability-only), and it is deliberate: forwarding those fields would let a caller enable tool-augmented generation (a `googleSearch` web-fetch egress the gateway does not govern), relax provider `safetySettings` the compliance story leans on, or smuggle user-intent text into `systemInstruction` that the prompt scanner (which reads only the canonical `prompt` leaf) never sees — defeating the generative hard-block. `/adapter-conformance-check` asserts a body carrying `tools` / `nexus.ext.*` 400s rather than reaching the wire.
- **Fan-out is bounded and accounted.** OpenAI `n` (multi-image) is capped and validated **before** translation; quota decrement + cost stamping reflect the **realized** image count / upstream call count, not one decrement per ingress request; a mid-fan-out kill-switch / quota-exhaustion aborts the remaining upstream calls. Otherwise one quota-passing request could multiply upstream spend (billing-DoS).
- **The codec builds the wire body by structured marshaling, never string interpolation** — caller-supplied text is only ever a JSON string *value*, so a crafted prompt cannot escape JSON structure to inject sibling `generationConfig` / `parts` / `role` keys.

---

## 8. Async-job — an honest stateful subsystem (deferred)

Async modalities (video, non-OpenAI long tail) share one missing capability. It is **not** "two-hop extraction"; it is a new subsystem with three parts:

1. **Job-correlation store** (job-id ↔ submit-descriptor ↔ result-descriptor), with defined failure/expiry semantics — separate from `traffic_event`.
2. **Authenticated webhook inbound** — provider→gateway callbacks carry no VK, so this needs per-provider HMAC signature verification, a nonce/timestamp replay window, and job-id→originating-principal binding for attribution + quota. It is a new inbound surface and must not be an unauthenticated open endpoint (open-proxy hardening applies).
3. **Artifact proxy/store** (§5).

Normalization of an async job is still §3 view-time observation, spread across the submit and retrieve hops. Build this **once** as a cross-cutting capability, not per adapter.

---

## 9. Phasing — demand-gated, cost-first, STT last

- **P0 — Whether-gate (no build).** Is there a real prospect for image/audio governance? If not, stop; reserved enums stay dormant (zero cost). Also rank this program against the new-provider-adapter / spec-coverage backlog on the same (demand × reuse-leverage × eng-weeks) table — a net-new chat/embeddings adapter reuses the fully-amortized machine and usually wins.
- **P1 — Cost + audit visibility, all three sync modalities, no text compliance.** Re-add `BillableUnits` units + per-kind formulas (§4) + register routes + reuse existing audit `endpoint_type`. Delivers "stop the bleeding on image/TTS/STT spend" fast, decoupled from any compliance work. Highest ROI, highest certainty.
- **P2 — Prompt-side compliance for image *generations* + TTS.** These are small JSON bodies (genuinely cheap). Reuse the rule-pack engine on the prompt text leaf; generative = hard-block default (§6). (`images/edits`/`variations` are multipart — grouped with P3.)
- **P2.5 — Cross-shape image codec (D9), owner-directed, per-leg demand-gated.** A self-built OpenAI-images↔provider-wire codec so a caller uses one canonical image contract (OpenAI images shape) across providers. v1 legs: OpenAI passthrough + Gemini Nano Banana (`:generateContent` + `responseModalities:["IMAGE"]`); each additional non-OpenAI leg independently whether-gated, native passthrough the default. **Allow-list-only** (out-of-schema fields — `tools`/`systemInstruction`/`safetySettings`/`nexus.ext.*` — 400'd, R-8); strict-§3a param translation with **caller-visible coercion** (`X-Nexus-Coerced`, never a silent drop, per §3a Rule 7); response `inlineData`→`b64_json`; `size`→`aspectRatio` lossy (image carved out of the round-trip-identity standard, §11); `response_format:"url"` via the D4 artifact store (with R-9 access control) or an explicit degrade. Needs net-new `canonicalbridge` image methods (today: chat + embeddings only). Field-level mapping in the SDD; conformance via `/adapter-conformance-check`. The mapping is not greenfield (Google's OpenAI-compat layer + LiteLLM implement it), but the bridge is net-new. Reverses `provider-adapter-architecture.md` §1/§3 — updated in lockstep.
- **P3 — STT (the hard one, last not first).** multipart binary input handling (parse fields, reference audio, never buffer bytes into the text audit budget), the `response_format` output matrix (json/text/srt/vtt/verbose_json + streaming), and output-side redaction (reuse the settled streaming redact→buffer decision, not incremental). Scoped explicitly as heavy; STT was mis-cast as "cheapest" — it is the worst engineering fit.
- **P4 — Async-job + artifact subsystem (§8).** Design spike now (so a triggered build is 2–4 weeks, not a quarter of cold start); full build gated on a hard contract.
- **P5 — Video / non-OpenAI long tail** on hard demand, on top of P4. Includes cross-shape translation for the *remaining* modalities (audio, video) and full chat if ever needed. (Image cross-shape is no longer here — it moved to P2.5 per D9.)

Each phase is independently shippable and has an explicit continue/park gate: if a shipped modality sees **no real (non-smoke) VK traffic within 6 weeks** of enablement, freeze the next phase and mark the modality dormant (route 404) rather than auto-advancing.

### Definition of done (binding, every phase)

A phase is not "done" on green tests alone. Every phase — including each sub-phase of P1 — closes only after a **multi-perspective adversarial review**: independent architect / security / performance reviewers (plus product / business when the phase changes a user surface or scope), run over the phase diff in **two rounds** — round 1 finds independently, round 2 converges over the pooled findings, all grounded in the real code. Every CONFIRMED finding is fixed (or explicitly accepted by the owner with a reason) before the phase is declared done. This sits after the standard verify gate (tests ≥95%, ai-gateway smoke where the blast radius touches `packages/ai-gateway/**` / `traffic_event` / cost, 4-question×2-round self-audit) and before the commit prompt. The review verdict is recorded (a Chinese artifact is the usual form).

---

## 10. Cross-service boundary

Compliance Proxy and Agent share the same `normalize` registry and `ClassifyPath`. Per-modality extraction is **AI-Gateway-only in v1**. Extending it into shared `normalize` is deferred and gated: it must be proven bounded and non-blocking on the Agent fail-open outbound path (NE five-rules review) before any multipart/large-binary handling runs there — a hang/OOM there takes down the host's network.

---

## 11. Non-goals (v1)

- A universal canonical body shape across modalities. (Per-modality canonical shapes only — image's canonical is the OpenAI images shape, D9; there is no cross-modality universal body.)
- Lossless canonical↔wire re-serialization for **audio/video**. (Image now has a self-built canonical↔wire codec, D9/P2.5 — but it is a purpose-built image translation, not a universal lossless serializer.)
- Cross-shape ingress translation for **audio, video, and full chat** (deferred to P5). Image cross-shape is IN scope (D9/P2.5): a self-built OpenAI-images↔`:generateContent` codec, canonical = OpenAI images. For audio/video/chat the `canonicalext` + `CanonicalBridge` seam remains a structural mount point; for image it is being built out (net-new `IngressImagesToCanonical` / `ImagesWireShapeForTarget` / image response reshaper — `canonicalbridge` today carries only chat + embeddings).
- **Lossless round-trip identity for image.** Image is carved out of the §3a round-trip-identity standard: OpenAI `size` (pixel dims) cannot round-trip through Gemini `aspectRatio`. The codec is correct-by-explicit-mapping (each param mapped / coerced-with-`X-Nexus-Coerced` / 400), not by the double-round-trip test that governs chat/embeddings adapters.
- Scanning/redacting binary output (images, audio, video) — declared in the residual-risk register (§6), needs a separate vision/audio pipeline.
- Response caching for generative image/video (they never touch the cache path, like the embeddings short-circuit). STT cache default OFF (O(bytes) key + near-zero hit + cached-transcript PII-at-rest). No per-modality cache knob is added.

---

## 12. Summary

The full-modality gateway is buildable and mostly additive, but only after the review's corrections: **descriptor assembly is view-time; compliance (scan + redact + prompt hard-block) runs at request time; the only write-path additions are two non-PII, GDPR-neutral `traffic_event` stamps (artifact fingerprint + compliance-coverage) — the normalized sidecar is never resurrected; cost is a real migration shipped with each route; `ext` is never a compliance/cost blind path; STT is the hard modality done last, not the cheap first; generative prompt-side is hard-block; artifacts carry a streaming-computed integrity hash without server-side dereference; compliance coverage is visible and legally signed off; async is an honest stateful subsystem.** Generation is governed by the declared endpoint, not inferred capability (inline-image-in-chat is governed as chat, a disclosed boundary). Image gets one self-built cross-shape codec (D9/P2.5, canonical = OpenAI images) so it reaches providers whose image model has no dedicated endpoint (Google's Nano Banana); audio/video/full-chat cross-shape stay deferred. The program is demand-gated at P0 (whether, not just when) and ships cost+audit visibility first — the highest-ROI slice that needs no text compliance at all.

# Proposal (design spike): Realtime voice gateway — WebSocket bidirectional proxy

> **Status:** Design **spike**, not a build-ready architecture. Per e88 FR-7 and
> proposal §9-P4 ("Design spike now so a triggered build is 2–4 weeks, not a
> quarter of cold start; full build gated on a hard contract"), this document
> maps the transport, the structural gaps in the current gateway, the compliance
> reality of a bidirectional audio stream, and a phased path — so that when UTA's
> requirement hardens into a contract, implementation starts from a decided shape
> rather than a blank page. It deliberately stops short of field-level wire specs
> and does not authorize code. **Owner-gated** (todo #9).
>
> **Confirmed demand:** UTA product — realtime voice conversation, using the
> Realtime API rather than REST TTS/STT. The **server-side WebSocket** transport
> is the relay this spike designs for; whether UTA also has **browser clients**
> (which would pull in the ephemeral-token / WebRTC touchpoints §6 defers) is an
> open P0 question — the "server-WS-only" shape below assumes the server-side
> answer and §4-P0 re-opens it. This is the one modality in the program with a
> named internal product pull.

---

## 0. Why this is its own track (not P3 REST audio, not P4 async)

Realtime voice is a **third transport face** the gateway does not have. Today the
AI Gateway serves sync HTTP + SSE only (`cmd/ai-gateway/wiring/routes.go` mounts only POST/GET
handlers; no upgrade handler in the data plane). REST TTS/STT (P3) is
request/response; async jobs (P4) are submit-then-webhook. Realtime is a
**single long-lived bidirectional connection** carrying interleaved control
events and audio frames for up to 60 minutes. Its cross-cutting concerns —
auth, compliance, audit, cost, kill-switch — all assume a request/response
lifecycle and must be re-derived for a connection lifecycle. That re-derivation
is the substance of this spike.

The async-job subsystem (§8 of the multimodal proposal) is the *nearest* prior
thinking ("long-connection/stateful subsystem"), but it is REST-shaped
(job-correlation store + authenticated webhook inbound + artifact proxy). The
realtime track shares its "stateful, not request-scoped" nature but none of its
mechanics. This is a distinct subsystem.

---

## 1. The transport (established facts)

**Sources:** every fact in this section is cited to an official page in the
companion [realtime-voice-protocol-facts.md](./realtime-voice-protocol-facts.md)
(a research appendix with per-claim URLs, **facts as of 2026-07-15 — re-verify at
the P0 gate**, since the Realtime API is under active GA iteration).

OpenAI Realtime (GA 2026, `gpt-realtime-2.x` family) offers three transports:
**WebRTC** (browser media), **WebSocket** (server-to-server), **SIP**
(telephony). **A gateway relays the WebSocket transport** — `wss://
api.openai.com/v1/realtime?model=<model>`, JSON text frames both directions, no
binary frames. WebRTC is SDP/SRTP media and is not a WS relay (a gateway could
proxy the plain-HTTPS `POST /v1/realtime/client_secrets` ephemeral-token mint and
the `POST /v1/realtime/calls` SDP exchange without WS machinery, and observe a
WebRTC/SIP call via the sideband `?call_id=` server-control WS — but the primary
deliverable is the server-to-server WS relay).

Load-bearing facts for the design:

- **Auth is header-based** (`Authorization: Bearer <key>`) for server WS, with a
  subprotocol-carried variant (`Sec-WebSocket-Protocol: openai-insecure-api-key.
  <key>`) for browser clients. A relay strips the client's credential and injects
  the provider key server-side — exactly the pattern the gateway already runs for
  REST, and exactly what the Hub WS server already demonstrates
  (`nexus.bearer` subprotocol + header bearer, `ws/server.go`).
- **Everything is inspectable JSON.** User-intent text rides in named events:
  `session.update.instructions` (system prompt), `conversation.item.create`
  (user text / `function_call_output`), `response.create.instructions`, and —
  server→client — `conversation.item.input_audio_transcription.delta/completed`
  (user speech transcript, **only if input transcription is enabled in session
  config**), `response.output_audio_transcript.delta`,
  `response.output_text.delta`, and `function_call` items in `response.done`.
- **Audio rides as base64** inside `input_audio_buffer.append` (client→server,
  ≤15 MB/append) and `response.output_audio.delta` (server→client); PCM16 mono
  24 kHz typical, ~6.4 KB base64 per 100 ms while talking.
- **Metering is fully in-band.** `response.done.usage` gives per-response
  text/audio/image/cached token splits; **context is re-billed each turn**, so
  per-session cost = Σ per-response usage, not the last response. Per-minute
  models (`gpt-realtime-translate`/`-whisper`) need wall-clock/audio-duration
  metering.
- **Sessions ≤ 60 min**, single WS; no documented ping/pong contract; `error`
  and `rate_limits.updated` arrive in-stream.

**Competitive position (established):** LiteLLM, Portkey, Cloudflare AI Gateway,
and Bifrost all proxy realtime WS as **auth-injection + bidirectional pipe with
selective event inspection for metering** — and **none documents deep
compliance/redaction on the realtime audio stream**. This is the same
governance gap the image work exploited (Portkey runs zero image compliance):
**a gateway that scans realtime transcripts and enforces the generative
hard-block on voice is ahead of the field.** Honest scope: audio-only user
intent (transcription disabled) is not text-scannable — the transcript is the
first scannable form (mirrors R-7).

---

## 2. Structural gaps in the current gateway (the real work)

Each row is a concrete collision between the connection lifecycle and a
request-scoped assumption, with the in-repo seam that mitigates it.

| # | Gap | Evidence | Seam / mitigation |
|---|---|---|---|
| G1 | **No WebSocket transport face.** The data plane is POST/GET only; `provcore.Transport` is HTTP-only (`Do(*http.Request)→*http.Response`; `AdapterSpec.Valid()` requires four HTTP components). | `wiring/routes.go`, `providers/core/spec.go` | `coder/websocket v1.8.15` is **already in the module graph** (direct dep of `shared`/thingclient, transitive in ai-gateway). Server accept (`ws/server.go`: origin allowlist, subprotocol auth, ping/liveness, read cap) and client dial+reconnect (`thingclient/client.go`) are production precedent. A realtime "adapter" does NOT fit `AdapterSpec`; it needs a parallel dial path keyed off `CallTarget` (BaseURL+APIKey+ProviderModelID+Extras — transport-neutral, directly reusable). |
| G2 | **Audit Record assumes one exchange.** One row enqueued at handler return; single StatusCode/TTFB/latency; pooled body capture reclaimed at marshal with a 256 MiB hard cap. A 30-min audio session = one giant row at close, or nothing on crash. | `stage_accounting.go`, `platform/audit/record.go` | Session-shaped audit needs a **session row (start + periodic checkpoint + finalize)**, not the per-exchange Record. **The load-bearing G2 decision is the checkpoint-row cost semantics** (correction from review): traffic_event rows are the unit of cost/usage aggregation in the shipped analytics rollups, so a periodic checkpoint row must either carry **delta** cost (changes what a row means to every aggregator — a 1.0-GA analytics-contract impact) or be **zero-until-finalize** (which resurrects the crash-loss the session row exists to prevent). That fork must be decided at build. The Hub's thing-connection model is cited only as an **analogy** for "durable connection with periodic touches" — it is an in-memory liveness pool that persists no audit rows, not a reusable audit substrate. Audio frames must NEVER enter the body-capture pool (STT R-7 posture: reference/duration only). |
| G3 | **No `realtime` EndpointKind, AND the cost schema cannot express realtime pricing.** Adding the kind is a declared coordinated change (hook gating, routing matcher, cost formula, traffic_event column, Prometheus labels); without a registered formula it silently prices as chat after one WARN. **But `RegisterFormula` alone is NOT sufficient** (correction from review): a realtime response bills text-in / audio-in / cached / text-out / audio-out **simultaneously at different rates**, and today `BillableUnits` has no audio-token fields (`{PromptTokens, CompletionTokens, Images, AudioSeconds, InputChars}`) while `ModelPrices` carries exactly 4 rates (in/out/cache-read/cache-write). The existing modality formulas work via a one-unit-type-per-model-row reinterpretation trick that a multi-rate realtime row breaks. | `typology/endpointkind.go`, `estimator/cost_formula_registry.go`, `platform/metrics/metrics.go` | Real work: additive enum + **a pricing-schema widening** (`BillableUnits` audio-token fields + `ModelPrices` audio rates + the model pricing catalog + `sync-provider-pricing` + the `Cost` breakdown consumers) THEN `RegisterFormula("realtime")` keyed on `response.done.usage` token splits summed per session; `AudioSeconds` (P1) is the per-minute-model fallback. This is a coordinated, shipped-contract-adjacent change (overlaps owner-gated pricing item #11), not a drop-in formula. traffic_event `endpoint_type` is `String @default("")` — DB-additive. |
| G4 | **Hook pipeline is per-request, single-execute, text-modality.** `BuildPipeline` per request, `Execute` once, decision→HTTP status/body rewrite, 15s total timeout. No request-direction repeated scanning; audio frames have no scanner. | `stage_hooks.go`, `generative_block.go` | The only repeated-execution machinery is the response-side **LivePipeline/modela checkpoint** model (`shared/transport/streaming/`). Realtime compliance = **per-event checkpoint scanning** of the text-bearing events (§3), reusing that engine's repeated-execute shape but driven by event type rather than byte offset. The generative hard-block (`maybeBlockGenerativePrompt`) is endpoint-kind-gated — a realtime kind would need explicit inclusion (and R-3's endpoint-vs-capability governance question resurfaces: is a voice session "generative"?). |
| G5 | **No per-VK concurrency limiter; no live-connection termination.** Only the global in-flight gate (which a WS holds for its whole life — the `admission_gate.go` comment anticipates the same slot-for-connection-lifetime pattern for SSE) and per-VK RPM; quota reconciles once per exchange; config/kill-switch/VK snapshots are read at request start and nothing cancels in-flight work (ai-gateway deliberately boot-seeds streaming policy because live changes "would be lossy"). | `admission_gate.go`, `policy/quota`, `passthrough/doc.go`, sse-streaming-compliance doc | Realtime needs (a) a **per-VK session-count cap** (expensive: $32–64/1M audio tokens — a leaked VK against unthrottled realtime is a severe billing-DoS), (b) **mid-session quota reconcile** (periodic, on each `response.done.usage`), (c) a **live-session registry** so the kill-switch can sever active sessions — the Hub `ws.Pool` + broadcast/close-all is the in-repo pattern. **A 60-min session pins ALL upgrade-time policy** (not just the kill-switch): VK validity (a revoked/disabled VK otherwise keeps a live $/min session for up to an hour), hook config, passthrough flags, routing. The live registry must re-evaluate at least **VK revocation** on a schedule, or the spike signs "session keeps upgrade-time policy for its lifetime" as a residual (RT-9). This overlaps the owner-gated generative-caps work (#10, e88 NFR-4). |
| G6 | **Re-dial ≠ retry.** Executor retry/failover is per-attempt around one HTTP call; a dropped realtime session cannot be transparently re-dialed mid-conversation (server-side conversation state is lost; even OpenAI's own WebRTC/SIP resumption is provider-mediated). | `execution/executor`, `thingclient` reconnect (config-snapshot replay, not session replay) | Realtime failover is **connection-establishment-time only** (pick a target when opening); once a session is live, an upstream drop propagates to the client as a terminal `error` event + close. No mid-session provider failover in v1. |

---

## 3. Compliance model (the differentiator, scoped honestly)

The gateway's edge is **transcript-level compliance on a voice stream** — which
no competitor documents. The model:

- **Client→provider text-bearing events are scanned BLOCKING store-and-forward,
  not tee-while-forward.** This is a correctness precondition, not a field
  detail: the response-side LivePipeline/modela engine tees a copy of bytes *as
  they flow*, but on the client→provider path a frame forwarded before its scan
  reaches a decision is already in the provider's conversation state — dropping
  it at the relay then does nothing (the intent is ingested). So each complete
  client→provider event (`session.update.instructions` /
  `conversation.item.create` / `response.create.instructions`) is **buffered,
  scanned to a terminal decision, then forwarded or rejected** — a blocking gate.
  The provider→client direction (input-transcription events, output transcript/
  text deltas, `function_call` items) may tee/redact-in-flight like the SSE path.
  A blocking match on a client→provider event **rejects that event** (emit an
  `error` event to the client, drop the frame — there is no HTTP status on a live
  WS); a redact match rewrites the text field before forwarding.
- **Scanning is session-accumulating, not purely per-event.** A per-event scanner
  sees each `conversation.item.create` in isolation, but the provider
  concatenates successive items into one context — so a prohibited prompt split
  across two benign-looking items reconstitutes provider-side (cross-event
  fragmentation, the same evasion class the program knows from streaming
  incremental redaction). The compliance model scans against a per-session
  accumulating text buffer, not just the current event. Residual RT-6 carries the
  case where accumulation is imperfect.
- **Enforcement is DIRECTIONAL, and the two directions have different honest
  ceilings** (this corrects the naive "redact rewrites the text both ways" that
  the SSE redact→buffer pivot already rejected for streaming):
  - **Client→provider (real pre-forward blocking possible):** the blocking
    store-and-forward gate above stops the intent before the provider ingests
    it. Redact = rewrite the text field before forwarding; block = reject the
    event. This is the strong direction and the real differentiator.
  - **Server→client (NO in-flight redaction of what the user hears):** the
    LivePipeline engine is **observe-only by design** (`streaming/live.go`: it
    never holds back, blocks, or rewrites the wire), and the enforcing engine
    escalates to **buffer-to-end redaction** (`streaming/modela/engine.go`) —
    which on a live voice response means stalling the conversation for seconds,
    infeasible for realtime. Worse, **audio and transcript are parallel
    streams**: the model's speech (`response.output_audio.delta`) reaches the
    user's ears independently of the transcript, and §3 forbids scanning/holding
    audio — so rewriting the transcript does NOT stop the user *hearing* the
    content. The only realtime-compatible server→client enforcement is
    **detect → cancel/terminate the response** (`response.cancel` /
    session-terminate) with a **bounded already-heard leak** (the audio played
    between the offending token and the cancel). Transcript redaction is
    therefore **record/downstream-display hygiene only**, never a guarantee the
    user did not hear it. RT-8 signs this leak.
- **The generative hard-block extends to realtime** (subject to R-3's
  endpoint-vs-capability question): an illegal-content match on a
  `conversation.item.create` text or a transcribed prompt escalates to a
  client→provider block (before ingest) or a server→client cancel-response (with
  the RT-8 bounded leak) for operators running the illegal-content rule. Owner +
  legal sign-off required (new residual-risk register rows — see §5).
- **Audio frames are never scanned or buffered into the audit budget** (R-7): the
  transcript is the first scannable form. When input transcription is **disabled**
  in session config, user intent exists only as audio and is NOT scannable — the
  capability matrix must state this per session, exactly as the image capability
  matrix states "output binary not scanned." An operator who requires prompt
  scanning could have the gateway force `input_audio_transcription` on in the
  relayed `session.update` — but this is **not a free policy bit**: provider-side
  input transcription is separately billed and adds latency, and the client can
  attempt to turn it back off in a later `session.update` (the relay must pin it,
  re-asserting on every client `session.update` — a build-phase control). Its
  cost/latency is a P0 input to the observe-vs-enforce decision, not decided
  blind.
- **Coverage honesty:** `compliance_coverage` gains realtime-appropriate values
  (e.g. `transcript` / `prompt-only` / `none`) so the capability matrix never
  claims audio-content scanning. Voice-clone / speaker-identity abuse is invisible
  at the audio layer (mirrors R-2 for TTS) — a signed residual.
- **Transcript PII-at-rest.** Transcripts (user speech-to-text + output
  transcripts) are text-bearing PII derived from voice; any transcript persisted
  on the session audit row inherits `storageAction` redaction-at-rest exactly
  like every other stored body (the program's redaction-storage-coverage
  binding), separately from the in-flight redact-before-forward above.
- **Credential hygiene on the dial.** The relay unconditionally reconstructs the
  upstream `Authorization` (or subprotocol) from the injected provider key and
  **never echoes a client-supplied `Sec-WebSocket-Protocol:
  openai-insecure-api-key.*` onto the upstream dial** — the client credential is
  stripped, not forwarded. Upstream `error` events forwarded to the client: keys
  are provider-masked (no key-exfil vector), but the forward-verbatim-vs-normalize
  choice is an explicit build decision (verbatim leaks provider-internal request
  IDs / rate-limit internals; the REST path forwards same-family errors verbatim
  today — `error_envelope.go`).

---

## 4. Phasing (spike → gated build)

- **P-spike (this doc):** transport decided (server WS relay), gaps mapped, seams
  identified, compliance model scoped, cost model identified. **No code.**
- **P0 — hard-contract gate (owner + UTA):** confirm the concrete requirement —
  which models, expected concurrent sessions, whether browser clients (ephemeral
  tokens / subprotocol auth) or server-only, whether transcription is on (drives
  whether any compliance is possible), and the compliance posture UTA needs
  (observe vs enforce). Rank against the rest of the backlog. No build before
  this.
- **P1 — relay skeleton, shipped DARK.** WS upgrade handler on a new route; VK
  auth on upgrade (header + subprotocol); `CallTarget`-keyed upstream dial with
  provider-key injection; bidirectional frame pump with **lossless** backpressure
  (the Hub conn's drop-on-full 64-slot buffer is control-plane-grade, NOT
  media-grade — a realtime relay needs a bounded-but-lossless or fail-the-session
  buffer with a **per-session byte budget** as the resource bound: the Hub conn
  caps inbound frames at 1 MiB but the protocol allows ≤15 MB
  `input_audio_buffer.append`, and the admission gate bounds session *count* not
  per-session memory, so P1 must size a per-session buffer budget explicitly);
  `realtime` EndpointKind, and the **`none` `compliance_coverage` stamp lands in
  P1's audit skeleton** (coverage honesty applies to every relayed row from the
  first, even before P3's scanning adds `transcript`/`prompt-only`). **P1 is NOT a payable feature and does NOT
  claim "independently shippable" as a live surface** — the program norm is "a
  route never lands before its cost formula" (e88 FR-2) and expensive endpoints
  carry built-in caps (NFR-4), so P1 ships with two hard, code-enforced gates
  from day one: (a) the `realtime` model is reachable **only** via a VK's
  `AllowedModels` entitlement enforced at upgrade/dial (existing
  `vkauth.VKMeta.AllowedModels` mechanism — access is a code gate, not an
  operational promise), so the route is dark to every VK not explicitly
  entitled; and (b) minimum viable metering — `response.done.usage` summation
  into a session-cost + `RegisterFormula("realtime")` — plus a **hardcoded
  built-in per-VK concurrent-session cap** (NFR-4 style, e.g. 2) — land IN P1,
  not P2, so no session is ever unmetered or unbounded. Session-row audit
  skeleton (audio never in the capture pool). Owner-gated smoke against a real
  Realtime session, on the owner's entitled smoke VK only.
- **P2 — full caps + live control:** mid-session quota reconcile (periodic, on
  each `response.done.usage`), live-session registry + kill-switch-severs,
  richer per-VK/per-provider limits, and **VK-revocation re-evaluation** on live
  sessions (G5). P1's built-in cap is the floor; P2 makes it administrable and
  adds live termination. (Metering itself is already in P1 — the billing-DoS
  floor closes in P1, P2 hardens it.)
- **P3 — transcript compliance:** per-event checkpoint scanning (both
  directions), redact-event / reject-event / terminate-session enforcement, the
  generative hard-block extension, coverage stamping, capability matrix, residual
  register sign-off.
- **P4 — Azure / other providers:** Azure Realtime is protocol-compatible but
  differs in URL shape (`/openai/v1/realtime?model=<deployment>`), auth (`api-key`
  header/query, Entra Bearer), and extra endpoints — a per-provider dial variant,
  not a new codec. Gemini Live is a **different protocol** (BidiGenerateContent) —
  cross-shape realtime translation, deferred like audio/video cross-shape (this is
  the realtime analogue of the image D9 codec, and would be a much larger build).

Each phase is a shippable increment **behind the P1 dark-launch gates** (never a
live payable surface before its cost formula + caps, which is why metering and
the built-in cap sit in P1, not P2); continue/park gate per the program norm (no
real VK traffic within 6 weeks → freeze next).

---

## 5. Residual-risk register additions (owner + legal, at build time)

New rows the build phase must carry (not accepted now — flagged so the spike is
honest about what a realtime build signs up for):

- **RT-1** — audio-content is not scanned (voice-clone / impersonation / illegal
  speech invisible at the audio layer); transcript is the first scannable form,
  and only when transcription is enabled. Remediation: enforce transcription-on
  policy for compliance-required VKs; audio moderation is a separate pipeline.
- **RT-2** — transcription-disabled sessions carry **zero** text compliance
  coverage (user intent is audio-only). Must be stated per session in the
  capability matrix; an operator requiring coverage runs the transcription-on
  policy.
- **RT-3** — mid-session upstream drop terminates the session (no transparent
  re-dial; G6). Availability trade-off, not a compliance gap.
- **RT-4** — a leaked VK against unthrottled realtime is a severe billing-DoS
  ($32–64/1M audio tokens, 60-min sessions). **Access is a P1 code gate**: the
  realtime model is reachable only via a VK's `AllowedModels` entitlement enforced
  at dial (existing `vkauth.VKMeta.AllowedModels`), and P1 also carries in-band
  metering + a built-in per-VK concurrent-session cap (§4). P2 makes the cap
  administrable and adds mid-session reconcile + live termination. Not an
  operational promise — a code gate from P1.
- **RT-5** — the generative hard-block on voice inherits R-3's
  endpoint-vs-capability ambiguity (is a voice session "generative"?). Governance
  key TBD at build; endpoint-declared is the program's chosen model.
- **RT-6** — **cross-event fragmentation evasion**: a prohibited prompt split
  across multiple `conversation.item.create` events each passes an isolated scan
  but reconstitutes provider-side. Mitigated by session-accumulating scanning
  (§3), residual where accumulation is imperfect (same class as streaming
  incremental-redaction evasion). Owner-signed residual.
- **RT-7** — **ephemeral-token direct-connect bypass**: an ephemeral client
  secret minted via `POST /v1/realtime/client_secrets` lets a browser open a
  WebRTC/WS session **straight to the provider**, bypassing the gateway entirely
  (zero compliance, zero metering, no kill-switch reach) while the gateway's
  provider key pays the bill. Proxying or minting that token is therefore a
  governance-and-billing hole, NOT a neutral convenience — **client_secrets
  minting/proxying is out of scope for this subsystem** (§6); a browser-client
  answer at P0 must re-open how ephemeral tokens are governed, not simply proxy
  them.
- **RT-8** — **server→client bounded already-heard leak**: output enforcement is
  detect→cancel-response, not in-flight redaction (audio reaches the user's ears
  on a stream parallel to the scanned transcript, §3). The audio played between
  the offending token and the cancel is heard. Redaction protects the record and
  downstream display, not the live listener. Owner + legal signed residual — the
  honest ceiling of voice output compliance.
- **RT-9** — **upgrade-time policy pinning**: absent live re-evaluation, a 60-min
  session keeps its upgrade-time VK validity / hook config / passthrough flags /
  routing for its whole life (a revoked VK keeps a paid session up to an hour).
  P2 adds VK-revocation re-evaluation; until then, or for the non-VK policy
  dimensions, this is a signed residual (§G5).

---

## 6. Non-goals (spike)

- WebRTC/SIP media relay, **and minting/proxying ephemeral client secrets**
  (`POST /v1/realtime/client_secrets`) — the latter is out of scope not because
  it is hard but because a gateway-minted ephemeral token creates a direct
  client→provider path that bypasses all gateway governance and billing controls
  (RT-7). Only the server-side WS relay is in scope. A browser-client requirement
  at P0 re-opens how (or whether) ephemeral tokens are governed.
- Cross-provider realtime translation (Gemini Live's different protocol) — the
  realtime analogue of the image cross-shape codec, deferred.
- Field-level wire specs, event-by-event mapping tables — those belong to the
  P1/P3 SDD once the P0 contract gate passes.
- Any code. This is a spike.

---

## 7. Summary

Realtime voice is a confirmed-demand (UTA), confirmed-transport (server-side
WebSocket) third transport face. The transport is fully inspectable JSON with
header-based auth a relay can inject, in-band token metering, and named
text-bearing events that make **transcript-level compliance — a documented
industry gap — achievable**. The real work is not the wire protocol (well
documented) but re-deriving six request-scoped assumptions for a connection
lifecycle: the HTTP-only Transport interface, the single-exchange audit Record,
the missing `realtime` EndpointKind, the per-request single-execute hook
pipeline, the absent per-VK concurrency/live-termination machinery, and
retry-vs-re-dial. Each has an in-repo seam (`coder/websocket` already present,
the Hub WS server as accept/auth/ping precedent, `CallTarget` transport-neutral,
the LivePipeline checkpoint engine, the Hub `ws.Pool` live registry). The path is
a hard-contract gate (P0, owner + UTA) then a four-phase build (relay → metering
+ caps → transcript compliance → multi-provider). This spike decides the shape;
it does not authorize the build.

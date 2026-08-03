# Realtime voice — protocol fact sheet (research appendix)

> Companion to [realtime-voice-gateway.md](./realtime-voice-gateway.md). The
> facts the spike's §1 relies on, each with an official source and a retrieval
> date. **Facts as of 2026-07-15 — re-verify at the P0 hard-contract gate**
> (the OpenAI Realtime API is under active GA iteration; model names and event
> names moved between beta and GA and may move again). Uncertainty is flagged
> inline. This file exists so a build triggered months later starts from a
> citable snapshot, not a re-research from zero.

## Transport & handshake

- Three official transports: **WebRTC** (browser/mobile media), **WebSocket**
  (server-to-server), **SIP** (telephony). A gateway relays the **WebSocket**
  transport (JSON text frames both ways); WebRTC is SDP/SRTP media, not a WS
  relay. [OpenAI Realtime guide](https://developers.openai.com/api/docs/guides/realtime); Azure latency framing [learn.microsoft.com Azure realtime-audio-websockets](https://learn.microsoft.com/en-us/azure/foundry/openai/how-to/realtime-audio-websockets)
- WS URL: `wss://api.openai.com/v1/realtime?model=<model>`. [OpenAI WebSocket guide](https://developers.openai.com/api/docs/guides/realtime-websocket)
- **Server auth:** `Authorization: Bearer <OPENAI_API_KEY>` header. **Browser
  auth** (no custom headers): via `Sec-WebSocket-Protocol` subprotocols —
  `openai-insecure-api-key.<key>` (deliberately named "insecure"), plus optional
  `openai-organization.<org>` / `openai-project.<proj>`. Optional
  `OpenAI-Safety-Identifier: <hashed-user-id>`. **UNCERTAIN:** exact subprotocol
  string for ephemeral (`ek_`) tokens over browser WS not directly confirmed —
  verify at build. [OpenAI WebSocket guide]
- **Ephemeral token flow:** `POST /v1/realtime/client_secrets` (standard API key,
  server-side) → `value` starting `ek_...` + session object; `expires_after`
  anchor `created_at`, `seconds` 10–7200, default 600. Older beta `POST
  /v1/realtime/sessions` being retired (2026-06 community report of "Invalid URL"
  on eu.api.openai.com — MEDIUM confidence on full removal).
  [client_secrets reference](https://developers.openai.com/api/docs/api-reference/realtime-sessions/create-realtime-client-secret)
- **WebRTC flow:** browser gets `ek_` → SDP offer to `POST /v1/realtime/calls` →
  answer SDP; events over a data channel named `oai-events`. [WebRTC guide](https://developers.openai.com/api/docs/guides/realtime-webrtc)
- **Sideband server control:** a server-held WS can attach to an existing
  WebRTC/SIP call: `wss://api.openai.com/v1/realtime?call_id={callId}` (call id
  from the `Location: /v1/realtime/calls/rtc_...` header on the WebRTC POST). SIP
  emits a `realtime.call.incoming` webhook. OpenAI recommends this so tools/
  business logic stay server-side. [Server controls guide](https://developers.openai.com/api/docs/guides/realtime-server-controls)
- **Models (GA 2026):** `gpt-realtime-2.1`, `-2.1-mini`, `gpt-realtime-translate`,
  `gpt-realtime-whisper`. Azure lineage: `gpt-4o-realtime-preview` (2024-12) →
  `gpt-realtime` (2025-08) → `-1.5`/`-2` (2026). [Realtime guide; Pricing; Azure doc]

## Wire protocol

JSON events as WS text frames, same ordering both directions. No binary frames
on the OpenAI WS transport.

- **Client→server:** `session.update`, `input_audio_buffer.append|commit|clear`,
  `conversation.item.create|delete|truncate|retrieve`, `response.create|cancel`.
  [Client events reference](https://developers.openai.com/api/docs/api-reference/realtime-client-events)
- **Server→client:** `session.created|updated`, `conversation.item.added|created|
  done|deleted`, `conversation.item.input_audio_transcription.delta|completed|
  failed|segment`, `input_audio_buffer.speech_started|speech_stopped|committed|
  timeout_triggered`, `response.created`, `response.output_audio.delta`,
  `response.output_audio_transcript.delta`, `response.output_text.delta`,
  `response.done`, `rate_limits.updated`, `error`. (GA renamed beta's
  `response.audio.delta`/`response.text.delta` → `response.output_audio.delta`/
  `response.output_text.delta`.) [Server events reference](https://developers.openai.com/api/docs/api-reference/realtime-server-events)
- **User-intent TEXT rides in:** `session.update.instructions` (system prompt;
  also `prompt.id` server-stored refs), `conversation.item.create` (user text /
  `function_call_output`), `response.create.instructions`; server→client
  `conversation.item.input_audio_transcription.delta|completed` (**user speech
  transcript — only if input transcription enabled in session config**),
  `response.output_audio_transcript.delta`, `response.output_text.delta`, and
  `function_call` items in `response.done`. [Conversations guide](https://developers.openai.com/api/docs/guides/realtime-conversations)
- **AUDIO rides as base64:** `input_audio_buffer.append.audio` (client→server,
  **≤15 MB per append**), `response.output_audio.delta` (server→client). Formats
  (GA): input `audio/pcm` (PCM16 mono 24 kHz), output `audio/pcm` and
  `audio/pcmu` (G.711 µ-law). [Conversations guide]
- **Tool calls:** model proposes `function_call` items (name, `arguments` JSON,
  `call_id`) in `response.done`; client replies via `conversation.item.create`
  with `function_call_output` + matching `call_id`. [Conversations guide]
- **Turn detection:** `turn_detection` in session config, `semantic_vad`
  default-on, disable-able (`null`). [Conversations guide]

## Size / rate

- Audio is base64 in JSON text events (~33% inflation). ~6.4 KB base64 per 100 ms
  at 24 kHz/16-bit mono (Azure sample cadence, 10 events/s while speaking).
- **Session max 60 min.** `idle_timeout_ms` under `turn_detection` →
  `input_audio_buffer.timeout_triggered` on fire.
- **Keepalive/ping: NOT documented** — no official ping/pong contract; treat as
  unspecified. Concurrency: no public OpenAI concurrent-session cap found; tiered
  TPM/RPM surfaced in-stream via `rate_limits.updated`.
  [Conversations guide; OpenAI dev blog](https://developers.openai.com/blog/realtime-api)

## Billing

Per 1M tokens [OpenAI pricing](https://developers.openai.com/api/docs/pricing):

| Model | Audio in / cached / out | Text in / cached / out |
|---|---|---|
| gpt-realtime-2.1 | $32.00 / $0.40 / $64.00 | $4.00 / $0.40 / $24.00 |
| gpt-realtime-2.1-mini | $10.00 / $0.30 / $20.00 | $0.60 / $0.06 / $2.40 |
| gpt-realtime-translate | $0.034/min | — |
| gpt-realtime-whisper | $0.017/min | — |

- Audio↔token: user audio ≈ 1 token/100 ms (600 tok/min); assistant audio ≈
  1 token/50 ms (1,200 tok/min). **Whole conversation context is re-sent
  (re-billed) as input on every response**; prompt caching discounts repeats.
  [Costs guide](https://developers.openai.com/api/docs/guides/realtime-costs)
- **In-protocol metering:** `response.done.usage` = `total_tokens`,
  `input_tokens`, `output_tokens`, `input_token_details{text_tokens, audio_tokens,
  image_tokens, cached_tokens, ...}`, `output_token_details{text_tokens,
  audio_tokens}`. Transcription usage on
  `conversation.item.input_audio_transcription.completed`. `rate_limits.updated`
  reports live limit state. Per-session cost = Σ per-response usage (not last).
  Per-minute models need wall-clock/audio-duration metering. [Costs guide; openai-node #1600]

## Competitive / compat landscape

- **Azure OpenAI:** same event protocol; GA URL `wss://<resource>.openai.azure.com
  /openai/v1/realtime?model=<deployment>`; auth `api-key` header/query or Entra
  Bearer; extra `?intent=transcription`, `/realtime/translations`. WebRTC + SIP.
  [Azure doc]
- **Google Gemini Live:** WSS but a **different, non-OpenAI schema**
  (BidiGenerateContent). PCM16 16 kHz in / 24 kHz out; ~10-min lifetime with
  session-resumption tokens. [ai.google.dev/gemini-api/docs/live](https://ai.google.dev/gemini-api/docs/live)
- **LiteLLM / Portkey / Cloudflare AI Gateway / Bifrost:** all proxy realtime WS
  as **auth-injection + bidirectional pipe with selective event inspection for
  metering**; LiteLLM logs only `session.created`/`response.create`/
  `response.done` by default. **None documents deep compliance/redaction on the
  realtime audio stream** — the governance gap this spike's §1 competitive claim
  rests on. [docs.litellm.ai/docs/realtime](https://docs.litellm.ai/docs/realtime); [portkey.ai realtime](https://portkey.ai/docs/product/ai-gateway/realtime-api); [Cloudflare AI Gateway realtime](https://developers.cloudflare.com/ai-gateway/usage/websockets-api/realtime-api/); [Bifrost](https://github.com/maximhq/bifrost)
- Kong: OpenAI provider page exists; **no evidence of realtime WS proxying** —
  unconfirmed.

## Security notes (official)

- "Only use standard OpenAI API keys on the server, not in the browser" — browsers
  get short-lived `ek_` client secrets. [WebRTC guide]
- Browser-WS raw-key subprotocol explicitly named `openai-insecure-api-key.`
  [WebSocket guide]
- `OpenAI-Safety-Identifier` set at secret-mint time is bound into the ephemeral
  token; browsers don't send it. [WebRTC guide]
- OpenAI's recommended production pattern is the sideband/server-control WS so
  tools + business logic stay server-side. [Server controls guide]

_All URLs retrieved 2026-07-15._

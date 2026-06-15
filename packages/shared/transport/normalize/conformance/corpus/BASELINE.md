# Conformance corpus baseline (current code, 2026-06-12)

One line per case: current → target (fixing phase). Tick by deleting the case's
BASELINE-WRONG.md and updating its golden when the phase lands.

## Invariant-fields rule

For every row whose target is "unchanged", the following golden fields are
**invariant** across all refactor phases: `kind`, `messages` (roles, block
types, block text/input), `usage`, `model`, `finishReason`. A diff in any of
them on an "unchanged" row is a regression, full stop. `confidence` and
`detectedSpec` are **allowed to change** — Phase 1 and Task 3.0 rework
both by design — but every such change must land with a per-case
recorded rationale in this file's row (e.g. "detectedSpec pattern:openai-chat-sse
→ openai-chat codec, Task 1.4 spec deletion"), never as silent drift.

**Confidence semantics (unified, Task 3.0).** One meaning per input
shape — confidence is the fraction of the input the decoder recognized:
SSE stream folds report frame coverage (recognized frames / total data
frames; `[DONE]` and blank lines are sentinels counted in neither) for
anthropic, openai, AND gemini — the fold sets `Confidence` itself and
the codec's `Normalize` wrapper stamps the single-document field-shape
score (`core.ScoreTier1Confidence` weighted field coverage) only when
the fold left it unset. Consumer-web patch streams report patch-frame
coverage; non-JSON detectors report weighted signal coverage;
generic-http projections report constant 1.0 (projection semantics,
amendment A3 below). One deliberate exception (USER-RATIFIED, Phase 3 gate): adapter rows
resolved by per-host match carry `selectionEvidence=host` and KEEP their
honest coverage (claude-web-class single-prompt specs cap near 0.6);
the Registry accepts them over its threshold on the strength of the
host evidence, not the number, and the UI renders a host-matched label
in place of the numeral so coverage and selection never read as one
scale. The 0.95 confidence floor it replaced is retired. The full table
lives in normalization-architecture.md §2.

**Fallback provenance invariant (Phase 2, amendment A3).** Every
generic-http payload — all routing branches and decode-error partials —
stamps `detectedSpec:"generic-http"` + `confidence:1` explicitly. The
1.0 means full confidence in the PROJECTION: a structural projection
(JSON tree / SSE frames / form map / text / binary digest) is always a
faithful rendering of what it claims to be. It makes zero claim about
AI semantics — "no AI spec identified" is what the `generic-http`
DetectedSpec value says, never a lowered score. Consequence: every
fallback golden carries the two fields; a fallback row missing them is
a regression.

| Case | Current | Target | Phase |
|---|---|---|---|
| anthropic-sse-tooluse-only | ai-chat anthropic-messages 1.0 + tool_use block (input + tool identity), model + finishReason + cache-aware usage | done — fixed Task 1.1 (anthropic codec SSE fold; confidence = frame coverage 10/10) | ✓ 1 (Task 1.1) |
| anthropic-sse-tooluse-bash | ai-chat anthropic-messages 1.0 + tool_use block (multi-line Bash command reassembled from 12 input_json_delta frames; renamed from `anthropic-sse-text` — it has 0 text_delta frames) | done — fixed Task 1.1 (frame coverage 18/18) | ✓ 1 (Task 1.1) |
| anthropic-sse-text | ai-chat anthropic-messages 1.0, model=claude-haiku-4-5-20251001 + finishReason=end_turn | done — fixed Task 1.1. detectedSpec pattern:anthropic-messages-sse → anthropic-messages codec; confidence 0.8 → 1.0 (frame coverage 8/8); messages + promptTokens/completionTokens invariant vs the Tier-2 golden, usage additionally gains totalTokens=41 per the canonical convention (matches the codec's non-stream path) | ✓ 1 (Task 1.1) |
| anthropic-sse-thinking | ai-chat anthropic-messages 1.0 with reasoning + text content blocks, model + finishReason captured, usage gains totalTokens=206 + reasoningTokens=41 (wire-explicit output_tokens_details.thinking_tokens) | done — fixed Task 1.1 (frame coverage 16/16; signature_delta recognized, no content) | ✓ 1 (Task 1.1) |
| anthropic-error-body | http-json (generic JSON tree; per amendment A4 NO first-class error kind — revisit only on operator demand) | done — fixed Task 2.2. Was http-text: the meta carries NO Content-Type (only adapterType+endpointPath), the anthropic codec rejects the body (no content/messages), and the old fallback dumped CT-less UTF-8 as text. The JSON byte-sniff now routes it by bytes. Golden gains detectedSpec=generic-http + confidence=1 per the fallback provenance invariant | ✓ 2 (Task 2.2) |
| anthropic-messages-request | ai-chat anthropic-messages 0.975 (system + 2 messages + tools) | unchanged — confirmed through Task 1.4: the keyed anthropic codec already claimed this row, so deleting the Tier-2 anthropic-messages spec produced zero golden drift | — |
| openai-sse-text-keymissed | ai-chat openai-chat 1.0, byte-identical to the keyed twin `openai-sse-text` | done — fixed Task 1.3 (Tier-1.5 sniff claims before Tier 2: detectedSpec pattern:openai-chat-sse → openai-chat, protocol pattern-extract → openai-chat, http.bodyView dropped — codec parity with the keyed twin; kind/messages/usage/model/finishReason invariant vs the Tier-2 golden). Deletion-safety net held at Task 1.4: the openai-chat-sse Tier-2 spec is deleted and the golden did not move (sniff pass still claims via the codec). Task 3.0: confidence 0.9889 → 1.0 (frame coverage replaces the first-chunk field-shape score; stays byte-identical to the keyed twin) | ✓ 1 (Tasks 1.3+1.4) |
| anthropic-sse-tooluse-keymissed | ai-chat anthropic-messages 1.0, byte-identical to the keyed twin `anthropic-sse-tooluse-only` | done — fixed Task 1.3 (was http-text raw dump; Tier-1.5 sniff lands the key-missed stream on the anthropic codec via the message_start LooksLike probe) | ✓ 1 (Tasks 1.1+1.3) |
| openai-chat-nonstream-basic | ai-chat openai-chat 0.9 | unchanged | — |
| chatgpt-web-req / chatgpt-web-resp-sse | ai-chat chatgpt-web 1.0 | unchanged — held through the Task 1.4 scoring rework: response confidence is now patch-frame coverage (applied/candidate frames) gated on a raw-frame signature hit; this stream applies 5/5 patch frames, so the value stays 1.0 by construction rather than by additive bonuses. Phase 2 addition: response goldens (resp-sse + resp-sse-noresume) gain model=gpt-5-5-thinking — the response spec now probes raw frames at ModelFramePaths (`v.message.metadata.model_slug`, telemetry `metadata.model_slug`), closing the gate-ledger model-extraction item; kind/messages/usage/finishReason invariant | — |
| claude-web-req | ai-chat claude-web 0.6 + selectionEvidence=host | confidence 0.95 → 0.6 + selectionEvidence=host (USER-RATIFIED floor decision, Phase 3 gate: AdapterCallerConfidenceFloor retired in favor of threshold-bypass on host selection evidence — the registry accepts the row because the per-host adapter resolved it by host, and the HONEST 0.6 coverage reaches the row; the UI shows a host-matched label instead of the numeral). kind/messages/model invariant | ✓ 3 (floor) |
| cursor-connectrpc-resp | http-binary | http-binary digest is the ACCEPTED end-state (amendment A4); a connect-rpc frame decode in Phase 1 is a may, not a must. Task 2.2: golden gains detectedSpec=generic-http + confidence=1 per the fallback provenance invariant; digest invariant | — (1 optional) |
| json-no-content-type | http-json (decoded tree) | done — fixed Task 2.2 (JSON byte-sniff: first non-ws byte `{`/`[` + whole-body json.Valid routes to the JSON projection regardless of declared Content-Type). Golden gains detectedSpec=generic-http + confidence=1 per the fallback provenance invariant | ✓ 2 (Task 2.2) |
| json-messages-nonai | http-json (adversarial hand-rolled case — a non-AI export JSON carrying top-level `messages` array-of-objects + `model` string that the openai request sniffer must NOT claim; its message objects lack role/content, and the probe's `"author"` exclusion plus the role/content-free shape keep every sniffer off it; pinned in the codecs sniffer matrix) | unchanged | — |
| json-candidates-nonai | http-json (generic JSON tree; adversarial hand-rolled case — a non-AI API response whose top-level `candidates` array must NOT be claimed by the gemini sniffer, which requires a corroborating `usageMetadata`/`finishReason`/`content` marker) | unchanged — Task 2.2 golden gains detectedSpec=generic-http + confidence=1 per the fallback provenance invariant; JSON tree invariant | — |
| sse-unknown-shape | http-sse structured frames (per-frame event name + JSON-decoded data / verbatim dataText; raw text no longer duplicated into the payload — the Raw view owns the original bytes) | done — fixed Task 2.2 (generic-http SSE projection rewritten frame-structured, capped at 2000 frames + sseTruncated). Golden gains detectedSpec=generic-http + confidence=1 per the fallback provenance invariant | ✓ 2 (Task 2.2) |
| sse-comment-prefix | http-sse generic-http 1.0, 2 structured frames (event=message + wiki-edit JSON trees) | REAL capture (stream.wikimedia.org recentchange via local MITM proxy, Phase 3 live validation). Found the comment-preamble gap live: the stream opens with `:ok` and the first-line-only sniff dumped it to http-text; looksLikeSSE now skips leading SSE comment lines (256-byte window) and declared text/event-stream routes to the frame projection unconditionally | ✓ 3 (Task 3.3) |
| ndjson-two-lines | http-json (array) | unchanged — Task 2.2 golden gains detectedSpec=generic-http + confidence=1 per the fallback provenance invariant; array projection invariant | — |
| openai-sse-toolcalls | ai-chat openai-chat 1.0 + tool_use block | confidence 0.9889 → 1.0 (Task 3.0 unification: stream folds now report frame coverage — every frame recognized — instead of the field-shape score of the first chunk); kind/messages/usage/model/finishReason invariant | — |
| openai-sse-text | ai-chat openai-chat 1.0 | confidence 0.9889 → 1.0 (Task 3.0 frame coverage, same rationale as openai-sse-toolcalls); invariant fields held | — |
| gemini-sse-text | ai-chat gemini-generate 1.0 | confidence 0.95 → 1.0 (Task 3.0 frame coverage: 3/3 frames recognized); invariant fields held | — |
| anthropic-nonstream-tooluse | ai-chat anthropic-messages 0.98 + tool_use block | unchanged | — |

## Task 1.4 record (Tier-2 standard-API spec deletion)

Task 1.4 deleted the Tier-2 standard-API specs (openai-chat,
anthropic-messages, gemini-generate, anthropic-completions-legacy
request side; all six `*-sse` / `*-nonstream` response specs), rewired
the per-host adapters to shared-codec delegation, and removed the
tlsbump legacy adapter-direct dispatch. **Zero golden drift**: every
corpus case was already claimed by a Tier-1 key, the Tier-1.5 sniff
pass, a consumer-web adapter, or Tier 3 before the deletion, so the
full-registry output is byte-identical. The Tier-2-ALONE behavior
change (standard wires no longer pattern-claimed) is pinned by
`extract/probe_test.go` (`TestDetectChatShape_StandardAPIShapesNotClaimed`,
`TestDetectResponseShape_StandardAPIResponsesNotClaimed`) and the
parity pin's corpus invariant (registry never worse than Tier 2).
`openai-completions-legacy` is the one surviving non-consumer spec:
the OpenAI Chat codec rejects flat-prompt bodies (no messages[]), and
character.ai requests ship exactly that shape.

## Task 1.4 consequences addendum (Task 1.5 fixes)

Two regression classes surfaced by the round-1.4 reviews, both fixed in
Task 1.5, plus one accepted loss:

1. **Request-direction key-missed regression.** The Tier-1.5 sniffers
   shipped with response markers only, so a key-missed REQUEST body
   (openai / anthropic / gemini chat request on an unknown path) that
   Tier 2 used to pattern-claim regressed to http-json/http-text after
   the spec deletion. Fix: each codec's `LooksLike` now also probes
   request markers when `meta.Direction` is request or unset —
   anthropic `messages`+`max_tokens` (protocol-required pair), openai
   `messages`+`model` with an `"author"` exclusion (keeps chatgpt-web
   requests on their own spec), gemini `contents` plus one of
   `generationConfig`/`systemInstruction`/`safetySettings`. The
   anthropic/openai byte ambiguity (an Anthropic request satisfies both
   probe pairs) resolves by sniff registration order — anthropic first,
   stricter markers win. Pinned by `openai-req-keymissed/` and
   `anthropic-req-keymissed/` (the latter copies
   `anthropic-messages-request`'s wire with `adapterType:""`), plus
   `codecs/sniffer_test.go` `TestRequestSniffOrderDiscrimination`.
2. **chatgpt-web response signature gate over-tightening.** The gate
   probed signature keys (`conversation_id` / `message_id`) at frame
   top level only; the delta-encoding stream variant nests
   `conversation_id` inside the `v` patch envelope and ships no
   telemetry frames, so KEYED chatgpt-web streams that previously
   rendered fell to verbatim. Fix: the gate also probes `v.<field>`,
   and adapter-keyed callers (`NormalizeForAdapter`) satisfy the gate
   by host evidence alone. Key-missed Tier-2 traffic keeps the strict
   gate (coverage alone must not claim a foreign patch stream). Pinned
   by `chatgpt-web-resp-sse-noresume/` (the existing fixture with the
   resume-token and telemetry frames stripped — only delta frames with
   nested `conversation_id` remain).
3. **Accepted loss.** Truly unknown non-standard request shapes — no
   recognizable marker pair — land on http-json/http-text by design.
   Precision over recall: a sniffer loose enough to claim arbitrary
   JSON-with-a-messages-key would steal foreign producers' traffic, and
   the verbatim tiers still preserve the bytes for inspection.

## Hand-rolled cases (P0.5)

`anthropic-error-body` reproduces the prod error-envelope cluster (~90 rows
of `{"type":"error","error":{"type":"not_found_error",...},"request_id":...}`)
with synthetic ids. `anthropic-messages-request` is a realistic
request-direction /v1/messages body (model + system + tools + 2 messages)
matching the Anthropic Messages API shape the `anthropic-messages` extract
spec recognizes. The two `*-keymissed` cases copy the wire bytes of
`openai-sse-text` and `anthropic-sse-tooluse-only` verbatim but strip the
adapter key and endpoint path (`adapterType:""`) — they pin what key-missed
capture-side traffic gets TODAY, which is the only input class the Task 1.4
Tier-2 spec deletion can regress. `json-messages-nonai` (Task 2.2) is the
adversarial twin of `json-candidates-nonai` for the request side: a non-AI
export document carrying top-level `messages` (array of objects without
role/content) + `model` (a non-model string) that must stay http-json —
the openai request probe's `author` exclusion keeps it unclaimed, pinned
by the sniffer matrix.

## Local capture reproduction (Tasks 0.3 + P0.5)

The cases below were captured through the LOCAL AI Gateway
(`localhost:3050`) with a freshly created+approved virtual key (`$VK`).
Response bodies were saved verbatim with `curl -sN -o <case>/wire` (curl
decodes chunked transfer-encoding; the wire file is the body bytes exactly as
an ingress client reads them, matching the rest of the corpus). No PII; the
virtual key never appears in any saved file.

`anthropic-sse-text` and `anthropic-sse-thinking` are local captures by
necessity: prod holds NO pure text-delta /v1/messages stream (verified
2026-06-12 — the only two prod rows whose http-text dump contains
`text_delta` are tool-dominated mixed streams: 1 text_delta / 20
input_json_delta and 3 / 61), and no prod row contains `thinking_delta`.

```bash
# openai-sse-toolcalls — OpenAI ingress, stream, one function tool, forced call
curl -sN http://localhost:3050/v1/chat/completions \
  -H "Authorization: Bearer $VK" -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"What is the weather in Paris? Use the tool."}],"tools":[{"type":"function","function":{"name":"get_weather","description":"Get current weather for a location","parameters":{"type":"object","properties":{"location":{"type":"string","description":"City name"}},"required":["location"]}}}]}' \
  -o openai-sse-toolcalls/wire

# openai-sse-text — OpenAI ingress, stream, plain text
curl -sN http://localhost:3050/v1/chat/completions \
  -H "Authorization: Bearer $VK" -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"In one short sentence, what is a gateway?"}]}' \
  -o openai-sse-text/wire

# gemini-sse-text — Gemini NATIVE ingress (gemini-shaped SSE, not OpenAI-shaped)
curl -sN 'http://localhost:3050/v1beta/models/gemini-2.5-flash-lite:streamGenerateContent?alt=sse' \
  -H "Authorization: Bearer $VK" -H 'Content-Type: application/json' \
  -d '{"contents":[{"role":"user","parts":[{"text":"In one short sentence, what is a gateway?"}]}]}' \
  -o gemini-sse-text/wire

# anthropic-nonstream-tooluse — Anthropic NATIVE ingress, non-stream, tool_use
curl -sN http://localhost:3050/v1/messages \
  -H "Authorization: Bearer $VK" -H 'Content-Type: application/json' \
  -d '{"model":"claude-haiku-4-5-20251001","max_tokens":512,"stream":false,"messages":[{"role":"user","content":"What is the weather in Paris? Use the tool."}],"tools":[{"name":"get_weather","description":"Get current weather for a location","input_schema":{"type":"object","properties":{"location":{"type":"string","description":"City name"}},"required":["location"]}}]}' \
  -o anthropic-nonstream-tooluse/wire

# anthropic-sse-text — Anthropic NATIVE ingress, stream, pure text deltas
curl -sN http://localhost:3050/v1/messages \
  -H "Authorization: Bearer $VK" -H 'Content-Type: application/json' \
  -d '{"model":"claude-haiku-4-5-20251001","max_tokens":256,"stream":true,"messages":[{"role":"user","content":"In one short sentence, what is a gateway?"}]}' \
  -o anthropic-sse-text/wire

# anthropic-sse-thinking — Anthropic NATIVE ingress, stream, extended thinking
curl -sN http://localhost:3050/v1/messages \
  -H "Authorization: Bearer $VK" -H 'Content-Type: application/json' \
  -d '{"model":"claude-sonnet-4-6","max_tokens":2048,"stream":true,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"What is 27*43? Think step by step."}]}' \
  -o anthropic-sse-thinking/wire
```

`tests/scripts/export-normalize-corpus.sh` was intentionally NOT extended
with these: it re-exports prod `traffic_event_payload` rows by event id,
which does not model local curl captures; the commands above are the
reproducibility record. The script's CASES list does carry the rename
`anthropic-sse-text → anthropic-sse-tooluse-bash` for the prod row it owns.

## Phase-1 gate conditions ledger (2026-06-12)

- Sniff probes inspect only the first 512 bytes; a key-missed request whose
  markers trail a long leading field misses the sniff and lands http-json.
  Accepted recall bound, same rationale as the addendum's precision-over-
  recall entry — bytes stay readable in the typed projection.
- Confidence semantics are split (anthropic SSE = frame coverage;
  openai/gemini non-stream = field-shape score; chatgpt-web = patch coverage;
  adapter-keyed floor 0.95; generic-http = explicit 1.0 in the projection
  per the fallback provenance invariant above). Unification is OWNED by
  Phase 3 (plan Task 3.0).
- DONE (Phase 2 opener, Task 2.2): chatgpt-web response model extraction —
  `model_slug` is probed off raw frames via the spec's ModelFramePaths and
  both chatgpt-web response goldens now pin model=gpt-5-5-thinking. The
  adversarial `json-messages-nonai` request-probe pin landed alongside it
  (golden http-json; sniffer matrix row nil — no sniffer claims it).

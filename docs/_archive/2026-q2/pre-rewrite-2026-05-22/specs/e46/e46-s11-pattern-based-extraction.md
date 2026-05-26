# E46 S11 — Pattern-based extraction (multi-spec chat detection + JSON-patch SSE accumulator)

**Epic:** E46
**Requirements:** [e46-traffic-normalization.md](../../../../docs/developers/specs/e46/e46-traffic-normalization.md)
**OpenAPI:** (no new endpoints)
**Status:** Approved
**Date:** 2026-05-15
**Predecessor:** [e46-s10-generic-http-sse-robustness.md](./e46-s10-generic-http-sse-robustness.md)

---

## Problem

E46-S10 made `GenericHTTPNormalizer` robust enough to produce a readable text dump for consumer-LLM web traffic (chatgpt.com, claude.ai, cursor, gemini.google.com) that hits the `*:*:*` fallback. The dump renders the full SSE protocol — operators can read the conversation but they're scrolling through `event: delta\ndata: {"v": "..."}\n\n` frames mixed with title-generation events, metadata, and JSON-patch ops. Hooks that consume `Normalized.Content` get a Kind=http-text payload with no role/message structure, so PII regex hooks scan the entire SSE wire format including event names and JSON keys (false positives) and DLP hooks can't distinguish "user prompt" from "assistant reply".

## Architecture: Coordinator + 3-Tier with Confidence

E46-S11 establishes a **confidence-based Coordinator** that orchestrates three tiers. The Coordinator is the single entrypoint every caller (Hub audit consumer, ai-gateway L3, future ones) uses. Internal tier walking is hidden.

```
Caller ──→ normalize.Registry.Normalize(raw, meta)    [Coordinator semantics]
                       │
                       ├─[Tier 1]──→ Per-adapter Normalizer (one per interception-domain
                       │             AdapterID + the existing openai/anthropic/gemini)
                       │             returns (payload, confidence)
                       │             confidence ≥ tier1Threshold ⇒ done
                       │             confidence  < tier1Threshold ⇒ fall through
                       │
                       ├─[Tier 2]──→ Pattern-based extract (multi-spec probe +
                       │             SSE walker + JSON-patch accumulator) — THIS STORY
                       │             returns (payload, confidence)
                       │             confidence ≥ tier2Threshold ⇒ done
                       │             confidence  < tier2Threshold ⇒ fall through
                       │
                       └─[Tier 3]──→ GenericHTTPNormalizer's verbatim projection
                                     (http-text / http-json / http-form / http-binary)
                                     confidence = 1.0 by definition; terminal
```

| Tier | Owner | Status | Confidence policy |
|------|-------|--------|-------------------|
| **1** Per-adapter Normalizer | `openai_chat` / `anthropic_messages` / `gemini_generate` today; `chatgpt-web` / `claude-web` / `anthropic-api-direct` / `openai-web` / `gemini-web` / `cursor-grpc` carved out as **E46-S12**. | Already shipped for 3 adapters; 6 more to come. | Returns 1.0 on full parse, lower if it recognised the shape but couldn't extract some fields. Existing 3 normalizers stay binary (success = implicit 1.0) for backward compat. |
| **2** Pattern-based extraction | This story (S11). Lives at `packages/shared/transport/normalize/extract/` + a thin `Normalizer` wrapper. | New. | Multi-spec probe scores 0..1 (locator match 0.4 + role found per msg 0.3 + content extracted per msg 0.3). Default `tier2Threshold = 0.7`. |
| **3** Verbatim dump | `GenericHTTPNormalizer` (E46-S10). | Done. | Always 1.0 — text is text, JSON is JSON, binary is metadata. |

### Coordinator implementation (small, surgical)

`Registry.Normalize` already walks a chain on `ErrUnsupported`. Extend the walk condition to ALSO continue when a returned payload's `Confidence` is set and below the per-step threshold. Backward-compat: existing normalizers don't set `Confidence` → treated as confident (= terminal). The new pattern-based normalizer DOES set `Confidence` → Coordinator may continue.

### "Extract As Much As Possible" — high-value extraction not just Messages

Per user direction, the pattern-based layer must extract **everything cheap and meaningful**, not just user/assistant text:

| Field | When extracted by Tier 2 | Notes |
|---|---|---|
| `Messages[]` (role + content) | Always when shape detected | The core deliverable |
| `Model` | Top-level `model` / `model_id` field (request body) OR top-level field on response JSON if visible | Free win; doesn't add brittleness |
| `Stream` | Set true when SSE detected | Already known from sniff |
| `FinishReason` | `finish_reason` / `stop_reason` / `end_turn` (response) | Often in last frame of SSE — accumulator captures |
| `Tools[]` | `tools` / `functions` array on request body | When detected, copy through |
| Tool calls in messages | `tool_calls` / `function_call` / `functionCall` per message | Per-spec path extraction |
| `Usage` (tokens) | **NOT extracted** | Inconsistent across consumer surfaces, brittle to maintain; Tier-1 per-adapter normalizers handle it when present. |
| `DetectedSpec` | Always set when Tier 2 fires | Recorded so audit reader sees which spec won the probe. New field, additive. |

The next-tier fix at S11 is to **detect chat shapes by pattern, not by host name**. Across the entire LLM industry, JSON chat bodies fall into a small set of spec shapes:

- **OpenAI Chat Completions**: `{messages: [{role, content: string}]}` + top-level `model`
- **Anthropic Messages**: `{messages: [{role, content: array_of_blocks}]}` + `model` + `max_tokens`
- **Gemini Generate**: `{contents: [{role, parts: [{text}]}]}`
- **ChatGPT-web** (consumer): `{messages: [{author: {role}, content: {parts: [strings]}}]}` + top-level `model`
- **Anthropic-web** (consumer): `{prompt} + completion` legacy OR Anthropic-API-shaped
- **OpenAI Completions** legacy: `{prompt, model}`

Response bodies fall into matching streaming shapes:
- **OpenAI SSE**: `data: {"choices": [{"delta": {"content": "..."}}]}`
- **Anthropic SSE**: `event: content_block_delta\ndata: {"delta": {"text": "..."}}`
- **Gemini SSE**: `data: {"candidates": [{"content": {"parts": [{"text": "..."}]}}]}`
- **ChatGPT-web SSE**: `event: delta\ndata: {"o": "add"/"append"/"patch", "p": "/path", "v": ...}` — JSON-patch-flavored deltas accumulated into a final document tree

A byte-sniff + multi-spec probe at the audit normalization layer can recognize these shapes regardless of which `adapter_type` the audit envelope was tagged with. When confidence ≥ 0.7 the payload becomes `Kind=KindAIChat` with structured `Messages[]`; below threshold falls back to text/binary projection.

Per [[feedback_compliance_proxy_text_first]] the goal is human-readable + hook-scannable content; token counts / `finish_reason` / `tool_calls` remain out of scope (they're inconsistently surfaced across consumer surfaces and brittle to extract).

## Goals (in scope)

- Introduce `packages/shared/transport/normalize/extract/` — a reusable pattern-extraction toolkit.
- **SSE walker**: a single `WalkSSE(raw, fn)` helper that produces `(event, data)` frames; replaces the duplicated bufio.Scanner code in `openai_chat.go` and `anthropic_messages.go` over time (this PR adds the helper; it does NOT refactor those files yet — Path A integration).
- **JSON Patch accumulator**: applies ChatGPT-flavored RFC 6902 ops (`add`/`append`/`replace`/`remove`/`patch`) to an in-memory tree, with shorthand support (a frame missing `p`/`o` continues the last append at the same path).
- **Spec registry**: a list of `ChatSpec` entries (OpenAI / Anthropic / Gemini / ChatGPT-web / Anthropic-web / completions-legacy) and `ChatResponseSpec` entries (matching streaming frames). Each spec carries a locator, a role-path, a content-path, and a content-shape hint.
- **Detector**: `DetectChatShape(body)` probes every spec, returns the best match by confidence (locator hit 0.4 + per-message role found 0.3 + per-message content extracted 0.3, max 1.0).
- **GenericHTTPNormalizer integration**: at confidence ≥ 0.7 emit `Kind=KindAIChat` with extracted `Messages[]`, `Model` (if visible), and original raw body preserved in `BodyView.Text`. Below threshold the existing E46-S10 behavior (text/json/binary) stays.
- **Dual view**: confidence-high payloads still ship `BodyView.Text` with the raw bytes so operators can switch the UI to Raw tab to verify extraction didn't lose anything.

## Non-goals (explicitly out)

- Refactoring `openai_chat.go` / `anthropic_messages.go` / `gemini_generate.go` to use the shared SSE walker. The walker matches their existing pattern but those normalizers stay untouched in this story — separate refactor PR.
- Token usage / `finish_reason` / `tool_calls` extraction for consumer surfaces. Per [[feedback_compliance_proxy_text_first]]: text-first.
- Per-host adapters. Pattern matching is host-agnostic.
- Protobuf / gRPC-Web payload extraction (Cursor traffic). Out of scope for E46; tracked separately.
- Brotli decompression at the audit layer. Same status as E46-S10.

## Story

### S11 — Pattern-based extraction layer

**User story:** As a security admin reviewing a captured ChatGPT chat turn (or any consumer-LLM web SSE), the Traffic Detail's Normalized panel shows the conversation as a structured user/assistant message tree — not just a raw SSE dump — and PII hooks scan only the message content, not the protocol scaffolding.

**Tasks:**

- **T11.1** — Create `packages/shared/transport/normalize/extract/` package:
  - `types.go`: `ChatSpec`, `ChatResponseSpec`, `ChatDetection`, `JSONPatchOp`, `ContentShape` enum (`String` / `BlockArray` / `NestedTextArray`).
  - `sse.go`: `func WalkSSE(raw []byte, fn func(Event, Data string) error) error` — bufio scanner with 64 KiB initial / 8 MiB max buffer matching openai_chat.go.
  - `accumulator.go`: `type JSONPatchAccumulator` with `Apply(op JSONPatchOp) error`, `State() any`, `ExtractByPath(path string) (string, bool)`. Op support: `add` (set at path), `append` (string-concat at path), `replace` (overwrite), `remove`, `patch` (recurse into array of nested ops). Shorthand: when an incoming op has no `p` and no `o`, replay the previous append at the last-recorded path.
  - `specs.go`: declare `KnownChatSpecs` (6 request specs) and `KnownResponseSpecs` (6 response specs) with per-spec config.
  - `probe.go`: `DetectChatShape(body) ChatDetection`, `DetectResponseShape(rawSSE_or_JSON) ChatDetection`. Iterate specs, score, return best.

- **T11.2** — `GenericHTTPNormalizer.Normalize` integration (`packages/shared/transport/normalize/generic_http.go`):
  - For SSE-sniffed bodies: run `WalkSSE` collecting frames; try `JSONPatchAccumulator` on op-shaped frames (detected by presence of `o` field on the parsed data); after the stream ends, run `DetectChatShape` against the accumulator's final state OR a synthesized response document.
  - For JSON-sniffed bodies (the byte-sniff or content-type both): run `DetectChatShape` first.
  - On confidence ≥ 0.7: emit `Kind=KindAIChat`, `Protocol="generic-http"` (audit trail) but also stamp `DetectedSpec` in a new metadata field for UI badge, `Messages=[]` populated, `Model` if locatable, and `HTTP.BodyView.Text=raw` for the Raw-tab fallback.
  - Below 0.7: existing E46-S10 path (text/json/binary).

- **T11.3** — `NormalizedPayload` schema (`packages/shared/transport/normalize/types.go`):
  - Add `DetectedSpec string` field (e.g. `"chatgpt-web"`, `"openai-chat"`, `"gemini"`). Omitempty JSON tag.
  - Existing `Messages []Message`, `Model string`, `Stream bool` fields stay as-is — already shaped for chat.

- **T11.4** — Update memory binding: append to [[feedback_compliance_proxy_text_first]] a paragraph clarifying that pattern-based `KindAIChat` extraction is now permitted (the binding is about NOT writing per-host adapters and NOT extracting token/usage — it is NOT about avoiding `KindAIChat` entirely when the shape is unmistakable).

- **T11.5** — Tests in `packages/shared/transport/normalize/extract/*_test.go`:
  - `sse_test.go` — frame splitting, multi-line `data:`, blank-line separation, oversized line handling.
  - `accumulator_test.go` — add / append (string concat at path) / replace / remove / patch (nested ops) / shorthand frame (missing p+o continues last path).
  - `probe_test.go` — one positive case per spec (request body); confidence scoring on partial matches; negative case (random JSON that has `messages` key but no role) routes below threshold.
  - `response_probe_test.go` — one positive case per response spec; ChatGPT-web full SSE replay using the baa07c15 fixture verifies the accumulator → detector pipeline produces an assistant message with "A few that stand out recently..." text.

- **T11.6** — Integration tests in `packages/shared/transport/normalize/generic_http_test.go`:
  - ChatGPT-web request body → Kind=KindAIChat, DetectedSpec=chatgpt-web, Messages[0].Role=user, Messages[0].Content[0] contains "have you read any good books".
  - ChatGPT-web full SSE (multi-frame including append + patch ops) → Kind=KindAIChat, DetectedSpec=chatgpt-web, Messages[0].Role=assistant, Content matches "A few that stand out recently...".
  - OpenAI chat completion body (the canonical spec) — passes existing real-JSON path, also produces Kind=KindAIChat via detector. Should not regress.
  - Plain `{"foo": "bar"}` JSON → Kind=KindHTTPJSON (below threshold).
  - Random text → Kind=KindHTTPText.

## Acceptance criteria

- **AC-S11.1** — A ChatGPT-web request body (the shape in this story's predecessor doc) decodes to `Kind=KindAIChat`, `DetectedSpec="chatgpt-web"`, exactly one user `Message` whose content text equals the prompt string.
- **AC-S11.2** — A ChatGPT-web SSE response (full multi-frame including JSON-patch ops) decodes to `Kind=KindAIChat`, `DetectedSpec="chatgpt-web"`, one assistant `Message` whose content contains the complete final answer text (verified via fixture from `baa07c15`).
- **AC-S11.3** — `BodyView.Text` (raw bytes) is preserved in EVERY pattern-extracted payload so operators can switch to the Raw tab.
- **AC-S11.4** — A JSON document with a `messages` key but no recognizable role/content shape produces `Kind=KindHTTPJSON` (below threshold) — no false-positive `KindAIChat`.
- **AC-S11.5** — Existing AI normalizer outputs (openai_chat, anthropic_messages, gemini_generate) are unchanged byte-for-byte by this story (verified by the cross-service consistency test suite still passing).
- **AC-S11.6** — `go test -race -count=1 ./packages/shared/transport/normalize/...` green.

## Rollout

Single commit, no DB changes, no migrations. Production deploy is deferred to a separate release window after E46-S10 has been observed for a few hours of real consumer-surface traffic on prod (so any pattern-extractor regression doesn't ride on the back of a freshly-shipped story). Per CLAUDE.md "no backward compatibility": new field `DetectedSpec` is additive; old `traffic_event_normalized` rows simply omit it; UI reads as undefined which renders as no badge.

---

## Phases (this PR ships ALL of them)

The architecture spans S11 (Tier 2 framework + Coordinator) and S12 (Tier 1 per-adapter unification). Per user direction to implement the entire architecture with todo tracking, both ship together with explicit phase boundaries so reviewers can audit progress incrementally.

| Phase | Scope | Deliverables | Adapter coverage |
|---|---|---|---|
| **P1** Framework | `NormalizedPayload.Confidence` + `DetectedSpec`; Coordinator semantics in `Registry.Normalize`; `extract/` package skeleton | types.go edit + registry.go edit + extract/types.go | n/a (no adapters touched) |
| **P2** Tier 2 — SSE walker | `extract/sse.go`: WalkSSE helper (frame splitter). Single source for openai/anthropic stream paths to share later. | extract/sse.go + sse_test.go | n/a |
| **P3** Tier 2 — JSON Patch Accumulator | `extract/accumulator.go`: RFC 6902 + ChatGPT-flavored ops (`add`/`append`/`replace`/`remove`/`patch`) + shorthand frames | extract/accumulator.go + accumulator_test.go | n/a |
| **P4** Tier 2 — Multi-spec Probe | `extract/specs.go`: 6 KnownChatSpecs (openai-chat / anthropic-messages / gemini-generate / chatgpt-web / anthropic-web / completions-legacy) + 6 KnownResponseSpecs; `extract/probe.go`: DetectChatShape, DetectResponseShape with confidence scoring + extract-as-much-as-possible (Model/Tools/Usage/FinishReason where visible) | extract/specs.go + extract/probe.go + tests | n/a |
| **P5** Tier 2 — Normalizer wrapper | `extract/normalizer.go`: PatternNormalizer implements `normalize.Normalizer`; runs SSE detect → walker → accumulator → DetectResponseShape; or JSON → DetectChatShape; emits `Kind=KindAIChat`, `DetectedSpec=pattern:<spec>` when confidence ≥ 0.7; ErrUnsupported below | extract/normalizer.go + integration tests | n/a |
| **P6** Wire Tier 2 into Registry | Register `extract.PatternNormalizer` between adapter-keyed entries and `*:*:*` generic-http catch-all. `auditbridge.RegisterDefaultAIBuiltins` call updated. | shared/normalize/auditbridge.go edit | n/a |
| **P7** Adapter interface extension | `traffic.Adapter` interface gains `Normalize(ctx, raw, meta) (NormalizedPayload, error)` method. Default base struct (embeddable) returns ErrUnsupported so all existing adapters compile without immediate refactor; coordinator falls them through to Tier 2 / Tier 3 naturally. | shared/traffic/adapter.go + base struct in shared/traffic/adapter_base.go | All 50+ adapters compile clean |
| **P8** Implement Normalize for top consumer-surface adapters | chatgptweb, claudeweb, anthropic, gemini, geminiweb, openai (the highest-impact surfaces — drives ≥80% of consumer audit volume) | Per-adapter `normalize.go` file alongside the existing implementation, returns NormalizedPayload with `DetectedSpec="<adapterID>"` and `Confidence=1.0` on full parse | 6 adapters filled |
| **P9** Wire adapter Normalize to Hub Registry | Adapters self-register their Normalize method into shared `normalize.Registry` under their AdapterID key. Adapter's own `RegisterFor(reg)` helper called by adapter builtin loader. | shared/traffic/adapters/builtins.go edit | 6 adapters live as Tier 1 in Hub |
| **P10** Tests + verification | Full unit test pass (`go test -race -count=1 ./packages/shared/...`); cross-service consistency test still pins existing AI normalizer outputs byte-identical; UI build clean. | All test files | n/a |
| **P11** Commit + push (no prod deploy) | Single commit, push to main + tag for future deploy. Defer prod deploy until E46-S10 burns in. | git commit | n/a |

**Adapter coverage policy after this PR:** the 6 P8 adapters are Tier 1 (precise). The remaining ~44 adapters keep their `ExtractRequest`-based hook segment path (no runtime regression) and fall to Tier 2 (pattern-based) at audit time (richer than today's verbatim). They get filled in incrementally as ops priority dictates; the framework supports adding new per-adapter Normalize methods without further interface changes.

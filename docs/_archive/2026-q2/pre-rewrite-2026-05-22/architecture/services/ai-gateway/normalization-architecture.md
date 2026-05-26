---
doc: normalization-architecture
area: service
service: ai-gateway
tier: 1
updated: 2026-05-20
---

# Nexus normalization architecture

This document describes the Hub-side audit normalization pipeline that
turns captured request/response bytes into the canonical
`normalize.NormalizedPayload` persisted onto `traffic_event_normalized`
and consumed by hooks + UI. Reads as the long-form companion to the
E46 SDD series.

Authoritative source code: `packages/shared/transport/normalize/`.

---

## Overview

```
Caller (Hub audit consumer / ai-gateway L3 / compliance-proxy normalize wire)
    │
    └─→ normalize.Registry.Normalize(raw, meta) → (NormalizedPayload, error)
                │
                ├─[Tier 1]──→ per-adapter Normalizer keyed by meta.AdapterType
                │             (openai_chat / anthropic_messages / gemini_generate
                │              + chatgpt-web / claude-web / gemini-web / openai-compat
                │              via E46-S12 per-host adapter Normalize methods).
                │             confidence ≥ threshold ⇒ done.
                │             ErrUnsupported or low confidence ⇒ fall through.
                │
                ├─[Tier 2]──→ extract.PatternNormalizer multi-spec probe.
                │             Byte-sniffs SSE / NDJSON / JSON, runs each
                │             KnownChatSpec against the body, picks best.
                │             confidence ≥ threshold ⇒ done.
                │             ErrUnsupported ⇒ fall through.
                │
                └─[Tier 3]──→ generic-http verbatim catch-all. Always
                              succeeds; emits Kind=http-text / http-json /
                              http-form / http-binary projection. Terminal.
```

The Coordinator (`Registry.Normalize`) does the tier walk on
confidence-aware fall-through:

- Each tier's returned payload carries a `Confidence` value in `[0, 1]`.
- `Confidence == 0` (the JSON zero value) is treated as fully confident
  (1.0) for backward compatibility with normalizers written before E46-S11.
- Tiers that explicitly fill `Confidence` below the registry threshold
  signal "I parsed but only partially / weakly" — the Coordinator keeps
  walking and remembers the best partial in case Tier 3 isn't more
  useful.

---

## Why three tiers (and not just one big switch)

A single Registry chain that walks candidate keys
(`adapter:ct:path`, `adapter`, `::path`, `:ct:`, `*:*:*`) and treats
`ErrUnsupported` as the only "keep walking" signal works for canonical
AI providers registered up-front (openai / anthropic / gemini) but
breaks on three pain points that consumer-surface traffic exposes:

1. **Wrong-Content-Type stamps.** Compliance-proxy can lose the
   response Content-Type, so SSE bodies arrive at the normalizer
   advertising `application/json`. A single-tier model that trusts
   the header would call `json.Unmarshal` on `event: delta_encoding...`
   bytes, producing a partial parse error and an empty BodyView.JSON.
2. **No coverage for consumer hosts.** chatgpt.com / claude.ai / cursor
   / gemini.google.com use Anthropic/OpenAI/Gemini-shaped bodies with
   browser-side extensions, but their `adapter_type` strings
   (`chatgpt-web`, `claude-web`) don't match any registered Tier-1
   normalizer key — a single-tier model lands every audit row in the
   `*:*:*` verbatim bucket.
3. **No graceful degradation.** A normalizer that recognises most of
   a body but fails on a new field would have to choose: return
   ErrUnsupported and lose everything to the catch-all, or return a
   half-populated payload with no signal that fields are missing.
   Neither option preserves partial extraction value.

The three-tier model addresses all three:

- **Tier 1** is the precise per-host parse. When it knows a body shape
  end to end, it claims with `Confidence=1.0`. When it has partial
  recognition, it reports a lower confidence and the Coordinator
  considers Tier 2.
- **Tier 2** is a two-pass shape-and-format recogniser:
  - **Pass A — JSON multi-spec probe.** Byte-sniffs first (so a
    mis-stamped Content-Type can't fool it) and iterates 7 known
    chat-request specs + 7 response specs (OpenAI / Anthropic /
    Gemini API + chatgpt-web / claude-web / completions-legacy),
    scoring each by locator + role + content + signature fields.
  - **Pass B — non-JSON detector chain.** Iterates
    `extract.NonJSONDetectors` for byte-level recognition of wire
    formats that aren't JSON at all — currently Connect-RPC +
    protobuf (`ConnectRPCProtobufDetector`, Cursor's chat shape)
    and Google batchexecute (`BatchExecuteDetector`,
    gemini.google.com / any Google web AI surface).

  Tier 2 picks the highest-confidence result from both passes. The
  payoff: a brand-new host that ships a familiar wire shape
  (cursor-clone IDE, new Google AI consumer surface, etc.) is
  recognised as `Kind=KindAIChat` even without a per-host adapter
  registered. Adding a new format ≈ writing one struct that
  implements `NonJSONDetector` and appending it to
  `NonJSONDetectors`.
- **Tier 3** is the safety net. The existing `GenericHTTPNormalizer`
  with the S10 SSE/NDJSON byte-sniff still produces a readable text
  dump for anything Tiers 1 and 2 didn't claim.

---

## The `extract` package

`packages/shared/transport/normalize/extract/` contains the Tier-2 building
blocks AND the reusable utilities Tier-1 normalizers consume:

| File | Purpose |
|---|---|
| `types.go` | `ChatSpec`, `ChatResponseSpec`, `ChatDetection`, `JSONPatchOp`, `ContentShape` |
| `sse.go` | `WalkSSE(raw, fn)` — bufio scanner over canonical Server-Sent Events frames |
| `accumulator.go` | `JSONPatchAccumulator` — ChatGPT-flavoured RFC 6902 op accumulator (add / append / replace / remove / patch + shorthand frames) |
| `specs.go` | `KnownChatSpecs` (7 request specs incl. claude-web) + `KnownResponseSpecs` (7 response specs) |
| `probe.go` | `DetectChatShape`, `DetectResponseShape` — multi-spec iteration with confidence scoring |
| `detector.go` | `NonJSONDetector` interface + `ConnectRPCProtobufDetector` (Cursor chat shape) + `BatchExecuteDetector` (Gemini web / Google batchexecute) + the `NonJSONDetectors` registry the PatternNormalizer iterates |
| `normalizer.go` | `PatternNormalizer` — Tier 2 wrapper running BOTH the JSON probe and the non-JSON detector chain; `NormalizeForAdapter` — Tier 1 helper for per-host adapters |

### Confidence scoring

Each spec scores against a body using up to 1.0 of confidence:

- Request specs:
  - Locator hit (non-empty `messages` / `contents` array, or
    non-empty `prompt` for legacy specs): +0.4
  - Per-message role present at `RolePath`: +0.3 (capped, divided by
    message count)
  - Per-message content extractable at `ContentPath`: +0.3 (capped,
    divided by message count)
  - SignatureField hits (existence of distinctive top-level fields
    like `chosen_suggestion`, `anthropic_version`, `generationConfig`):
    +0.05 each, capped at +0.2 total
- Response specs:
  - AssistantTextPath hits at the assembled doc: +0.5
  - SignatureField hits: +0.1 each, capped at +0.3
  - For SSE specs only: frame-count bonus (+0.3 for ≥1 matched delta
    frame, +0.5 for ≥3) so a fully-emitted stream scores higher than
    a single-frame partial

### Per-spec stream framing

Each `ChatResponseSpec` declares one of three `StreamFraming` values:
- `"single-json"` — body is one JSON document; spec applies
  `AssistantTextPath` directly.
- `"sse-event-data"` with `AccumulatorRule: "concat-text"` — body is
  SSE; probe walks frames and extracts text deltas at the spec's
  `StreamDeltaPath`. Spec-specific paths prevent OpenAI / Anthropic /
  Gemini SSE shapes from cross-claiming each other's frames.
- `"sse-event-data"` with `AccumulatorRule: "json-patch"` — body is
  ChatGPT-style SSE; probe routes each frame through
  `JSONPatchAccumulator` and applies `AssistantTextPath` to the final
  state.

---

## Tier 1: per-adapter Normalizers

Two kinds of Tier-1 entries:

1. **AI Gateway Normalizers** (existing — registered by
   `RegisterDefaultAIBuiltins`): `OpenAIChatNormalizer`,
   `AnthropicMessagesNormalizer`, `GeminiGenerateNormalizer`. These
   parse their providers' native wire formats end-to-end with bespoke
   code paths, including Usage extraction, ReasoningContent tracking,
   tool-call decoding, and SSE accumulation. Registered under
   `openai`, `anthropic`, `gemini`, plus the 14 OpenAI-compatible
   aliases (`deepseek`, `groq`, `moonshot`, …). These are the
   precision Tier-1 normalizers — they always win when matched.

2. **Per-host Adapter Normalizers** (E46-S12 — wired by
   `adapters.RegisterTier1AdapterNormalizers`): each
   `traffic.Adapter` that opts in implements
   `normalize.Normalizer` via a thin `Normalize` method that delegates
   to `extract.NormalizeForAdapter` with a pre-chosen
   `AdapterSpecHint`. Currently shipped: `chatgpt-web`, `claude-web`,
   `gemini-web`, `openai-compat` (the consumer-surface and
   OpenAI-compatible web traffic adapters). Adding a new host adapter
   to Tier 1 is a one-method change — the registration loop type-asserts
   automatically.

### Why two flavours?

The AI Gateway normalizers existed before extract/ and parse their
formats in detail; the per-host adapters are newer and reuse the
extract building blocks. The two flavours coexist peacefully because
both ultimately implement `normalize.Normalizer` and register under
their adapter IDs.

E58-S0 takes the next step the original S11 design left open: it makes
**ai-gateway's own `providers/specs/<adapter>/codec/` layer delegate to these
same Tier-1 normalizers** rather than carrying a third copy of the
same parsing logic. See the next section.

---

## Ai-gateway codec delegation

`packages/shared/transport/normalize/codecs/<format>` Tier-1 normalizers are the **only** place wire-format parsing lives. Both the ai-gateway hot path and the Hub audit consumer go through the same parse → `NormalizedPayload` step. The
gateway differs only in what it does *with* the NormalizedPayload: it
projects to its own wire-shape canonical (OpenAI chat-completions form)
to feed the canonical-translation pipeline, then re-emits to the target
upstream's wire format.

```
upstream response bytes
    │
    ▼
┌─────────────────────────────────────────────────┐
│  shared/transport/normalize/codecs/<format>.Normalize(raw, meta) │  ← single parse
│      → NormalizedPayload (AST)                  │     (Tier 1)
└────────┬───────────────────────────────────────┬┘
         │                                       │
         ▼                                       ▼
  Hub audit consumer                  ai-gateway/internal/canonicalbridge.
  shared/traffic/adapters             ProjectToCanonical(NormalizedPayload)
  compliance-proxy capture                → (Canonical, providers.Usage)
                                          │
                                          ▼
                                   downstream codec emission
                                   (canonical → wire bytes for next call)
```

The **canonical → wire emission** half of the gateway codec stays in
`providers/specs/<adapter>/codec/`. That half encodes the per-model parameter strip rules, HTTP
400 deprecation handling, and other concerns documented in
`provider-adapter-architecture.md` § 3a Rules 1–7. Those rules belong
with the wire-emission code that talks to a specific upstream — they
are not "parsing" concerns.

### The bridge package

`packages/ai-gateway/internal/execution/canonicalbridge/` extends the canonical-bridge layer used for cross-format requests. The decode entry point is a method on `*Bridge` (`bridge.go:310`):

```go
package canonicalbridge

// DecodeViaShared parses upstream response bytes through the
// shared/normalize Tier-1 normalizer for the given wire format, then
// projects the resulting NormalizedPayload into the gateway's
// wire-shape canonical (OpenAI chat-completions form) plus the
// canonical Usage. Used by every providers/specs/<adapter>/codec.DecodeResponse.
//
// This is the single decode path. The wire-emission half of each
// codec (EncodeRequest, PrepareBody) stays in providers/specs/<adapter>/codec/.
func (b *Bridge) DecodeViaShared(
    format provcore.Format,         // FormatOpenAI / FormatAnthropic / FormatGemini
    endpoint provcore.Endpoint,     // Chat / Responses / Messages / generateContent
    body []byte,
) ([]byte, provcore.Usage, error)
```

### Per-spec delegation pattern

Each `providers/specs/<adapter>/codec.DecodeResponse` (non-identity adapters; the OpenAI codec stays identity per `openai/codec/codec.go:60-66`) follows a 4-step shape — illustrated by `packages/ai-gateway/internal/providers/specs/anthropic/codec/codec.go:596-660`:

1. Run the matching shared Tier-1 normalizer (`normcodecs.NewAnthropicMessagesNormalizer().Normalize(ctx, nativeBody, Meta{...})`); zero-fill on parse error.
2. Extract canonical `Usage` via `provcore.ExtractUsage(nativeBody, provcore.FormatAnthropic)` — the shared funnel.
3. Project to OpenAI canonical shape via `normcodecs.ProjectToOpenAIChatCompletion(payload, ProjectionWireMetadata{...})`.
4. Stamp provider-specific extras into `nexus.ext.<provider>.<key>` per provider-adapter Rule 4.

Streaming variants (`providers/specs/<adapter>/codec/stream.go`) delegate to the SSE walker + accumulator in `shared/transport/normalize/extract/`. No bespoke per-codec SSE walker remains.

### Provider-specific extensions still flow

Anthropic's `cache_creation_input_tokens`, the Anthropic cache TTL
class, Gemini's `thoughts_token_count`, the OpenAI Responses API's
`input_tokens_details.cached_tokens` — every provider-specific extra is
captured by the Tier-1 normalizer (which already does, or for missing
cases is enhanced as part of S0). The bridge's `ProjectToCanonical`
stamps these onto the canonical body under `nexus.ext.<provider>.<key>`
per provider-adapter Rule 4, just as the legacy codec did directly.

### What S0 explicitly does NOT do

- Replace `providers.Canonical` (the OpenAI wire-shape JSON) with
  `NormalizedPayload`. The two abstractions serve different needs and
  stay separate: `NormalizedPayload` is the AST for inspection /
  hooks / UI; `Canonical` is the JSON bytes the canonical-translation
  pipeline operates on. The bridge converts.
- Migrate the request-encoding side (`EncodeRequest`, `PrepareBody`).
  Wire-emission is downstream of routing decisions and per-model strip
  rules — it stays in `providers/specs/<adapter>/codec/`.
- Touch the `traffic.Adapter` interface in `shared/traffic`. Adapters'
  `Normalize` already delegates (E46-S12). The pre-existing
  `ExtractRequest` / `ExtractResponse` / `NormalizedContent` legacy
  surface stays for now (still consumed by some hook paths); a future
  story may consolidate it onto NormalizedPayload.

### Cross-component consistency invariant

A new test in `packages/shared/transport/normalize/` asserts that for every
fixture under `testdata/<format>/*.json`, the result of:

1. `shared/normalize.Registry.Normalize(body, meta)` (direct Tier-1 hit), and
2. `canonicalbridge.DecodeViaShared(body, format, endpoint)` (the bridge), and
3. `shared/traffic/adapters/<format>.Adapter.Normalize(body, meta)` (the adapter delegation)

— all produce byte-identical NormalizedPayload values and identical
projected Usage. CI runs this on every PR.

---

## Tier 2: the PatternNormalizer

Registered via `extract.WireTier2(reg)` from each binary main. Runs
whenever Tier 1 doesn't claim (either no entry for that adapter_type,
or the entry returned ErrUnsupported / low confidence).

`PatternNormalizer.Normalize` routes by `meta.Direction`:
- `DirectionRequest` → `DetectChatShape` over the body, iterates
  `KnownChatSpecs`, picks best.
- `DirectionResponse` → `DetectResponseShape`, byte-sniffs SSE vs
  JSON, runs the appropriate stream framing on each KnownResponseSpec,
  picks best.
- `Direction` unset → tries both, picks the higher-confidence detection.

Returns a populated `NormalizedPayload` with:
- `Kind = KindAIChat`
- `DetectedSpec = "pattern:<spec-id>"` (the prefix distinguishes
  multi-spec probe matches from confirmed Tier-1 per-host parses)
- `Confidence = <detection score>`
- `Protocol = "pattern-extract"`
- `Messages[]`, `Model`, `FinishReason`, `Tools[]`, `Usage` (when
  detected) — extract-as-much-as-feasible philosophy per
  [[feedback_compliance_proxy_text_first]]
- `HTTP.BodyView.Text = string(raw)` (Raw-tab dual view)

Below the configured threshold (default 0.7) returns
`normalize.ErrUnsupported` to let the Coordinator fall through.

---

## Tier 3: GenericHTTPNormalizer

The unchanged `*:*:*` catch-all. Always succeeds — projects the body
to one of `http-json`, `http-text`, `http-form`, `http-multipart`, or
`http-binary` (with `BinaryRef` metadata only). Includes the E46-S10
byte-sniff that detects SSE / NDJSON shapes even when the
Content-Type lies, so even Tier-2 fall-throughs land on a readable
text dump rather than a `Kind=http-json` row with an empty
`BodyView.JSON`.

---

## Per-binary wiring

Each of the three Go binaries (`nexus-hub`, `ai-gateway`,
`compliance-proxy`) sets up its Registry the same way at startup:

```go
reg := normalize.NewRegistry()
normalize.RegisterDefaultAIBuiltins(reg)        // Tier 1 — AI builtins (openai/anthropic/gemini/…)
adapters.RegisterTier1AdapterNormalizers(reg)   // Tier 1 — per-host (chatgpt-web/claude-web/…)
extract.WireTier2(reg)                          // Tier 2 — pattern probe
reg.Freeze()
```

The order matters only weakly — Tier 1 is queried via candidate-key
lookup (always preferred when registered), Tier 2 only fires when no
Tier-1 entry claimed, Tier 3 (the `*:*:*` entry registered inside
`RegisterDefaultAIBuiltins`) is terminal. Freezing prevents
late-registration race conditions in tests.

---

## Storage: the `traffic_event_normalized` sidecar

`NormalizedPayload` produced by the Registry is persisted into a
dedicated sidecar table rather than recomputed on read. The schema and
write/read paths trade modest storage overhead for read-path
determinism, failure visibility, and historical stability — properties
that matter more than CPU savings in an audit context.

### Schema (1:1 sidecar of `traffic_event`)

`tools/db-migrate/schema.prisma` → `TrafficEventNormalized`:

| Column | Type | Purpose |
|---|---|---|
| `traffic_event_id` | FK | 1:1 to `traffic_event` (parent owns lifecycle / retention) |
| `request_normalized` | `jsonb` | Assembled canonical request `NormalizedPayload` |
| `response_normalized` | `jsonb` | Assembled canonical response `NormalizedPayload` |
| `request_status` / `response_status` | enum `ok | partial | failed` | Per-direction normalize outcome |
| `request_error_reason` / `response_error_reason` | text | Why normalize failed; surfaced in the UI Raw-tab banner |
| `request_redaction_spans` / `response_redaction_spans` | `jsonb` | Applied redactions (`rule_id`, `start`, `end`, `replacement`) |
| `normalize_version` | `String @default("1")` | Schema version of the stored `NormalizedPayload` |

Raw bytes stay in `traffic_event_payload` (separate sidecar, separate
lifecycle); the normalized row is the "human-readable, hook-consumable,
UI-ready" view.

### Why eager precompute, not on-demand

1. **Read-path determinism.** The CP admin handler
   `GET /api/admin/traffic/:id/normalized`
   (`packages/control-plane/internal/traffic/handler/traffic/traffic.go`)
   does a single point SELECT — O(1), <1 ms typical. Re-parsing
   SSE framing + content blocks + redaction on every view would add
   10–50 ms per call and amplify for list / batch views.
2. **Failure isolation.** At the Hub consumer
   (`packages/nexus-hub/internal/jobs/consumer/traffic.go`,
   `insertNormalizedPayloads`), normalized rows are batch-inserted
   in a separate statement from the raw `traffic_event` /
   `traffic_event_payload` rows. A normalize regression fails the
   sidecar row (stamped `status = failed` + `error_reason`) without
   rolling back the audit trail. On-demand normalization would
   instead fail silently on every read after the regression ships.
3. **Three-source contract.** The same `NormalizedPayload` shape is
   produced from ai-gateway, compliance-proxy, and agent. Storing the
   result freezes the contract at write time — historical rows do not
   drift if normalizer code later changes.
4. **Schema evolution.** `normalize_version` lets the
   `NormalizedPayload` format evolve without back-filling historical
   rows; old rows stay readable at their captured version.
5. **Compaction by design.** Per E46 NFR, normalized JSON is 30–50%
   smaller than raw provider bytes (SSE framing / heartbeats / chunk
   boundaries stripped, binary blocks replaced by `BinaryRef`
   metadata). The sidecar therefore costs less than re-storing raw,
   while remaining the audit-friendly representation.

### Write path

- **Capture side** (ai-gateway / compliance-proxy / agent): normalize
  request body at Phase 3.5 (≤2 ms p99) and response body at stream
  finalize (≤10 ms p99). Apply `storageAction` redaction spans.
  Stamp the result + status + error_reason into the
  `TrafficEventMessage` MQ payload.
- **Hub consumer side**: `insertNormalizedPayloads` batch-inserts
  with `ON CONFLICT DO NOTHING` (idempotent against duplicate MQ
  delivery). NUL bytes (`\x00`) are stripped to keep Postgres
  UTF-8 happy. Insert failures are logged and skipped — the parent
  `traffic_event` row remains.

### Read path

`packages/control-plane/internal/traffic/store/trafficstore/traffic_event_normalized.go`
`GetTrafficEventNormalized` returns `request_normalized` /
`response_normalized` as `json.RawMessage` directly to the admin API.
There is no spill resolution step on this path (contrast with the raw
payload path below).

### Storage strategy: divergence from `traffic_event_payload` and the risk

Raw and normalized payloads use **different storage strategies** —
this is intentional but carries an open risk worth flagging.

| Table | Column | Type | Out-of-row offload | Threshold |
|---|---|---|---|---|
| `traffic_event_payload` | `inline_request_body` / `inline_response_body` | `jsonb` | yes | ≤ 256 KiB inline |
| `traffic_event_payload` | `request_spill_ref` / `response_spill_ref` | `jsonb` (pointer `{backend, key, size, sha256}`) | yes | > 256 KiB → S3 via `shared/storage/spillstore` |
| `traffic_event_normalized` | `request_normalized` / `response_normalized` | `jsonb` | **no** | **none — Postgres TOAST only** |

**The bet.** Normalized payloads are expected to be small enough to
live in a single JSONB column after SSE framing, heartbeats, and chunk
boundaries are stripped and binary content is replaced by
`BinaryRef {size, sha256, content_type, spillKey}` metadata. Postgres
TOAST compression handles the moderate-tail rows. Per E46 NFR,
normalized JSON is typically 30–50% smaller than the raw stream.

**The risk.** The write path performs zero size checks, zero
truncation, and zero spill — only `\x00` stripping. A pathologically
large single normalized payload (e.g., a model returning a multi-MB
structured JSON block, or a long-context conversation with
hundreds of large assistant turns) lands directly in JSONB. TOAST
compresses but does not refuse. The hard ceiling is Postgres' per-row
~1 GB limit; before that, very large rows degrade SELECT latency,
WAL volume, and `pg_dump` time. The read path has no fallback —
`GetTrafficEventNormalized` always returns the whole column.

**Today's mitigation.** Retention policy on `traffic_event` (the
parent) — E46 recommends matching retention so oversized normalized
rows age out with their parent. No write-time protection exists.

**If the bet breaks** (operator observes large normalized rows), the
mitigation path mirrors `traffic_event_payload`:

1. Add `request_normalized_spill_ref` / `response_normalized_spill_ref`
   columns to `TrafficEventNormalized`.
2. At write time in `insertNormalizedPayloads`, threshold-check and
   offload to `shared/storage/spillstore` when the serialized
   payload exceeds a tunable cap (256 KiB to mirror the raw path is
   a reasonable starting point).
3. At read time in `GetTrafficEventNormalized`, resolve the spill
   ref via `SpillStore.Resolve()` when the inline column is null —
   identical pattern to the raw payload store.

This work is **not** scheduled. The design bet is that normalize
output stays bounded enough not to need it. Document any observed
violation here and revisit before adding code.

---

## Hooks and UI consumption

Both hooks and the UI consume `NormalizedPayload` without caring
which Tier produced it:

- **Hooks** (`packages/shared/policy/pipeline/`) filter by
  `Kind` against `HookConfig.ApplicableTrafficKinds` and scan
  `Messages[].Content[].Text` for content rules. After E46-S11 /
  S12, consumer-surface SSE bodies arrive as `KindAIChat` with
  structured Messages, so hooks scoped to `"ai-chat"` finally cover
  chatgpt-web / claude-web / gemini-web turns. Previously these
  rolled into `http-text` and were skipped.
- **UI** (`packages/control-plane-ui/src/pages/traffic/
  NormalizedPayloadView.tsx`) routes by `Kind`; the existing
  `ai-chat` renderer (user + assistant pane, tool calls, reasoning
  blocks) handles pattern-extracted payloads without changes. The
  Raw tab still works because `HTTP.BodyView.Text` is preserved by
  PatternNormalizer.

---

## Adding a new per-host adapter

To upgrade a consumer-surface adapter from Tier 2 (pattern probe) to
Tier 1 (per-host precision):

1. Add a `normalize.go` file alongside the adapter's existing
   implementation under `packages/shared/traffic/adapters/<host>/`.
2. Implement the `Normalize` method on the adapter struct:
   ```go
   func (a *Adapter) Normalize(_ context.Context, raw []byte, meta normalize.Meta) (normalize.NormalizedPayload, error) {
       return extract.NormalizeForAdapter(raw, meta, extract.AdapterSpecHint{
           AdapterID:     adapterID, // or "host-name"
           ReqSpecIDs:    []string{"openai-chat"},  // or whatever upstream shape
           RespSpecIDs:   []string{"openai-chat-nonstream", "openai-chat-sse"},
           MinConfidence: 0.5,
       })
   }
   ```
3. Done — `adapters.RegisterTier1AdapterNormalizers` picks it up
   automatically via the type-assert loop. Add the adapter ID to
   `alreadyCoveredByAIBuiltins` if it conflicts with an existing
   AI normalizer entry.

For a host with a unique JSON wire shape not covered by the 7 existing
`KnownChatSpecs`, add a new `ChatSpec` entry to `extract/specs.go` and
reference its ID in `AdapterSpecHint.ReqSpecIDs`.

## Adding support for a new non-JSON wire format

When the new host doesn't speak JSON at all (binary protobuf, gRPC-Web,
some private Google or Microsoft envelope, etc.) write a
`NonJSONDetector` instead of a `ChatSpec`. The interface is small:

```go
type NonJSONDetector interface {
    ID() string
    LooksLike(raw []byte) bool                                   // cheap byte sniff
    Decode(raw []byte, direction string) (ChatDetection, bool)   // full extract
}
```

`LooksLike` is the gate — it MUST be O(constant) bytes (16-256 byte
prefix check, not a full parse). The PatternNormalizer screens every
audit row through every detector's `LooksLike`, so an expensive sniff
multiplies into a real-world cost.

`Decode` produces the same `ChatDetection` struct the JSON probe
produces, with:
- `SpecID` set to a stable identifier (e.g. `"protobuf-connectrpc-chat"`)
- `MessageRoles` + `MessageContents` (request) or `AssistantText`
  (response)
- `Model` if recoverable from the body
- `Confidence` ≥ 0.7 to claim, lower to fall through

Once the struct exists, append it to the `NonJSONDetectors` slice in
`extract/detector.go`. No other changes needed — the Tier-2 walk picks
it up automatically. Existing detectors:

| Detector | Wire format | Today's host | Future-coverage candidates |
|---|---|---|---|
| `ConnectRPCProtobufDetector` | 5-byte Connect-RPC envelope + GetChatRequest/StreamChatResponse protobuf | cursor.com | Any IDE / agent shipping the same Buf Connect-RPC shape |
| `BatchExecuteDetector` | form-urlencoded `f.req=` request + `)]}'`-prefixed chunked JSON response | gemini.google.com | Translate / Docs AI assist / any Google web AI consumer surface |

When a future host already has a Tier-1 adapter (`packages/shared/traffic/adapters/<host>/normalize.go`),
the adapter typically delegates: it calls `detector.Decode()` and
stamps its own `adapterID` on the result so the audit row attributes
the parse to the precise host identity rather than the generic
detector ID. See `cursor/normalize.go` and `geminiweb/normalize.go`
for that pattern.

---

## Cross-references

- SDD source: `docs/developers/specs/e46/e46-s10-generic-http-sse-robustness.md` (S10 — Tier 3 SSE/NDJSON byte-sniff)
- SDD source: `docs/developers/specs/e46/e46-s11-pattern-based-extraction.md` (S11 + S12 — Coordinator + Tiers 1 + 2)
- Requirements: `docs/developers/specs/e46/e46-traffic-normalization.md`
- Code root: `packages/shared/transport/normalize/`
- Test fixtures: `packages/shared/transport/normalize/extract/*_test.go`

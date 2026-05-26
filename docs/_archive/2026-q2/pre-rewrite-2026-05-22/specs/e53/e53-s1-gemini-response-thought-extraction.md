# E53-S1 — Gemini response thought-part extraction

**Epic:** E53 Reasoning Content Passthrough
**Type:** Bug fix (wire-format integrity)
**Owner:** nexus

## User story

> As an audit operator, I want Gemini 2.5 responses that contain thinking
> text (`parts[].thought = true`) to surface as canonical
> `ContentReasoning` blocks in `traffic_event_normalized.response_normalized`,
> so the audit trail captures the model's reasoning rather than dropping
> it on the floor.

This story is a **bug fix** independent of FR-2 / FR-3. Even when the
client does not explicitly request thinking via `nexus.ext`, the upstream
Gemini API may emit thought parts (e.g. when an operator-level
`thinkingConfig.includeThoughts` is configured at the model catalog).
Today those parts are silently dropped.

## Tasks

### T1.1 — Non-stream response codec

**File:** `packages/ai-gateway/internal/providers/spec_gemini/codec.go`

The current code reads `candidates[].content.parts[].text` and emits a
single OpenAI-shape `choices[0].message.content` string. It must instead:

- Iterate all parts in the assistant candidate.
- For each part where `thought == true`, emit a canonical
  `ContentBlock{Type: ContentReasoning, Text: part.text}`.
- For each part where `thought` is absent or false, emit
  `ContentBlock{Type: ContentText, Text: part.text}` as today.
- Preserve order of parts in the resulting content array.

When the codec re-encodes for OpenAI-shape ingress response, the
`ContentReasoning` blocks flow through the existing
`canonicalbridge/stream_encoders.go:82-89` path which emits
`reasoning_content` field — no change needed there.

### T1.2 — Streaming SSE decoder

**File:** `packages/ai-gateway/internal/providers/spec_gemini/stream.go`

The streaming Gemini protocol emits incremental candidate parts. The
decoder MUST detect `thought = true` on each delta and route the chunk
to `canonical.ChunkUpdate.ReasoningDelta` instead of `ContentDelta`.
Order preservation: a single response may contain multiple reasoning
deltas interleaved with text deltas; both must surface correctly to the
canonical stream.

### T1.3 — Audit pipeline projection

**File:** `packages/shared/transport/normalize/gemini_generate.go`

Lines 23-237 and 477 already reference `ContentReasoning` — verify the
existing projection logic correctly handles thought parts. The bug may
be only in `spec_gemini/codec.go` (the live gateway path) and the audit
normalizer might already be correct. T1.3 is a **read-only audit task**:
if the normalizer is correct, the SDD acceptance is met by T1.1 + T1.2
alone.

### T1.4 — Tests

- `packages/ai-gateway/internal/providers/spec_gemini/codec_test.go`:
  add table-driven cases with mixed thought/text parts (thought-only,
  text-only, interleaved).
- `packages/ai-gateway/internal/providers/spec_gemini/stream_test.go`:
  add cases with streaming thought deltas before, after, and interleaved
  with text deltas.

## Acceptance criteria

- AC-1.1: For a Gemini upstream response containing one thought part and
  one text part, the canonical response carries two content blocks in
  order: `[ContentReasoning, ContentText]`.
- AC-1.2: When that canonical response is re-encoded as OpenAI-shape
  Chat Completions, the streaming variant emits at least one chunk with
  `delta.reasoning_content` and at least one chunk with `delta.content`,
  in order.
- AC-1.3: `traffic_event_normalized.response_normalized.messages[0].content[]`
  contains a block where `type = "reasoning"` and `text` matches the
  upstream thought part text byte-for-byte.
- AC-1.4: When the upstream returns no thought parts, behavior is byte-
  identical to today (no regression).
- AC-1.5: All existing `spec_gemini` unit tests pass; new tests cover
  the thought-part paths.

## Verification

- `go test ./packages/ai-gateway/internal/providers/spec_gemini/...
  -race -count=1`
- Live curl against `api.example.com` with
  `nexus.ext.gemini.thinking_config: {include_thoughts: true}` (depends
  on s3 also shipped, but bug fix verifies independently with operator-
  level config).
- Inspect `traffic_event_normalized` row for the curl: confirm a
  `type=reasoning` block exists in `response_normalized.messages[0].content`.

## Risks

- **R-1.1**: The streaming decoder is delicate (E46 spent significant
  effort getting Gemini SSE event ordering right). Adding a thought
  branch must not introduce ordering bugs in the existing text path.
  Mitigation: snapshot the existing canonical chunk sequence in a golden
  test before the change, re-run after, diff.
- **R-1.2**: Upstream Gemini's `thought` flag semantics may differ
  between non-stream and stream payloads (the field could be on the part
  itself in non-stream but on a different envelope in stream). Verify by
  capturing a real stream against `api.example.com` before coding.

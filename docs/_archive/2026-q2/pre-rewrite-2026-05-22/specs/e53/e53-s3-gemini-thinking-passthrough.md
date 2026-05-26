# E53-S3 — Gemini thinkingConfig passthrough via nexus.ext

**Epic:** E53 Reasoning Content Passthrough
**Type:** Feature
**Owner:** nexus
**Depends on:** s1 (for response-side extraction). Request injection
without response extraction would mean clients pay for thinking tokens
but never see the content.

## User story

> As an API consumer using the OpenAI-compat ingress
> (`POST /v1/chat/completions`), I want to enable Gemini's thinking
> summary by setting a single field on my request body, so I receive
> reasoning content in the response without writing Gemini-specific
> SDK code.

## Tasks

### T3.1 — Codec request path: read nexus.ext.gemini.thinking_config

**File:** `packages/ai-gateway/internal/providers/spec_gemini/codec.go`

In the function that builds the outgoing Gemini request body, add:

```go
if ext := canonicalext.Get(canonicalBody, "gemini", "thinking_config"); ext.Exists() {
    if ext.IsObject() {
        outgoing, err = sjson.SetBytes(outgoing,
            "generationConfig.thinkingConfig", ext.Value())
        ...
        metrics.ReasoningPassthroughCounter.WithLabelValues("gemini", "injected").Inc()
    } else {
        canonicalext.WarnOnce("gemini", "thinking_config", ...)
        metrics.ReasoningPassthroughCounter.WithLabelValues("gemini", "skipped_malformed").Inc()
    }
}
```

**Merge correctness:** `sjson.SetBytes` correctly merges into existing
`generationConfig` keys (it sets the nested path, preserving sibling keys
like `temperature`, `topP`). Confirm with a test where `temperature` is
already set.

### T3.2 — Allow-list

**File:** `packages/shared/traffic/adapters/gemini/detect.go` or similar

If there is an explicit keep-fields whitelist for the outgoing Gemini
request body, add `generationConfig.thinkingConfig.*` paths. Verify
during implementation whether such a whitelist exists; the codec grep
earlier (line 78) suggests usage-related code but not necessarily a
field whitelist. Read-only audit.

### T3.3 — Tests

**File:** `packages/ai-gateway/internal/providers/spec_gemini/codec_test.go`

Table-driven cases:
1. OpenAI-spec request with `nexus.ext.gemini.thinking_config:
   {include_thoughts: true}` → outgoing body has
   `generationConfig.thinkingConfig.include_thoughts: true`.
2. Same request with `temperature: 0.5` already set → outgoing body has
   BOTH `temperature: 0.5` AND `thinkingConfig.include_thoughts: true`.
3. Missing extension → no thinkingConfig in outgoing body.
4. Malformed extension (array instead of object) → no injection +
   counter increments `skipped_malformed`.

## Acceptance criteria

- AC-3.1: A live curl to `api.example.com/v1/chat/completions` with
  `model: "gemini-2.5-pro"` and `nexus.ext.gemini.thinking_config:
  {include_thoughts: true}` returns a response with at least one
  `reasoning_content` segment containing Gemini's thinking summary.
- AC-3.2: Same curl without the extension returns no `reasoning_content`
  (today's behavior).
- AC-3.3: When combined with s1 (response extraction), `traffic_event_normalized`
  row for the curl contains a `type = "reasoning"` block whose text
  matches the Gemini thought summary.
- AC-3.4: `temperature` and other `generationConfig` keys are preserved
  when set alongside the extension.
- AC-3.5: `nexus_aigw_reasoning_passthrough_total{provider="gemini",
  action="injected"}` increments correctly.

## Verification

- `go test ./packages/ai-gateway/internal/providers/spec_gemini/...
  -race -count=1`
- Live curl above; verify reasoning_content presence and quality.
- DB query as in s2.

## Risks

- **R-3.1**: Gemini's `thinkingConfig` schema has evolved (the
  `thinkingBudget` field is recent, was previously `dynamicThinking`).
  The gateway forwards verbatim; document in OpenAPI which subkeys are
  known-good as of 2026-05-15.
- **R-3.2**: Some Gemini models (e.g. `gemini-2.5-flash-lite`) may
  ignore `thinkingConfig` entirely. The gateway is not responsible for
  client targeting; document in the model catalog if the operator wants
  to surface "supports thinking" per model.
- **R-3.3**: `sjson` path syntax handles nested merges, but if the
  outgoing body already has `generationConfig` populated by the codec,
  the merge must not overwrite existing keys. Verify with T3.3 case 2.

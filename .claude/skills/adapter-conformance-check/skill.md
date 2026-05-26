---
name: adapter-conformance-check
description: Audit ai-gateway adapter codecs against provider-adapter-architecture.md Section 3a Rules 1-7. Detects per-adapter logic that leaked into the generic spec_adapter dispatcher, ingress bodies passed to PrepareBody without canonicalize, error envelopes that bypass the helper, prefix-lists without empirical evidence, and missing PassthroughRewrite wiring. Trigger keywords: adapter audit, adapter conformance, codec audit, provider adapter check, /adapter-conformance-check.
---

# adapter-conformance-check

Walk the 7-step audit when **anything in `packages/ai-gateway/internal/providers/spec_*/`** is added, refactored, or even read-and-extended. The architectural contract lives in `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md` §3a Rules 1-7; this skill is the operational checklist.

Run **before** completing any adapter-touching PR. The audit-time gaps (G1–G9 in §11 of the arch doc) all came from drift that this skill is designed to catch.

---

## What this skill checks

| Rule | What to catch |
|---|---|
| Rule 1 | Adapters silently add canonical fields outside the canonical OpenAI surface. |
| Rule 2 | Non-OpenAI adapter's `DecodeResponse` drops content blocks (e.g. Anthropic `thinking`) instead of preserving them. |
| Rule 3 | Per-adapter logic (model prefix-lists, parameter rewrites) leaked into `spec_adapter.go` instead of living in the adapter's own package. |
| Rule 4 | Provider-specific extension fields written to raw paths (e.g. `nexus_anthropic_...`) instead of `nexus.ext.<provider>.<key>` via `canonicalext`. |
| Rule 5 | Handler / cache-prep / executor passes raw ingress body to `adapter.PrepareBody` without canonicalize, when ingress format ≠ target format. |
| Rule 6 | Streaming path's per-model rule diverges from non-streaming (param strip on non-stream but not on stream, or vice versa). Streaming error frame hand-built outside `synthesizeSSEErrorFrame`. |
| Rule 7 | Prefix-list entries (`anthropicModelRejectsSamplingParams`, `IsReasoningModel`, `IsFixedTempModel`, …) added with no observation comment. |

---

## Step 1 — Scan spec_adapter.go for per-adapter leakage (Rule 3)

```bash
# Per-adapter logic must NOT live in spec_adapter.go. The only generic
# logic allowed is endpoint dispatch + PassthroughRewrite callback +
# applyStreamUsageOption (purely OpenAI-shape, no per-vendor knowledge).
grep -nE "FormatAnthropic|FormatGemini|FormatMoonshot|FormatBedrock|FormatCohere|FormatReplicate|FormatVertex|FormatDeepSeek|FormatGLM|FormatMinimax|FormatMistral|FormatXai|FormatGroq|FormatPerplexity|FormatTogether|FormatFireworks|FormatHuggingFace|FormatAzureOpenAI" \
  packages/ai-gateway/internal/providers/spec_adapter.go
# Expected: empty. Any hit = per-adapter case-statement leaked here.

# Per-model identifiers in spec_adapter.go are also forbidden.
grep -nE 'claude-|gpt-[345]|kimi-|deepseek-|gemini-|o[1-9]|"thinking"' \
  packages/ai-gateway/internal/providers/spec_adapter.go
# Expected: empty.
```

If anything matches, move the logic into the adapter's own package and wire it via `AdapterSpec.PassthroughRewrite` (or codec-internal methods if it's a codec concern).

## Step 2 — Verify every adapter wires PassthroughRewrite when applicable

```bash
# Adapters that ARE expected to have a PassthroughRewrite:
# - spec_openai      (gpt-5.x / o-series reasoning rewrites)
# - spec_azure_openai (same — reuses spec_openai.ApplyReasoningRewrites)
# - spec_moonshot    (kimi-k2.5 / k2.6 fixed-temp strip)
#
# Adapters that DO NOT need one (today):
# - All Tier-1 codecs (spec_anthropic, spec_gemini, spec_bedrock,
#   spec_cohere, spec_replicate) — per-model quirks live in codec.go
# - OpenAI-compat siblings with no current per-model quirks
#   (spec_deepseek, spec_glm, spec_minimax, spec_mistral, spec_xai,
#   spec_groq, spec_perplexity, spec_together, spec_fireworks,
#   spec_huggingface, spec_vertex)
grep -L "PassthroughRewrite" \
  packages/ai-gateway/internal/providers/spec_openai/spec.go \
  packages/ai-gateway/internal/providers/spec_azure_openai/spec.go \
  packages/ai-gateway/internal/providers/spec_moonshot/spec.go
# Expected: empty (the field IS wired in all three).
```

When you add an adapter (or a new per-model quirk to an existing one), add `PassthroughRewrite: <YourAdapter>.ApplyRewrites` to `NewSpec`.

## Step 3 — Scan hand-rolled error envelopes (Rule 6, §9.5)

```bash
# All 4xx + SSE-error frames must flow through encodeErrorEnvelopeForIngress
# or synthesizeSSEErrorFrame. Hand-rolling them in proxy.go / proxy_cache.go
# silently breaks cross-format clients.
grep -rnE '"error"[[:space:]]*:[[:space:]]*map\[string\]any' \
  packages/ai-gateway/internal/handler/proxy.go \
  packages/ai-gateway/internal/handler/proxy_cache.go \
  | grep -vE "encodeErrorEnvelopeForIngress|synthesizeSSEErrorFrame|encode(OpenAI|Anthropic|Gemini)ErrorEnvelope"
# Expected: empty. Any hit is a candidate replacement target.
```

## Step 4 — Scan for ingress-body-to-PrepareBody without canonicalize (Rule 5)

```bash
# When ingress != target, the caller MUST canonicalize the body before
# adapter.PrepareBody / SchemaCodec.EncodeRequest fires. Otherwise the
# codec contract is violated (gets non-canonical input).
grep -nE "adapter\.PrepareBody|SchemaCodec.EncodeRequest" \
  packages/ai-gateway/internal/handler/*.go \
  packages/ai-gateway/internal/execution/executor/*.go
# Then for each caller, eyeball: is IngressChatToCanonical called first
# when resolved.BodyFormat != target.Format? Known correct caller sites:
# - executor.go: bridge.IngressChatToWire (does it inside)
# - proxy.go cache-prep: explicit IngressChatToCanonical guard (G3 fix)
```

If a new call site appears without the canonical guard, add the guard or route through `canonicalbridge.IngressChatToWire`.

## Step 5 — Prefix-list empirical-evidence check (Rule 7)

```bash
# Every model prefix-list entry needs a comment citing the observed 400.
# Find all current prefix lists and read the comment above the switch.
grep -rn "strings.HasPrefix(model" packages/ai-gateway/internal/providers/spec_*/ \
  | grep -v "_test.go"
# Read each match's surrounding comment. If a prefix is added without
# an "Observed YYYY-MM" + error-message citation, mark as drift.
```

Speculative prefixes silently flatten caller intent. Either remove the
prefix or run a test against the real upstream and capture the message.

## Step 6 — Streaming + non-streaming parity (Rule 6)

For every adapter that has a `stream.go`:

1. Identify the per-model rules applied in `codec.go EncodeRequest`.
2. Confirm the same rules either:
   - Apply naturally because streaming uses the same `EncodeRequest` path (most cases).
   - Are repeated in any pre-dispatch stream-only path (none today, but possible if stream gets its own body construction).
3. Identify error-frame synthesis points in the stream session.
4. Confirm they call `synthesizeSSEErrorFrame(ingressFormat, pe)`, not hand-roll JSON.

## Step 7 — nexus.ext.* discipline (Rule 4)

```bash
# Find all canonicalext usage. Each entry should follow the
# nexus.ext.<provider>.<key> convention.
grep -rn "canonicalext\." packages/ai-gateway/internal/providers/spec_*/ \
  | grep -v "_test.go"

# Inverse: find direct gjson reads at nexus.ext paths that should use canonicalext.
grep -rnE 'gjson\.GetBytes.*"nexus\.ext\.|gjson\.GetBytes.*"_nexus_' \
  packages/ai-gateway/internal/providers/spec_*/ \
  | grep -v "canonicalext"
# Expected: empty. Direct gjson reads bypass the typo-protection layer.
```

## What to report

Produce a Markdown summary with:

- ✅ rules verified clean
- ⚠ rules with minor drift (e.g. one missing observation comment) — fix in same PR
- ✗ rules with violations (any per-adapter case in spec_adapter.go, missing canonicalize guard, hand-rolled error envelope) — block the PR until fixed

When a violation is found, the fix is **always** "move the per-adapter logic to its own package, or wire the canonicalize step, or replace the hand-rolled envelope with the helper". Never paper over with a `// TODO: refactor later` comment — that breaks the **Completion-time self-audit** rule.

## Cross-references

- `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md` §3a (rules) + §11 (gap tracker)
- `feedback_token_field_handler_sweep.md` memory — related "do the sweep" pattern
- `feedback_temperature_check_before_adding_rule.md` memory — empirical-evidence rule

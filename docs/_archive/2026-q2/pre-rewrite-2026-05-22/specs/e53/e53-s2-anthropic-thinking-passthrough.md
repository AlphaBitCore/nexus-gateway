# E53-S2 — Anthropic thinking passthrough via nexus.ext

**Epic:** E53 Reasoning Content Passthrough
**Type:** Feature
**Owner:** nexus
**Depends on:** none (s1 / s2 / s3 are independent)

## User story

> As an API consumer using the OpenAI-compat ingress
> (`POST /v1/chat/completions`), I want to enable Anthropic's extended
> thinking mode by setting a single field on my request body, so I receive
> reasoning content in the response without writing Anthropic-specific
> SDK code.

## Tasks

### T2.1 — Codec request path: read nexus.ext.anthropic.thinking

**File:** `packages/ai-gateway/internal/providers/spec_anthropic/codec.go`

In the function that builds the outgoing Anthropic request body
(after canonical → Anthropic translation, before HTTP send), add:

```go
if ext := canonicalext.Get(canonicalBody, "anthropic", "thinking"); ext.Exists() {
    // Validate shape: must be an object with at minimum {type: "enabled"}.
    // Anthropic-specific subkey validation happens upstream; we forward as-is.
    if ext.IsObject() {
        outgoing, err = sjson.SetBytes(outgoing, "thinking", ext.Value())
        if err != nil {
            // Log & continue without injection.
            ...
        }
        metrics.ReasoningPassthroughCounter.WithLabelValues("anthropic", "injected").Inc()
    } else {
        canonicalext.WarnOnce("anthropic", "thinking",
            "expected object, got " + ext.Type.String())
        metrics.ReasoningPassthroughCounter.WithLabelValues("anthropic", "skipped_malformed").Inc()
    }
}
```

The exact location is around the existing `cache_creation_input_tokens`
read (codec.go:587) — same pattern.

### T2.2 — Hub ingress: do NOT re-inject

**File:** `packages/ai-gateway/internal/providers/spec_anthropic/hub_ingress.go`

When the ingress is native Anthropic (`POST /v1/messages`), the client
already sends `thinking: {...}` directly in the request body. The ingress
parser must NOT also read `nexus.ext.anthropic.thinking` to avoid double
injection. Verify the current code at hub_ingress.go:366 (which reads
`cache_creation_input_tokens` extension) is gated on OpenAI-spec ingress
only; if not, gate it.

### T2.3 — Prometheus counter

**File:** `packages/ai-gateway/internal/observability/metrics/metrics.go` (or wherever
existing `nexus_aigw_*` counters live)

Register:
```go
ReasoningPassthroughCounter = promauto.With(reg).NewCounterVec(
    prometheus.CounterOpts{
        Namespace: "nexus_aigw",
        Name:      "reasoning_passthrough_total",
        Help:      "Count of nexus.ext.<provider>.<key> reasoning passthrough actions",
    },
    []string{"provider", "action"},
)
```

Labels: provider ∈ {anthropic, gemini}; action ∈ {injected, skipped_malformed,
absent}. Increment from T2.1 and the Gemini equivalent in s3.

### T2.4 — Tests

**File:** `packages/ai-gateway/internal/providers/spec_anthropic/codec_test.go`

Table-driven cases:
1. OpenAI-spec request with `nexus.ext.anthropic.thinking: {type: "enabled", budget_tokens: 4096}` → outgoing body has `thinking: {type: "enabled", budget_tokens: 4096}`.
2. OpenAI-spec request without the extension → outgoing body has no `thinking` field.
3. OpenAI-spec request with `nexus.ext.anthropic.thinking: "invalid"` (string not object) → outgoing body has no `thinking` field; counter increments `skipped_malformed`.
4. Native Anthropic ingress with `thinking` directly in request → outgoing body has `thinking` (the original); the codec does NOT double-set.

## Acceptance criteria

- AC-2.1: A live curl to `api.example.com/v1/chat/completions` with
  `model: "claude-opus-4-7"` and `nexus.ext.anthropic.thinking:
  {type: "enabled", budget_tokens: 4096}` returns a response body with
  `choices[0].message.reasoning_content` populated by Claude's thinking
  text.
- AC-2.2: The same curl without the extension returns a response with no
  `reasoning_content` field (today's behavior — no regression).
- AC-2.3: `traffic_event_normalized.response_normalized.messages[0].content`
  contains a block of `type = "reasoning"` whose text matches the
  Anthropic `thinking_blocks` text.
- AC-2.4: `nexus_aigw_reasoning_passthrough_total{provider="anthropic",
  action="injected"}` increments by 1 per request with valid extension.
- AC-2.5: Bedrock Claude (which uses the same `spec_anthropic` codec)
  also surfaces reasoning content. Not explicitly tested in smoke (per
  scope decision), but verified by code path coverage.

## Verification

- `go test ./packages/ai-gateway/internal/providers/spec_anthropic/...
  -race -count=1`
- Live curl above; visually inspect response JSON.
- DB query: `SELECT reasoning_tokens FROM traffic_event WHERE ...` should
  show the upstream Anthropic-reported thinking token count once s4
  ships.
- `curl localhost:3050/metrics | grep reasoning_passthrough_total` shows
  the counter.

## Risks

- **R-2.1**: Anthropic rejects unknown subkeys inside `thinking`. The
  gateway must forward verbatim; client-side validation is out of scope.
  Mitigation: document in the OpenAPI spec which subkeys are known-good
  (`type`, `budget_tokens`).
- **R-2.2**: `thinking` consumes part of the `max_tokens` budget on
  Anthropic. If the client sets a small `max_tokens` and a large
  `budget_tokens`, the response may be cut off. Document in OpenAPI as
  a caveat; not a gateway bug.

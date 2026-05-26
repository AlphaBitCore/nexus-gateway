# E56-S5 — canonicalbridge wiring for FormatOpenAIResponses

**Epic:** E56 OpenAI Responses-API ingress
**Type:** Bridge integration
**Owner:** nexus
**Depends on:** S2, S3, S4.

## User story

> As a Nexus operator, I want a Responses-API request routed to any
> registered provider — OpenAI same-shape (verbatim), or Anthropic /
> Gemini / Bedrock / Moonshot etc. (canonicalized) — and the response
> always returned to the client in Responses shape (with usage in
> Responses field names), without any per-target branch in handler code.

## Tasks

### T5.1 — Capability check helper

**File:** `packages/ai-gateway/internal/execution/canonicalbridge/bridge.go`

```go
func TargetNativelySupportsShape(target providers.Adapter, ingress providers.Format) bool {
    shape := shapeForFormat(ingress)            // "responses-api" for FormatOpenAIResponses
    for _, s := range target.Manifest().RequestShapes {
        if s == shape { return true }
    }
    return false
}
```

`shapeForFormat`:

| ingress Format | shape string |
|---|---|
| `FormatOpenAI` | `"chat-completions"` |
| `FormatOpenAIResponses` | `"responses-api"` |
| `FormatAnthropic` | `"messages"` |
| `FormatGemini` | `"generate-content"` |
| (others — only call sites that need cross-format care need wiring) | |

### T5.2 — Request bridge

**File:** same.

Extend `IngressChatToCanonical` and the existing `IngressChatToWire` family:

```go
func IngressChatToWire(ingress providers.Format, target providers.Adapter,
                       body []byte, modelID string) ([]byte, BridgeMode, error) {
    if TargetNativelySupportsShape(target, ingress) {
        return body, BridgePassthrough, nil
    }
    canonical, err := IngressChatToCanonical(ingress, body)
    if err != nil { return nil, BridgeError, err }
    wireBody, err := target.SchemaCodec().EncodeRequest(canonical, modelID)
    return wireBody, BridgeCanonical, err
}
```

`IngressChatToCanonical` dispatches:

```go
case providers.FormatOpenAIResponses:
    return spec_openai.DecodeResponsesRequest(body)
```

### T5.3 — Response bridge

**File:** same.

`ResponseCanonicalToIngress`:

```go
case providers.FormatOpenAIResponses:
    return spec_openai.EncodeResponsesResponse(canonical, opts)
```

For same-shape passthrough, this is never reached — passthrough copies upstream bytes through unchanged.

### T5.4 — Stream bridge

**File:** same.

`ResponseStreamCanonicalToIngress`:

```go
case providers.FormatOpenAIResponses:
    return spec_openai.NewResponsesReverseStreamSession(opts)  // S4 reverse encoder
```

For same-shape passthrough, the upstream SSE bytes copy through verbatim — no encoder invoked.

### T5.5 — Executor wire-up

**File:** `packages/ai-gateway/internal/handler/proxy.go`

At the cache-prep + executor-dispatch sites, replace the existing format-check branch with a `bridge.IngressChatToWire(...)` call. Same for the response side. This collapses two existing branches into one capability-driven path and removes the temptation to add new per-format case statements (§3a Rule 3).

### T5.6 — Tests

**File:** `packages/ai-gateway/internal/execution/canonicalbridge/bridge_test.go` (extend)

1. `TargetNativelySupportsShape(spec_openai_adapter, FormatOpenAIResponses) == true`.
2. `TargetNativelySupportsShape(spec_anthropic_adapter, FormatOpenAIResponses) == false`.
3. `IngressChatToWire(FormatOpenAIResponses, spec_openai_adapter, body, model)` returns `BridgePassthrough` + original body.
4. `IngressChatToWire(FormatOpenAIResponses, spec_anthropic_adapter, body, model)` returns `BridgeCanonical` + an Anthropic /v1/messages wire body containing the canonicalized prompt + system.
5. `ResponseCanonicalToIngress(canonical, FormatOpenAIResponses)` produces a Responses-shape response body.
6. Streaming: feed Anthropic SSE through `spec_anthropic.NewStreamSession`, then through `ResponseStreamCanonicalToIngress(_, FormatOpenAIResponses, ...)`, assert valid Responses SSE event sequence.

## Acceptance criteria

- AC-5.1: 6 tests pass.
- AC-5.2: No `switch ingress` in `handler/proxy.go` — all format-aware branching collapsed into `bridge.IngressChatToWire`. Validated by `grep -n "switch.*BodyFormat" packages/ai-gateway/internal/handler/proxy.go` returning zero hits (besides one already-isolated audit-only switch acknowledged in comments).
- AC-5.3: `adapter-conformance-check` skill reports clean.

## Verification

```
go test ./packages/ai-gateway/internal/execution/canonicalbridge/ -race -count=1
go test ./packages/ai-gateway/internal/handler/ -race -count=1
/adapter-conformance-check
```

## Risks

- **R-5.1:** Collapsing branches in `proxy.go` can regress existing /v1/chat/completions traffic if the bridge dispatch is wrong. Mitigation: keep the `chat-completions` capability check explicit, ensure `spec_openai.Manifest.RequestShapes[0] == "chat-completions"` (S1 pins this), and add an integration test exercising a vanilla `POST /v1/chat/completions` after S5 lands.

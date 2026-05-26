# E56-S8 — Error envelope (non-stream JSON + SSE response.failed frame)

**Epic:** E56 OpenAI Responses-API ingress
**Type:** Error path
**Owner:** nexus
**Depends on:** S1, S6.

## User story

> As an application developer, I want errors from `/v1/responses` —
> whether produced by upstream 4xx, by Nexus's cross-format guard, or by
> a mid-stream provider failure — delivered in Responses-API shape (JSON
> for non-stream, `response.failed` SSE event for stream). The OpenAI
> SDK parses these shapes natively; if Nexus sends a chat-completions
> error envelope on `/v1/responses`, my SDK throws an unrelated parse
> error and the real cause is hidden.

Binding by `provider-adapter-architecture.md` §9.5.

## Tasks

### T8.1 — Non-stream error envelope

**File:** `packages/ai-gateway/internal/handler/error_envelope.go`

Extend `encodeErrorEnvelopeForIngress`:

```go
case providers.FormatOpenAIResponses:
    return jsonMarshal(map[string]any{
        "error": map[string]any{
            "message": pe.Message,
            "type":    responsesErrorType(pe.Class),
            "param":   pe.Param,                       // populated by S6 for guard rejections
            "code":    pe.Code,
        },
    })
```

`responsesErrorType` maps `ErrorClass` → Responses error.type:

| ErrorClass | type |
|---|---|
| `RateLimit` | `"rate_limit_error"` |
| `Auth` / `PermissionDenied` | `"authentication_error"` |
| `BadRequest` / `Validation` | `"invalid_request_error"` |
| `Server` / `Upstream5xx` | `"server_error"` |
| (unsupported feature — S6) | `"unsupported_feature"` |
| (default) | `"api_error"` |

### T8.2 — Stream error event

**File:** `packages/ai-gateway/internal/handler/error_envelope.go` (extend `synthesizeSSEErrorFrame`)

```go
case providers.FormatOpenAIResponses:
    payload := map[string]any{
        "type":            "response.failed",
        "sequence_number": seqCounter.Next(),
        "response": map[string]any{
            "id":          synthResponseID(reqID),
            "object":      "response",
            "status":      "failed",
            "model":       routedModel,
            "error": map[string]any{
                "message": pe.Message,
                "code":    pe.Code,
            },
        },
    }
    return []byte("event: response.failed\ndata: " + jsonMarshal(payload) + "\n\n")
```

NOTE: `sequence_number` MUST be drawn from the same counter the live stream uses (S4's `responsesStreamState.seq`). On a pre-stream error (no stream session opened yet), the counter starts at 0 and this frame is the entirety of the SSE body.

### T8.3 — Wire every SSE error path through the helper

**Files audited:** `packages/ai-gateway/internal/handler/proxy.go`, `proxy_cache.go`.

Find all hand-rolled `data: {"error":...}` writes and route them through `synthesizeSSEErrorFrame(ingressFormat, pe, seqCtx)`. Per §9.5 forbidden patterns: no `event:` / `data:` strings outside this helper for error paths. The recent G4 closure gives us the helper; we extend its dispatch.

### T8.4 — Tests

**File:** `packages/ai-gateway/internal/handler/error_envelope_test.go` (extend)

| Case | Input | Assertion |
|---|---|---|
| E1 | non-stream, ingress=Responses, upstream 429 | JSON envelope: `error.type == "rate_limit_error"` |
| E2 | non-stream, ingress=Responses, S6 guard 400 with param="store" | JSON envelope: `error.type == "unsupported_feature"`, `error.param == "store"`, `error.code == "feature_requires_native_responses_target"` |
| E3 | stream, ingress=Responses, mid-stream provider 500 | SSE frame: `event: response.failed` + payload parseable by S4's forward decoder |
| E4 | stream, ingress=Responses, pre-stream guard 400 | SSE frame with `sequence_number: 0` |
| E5 | non-stream, ingress=Responses, upstream returns 401 | `error.type == "authentication_error"` |
| E6 | stream, ingress=Responses, sequence_number continuity across `response.in_progress` (S4) + final `response.failed` | counters are monotonic without gaps |

### T8.5 — Cross-check with S4

Feed the SSE error frame emitted by T8.2 back through S4's `responsesStreamSession.parseEvent` and assert it produces a `CanonicalChunk{Err: ProviderError{...}}` — round-trip pin.

## Acceptance criteria

- AC-8.1: 6 tests pass.
- AC-8.2: `grep -rn "data: {\"error\"" packages/ai-gateway/internal/handler/` returns zero hits outside `error_envelope.go` and `error_envelope_test.go`.
- AC-8.3: Forbidden patterns from §9.5 all absent (verified by grep + adapter-conformance-check skill).

## Verification

```
go test ./packages/ai-gateway/internal/handler/ -run TestErrorEnvelope -race -count=1
/adapter-conformance-check
```

## Risks

- **R-8.1:** OpenAI's `response.failed` payload shape has been observed in two variants in 2025: with `response.error` and with a top-level `error`. The snapshot we follow is "error nested under response" per the context7 reference 2026-05-16. If a real upstream sends a different shape, our forward decoder (S4) must accept both. Pin both variants in S4 golden tests.
- **R-8.2:** A pre-stream error has `sequence_number: 0` per OpenAI's convention. If we accidentally start the counter at 1, strict SDK parsers reject the frame. Test E4 pins this.

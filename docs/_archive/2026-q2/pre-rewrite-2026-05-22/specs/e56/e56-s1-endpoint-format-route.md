# E56-S1 — Endpoint + Format constants, route mount, ingress detection

**Epic:** E56 OpenAI Responses-API ingress
**Type:** Infrastructure (additive)
**Owner:** nexus
**Depends on:** S0 docs only.

## User story

> As an application developer, I want `POST /v1/responses` exposed on
> AI Gateway under VK auth so my OpenAI Responses-API client (Python /
> TS / Go SDK) can route Responses traffic through Nexus without code
> changes.

## Tasks

### T1.1 — Provider constants

**File:** `packages/ai-gateway/internal/providers/types.go`

```go
const (
    EndpointResponsesAPI Endpoint = "responses"
    FormatOpenAIResponses Format = "openai-responses"
)
```

Add `FormatOpenAIResponses` to the `Format.Valid()` switch. Add `EndpointResponsesAPI` to the `Endpoint.Valid()` switch.

**Intentionally NOT in `AllFormats()`.** During implementation we discovered that `AllFormats()` doubles as the chat-routing matrix enumeration (consumed by `canonicalbridge.SelfCheck` + the cross-pair matrix tests). `FormatOpenAIResponses` serves `EndpointResponsesAPI`, not `EndpointChatCompletions`, so it does not belong in that matrix — including it would require every adapter to provide a Responses-shape codec (which is not the architectural model). Route-layer detection only needs `.Valid()`. Documented in the comment above `AllFormats()`.

**Intentionally NOT in `IsOpenAIWireShape()`.** Body shape differs from chat-completions; passthrough rewrites that work by `payload["model"]=X` would corrupt the Responses body.

### T1.2 — EndpointTypeString

**File:** `packages/ai-gateway/internal/handler/ingress.go`

Extend `EndpointTypeString`:

```go
case providers.EndpointResponsesAPI:
    return "responses"
```

### T1.3 — `AdapterSpec.RequestShapes` wiring

**File:** `packages/ai-gateway/internal/providers/spec.go` + `packages/ai-gateway/internal/providers/spec_openai/spec.go` + `packages/ai-gateway/internal/providers/adapter.go` + `packages/ai-gateway/internal/providers/spec_adapter.go`

During implementation we discovered there is no separate `Manifest` type in the codebase; `AdapterSpec` is the declarative struct each subpackage returns. Adding `RequestShapes` to `AdapterSpec` (rather than introducing a new `Manifest` type) keeps the API surface flat and matches the existing PassthroughRewrite pattern.

Implementation:

```go
// In spec.go:
type AdapterSpec struct {
    Format             Format
    Transport          Transport
    SchemaCodec        SchemaCodec
    StreamDecoder      StreamDecoder
    ErrorNormalizer    ErrorNormalizer
    PassthroughRewrite func(payload map[string]any, modelID string) []string
    RequestShapes      []string  // NEW — E56
}

func (s AdapterSpec) SupportsShape(shape string) bool {
    if len(s.RequestShapes) == 0 {
        return shape == "chat-completions"
    }
    for _, sh := range s.RequestShapes {
        if sh == shape { return true }
    }
    return false
}

// In spec_openai/spec.go:
RequestShapes: []string{"chat-completions", "responses-api"},

// In adapter.go (Adapter interface):
SupportsShape(shape string) bool

// In spec_adapter.go (specAdapter):
func (a *specAdapter) SupportsShape(shape string) bool { return a.spec.SupportsShape(shape) }
```

Every other adapter keeps `RequestShapes` unset (empty slice defaults to `["chat-completions"]`). Per §3a Rule 7 (binding): adding `"responses-api"` to any other adapter requires a captured 200 from that provider's real `/v1/responses` endpoint.

**Test-double compatibility:** Adding `SupportsShape` to the `Adapter` interface means any in-tree mock / stub / fake adapter must also implement it. The S1 commit updates 5 test files (6 mock types): `adapter_registry_test.go` (stubOpenAIAdapter + invalidFormatAdapter), `executor_test.go` (mockAdapter), `backend_provider_test.go` (fakeAdapter), `credential_probe_endpoint_test.go` (stubProbeAdapter), `adapter_decider_test.go` (fakeAdapter) — each adds `func (X) SupportsShape(shape string) bool { return shape == "chat-completions" }`.

### T1.4 — Route mount

**File:** `packages/ai-gateway/cmd/ai-gateway/main.go`

Mount `POST /v1/responses` next to the existing `/v1/chat/completions` mount:

```go
v1.POST("/responses", proxyHandler.ServeProxy, handler.IngressMiddleware(handler.Ingress{
    Endpoint:     providers.EndpointResponsesAPI,
    BodyFormat:   providers.FormatOpenAIResponses,
    EndpointType: "responses",
}))
```

### T1.5 — Routing simulate endpoint

**File:** `packages/ai-gateway/internal/handler/routing_simulate_endpoint.go`

`normalizeEndpointType("responses") → "responses"`. Empty / "chat" default stays `"chat/completions"`. Add the case explicitly so admins can simulate Responses routing.

### T1.6 — Tests

- `providers/types_test.go`: pin `FormatOpenAIResponses.Valid()` + `EndpointResponsesAPI.Valid()`.
- `handler/ingress_test.go`: pin that `POST /v1/responses` produces `Ingress{Endpoint: EndpointResponsesAPI, BodyFormat: FormatOpenAIResponses, EndpointType: "responses"}` on the request context.
- `providers/spec_openai/spec_test.go`: pin `RequestShapes` order and contents.
- `handler/routing_simulate_endpoint_test.go`: add "responses" case.

## Acceptance criteria

- AC-1.1: `POST /v1/responses` returns 401 without VK (auth middleware fires).
- AC-1.2: With a valid VK + a routing rule pointing at any provider, the request reaches `ProxyHandler.ServeProxy` with `Ingress.Endpoint == EndpointResponsesAPI`.
- AC-1.3: `EndpointTypeString(EndpointResponsesAPI) == "responses"`.
- AC-1.4: `spec_openai.Manifest.RequestShapes` includes `"responses-api"`; no other adapter does.
- AC-1.5: `go test ./packages/ai-gateway/... -race -count=1` is green.

## Verification

```
go test ./packages/ai-gateway/internal/handler/... -race -count=1
go test ./packages/ai-gateway/internal/providers/... -race -count=1
```

## Risks

- **R-1.1:** Existing `proxy.go` code branches on `Ingress.BodyFormat == FormatOpenAI`. Search for every such branch; for now, `FormatOpenAIResponses` should follow the same path until S2-S5 add the codec hooks. Risk = unintended behavior change for `/v1/chat/completions`. Mitigation: a regression test in `handler/proxy_test.go` for a vanilla chat-completions call after the constants land.

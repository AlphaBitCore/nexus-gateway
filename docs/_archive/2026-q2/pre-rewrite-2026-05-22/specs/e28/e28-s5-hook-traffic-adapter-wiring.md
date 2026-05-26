# E28 — Story 5: Hook traffic-adapter wiring in AI Gateway

## Context

Content extraction for the hook pipeline (keyword filter, PII detector, content safety, etc.) is implemented in `packages/shared/traffic/adapters/` with one adapter per provider format (`openai-compat`, `anthropic`, `gemini`, `azure-openai`, `minimax`, `glm`, `deepseek`, `bedrock`, `vertex`, `generic-jsonpath`). Today ai-gateway hardcodes `openai-compat` as its `trafficAdapter()`, so a native Anthropic request on `/v1/messages` would hit the hook layer with an OpenAI extractor that cannot read `system` + `messages[]` + `content[].text`, silently bypassing compliance.

With native ingress routes added in story s4, this breaks hooks for non-OpenAI formats. Fix: dispatch to the right `shared/traffic.Adapter` per detected ingress `BodyFormat`.

Compliance-proxy and agent **already** dispatch per `InterceptionDomain.adapter_id` / local adapter mapping — no changes there. This story is scoped to ai-gateway only, but it preserves the single-source-of-truth contract: one content-extraction codebase (`shared/traffic/adapters/`) used by all three data planes.

## User Story

**As a** compliance officer,
**I want** every AI request through the AI Gateway — no matter which ingress schema it uses — to be inspected by the same hook pipeline with the correct content extractor,
**so that** adding native ingress support does not silently create a compliance hole.

## Tasks

### 1. Registry injection — `packages/ai-gateway/cmd/ai-gateway/main.go`

- Construct a `traffic.AdapterRegistry` at startup.
- Call `shared/traffic/adapters.RegisterBuiltins(registry)`.
- Pass the registry into the `handler.New(...)` dependency bag as `TrafficAdapters *traffic.AdapterRegistry`.

### 2. Format-aware lookup — `packages/ai-gateway/internal/handler/traffic_adapter.go`

Replace the current `defaultTrafficAdapter = &openai.Adapter{}` helper with:

```go
// formatToTrafficAdapterID is the single bridge between the provider
// Format enum and shared/traffic/adapters registry IDs. The mapping is
// one-to-one for every Format; only `openai → openai-compat` is a
// non-identity rename (the traffic registry uses the longer suffix to
// disambiguate from third-party "openai" naming). MUST stay
// synchronized with shared/traffic/adapters/builtins.go.
func formatToTrafficAdapterID(f providers.Format) string {
    switch f {
    case providers.FormatOpenAI:
        return "openai-compat"
    case providers.FormatDeepSeek:
        return "deepseek"
    case providers.FormatGLM:
        return "glm"
    case providers.FormatAzureOpenAI:
        return "azure-openai"
    case providers.FormatAnthropic:
        return "anthropic"
    case providers.FormatGemini:
        return "gemini"
    case providers.FormatMiniMax:
        return "minimax"
    case providers.FormatBedrock:
        return "bedrock"
    case providers.FormatVertex:
        return "vertex"
    default:
        // Unreachable: every Format enum value has a case above. A new
        // Format added without updating this switch fails the
        // exhaustiveness test in §7.
        return "generic-jsonpath"
    }
}

func (h *Handler) trafficAdapterFor(format providers.Format) traffic.Adapter {
    id := formatToTrafficAdapterID(format)
    a, ok := h.trafficAdapters.New(id)
    if !ok {
        h.log.Warn("unknown traffic adapter format", "format", format, "id", id)
        a, _ = h.trafficAdapters.New("generic-jsonpath")
    }
    return a
}
```

Every provider Format on the AI Gateway side has a dedicated content-extraction adapter on the traffic side — no piggybacking. `generic-jsonpath` exists only as a fallback safety net for a hypothetical unknown Format reaching this layer (and as a defense-in-depth runtime fallback if a registry registration fails at startup); the exhaustiveness test below makes that path unreachable in practice.

### 3. Replace all call sites

- `handler/proxy.go`: every call to `h.trafficAdapter()` (zero-arg) becomes `h.trafficAdapterFor(ctx.IngressFormat)` where `ctx.IngressFormat` is the detected ingress `BodyFormat` carried on the request context from s4's detection.
- Specifically `ExtractRequest`, `ExtractResponse`, `ExtractStreamChunk`, `DetectRequestMeta`, `DetectResponseUsage`, `RewriteRequestBody`, `RewriteResponseBody` all use the format-aware adapter.
- Delete the zero-arg `trafficAdapter()` method and the embedded OpenAI singleton.

### 4. Hook modify decisions

`ExtractRequest` / `RewriteRequestBody` operate on the ingress bytes in ingress format. That means:

- A hook modifying request content (`ContentModify`) must return content suitable for the ingress format. The hook framework already writes the returned content back via `RewriteRequestBody` which the format-specific traffic adapter handles.
- For passthrough upstream calls (ingress == provider format), the modified ingress bytes are forwarded as-is.
- For translated upstream calls (ingress == openai, provider != openai), the `providers.SchemaCodec.EncodeRequest` runs **after** the hook rewrite so the provider receives the hook-modified content in its native shape.

This ordering is documented inline in `handler/proxy.go` as a short comment block.

### 5. Metric alignment

- Add `ingress_format` label to `nexus_ai_gateway_hook_request_total` and `nexus_ai_gateway_traffic_extract_total` so operators can see per-format hook activity.
- No new metric names; existing metrics gain a label.

### 6. Delete the old default

- Remove the `defaultTrafficAdapter = &openai.Adapter{}` singleton from `handler/proxy.go`.
- Remove any `import _ ".../traffic/adapters/openai"` side-effect imports in ai-gateway that were relying on the hardcoded default.

### 7. Unit tests

Package `packages/ai-gateway/internal/handler`:

1. `traffic_adapter_test.go` — table-driven: every `providers.Format` enum value maps to the expected traffic adapter ID; the test enumerates the Format enum (via reflect or a generated list) so adding a new Format without updating the switch fails the test (`exhaustiveness assertion`); the default case (impossible for enum members) returns `generic-jsonpath`.
2. `proxy_hook_format_test.go` — with a fake `traffic.AdapterRegistry`:
   - Anthropic ingress on `/v1/messages` → hook `ExtractRequest` called on the `anthropic` adapter instance, **not** openai-compat.
   - Gemini ingress → `gemini` adapter.
   - OpenAI ingress → `openai-compat` adapter.
   - GLM ingress on `/api/paas/v4/chat/completions` → `glm` adapter (regression: pre-E28 code would have used openai-compat).
3. `proxy_hook_modify_order_test.go` — hook modifies request → `RewriteRequestBody` runs on ingress adapter → `SchemaCodec.EncodeRequest` runs after, so the upstream sees the modified content translated into the target format.

## Acceptance Criteria

- `grep -R "defaultTrafficAdapter\\|openai.Adapter{}" packages/ai-gateway` returns **zero** matches after this story.
- `go test -race -count=1 ./packages/ai-gateway/internal/handler/...` passes including the three new tests.
- Anthropic-ingress + Anthropic-provider request with a keyword-filter hook blocks correctly (end-to-end integration test against fixed fixture text).
- GLM-ingress + GLM-provider request with a keyword-filter hook blocks correctly using the `glm` content extractor (regression test; pre-E28 the request would have been inspected with the openai-compat extractor).
- `nexus_ai_gateway_hook_request_total{ingress_format="anthropic"}` counter increments on the Anthropic-ingress integration test, and `{ingress_format="glm"}` increments on the GLM-ingress integration test.
- Compliance-proxy and agent test suites are **not** touched — this story does not change their adapter dispatch.

## Out of scope

- Any change to `shared/traffic/adapters/` implementations.
- Hook payload schema changes.
- Compliance-proxy / agent wiring.

# E56-S6 — Cross-format guards (stateful fields + built-in tools)

**Epic:** E56 OpenAI Responses-API ingress
**Type:** Validation + error path
**Owner:** nexus
**Depends on:** S2, S3, S5.

## User story

> As an operator routing `/v1/responses` traffic to Anthropic / Gemini /
> Moonshot / etc., I want stateful fields (`previous_response_id`,
> `store: true`) and OpenAI-native built-in tools (web_search,
> file_search, computer, image_gen, mcp, code_interpreter) rejected with
> a clear structured 400 BEFORE the request hits upstream — so the caller
> sees a predictable error instead of a confusing cross-format crash.

## Tasks

### T6.1 — Built-in tools registry

**File:** `packages/ai-gateway/internal/providers/spec_openai/responses_builtin_tools.go` (new)

```go
// IsResponsesBuiltinTool reports whether the given tool entry's "type"
// field names an OpenAI-native Responses-API built-in tool. These tools
// require server-side execution that only OpenAI itself can perform;
// cross-format routes (target != spec_openai) reject them per E56-S6.
//
// Source: OpenAI Responses API reference, snapshot 2026-05-16
// (https://platform.openai.com/docs/api-reference/responses, captured
// via context7 /openai/openai-python).
//
// Extending this list: add empirical evidence (a captured tool entry
// from a real Responses request) + update §3a Rule 7 if the new entry
// is a non-OpenAI-native tool.
var responsesBuiltinToolTypes = map[string]struct{}{
    "web_search":           {},
    "web_search_preview":   {},  // legacy alias observed in some SDK versions
    "file_search":          {},
    "computer_use_preview": {},
    "image_generation":     {},
    "mcp":                  {},
    "code_interpreter":     {},
    "custom":               {},  // CustomTool: only OpenAI's Responses runtime can execute
    "apply_patch":          {},  // ApplyPatchTool — same reason
    "tool_search":          {},  // ToolSearchTool — same reason
    "function_shell":       {},  // FunctionShellTool — same reason
}

func IsResponsesBuiltinTool(typeStr string) bool {
    _, ok := responsesBuiltinToolTypes[typeStr]
    return ok
}
```

(Note: `"function"` and `"function_tool"` are NOT in this list — those are caller-defined functions which we DO support on cross-format.)

### T6.2 — Guard implementation

**File:** `packages/ai-gateway/internal/handler/cross_format.go` (extend)

Add `validateResponsesIngressForCrossFormat(req []byte) error`. Returns a Responses-shape `ProviderError` with `code = "feature_requires_native_responses_target"` and `param = "<field>"` on:

| Field | Trigger condition |
|---|---|
| `previous_response_id` | present and non-empty |
| `store` | present and `== true` |
| `truncation` | present and `!= "disabled"` |
| `tools[*].type` | matches `IsResponsesBuiltinTool` |

Invoked by `handler.proxyDispatch` ONLY when `bridge.TargetNativelySupportsShape(target, FormatOpenAIResponses) == false` — i.e. cross-format. Same-shape passthrough doesn't call this.

### T6.3 — Error encoding

The 400 envelope reuses `encodeErrorEnvelopeForIngress(FormatOpenAIResponses, ...)` (S8). Body shape:

```json
{
  "error": {
    "message": "Field 'previous_response_id' requires routing to a target that natively supports the Responses API. Configure a routing rule that resolves to an OpenAI provider, or remove the field.",
    "type":    "unsupported_feature",
    "param":   "previous_response_id",
    "code":    "feature_requires_native_responses_target"
  }
}
```

### T6.4 — Tests

**File:** `packages/ai-gateway/internal/handler/cross_format_test.go` (extend)

| Case | Setup | Expectation |
|---|---|---|
| C1 | ingress=Responses, target=spec_anthropic, body has `previous_response_id:"resp_abc"` | 400 with `param:"previous_response_id"` |
| C2 | ingress=Responses, target=spec_anthropic, body has `store:true` | 400 with `param:"store"` |
| C3 | ingress=Responses, target=spec_gemini, body has `tools:[{type:"web_search"}]` | 400 with `param:"tools[0].type"` |
| C4 | ingress=Responses, target=spec_openai, body has `previous_response_id` | passes guard (same-shape passthrough); 200 from upstream mock |
| C5 | ingress=Responses, target=spec_anthropic, body has `tools:[{type:"function","function":{...}}]` | passes guard (function tools are legal on cross-format); proceeds to canonicalization |
| C6 | ingress=Responses, target=spec_anthropic, body has `truncation:"auto"` | 400 with `param:"truncation"` |
| C7 | ingress=Responses, target=spec_anthropic, body has `truncation:"disabled"` | passes guard (explicit no-op) |

## Acceptance criteria

- AC-6.1: All 7 cases pass.
- AC-6.2: Guard runs strictly before canonicalization (so a malformed cross-format request returns 400 in <1ms, never hits upstream).
- AC-6.3: Same-shape passthrough is untouched (C4 stays a 200).
- AC-6.4: New file `responses_builtin_tools.go` is referenced only from `cross_format.go` + tests — no leakage into the generic dispatcher.

## Verification

```
go test ./packages/ai-gateway/internal/handler/ -run TestResponsesCrossFormat -race -count=1
```

## Risks

- **R-6.1:** Built-in tool list will grow. The registry in T6.1 is the single source of truth; CI script `scripts/check-arch-doc-triggers.mjs` or an equivalent could be extended to assert no new tool type sneaks into spec_openai/codec_responses.go without also being classified in this registry. Out of scope for E56 but worth a follow-up note.
- **R-6.2:** A caller could `{type:"function","function":{"name":"web_search",...}}` — that is a custom function NAMED web_search, NOT the built-in tool. The guard correctly checks `type`, not `function.name`. Test case C5 pins this.

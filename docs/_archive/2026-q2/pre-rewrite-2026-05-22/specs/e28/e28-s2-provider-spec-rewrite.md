# E28 — Story 2: Provider spec rewrite

## Context

Story s1 defines the `AdapterSpec` scaffolding. This story fills in the **nine** built-in adapters as `AdapterSpec{Transport, SchemaCodec, StreamDecoder, ErrorNormalizer}` tuples, replacing the legacy `BaseAdapter` override pattern. Each spec is self-contained — no subclassing, no cross-adapter inheritance.

The set is one-to-one with the non-fallback IDs in `shared/traffic/adapters/builtins.go`: `openai`, `deepseek`, `glm`, `azure-openai`, `anthropic`, `gemini`, `minimax`, `bedrock`, `vertex`. Three of these are new on the AI Gateway side relative to the codebase pre-E28: `deepseek`, `bedrock`, `vertex`. Bedrock and Vertex are **egress-only** in this round (their AWS SigV4 / GCP SA JWT credential models do not map onto the single-token VK so no native ingress is mounted — see s4 and the requirements doc FR-13a). DeepSeek shares the OpenAI-compat URL path so it is also accessible without a dedicated ingress, but it gets its own egress AdapterSpec so observability and probes target the DeepSeek host.

## User Story

**As an** AI Gateway maintainer,
**I want** each provider implemented as an explicit four-part spec,
**so that** I can see in one file exactly how a given provider's wire URL, auth, body shape, stream frames, and error envelope differ from the defaults.

## Tasks

### File layout

New directory tree:

```
packages/ai-gateway/internal/providers/
  spec_openai/
    spec.go            // AdapterSpec{...}
    transport.go
    codec.go
    stream.go           // StreamDecoder: OpenAI SSE
    errors.go
    spec_test.go
  spec_anthropic/
    spec.go
    transport.go
    codec.go            // SchemaCodec: anthropic ↔ openai
    stream.go           // StreamDecoder: Anthropic SSE (event: message_delta etc.)
    errors.go
    spec_test.go
  spec_gemini/ ...
  spec_azure_openai/ ...
  spec_minimax/ ...
  spec_glm/ ...
  spec_deepseek/ ...
  spec_bedrock/ ...        // egress only this round
  spec_vertex/ ...         // egress only this round
  builtins.go              // single place that wires all 9 into RegisterBuiltins
```

Subpackages avoid one giant `providers` file and keep each adapter importable by tests in isolation.

### Per-adapter responsibilities

For every subpackage, implement and export `NewSpec(log *slog.Logger) providers.AdapterSpec` returning a fully wired spec. Each adapter handles the following concerns; see the table below for provider-specific details.

| Provider | Format | URL pattern (Endpoint → path) | Auth | Stream framing | Notable codec facts |
|---|---|---|---|---|---|
| **openai** | `openai` | `chat_completions` → `/v1/chat/completions`; `embeddings` → `/v1/embeddings`; `models` → `/v1/models` | `Authorization: Bearer {APIKey}` | SSE `data:` lines, `[DONE]` terminator | Identity codec (canonical == native). |
| **deepseek** | `deepseek` | `chat_completions` → `/v1/chat/completions`; `embeddings` → `/v1/embeddings` (DeepSeek does not currently expose embeddings — endpoint returns 404 from upstream and surfaces as `ProviderError{Code: "endpoint_unsupported"}`) | `Authorization: Bearer {APIKey}` | OpenAI-compat SSE | Identity codec. Separate spec from OpenAI so probe targets DeepSeek host (`https://api.deepseek.com/v1/models`) and observability metrics carry `provider="deepseek"`. |
| **glm** | `glm` | `chat_completions` → `/api/paas/v4/chat/completions`; `embeddings` → `/api/paas/v4/embeddings`; `models` → `/api/paas/v4/models` | `Authorization: Bearer {APIKey}` (GLM JWT-style API key — passed verbatim) | OpenAI-compat SSE | Identity codec for the chat schema. GLM-specific request fields (`do_sample`, `tools[].type=web_search`, `request_id`) pass through unchanged on passthrough; on translated input from canonical OpenAI, no GLM-only field is generated. |
| **azure-openai** | `azure-openai` | `chat_completions` → `/openai/deployments/{ProviderModelID}/chat/completions?api-version={apiVersion}`; `embeddings` → `/openai/deployments/{ProviderModelID}/embeddings?api-version=...` | `api-key: {APIKey}` header | OpenAI SSE | Codec: strip `"model"` from canonical body before egress (Azure derives model from URL deployment); on response, inject `"model"` from `target.ProviderModelID` into canonical output. `apiVersion` comes from `CallTarget.Extras["azure.apiVersion"]`. |
| **anthropic** | `anthropic` | `chat_completions` → `/v1/messages` | `x-api-key: {APIKey}`, `anthropic-version: 2023-06-01`, `anthropic-beta: ...` when needed | SSE with `event:` + `data:` per frame: `message_start`, `content_block_delta`, `message_delta`, `message_stop` | Codec: `messages` → Anthropic `messages[]` + top-level `system` (extract first `role=system` message); `max_tokens` required and default 4096 if absent; tool_calls ↔ `tool_use` content blocks. Response: flatten Anthropic `content[]` text blocks and emit `choices[0].message.content`. Usage: `input_tokens`/`output_tokens` → `prompt_tokens`/`completion_tokens`. |
| **gemini** | `gemini` | `chat_completions` + non-stream → `/v1beta/models/{ProviderModelID}:generateContent`; stream → `/v1beta/models/{ProviderModelID}:streamGenerateContent?alt=sse` | `x-goog-api-key: {APIKey}` (preferred) or `?key=` query | SSE with `alt=sse`; events are single JSON objects framed as `data:` lines | Codec: `messages` → `contents[].parts[].text`; `system` → top-level `systemInstruction`; `tools` / `toolConfig` mappings; response: `candidates[0].content.parts[].text` → assistant message; usage: `usageMetadata.promptTokenCount` / `candidatesTokenCount`. |
| **minimax** | `minimax` | `chat_completions` → `/v1/text/chatcompletion_pro` | `Authorization: Bearer {APIKey}` | Custom SSE: provider emits `data:` JSON with `choices[0].messages[]`, final frame has `usage` | Codec: `messages[]` with `role` → `messages[]` with `sender_type` (`USER`/`BOT`/`FUNCTION`) and `sender_name`; `bot_setting` + `reply_constraints` synthesized from canonical `system` + response constraints; response: extract `choices[0].messages[0].text`. |
| **bedrock** | `bedrock` | `chat_completions` → `/model/{ProviderModelID}/converse` (Converse API) or `/model/{ProviderModelID}/invoke` (legacy Invoke API) — Converse is the default; Invoke selected when `CallTarget.Extras["bedrock.api"] == "invoke"` | **AWS SigV4** signed per request using `CallTarget.Extras["aws.accessKey"]`, `Extras["aws.secretKey"]`, optional `Extras["aws.sessionToken"]`, `Extras["aws.region"]` | Bedrock streaming uses event-stream framing (`amazon.eventstream`); decoded into uniform `Chunk` stream | Codec: canonical → Converse API `messages[]` with `content[].text` blocks; `system` → top-level `system[]`; tools mapped to Converse `toolConfig`. Response: Converse `output.message.content[]` → canonical assistant message. Usage: `usage.inputTokens`/`outputTokens` → `prompt_tokens`/`completion_tokens`. **Egress only**: no native ingress route is mounted in this round. |
| **vertex** | `vertex` | `chat_completions` → `/v1/projects/{Extras["gcp.project"]}/locations/{Extras["gcp.region"]}/publishers/google/models/{ProviderModelID}:generateContent` (or `:streamGenerateContent` for streaming) | OAuth 2 access token derived from `CallTarget.Extras["gcp.serviceAccountJSON"]` (cached per-target with 50-minute TTL); injected as `Authorization: Bearer {token}` | Same SSE framing as Gemini public endpoint | Codec: shares the Gemini codec (Vertex uses the same Gemini wire schema). The only difference vs `spec_gemini` is URL construction and the OAuth token exchange in `Transport.ApplyAuth`. **Egress only**: no native ingress route is mounted in this round. |

**Stream adapter IDs** must match `shared/traffic/adapters` IDs exactly: `openai` → `openai-compat` adapter slot used by hooks; `deepseek` → `deepseek`; `glm` → `glm`; `azure-openai` → `azure-openai`; `anthropic` → `anthropic`; `gemini` → `gemini`; `minimax` → `minimax`; `bedrock` → `bedrock`; `vertex` → `vertex`. No string drift. The single bridge `Format → traffic adapter ID` is in s5's `formatToTrafficAdapterID`; only `openai → openai-compat` is a non-identity rename, the other eight are identity.

### `Probe` per provider

| Provider | Probe |
|---|---|
| openai / deepseek | `GET {BaseURL}/v1/models`. 2xx ⇒ OK. |
| glm | `GET {BaseURL}/api/paas/v4/models`. 2xx ⇒ OK. |
| azure-openai | `GET {BaseURL}/openai/deployments?api-version={apiVersion}`. 2xx ⇒ OK. |
| anthropic | Short `POST /v1/messages` with `max_tokens=1`, `messages=[{"role":"user","content":"ping"}]`, `stream=false`. 2xx or quota-style 429 ⇒ OK (server reachable, key valid). |
| gemini | `GET {BaseURL}/v1beta/models` with key query. 2xx ⇒ OK. |
| minimax | `POST /v1/text/chatcompletion_pro` with minimal body. 2xx ⇒ OK. |
| bedrock | `GET https://bedrock.{region}.amazonaws.com/foundation-models` (the management endpoint, NOT bedrock-runtime) signed with SigV4. 2xx ⇒ OK. Probe uses the management host because Converse/Invoke return 4xx for an empty body. |
| vertex | `GET https://{region}-aiplatform.googleapis.com/v1/projects/{project}/locations/{region}/publishers/google/models` with the OAuth bearer. 2xx ⇒ OK. |

All probes wrap a 5 s timeout and never stream. Errors flow through `ErrorNormalizer`.

### `builtins.go`

```go
func RegisterBuiltins(reg *Registry, log *slog.Logger) {
    reg.MustRegister(NewSpecAdapter(spec_openai.NewSpec(log), log))
    reg.MustRegister(NewSpecAdapter(spec_deepseek.NewSpec(log), log))
    reg.MustRegister(NewSpecAdapter(spec_glm.NewSpec(log), log))
    reg.MustRegister(NewSpecAdapter(spec_azure_openai.NewSpec(log), log))
    reg.MustRegister(NewSpecAdapter(spec_anthropic.NewSpec(log), log))
    reg.MustRegister(NewSpecAdapter(spec_gemini.NewSpec(log), log))
    reg.MustRegister(NewSpecAdapter(spec_minimax.NewSpec(log), log))
    reg.MustRegister(NewSpecAdapter(spec_bedrock.NewSpec(log), log))
    reg.MustRegister(NewSpecAdapter(spec_vertex.NewSpec(log), log))
}
```

A startup invariant test (`builtins_test.go`) asserts `len(Registry.List()) == len(Format enum) == 9` and that the registered set equals the enum exactly. Any future addition has to update both sides or fail at boot.

### Extras on `CallTarget`

Extend the s1 `CallTarget` with a minimal extras map to carry provider-specific data that is not universal:

```go
type CallTarget struct {
    // ...existing fields
    Extras map[string]string
}
```

Reserved key namespace (string keys, dot-separated):

| Key | Used by | Source |
|---|---|---|
| `azure.apiVersion` | spec_azure_openai | provider catalog row |
| `bedrock.api` | spec_bedrock | provider catalog row, default `"converse"` |
| `aws.accessKey`, `aws.secretKey`, `aws.sessionToken`, `aws.region` | spec_bedrock | credential vault (per-credential row), region from provider row |
| `gcp.project`, `gcp.region` | spec_vertex | provider catalog row |
| `gcp.serviceAccountJSON` | spec_vertex | credential vault (raw SA JSON) |

`provtarget.Resolver` populates `Extras` from the provider catalog and credential vault — callers (executor / smart / aiguard) never touch the map.

### Unit tests per adapter

Each `spec_*/spec_test.go` must cover:

1. **URL build** — Golden-file tests for `Transport.BuildURL` across each supported `Endpoint` and `Stream=true/false`.
2. **Auth header rewrite** — Ingress request has no auth; after `ApplyAuth`, the correct header is set and **no** `Authorization` leaks from caller headers.
3. **Codec round-trip** — A canonical OpenAI `chat/completions` body is encoded to native and back to canonical; non-ambiguous fields must be stable.
4. **Error normalize** — 3 representative provider error payloads map to the right canonical `Code`.
5. **Stream decode** — A captured golden stream (fixture in `testdata/`) decodes to the expected `Chunk` sequence with correct `Usage` at terminal.

Fixtures are stored under `packages/ai-gateway/internal/providers/spec_<name>/testdata/` and are the single golden source for regression tests.

## Acceptance Criteria

- Every built-in format registers successfully at startup; `Registry.List()` returns exactly 9 formats and the set equals the `Format` enum.
- `go test -race -count=1 ./packages/ai-gateway/internal/providers/...` passes (new scaffolding + per-adapter tests for all 9 specs).
- No remaining references to `BaseAdapter`, `defaultPrepareRequest`, `PrepareRequestFn`, `*Fn` hooks, or `NormalizeProviderError` anywhere in the repo.
- Every adapter's `Format()` return value is a member of the `Format` enum declared in s1.
- Streaming integration test — one per adapter — sends a canonical canonical-body chat completion with `stream=true` to a fake upstream and observes the expected uniform `Chunk` stream.
- Bedrock SigV4 signature is byte-identical to a reference implementation (`aws-sdk-go-v2/v4` request signer) on a fixture request.
- Vertex OAuth token exchange is unit-tested with a fixture SA JSON; cached token is reused within TTL and refreshed on the boundary.

## Out of scope

- Live integration tests against vendor servers (defer to manual Probe smoke tests).
- Bedrock / Vertex **native ingress** routes — egress only this round (see s4 §1 and the requirements doc FR-13a).
- Bedrock Invoke API beyond what is needed to support `Extras["bedrock.api"] == "invoke"` for legacy customers; the default and primary code path is Converse.
- Cross-region Vertex / Bedrock failover. The provider catalog row carries one region per credential.

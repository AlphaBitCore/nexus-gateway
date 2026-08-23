# OpenAI SDK compatibility

The Nexus AI Gateway speaks the OpenAI wire protocol. An unmodified OpenAI SDK
reaches it by changing two things — the base URL and the API key — and nothing
else.

```python
from openai import OpenAI

client = OpenAI(
    base_url="https://your-gateway.example.com/nexus/v1",
    api_key="nvk_your_virtual_key",
)
```

```javascript
import OpenAI from 'openai'

const client = new OpenAI({
  baseURL: 'https://your-gateway.example.com/nexus/v1',
  apiKey: 'nvk_your_virtual_key',
})
```

Your Nexus virtual key replaces the provider key. The gateway holds the real
provider credentials, so no provider key ever reaches the client.

## Tested SDK versions

| SDK | Package | Version under test |
| --- | --- | --- |
| Python | `openai` | ≥ 1.55.0 (the regression suite records the resolved version per run) |
| Node | `openai` | 6.49.0 |

The regression suites live at `tests/e2e-python/sdk_compat/` (60 cases) and
`tests/e2e-node/` (22 cases).

## Supported surface

| Capability | Status | Notes |
| --- | --- | --- |
| `chat.completions.create` | Supported | Full OpenAI envelope, populated `usage` |
| `chat.completions.create(stream=True)` | Supported | `chat.completion.chunk` frames, content deltas, terminal `finish_reason` |
| `embeddings.create` | Supported | String, string array, and token-array input |
| `embeddings` + `dimensions` | Model-dependent | Honoured when the model declares the value; see [Embeddings](#embeddings) |
| `embeddings` + `encoding_format="base64"` | Supported | Guaranteed for every embedding model, whatever the provider wire emits |
| Tool / function calling | Supported | Including the `role: "tool"` result round-trip |
| `parallel_tool_calls` | Supported | Also mapped onto non-OpenAI targets |
| `tool_choice` (`auto`/`required`/`none`) | Supported | |
| Streamed tool-call arguments | Supported | Fragments reassemble into parseable JSON |
| `response_format: json_object` | Supported | Falls back to a prompt instruction on targets with no JSON mode |
| `response_format: json_schema` (strict) | OpenAI-family targets | Rejected with a 400 on Anthropic targets — see divergences |
| Vision (`image_url`, data URI) | Supported | Converted to each target's native image shape |
| Reasoning (`reasoning_effort`, `reasoning_tokens`) | OpenAI-family targets | Dropped on targets with no equivalent |
| `models.list` / `models.retrieve` | Supported | Plus Nexus capability fields (below) |
| `n`, `seed`, `logprobs`, `stop`, `temperature`, `top_p`, `user` | Supported | Dropped on non-OpenAI targets — see divergences |
| `/v1/completions` (legacy) | Not available | Use `/v1/chat/completions` |
| Moderations | Not available | |

### Catalog extension fields

`GET /v1/models` returns the standard OpenAI fields plus Nexus extensions, so a
caller can pick a model without a second round-trip: `type`, `features`,
`inputModalities`, `outputModalities`, `maxContextTokens`, `maxOutputTokens`,
`pricing`, and `capabilityJson`. Extra fields are ignored by both SDKs.

### Embeddings

`dimensions` is validated against the model's `supported_dimensions` in
`capabilityJson.embeddings`, rather than forwarding a value the provider will
reject. OpenAI's `text-embedding-3-*` models offer several; Cohere's
`embed-*-v3.0` models support only their default.

`encoding_format` needs no such check — both `float` and `base64` work on every
embedding model. Where a provider wire cannot emit base64 (Cohere has no base64
embedding type; Gemini and Bedrock always return floats), the gateway requests
floats and encodes them to base64 itself, byte-for-byte as api.openai.com does.

This matters more than it looks: **both official SDKs request base64 implicitly**
when you do not pass `encoding_format`, then decode the reply as packed float32.
So a plain `client.embeddings.create(model=…, input=…)` depends on base64 being
honoured — which is why it is a gateway guarantee rather than a per-model
capability.

## Deliberate divergences

These differ from api.openai.com on purpose. Each is pinned by a test, so a
silent change breaks a build rather than a caller.

| Divergence | Behaviour | Why |
| --- | --- | --- |
| `error.code` vocabulary | Nexus UPPER_SNAKE (`ROUTING_NO_MATCH`, `AUTH_INVALID_KEY`) instead of OpenAI's `model_not_found` / `invalid_api_key` | These codes are matched by the Control Plane UI and alert aggregators. OpenAI's `error.code` is a free-form string, so SDK callers lose nothing — `error.type` carries the standard vocabulary. |
| Streamed `json_object` on non-OpenAI targets | Content may arrive wrapped in a markdown code fence | Targets with no native JSON mode are steered by a system instruction, which a model can ignore. The gateway strips a stray fence on non-streaming responses; doing so mid-stream would mean buffering the whole completion and giving up time-to-first-token. Parse defensively if you stream `json_object` against a non-OpenAI model. |
| Streaming `usage` | A terminal usage frame arrives even without `stream_options.include_usage` | The gateway always meters cost. Both SDKs surface it as a chunk with empty `choices`; a consumer asserting OpenAI's exact frame sequence sees one extra frame. |
| `max_tokens` over the model ceiling | Clamped to the model's limit and disclosed in `X-Nexus-Coerced`; stock OpenAI returns 400 | Serving a slightly smaller completion beats failing the call. |
| `max_tokens` on `gpt-5*` / o-series | Renamed to `max_completion_tokens`, disclosed in `X-Nexus-Coerced` | Those wires reject `max_tokens`. |
| `temperature` / `top_p` on reasoning models | Stripped and disclosed; `gpt-5.4` is a probed carve-out that keeps them | Those wires 400 on caller-supplied sampling params. |
| OpenAI-only params on non-OpenAI targets | `n`, `seed`, `logprobs`, `user`, `service_tier` are dropped, not rejected — `n=2` returns one choice with a 200 | No equivalent exists on the target wire; failing the request would be worse than serving it. |
| `reasoning_effort` on non-OpenAI targets | Dropped silently | As above. |
| Strict `json_schema` on Anthropic targets | Hard 400 naming `response_format` | Anthropic has no schema-enforced mode. Degrading an output *contract* to a prompt hint would risk a silently wrong answer — unlike `json_object`, where a prompt hint is a fair degradation. |

### Error mapping

The SDKs choose their exception class from the HTTP status, so standard
error handling works unchanged:

| Condition | Status | SDK exception | `error.type` | `error.code` |
| --- | --- | --- | --- | --- |
| Unknown / unroutable model | 404 | `NotFoundError` | `not_found_error` | `ROUTING_NO_MATCH` |
| Missing or invalid virtual key | 401 | `AuthenticationError` | `authentication_error` | `AUTH_INVALID_KEY` |
| Missing `model` field | 400 | `BadRequestError` | `invalid_request_error` | `MODEL_REQUIRED` |
| Model outside the key's allowlist | 403 | `PermissionDeniedError` | `permission_error` | `MODEL_NOT_ALLOWED` |
| Unsupported endpoint | 404 | `NotFoundError` | `not_found_error` | `ENDPOINT_NOT_SUPPORTED` |
| All upstreams failed | 502 | `InternalServerError` | `api_error` | `PROVIDER_UNAVAILABLE` |
| No provider target could be prepared | 500 | `InternalServerError` | `api_error` | `PROVIDER_TARGET_UNAVAILABLE` |
| Model catalog not loaded yet | 503 | `InternalServerError` | `api_error` | `MODEL_CATALOG_UNAVAILABLE` |

Errors always carry a JSON body with a `message`, including on endpoints the
gateway does not serve, so `err.message` is never empty.

The last two rows look similar and mean opposite things. `PROVIDER_UNAVAILABLE`
means providers were called and every one of them failed — an upstream incident,
and worth retrying. `PROVIDER_TARGET_UNAVAILABLE` means no provider was called at
all: the gateway could not prepare a target for the request, typically because
the credential behind it is missing or cannot be decrypted. Retrying that will
not help until an operator fixes the configuration, which is why it is a `500`
rather than a `502` — SDKs treat `502` as transient and will back off and try
again, turning one broken credential into sustained load.

`MODEL_CATALOG_UNAVAILABLE` is the third member of that family and points the
other way. `ROUTING_NO_MATCH` is a `404`: the model is not in the catalog, and a
client should stop asking. `MODEL_CATALOG_UNAVAILABLE` is a `503`: the catalog
had not finished loading, so the model was never looked up at all — the response
says nothing about whether it exists, and retrying shortly is the right move.
The two shared a `404` until 2026-08-19, which meant a gateway that had just
restarted told clients their models had been deleted.

## See also

- [`../features/`](../features/) — product feature documentation
- [`openapi/`](./openapi/) — OpenAPI 3.1 specifications
- `docs/developers/architecture/services/ai-gateway/ingress-api.md` — the
  ingress contract and the engineering detail behind each divergence

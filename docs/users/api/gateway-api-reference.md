# AI Gateway API Reference

Complete reference for the Nexus AI Gateway data plane: every endpoint, its parameters, its
response shape, and the errors it can return.

For a narrative introduction — how to point an existing SDK at the gateway and what happens when
you do — read [gateway-api.md](gateway-api.md) first. This document is the lookup table.

## Base URL

```bash
export NEXUS_URL="https://api.<your-domain>"
export NEXUS_KEY="<your virtual key>"
```

Every path below is relative to that base. All requests are HTTPS.

---

## 1. The ingress surface

The gateway serves three request dialects. A dialect fixes five things at once: the paths it
answers, the authentication headers it accepts, the request body it parses, the response envelope
it returns, and the error envelope it returns. Pick the dialect your SDK already speaks; you do not
have to match it to the model you want.


|                       | **OpenAI**                                       | **Anthropic**                                                 | **Gemini**                                          |
| --------------------- | ------------------------------------------------ | ------------------------------------------------------------- | --------------------------------------------------- |
| Chat                  | `POST /v1/chat/completions`                      | `POST /v1/messages`                                           | `POST /v1beta/models/{model}:generateContent`       |
| Chat, streaming       | same path, `"stream": true`                      | same path, `"stream": true`                                   | `POST /v1beta/models/{model}:streamGenerateContent` |
| Responses API         | `POST /v1/responses`                             | —                                                             | —                                                   |
| Embeddings            | `POST /v1/embeddings`                            | —                                                             | —                                                   |
| Image generation      | `POST /v1/images/generations`                    | —                                                             | —                                                   |
| Text to speech        | `POST /v1/audio/speech`                          | —                                                             | —                                                   |
| Speech to text        | `POST /v1/audio/transcriptions`                  | —                                                             | —                                                   |
| Model catalog         | `GET /v1/models` `GET /v1/models/{model}`        | same paths, Anthropic-shaped when `anthropic-version` is sent | —                                                   |
| Auth headers accepted | `Authorization: Bearer` `X-Nexus-Virtual-Key`    | the two above, plus `x-api-key`                               | the two above, plus `x-goog-api-key`                |
| Success envelope      | OpenAI                                           | Anthropic (`{"type":"message",…}`)                            | Gemini (`{"candidates":[…]}`)                       |
| Error envelope        | `{"error":{message,type,code}}`                  | `{"type":"error","error":{…}}`                                | `{"error":{code,message,status}}`                   |
| Streaming frames      | `data: {…}` chunks, terminated by `data: [DONE]` | named SSE events (`message_start` … `message_stop`)           | `data: {…}` frames                                  |


Endpoints below are dialect-neutral — they have no vendor standard, so they exist once:


| Endpoint                             | Purpose                                                  |
| ------------------------------------ | -------------------------------------------------------- |
| `POST /v1/rerank`                    | Rerank documents against a query                         |
| `POST /v1/guardrail`                 | Run the content policy over text without calling a model |
| `POST /v1/estimate`                  | Price a request across up to 10 targets before sending it |
| `GET /v1/usage`                      | This key's spend for the current period                  |
| `GET /v1/usage/daily`                | This key's spend per day, broken out by model            |
| `GET /api/v1/open/models`            | Public model catalog — no key required                   |
| `GET /api/v1/open/models/{model_id}` | Public detail for one model — no key required            |


`**model` is the only routing input.** Send a model name and the gateway selects the provider,
translates your request onto that provider's wire, and translates the answer back into your
dialect's envelope. On chat endpoints you may send `"model": "auto"` and let the gateway choose the
model as well; `auto` is not accepted on `/v1/embeddings`.

Two consequences worth stating plainly:

- **The model that serves you may not be the model you named.** A deployment can carry routing
rules that substitute a target. When that happens the response's `model` field and the
`X-Nexus-Routed-Model` response header both report the model that actually served.
- **A dialect's capabilities are the dialect's, not the model's.** The Anthropic Messages protocol
defines no audio content block, so there is no way to send audio on `/v1/messages` even to a
model that hears — use `/v1/chat/completions`, `/v1/responses`, or the Gemini path, which all
define one.

---

## 2. Authentication

Every data-plane endpoint requires a virtual key, except the two public catalog endpoints under
`/api/v1/open/`.

The gateway reads the key from the first of these that is present:


| Header                        | Accepted on                        |
| ----------------------------- | ---------------------------------- |
| `X-Nexus-Virtual-Key: <key>`  | every endpoint                     |
| `Authorization: Bearer <key>` | every endpoint                     |
| `x-api-key: <key>`            | `POST /v1/messages` only           |
| `x-goog-api-key: <key>`       | `POST /v1beta/models/{model}` only |


The Google convention of passing the key as a `?key=` query parameter is **not** accepted; a
request that carries the key only that way is answered `401`.

```bash
curl -sS $NEXUS_URL/v1/models -H "Authorization: Bearer $NEXUS_KEY"
```

### Authentication failures


| Status | `code`                     | Meaning                                                  |
| ------ | -------------------------- | -------------------------------------------------------- |
| 401    | `AUTH_KEY_MISSING`         | no key on the request                                    |
| 401    | `AUTH_INVALID_KEY`         | the key is not recognised                                |
| 401    | `AUTH_KEY_DISABLED`        | the key exists but is disabled                           |
| 401    | `AUTH_KEY_EXPIRED`         | the key has passed its expiry                            |
| 403    | `MODEL_NOT_ALLOWED`        | the key is valid but not entitled to the requested model |
| 503    | `AUTH_BACKEND_UNAVAILABLE` | the key could not be looked up at all                    |


```json
{
  "error": {
    "code": "AUTH_INVALID_KEY",
    "hint": "Verify your virtual key is correct",
    "message": "vkauth: virtual key invalid",
    "type": "authentication_error"
  }
}
```

---

## 3. Request and response conventions

### Request headers

Beyond authentication, the gateway reads these. All are optional.


| Header                           | Effect                                                                                                                                                       |
| -------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `Content-Type: application/json` | required on every JSON endpoint                                                                                                                              |
| `anthropic-version: 2023-06-01`  | conventional on `/v1/messages`; never validated. Its **presence** also switches `GET /v1/models` and `GET /v1/models/{model}` to the Anthropic catalog shape |
| `x-request-id`                   | your own request id. Recorded against the request, and echoed back on the response                                                                            |
| `X-Nexus-Request-Id`             | the trace id for a unit of work spanning several calls. Honoured when you send one, minted otherwise                                                          |
| `X-Nexus-End-User-Id`            | opaque end-user attribution tag, trimmed and capped at 256 bytes                                                                                             |
| `X-Nexus-Session-Id`             | opaque session attribution tag, same cap                                                                                                                     |
| `X-Nexus-Client-Tags`            | opaque `key=value` bag, at most 8 pairs                                                                                                                      |
| `X-Nexus-No-Cache`               | bypass the response cache for this request                                                                                                                   |


Attribution comes from these headers only. The gateway does not read a protocol's own end-user
field — OpenAI's `user`, Anthropic's `metadata.user_id` — for attribution; those are forwarded to
the provider untouched.

Headers you send are not relayed upstream wholesale: the gateway forwards an allowlist, and every
`x-nexus-` header you send is dropped before the request leaves.

### Response headers


| Header                                                   | When                               | Meaning                                                                        |
| -------------------------------------------------------- | ---------------------------------- | ------------------------------------------------------------------------------ |
| `X-Nexus-Via`                                            | always                             | hop marker, `ai-gateway`                                                       |
| `X-Nexus-Mode` `X-Nexus-Hook`                            | always                             | per-hop chains aligned 1:1 with `X-Nexus-Via`. The gateway has no mode, so its position is empty; read them by position, not by presence |
| `X-Nexus-Request-Id`                                     | always                             | the id to quote when reporting a problem                                       |
| `x-request-id`                                           | when you sent one, or the provider returned one | yours if you sent one, otherwise the provider's — one value, never both     |
| `X-Nexus-Attempts`                                       | always                             | upstream attempts made, at least 1                                             |
| `X-Nexus-Cache`                                          | JSON proxy routes                  | `HIT` or `MISS`. Endpoints that are never cached always report `MISS`          |
| `X-Nexus-Hook`                                           | always                             | compliance pipeline outcome, e.g. `passed:pii-scanner`                         |
| `X-Nexus-Routed-Model` `X-Nexus-Routed-Provider`         | when routing substituted the model | what actually served the request                                               |
| `X-Nexus-Coerced`                                        | when a parameter was rewritten     | comma-separated list of the rewrites applied, e.g. `max_tokens→4096_model_max` |
| `X-Nexus-Quota-Used` `X-Nexus-Quota-Limit`               | when a quota is configured         | dollars used and the ceiling                                                   |
| `X-Nexus-Quota-Warning`                                  | quota mode `notify-and-proceed`    | the warning text                                                               |
| `X-Nexus-Quota-Downgrade` `X-Nexus-Quota-Original-Model` | quota mode `downgrade`             | the model you asked for, and that it was swapped                               |
| `Server-Timing`                                          | non-streaming responses            | `gw;dur=<ms>`, plus upstream timing breakdowns                                 |
| `Retry-After`                                            | on 429                             | seconds to wait                                                                |


`X-Nexus-Coerced` is worth reading in development: it is how the gateway tells you it changed your
request to make it acceptable to the model that served it.

### Streaming

Send `"stream": true` on the OpenAI and Anthropic chat endpoints, or call the Gemini
`:streamGenerateContent` action. The response is `Content-Type: text/event-stream` and the frame
sequence is your dialect's, not the upstream's.

On `/v1/chat/completions` the gateway sets `stream_options.include_usage` to `true` for you when
you omit it, so the final chunk carries token counts, and the stream terminates with
`data: [DONE]`.

The terminating sentinel is dialect-specific and only that endpoint family emits it:


| Endpoint                                       | Stream ends with                                                         |
| ---------------------------------------------- | ------------------------------------------------------------------------ |
| `/v1/chat/completions`                         | `data: [DONE]`                                                           |
| `/v1/responses`                                | a terminal Responses event; **no `[DONE]`**                              |
| `/v1/messages`                                 | the `message_stop` event; **no `[DONE]`**                                |
| `/v1beta/models/{model}:streamGenerateContent` | the last `data:` frame, the one carrying `finishReason`; **no `[DONE]`** |


Once a streaming response has begun the HTTP status is committed; a failure after that point
arrives as an in-band error frame, not as a status code.

### Request size limits


| Endpoint kind                                         | Body ceiling                     |
| ----------------------------------------------------- | -------------------------------- |
| chat, responses, embeddings, rerank, messages, Gemini | 10 MiB (deployment-configurable) |
| image generation, text to speech                      | 256 KiB                          |
| speech to text                                        | 26 MiB                           |
| guardrail                                             | 1 MiB                            |


Exceeding a ceiling returns `413`, with a code that depends on the endpoint:
`PAYLOAD_TOO_LARGE` on the JSON proxy routes, `STT_UPLOAD_TOO_LARGE` on transcription, and
`GUARDRAIL_BODY_TOO_LARGE` on the guardrail endpoint.

These endpoints also cap concurrent in-flight requests per key — 4 for image generation, 8 for text
to speech, 4 for speech to text, 4 for guardrail — and answer `429 GENERATIVE_CONCURRENCY_LIMIT`
with `Retry-After: 1` above that.

### Response caching

Only `/v1/chat/completions`, `/v1/responses`, `/v1/messages` and the Gemini chat path are
cacheable. Embeddings, image generation, speech, transcription, and rerank are never cached.
`X-Nexus-Cache` reports the outcome; `X-Nexus-No-Cache` suppresses it for one request.

### Gateway extensions in the request body

Provider-specific options that have no home in your dialect's schema ride under a root `nexus` key
and are stripped before the request leaves the gateway:

```json
{
  "model": "embed-v4.0",
  "input": "hello",
  "nexus": { "ext": { "cohere": { "input_type": "search_document" } } }
}
```

Keys are namespaced per provider — `nexus.ext.cohere.*` (`input_type`, `embedding_types`,
`truncate`), `nexus.ext.gemini.*` (`taskType`, `title`, `thinking_config`, `batch`),
`nexus.ext.anthropic.thinking`, plus `nexus.ext.voyage.*` and `nexus.ext.bedrock.*`. The `nexus`
key is rejected on `/v1/images/generations`.

---

## 4. OpenAI dialect

### 4.1 Create a chat completion

`POST /v1/chat/completions`

Generates a model response for a conversation. Any chat model in the catalog can serve this
endpoint, whichever vendor it belongs to.

**Request body**


| Parameter               | Type            | Required | Description                                                                                                                                                                                                          |
| ----------------------- | --------------- | -------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `model`                 | string          | **yes**  | Model id from `GET /v1/models`, or `"auto"` to let the gateway choose. An empty or absent value returns `400 MODEL_REQUIRED`.                                                                                        |
| `messages`              | array           | **yes**  | The conversation. Content may be a plain string or an array of content parts — `text`, `image_url`, `input_audio`, `file`.                                                                                           |
| `stream`                | boolean         | no       | Return server-sent events instead of one JSON body. Defaults to `false`.                                                                                                                                             |
| `stream_options`        | object          | no       | When streaming, the gateway sets `include_usage` to `true` if you omit it, so the final chunk carries token counts.                                                                                                  |
| `max_tokens`            | integer         | no       | Output ceiling. Clamped down to the model's advertised maximum when you ask for more, and renamed to `max_completion_tokens` for models that require that spelling. Either rewrite is reported in `X-Nexus-Coerced`. |
| `max_completion_tokens` | integer         | no       | Same clamp as `max_tokens`.                                                                                                                                                                                          |
| `temperature`           | number          | no       | Removed before dispatch for models that reject sampling parameters; reported as `temperature→removed`.                                                                                                               |
| `top_p`                 | number          | no       | Same handling as `temperature`.                                                                                                                                                                                      |
| `tools`                 | array           | no       | Function tools, in OpenAI's schema.                                                                                                                                                                                  |
| `tool_choice`           | string | object | no       | Forwarded unchanged.                                                                                                                                                                                                 |
| `response_format`       | object          | no       | `{"type":"json_object"}` or `{"type":"json_schema",…}`. Requesting `json_schema` narrows routing to models that support structured output.                                                                           |
| `modalities`            | array           | no       | Removed for models that do not accept the field; audio models keep it.                                                                                                                                               |
| `reasoning_effort`      | string          | no       | Forced to `"none"` on the one model family that rejects it alongside function tools, and only when `tools` is a non-empty array.                                                                                     |
| `nexus`                 | object          | no       | Gateway extensions; see §3. Stripped before dispatch.                                                                                                                                                                |


Every other OpenAI chat parameter — `n`, `stop`, `seed`, `presence_penalty`, `frequency_penalty`,
`logit_bias`, `logprobs`, `user`, … — is forwarded unchanged **when an OpenAI-shape model serves
the request**. When the request is translated onto the Anthropic or Gemini wire the body is rebuilt
from the parameters those protocols define, and ones with no counterpart — `n`,
`seed`, `presence_penalty`, `frequency_penalty`, `logit_bias`, `logprobs`, `top_logprobs`, `user`
— are not carried. `tool_choice` is
translated rather than forwarded. `X-Nexus-Routed-Provider` tells you which case you are in.

**Returns** — a chat completion object. `usage.prompt_tokens` is the total input count; when the
prompt cache was used, `usage.prompt_tokens_details.cached_tokens` is a **subset already inside**
`prompt_tokens`, so adding them double-counts.

```bash
curl -sS $NEXUS_URL/v1/chat/completions \
  -H "Authorization: Bearer $NEXUS_KEY" \
  -H 'content-type: application/json' \
  -d '{
    "model": "moonshot-v1-128k",
    "max_tokens": 8,
    "messages": [{"role": "user", "content": "Reply with only: ok"}]
  }'
```

```json
{
  "id": "chatcmpl-6a8904a9e75ac1ced699738f",
  "object": "chat.completion",
  "created": 1787364521,
  "model": "moonshot-v1-128k",
  "choices": [
    {
      "index": 0,
      "message": { "role": "assistant", "content": "ok" },
      "finish_reason": "stop"
    }
  ],
  "usage": { "prompt_tokens": 20, "completion_tokens": 2, "total_tokens": 22 }
}
```

**Streaming.** Add `"stream": true`. Each frame is a `chat.completion.chunk`; the final frame
carries `usage` and the stream ends with `data: [DONE]`.

```json
data: {"id":"chatcmpl-…","object":"chat.completion.chunk","created":1787364537,"model":"moonshot-v1-128k","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}

data: {"id":"chatcmpl-…","object":"chat.completion.chunk","created":1787364537,"model":"moonshot-v1-128k","choices":[],"usage":{"prompt_tokens":18,"completion_tokens":2,"total_tokens":20}}

data: [DONE]
```

#### Sending images

An image is a content part. Inline base64 works on every vision model; a remote URL works only on
providers that fetch one, and the gateway tells you when the routed provider does not.

```bash
B64=$(base64 -i photo.png | tr -d '\n')
curl -sS $NEXUS_URL/v1/chat/completions \
  -H "Authorization: Bearer $NEXUS_KEY" -H 'content-type: application/json' \
  -d "{\"model\":\"gpt-4o-mini\",\"max_tokens\":24,\"messages\":[{\"role\":\"user\",\"content\":[
        {\"type\":\"image_url\",\"image_url\":{\"url\":\"data:image/png;base64,$B64\"}},
        {\"type\":\"text\",\"text\":\"What number is in this image? Digits only.\"}]}]}"
```

#### Sending documents

A document is a `file` part and its `file_data` must be a `data:` URL — a bare base64 string or an
`https://` link is refused, because the gateway does not fetch documents by URL.

```bash
DOC=$(base64 -i runbook.md | tr -d '\n')
curl -sS $NEXUS_URL/v1/chat/completions \
  -H "Authorization: Bearer $NEXUS_KEY" -H 'content-type: application/json' \
  -d "{\"model\":\"gpt-4o-mini\",\"max_tokens\":24,\"messages\":[{\"role\":\"user\",\"content\":[
        {\"type\":\"file\",\"file\":{\"filename\":\"runbook.md\",
          \"file_data\":\"data:text/markdown;base64,$DOC\"}},
        {\"type\":\"text\",\"text\":\"What is the reference number? Digits only.\"}]}]}"
```

Providers differ in what they can do with a document, and a provider that cannot carry one refuses
with a message naming the media type and what to send instead. It never drops the attachment and
answers anyway.

#### Sending audio

Audio is an `input_audio` part carrying raw base64 — note this part takes bare base64, not a
`data:` URL, and `format` names the container.

```bash
WAV=$(base64 -i speech.wav | tr -d '\n')
curl -sS $NEXUS_URL/v1/chat/completions \
  -H "Authorization: Bearer $NEXUS_KEY" -H 'content-type: application/json' \
  -d "{\"model\":\"gpt-audio-mini\",\"max_tokens\":64,\"messages\":[{\"role\":\"user\",\"content\":[
        {\"type\":\"input_audio\",\"input_audio\":{\"data\":\"$WAV\",\"format\":\"wav\"}},
        {\"type\":\"text\",\"text\":\"Transcribe the attached audio exactly. Reply with the transcript only.\"}]}]}"
```

Send the whole file. A container cut short — a wav truncated mid-stream, for instance — still
carries a header describing the length it was supposed to have, and providers reject it as a
malformed format rather than transcribing what arrived.

Audio models are the one place a model may *require* a modality: an entry whose
`requiredModalities` includes `audio` cannot serve a plain text prompt at all.

### 4.2 Create a response

`POST /v1/responses`

OpenAI's Responses API shape. Accepts models that do not natively serve it — the gateway
translates to the chat wire and builds the Responses envelope on the way back.

**Request body**


| Parameter              | Type            | Required | Description                                                                                                                                                                                                                                                              |
| ---------------------- | --------------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `model`                | string          | **yes**  | Model id, or `"auto"`.                                                                                                                                                                                                                                                   |
| `input`                | string | array  | no       | A bare string becomes one user message. As an array, each item is an input item; content parts are `input_text`, `input_image`, `input_file`.                                                                                                                            |
| `instructions`         | string          | no       | Prepended as a system message.                                                                                                                                                                                                                                           |
| `max_output_tokens`    | integer         | no       | Output ceiling.                                                                                                                                                                                                                                                          |
| `temperature`, `top_p` | number          | no       | Same model-dependent removal as on the chat endpoint.                                                                                                                                                                                                                    |
| `stream`               | boolean         | no       | Server-sent events.                                                                                                                                                                                                                                                      |
| `tools`                | array           | no       | Caller-defined function tools stay on the chat wire; OpenAI's built-in tools (`web_search`, `file_search`, `code_interpreter`, `image_generation`, `mcp`, `computer_use_preview`, and siblings) are recognised and only work on a target that natively serves Responses. |
| `tool_choice`          | string | object | no       | Forwarded unchanged.                                                                                                                                                                                                                                                     |
| `text`                 | object          | no       | `text.format` maps to `response_format`.                                                                                                                                                                                                                                 |
| `reasoning`            | object          | no       | `reasoning.effort` maps to the chat wire's `reasoning_effort`.                                                                                                                                                                                                           |
| `parallel_tool_calls`  | boolean         | no       | Forwarded unchanged.                                                                                                                                                                                                                                                     |
| `metadata`             | object          | no       | Forwarded unchanged.                                                                                                                                                                                                                                                     |
| `previous_response_id` | string          | no       | Requires a target that natively serves Responses; otherwise `400`.                                                                                                                                                                                                       |
| `store`                | boolean         | no       | `true` requires a native Responses target; otherwise `400`.                                                                                                                                                                                                              |
| `truncation`           | string          | no       | On a non-native target only `"disabled"` is accepted.                                                                                                                                                                                                                    |
| `include`              | array           | no       | Forwarded unchanged.                                                                                                                                                                                                                                                     |


**Returns** — a response object.

```bash
curl -sS $NEXUS_URL/v1/responses \
  -H "Authorization: Bearer $NEXUS_KEY" \
  -H 'content-type: application/json' \
  -d '{
    "model": "moonshot-v1-128k",
    "max_output_tokens": 16,
    "input": "Reply with only: ok"
  }'
```

```json
{
  "id": "resp_1787364692882069382",
  "object": "response",
  "created_at": 1787364692,
  "model": "moonshot-v1-128k",
  "status": "completed",
  "output": [
    {
      "type": "message",
      "id": "msg_resp_1787364692882069382",
      "role": "assistant",
      "status": "completed",
      "content": [{ "type": "output_text", "text": "ok" }]
    }
  ],
  "usage": { "input_tokens": 22, "output_tokens": 2, "total_tokens": 24 }
}
```

**This endpoint's error envelope differs.** It carries no `hint` key — a hint is appended to
`message` in parentheses — and `code` is omitted rather than falling back to a number. Mid-stream
failures arrive as an `event: response.failed` frame. The rejections for Responses-only features
are the exception and do carry `param` and `code`.

### 4.3 Create embeddings

`POST /v1/embeddings`

**Request body**


| Parameter                     | Type           | Required | Description                                                                                                                                                                                                      |
| ----------------------------- | -------------- | -------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `model`                       | string         | **yes**  | Embedding model id. `"auto"` is **not** accepted here and returns `400`.                                                                                                                                         |
| `input`                       | string | array | **yes**  | One string, an array of strings, a token-id array, or an array of token-id arrays.                                                                                                                               |
| `dimensions`                  | integer        | no       | Must be a positive integer, and must fall within what the model supports — the gateway checks this before dispatch and names the offending parameter when it does not. Removed for models that do not accept it. |
| `encoding_format`             | string         | no       | `"float"` (default) or `"base64"`. When you ask for `base64` and the provider answers with float arrays, the gateway re-encodes them so you always get what you asked for.                                       |
| `nexus.ext.cohere.input_type` | string         | no       | Cohere `input_type`; validated against the model's supported set.                                                                                                                                                |
| `nexus.ext.gemini.taskType`   | string         | no       | Gemini task type; validated the same way.                                                                                                                                                                        |


Requesting a parameter no candidate model supports returns `400 NO_COMPATIBLE_CAPABILITY`, and the
response body lists each candidate's supported values under `available_capabilities` — dimensions,
batch size, encoding formats — so you can correct the request without guessing.

**Returns** — a list of embedding objects.

```bash
curl -sS $NEXUS_URL/v1/embeddings \
  -H "Authorization: Bearer $NEXUS_KEY" \
  -H 'content-type: application/json' \
  -d '{"model": "text-embedding-3-small", "input": "hello nexus"}'
```

```json
{
  "object": "list",
  "data": [
    { "object": "embedding", "index": 0, "embedding": [-0.0234985, -0.0088272, "…1536 floats"] }
  ],
  "model": "text-embedding-3-small",
  "usage": { "prompt_tokens": 2, "total_tokens": 2 }
}
```

There is no streaming form.

### 4.4 Create an image

`POST /v1/images/generations`

**Request body**


| Parameter                  | Type    | Required | Description                                                                                                                                              |
| -------------------------- | ------- | -------- | -------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `model`                    | string  | **yes**  | Image model id.                                                                                                                                          |
| `prompt`                   | string  | **yes**  | Must be a non-empty string. An array of strings is refused rather than joined.                                                                           |
| `n`                        | integer | no       | Number of images, at most 4. Some providers are narrower — Gemini image models accept only 1 — and the refusal names the bound it enforced.               |
| `size`                     | string  | no       | An OpenAI image size such as `1024x1024`. On providers that express aspect ratio instead, it is mapped and the mapping is reported in `X-Nexus-Coerced`. |
| `quality`, `style`, `user` | string  | no       | Forwarded where the provider takes them, dropped where it does not, with the drop reported in `X-Nexus-Coerced`.                                         |
| `response_format`          | string  | no       | `"b64_json"` or `"url"`. On providers that cannot mint a URL this is coerced to `b64_json`.                                                              |


`stream` is ignored — image generation is always non-streaming. The `nexus` extension key is not
accepted on this endpoint.

A prompt that trips a content rule is blocked with `403` even when the matching policy is
configured observe-only, because the resulting image cannot be scanned.

**Returns** — a list of generated images.

```bash
curl -sS $NEXUS_URL/v1/images/generations \
  -H "Authorization: Bearer $NEXUS_KEY" \
  -H 'content-type: application/json' \
  -d '{
    "model": "gemini-3.1-flash-lite-image",
    "prompt": "a single red circle on white background",
    "n": 1
  }'
```

```json
{ "created": 1787364720, "data": [{ "b64_json": "iVBORw0KGgoAAAANS…" }] }
```

### 4.5 Create speech

`POST /v1/audio/speech`

Synthesises speech from text. Served by OpenAI-format targets.

**Request body**


| Parameter         | Type           | Required | Description                                                                                                                                            |
| ----------------- | -------------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `model`           | string         | **yes**  | Text-to-speech model id.                                                                                                                               |
| `input`           | string | array | **yes**  | The text to speak. This is the only field scanned by the content policy on this endpoint, and its character count is what per-character pricing bills. |
| `voice`           | string         | no       | Forwarded unchanged.                                                                                                                                   |
| `response_format` | string         | no       | Audio container, e.g. `mp3`. Forwarded unchanged.                                                                                                      |
| `speed`           | number         | no       | Forwarded unchanged.                                                                                                                                   |
| `instructions`    | string         | no       | Forwarded unchanged.                                                                                                                                   |


`stream` is ignored — this endpoint is always non-streaming.

**Returns** — raw audio bytes, with the provider's own `Content-Type`. Not JSON.

```bash
curl -sS $NEXUS_URL/v1/audio/speech \
  -H "Authorization: Bearer $NEXUS_KEY" \
  -H 'content-type: application/json' \
  -d '{"model": "gpt-4o-mini-tts", "voice": "alloy", "input": "ok"}' \
  --output speech.mp3
```

```
HTTP/2 200
content-type: audio/mpeg
```

### 4.6 Create a transcription

`POST /v1/audio/transcriptions`

Transcribes audio. This endpoint takes `multipart/form-data`, not JSON — a request with a JSON
content type returns `400 STT_BAD_MULTIPART`.

**Form fields**


| Field             | Type | Required | Description                                                                                                             |
| ----------------- | ---- | -------- | ----------------------------------------------------------------------------------------------------------------------- |
| the audio file    | file | **yes**  | Identified by carrying a `filename`, whatever the field name — conventionally `file`. Exactly one file part is allowed. |
| `model`           | text | **yes**  | Transcription model id.                                                                                                 |
| `response_format` | text | no       | `json` (default), `verbose_json`, or `text`. `srt` and `vtt` are refused.                                               |
| `prompt`          | text | no       | Transcription hint. Scanned by the content policy and may be redacted before dispatch.                                  |
| `stream`          | text | no       | Streaming transcription is not served; `stream=true` returns `400 STT_FORMAT_UNSUPPORTED`.                              |


Any other form field is forwarded to the provider unchanged. The request may carry at most 16
parts, each non-file field at most 8 KiB, and the whole body at most 26 MiB.

**Returns** — a transcription object.

```bash
curl -sS $NEXUS_URL/v1/audio/transcriptions \
  -H "Authorization: Bearer $NEXUS_KEY" \
  -F file=@speech.mp3 \
  -F model=gpt-4o-mini-transcribe
```

```json
{
  "text": "OK.",
  "usage": {
    "type": "tokens",
    "input_tokens": 14,
    "input_token_details": { "text_tokens": 0, "audio_tokens": 14 },
    "output_tokens": 4,
    "total_tokens": 18
  }
}
```

That is the transcript of the `speech.mp3` the previous example wrote, so the two run as a pair.
The token counts track the length of the audio the speech call happened to produce, so they move
a little from run to run.

This route relays a small allowlisted set of the provider's own response headers — the upstream's
request id and its token rate-limit counters — so you can quote them to that provider's support and
throttle against their budget rather than guessing. The gateway's `X-Nexus-Via` and
`X-Nexus-Request-Id` are present as on every route; the routing, cache and quota markers are not,
because this route does not run those stages.

### 4.7 List models

`GET /v1/models`

Lists the models this key can reach. Takes no parameters, and requires a key — an unauthenticated
call returns `401`. For a catalog without a key, use `GET /api/v1/open/models`.

```bash
curl -sS $NEXUS_URL/v1/models -H "Authorization: Bearer $NEXUS_KEY"
```

```json
{
  "object": "list",
  "data": [
    {
      "id": "deepseek-v4-flash",
      "name": "DeepSeek V4 Flash",
      "object": "model",
      "created": 1787364253,
      "owned_by": "deepseek",
      "owner_display_name": "DeepSeek",
      "type": "chat",
      "inputModalities": ["text"],
      "outputModalities": ["text"],
      "features": ["function_calling", "streaming", "json_mode", "reasoning"],
      "maxContextTokens": 1048565,
      "maxOutputTokens": 393216,
      "lifecycle": "ga",
      "pricing": {
        "inputPerMillion": 0.14,
        "outputPerMillion": 0.28,
        "cachedInputReadPerMillion": 0.0028,
        "cachedInputWritePerMillion": 0,
        "currency": "USD",
        "unit": "per_million_tokens"
      }
    }
  ]
}
```


| Field                                  | Meaning                                                                                          |
| -------------------------------------- | ------------------------------------------------------------------------------------------------ |
| `type`                                 | `chat`, `embedding`, `image`, `tts`, `stt`, `rerank`, `realtime`, or `video`                     |
| `inputModalities` / `outputModalities` | what the model accepts and produces — this is the data routing uses                              |
| `requiredModalities`                   | present only when the model *requires* a modality; such a model cannot serve a plain text prompt |
| `features`                             | capability flags such as `function_calling`, `streaming`, `json_mode`, `reasoning`               |
| `maxContextTokens` / `maxOutputTokens` | context window and output ceiling                                                                |
| `lifecycle`                            | `ga`, or a pre-release or retirement stage                                                       |
| `pricing`                              | per-million-token rates, including cached-input rates where the model has them                   |


`GET /v1/models/{model}` returns one such object, unwrapped.

---

## 5. Anthropic dialect

### 5.1 Create a message

`POST /v1/messages`

Anthropic's Messages API. Any chat model can serve it, including models from other vendors — the
gateway translates both ways and always returns the Anthropic envelope.

Authenticate with `x-api-key`, or with `Authorization: Bearer`. `anthropic-version: 2023-06-01` is
accepted and conventional.

**Request body**


| Parameter                       | Type           | Required | Description                                                                                                                                                                                                                                |
| ------------------------------- | -------------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `model`                         | string         | **yes**  | Model id, or `"auto"`.                                                                                                                                                                                                                     |
| `messages`                      | array          | **yes**  | The conversation. `content` is a string or an array of blocks.                                                                                                                                                                             |
| `max_tokens`                    | integer        | no       | Anthropic requires this; the gateway fills it for you when absent, using the model's advertised output ceiling and falling back to 8192. A value above the model's ceiling is clamped down and the clamp is reported in `X-Nexus-Coerced`. |
| `system`                        | string | array | no       | System prompt.                                                                                                                                                                                                                             |
| `stream`                        | boolean        | no       | Server-sent events.                                                                                                                                                                                                                        |
| `temperature`, `top_p`, `top_k` | number         | no       | Removed before dispatch for models that reject sampling parameters.                                                                                                                                                                        |
| `stop_sequences`                | array          | no       | Forwarded.                                                                                                                                                                                                                                 |
| `tools`                         | array          | no       | `name`, `description` and `input_schema` per tool.                                                                                                                                                                                         |
| `tool_choice`                   | object         | no       | `auto`, `any`, `none`, or `tool` with a `name`.                                                                                                                                                                                            |
| `thinking`                      | object         | no       | `type` is `enabled`, `disabled`, or `adaptive` on the families that take it; `budget_tokens` sets the depth. On a non-Anthropic model this is translated into that model's reasoning control.                                              |
| `output_config.format`          | object         | no       | `{"type":"json_schema","schema":{…}}` for structured output.                                                                                                                                                                               |


Content blocks understood on a message: `text`, `image`, `document`, `thinking`,
`redacted_thinking`, `tool_use`, `tool_result`. There is **no audio block** in this protocol — to
send audio, use `/v1/chat/completions`, `/v1/responses`, or the Gemini path.

When an Anthropic model serves the request, fields the table does not name ride through untouched;
the gateway still stamps the resolved model, fills or clamps `max_tokens`, strips sampling
parameters where the family rejects them, and refits `thinking.budget_tokens` if it lowered
`max_tokens`. When another vendor's model serves it, the request is rebuilt from the fields named
above, and Anthropic fields outside that set — `metadata`, `service_tier`, `container`,
`mcp_servers` — are not carried.

**Returns** — a message object.

```bash
curl -sS $NEXUS_URL/v1/messages \
  -H "x-api-key: $NEXUS_KEY" \
  -H 'anthropic-version: 2023-06-01' \
  -H 'content-type: application/json' \
  -d '{
    "model": "moonshot-v1-128k",
    "max_tokens": 8,
    "messages": [{"role": "user", "content": "Reply with only: ok"}]
  }'
```

```json
{
  "id": "chatcmpl-6a8904cae75ac1ced69974cb",
  "type": "message",
  "role": "assistant",
  "model": "moonshot-v1-128k",
  "content": [{ "type": "text", "text": "ok" }],
  "stop_reason": "end_turn",
  "usage": { "input_tokens": 18, "output_tokens": 2 }
}
```

**Usage on this dialect is additive.** `input_tokens` counts only what was neither read from nor
written to the prompt cache; your input total is `input_tokens + cache_read_input_tokens + cache_creation_input_tokens`. This is the opposite convention from the OpenAI dialect, and both
are correct for their SDK.

**Streaming** emits named events — `message_start`, `ping`, `content_block_start`,
`content_block_delta`, `content_block_stop`, `message_delta`, `message_stop` — and the
authoritative token counts ride on `message_delta`. There is **no `[DONE]` sentinel** on this
dialect; the stream ends at `message_stop`.

**Errors** use the Anthropic envelope:

```json
{ "type": "error", "error": { "type": "invalid_request_error", "message": "…" } }
```

---

## 6. Gemini dialect

### 6.1 Generate content

`POST /v1beta/models/{model}:generateContent`
`POST /v1beta/models/{model}:streamGenerateContent`

Google's `generateContent` shape. The model id and the action share one path segment, separated by
a colon, and the action is how you choose streaming. The Gemini wire defines no `stream` field, so
do not send one: a body carrying `stream: true` on `:generateContent` puts the response on the
event-stream path anyway.

**Only these two actions are served.** `:embedContent`, `:countTokens`, `:batchEmbedContents` and
the rest return a plain-text `404`. Use `POST /v1/embeddings` for embeddings.

Authenticate with `x-goog-api-key`, or with `Authorization: Bearer`. The `?key=` query parameter is
not accepted.

**Request body**


| Parameter                                              | Type    | Required | Description                                                                |
| ------------------------------------------------------ | ------- | -------- | -------------------------------------------------------------------------- |
| `contents`                                             | array   | **yes**  | The conversation. Each entry has a `role` (`user` or `model`) and `parts`. |
| `contents[].parts[].text`                              | string  | no       | Text part.                                                                 |
| `contents[].parts[].inlineData`                        | object  | no       | `{"mimeType":…,"data":"<base64>"}` — images, audio, video, documents.      |
| `contents[].parts[].fileData`                          | object  | no       | `{"mimeType":…,"fileUri":…}`.                                              |
| `contents[].parts[].functionCall` / `functionResponse` | object  | no       | Tool turns.                                                                |
| `systemInstruction`                                    | object  | no       | System prompt, as `parts[].text`.                                          |
| `generationConfig.temperature` / `topP` / `topK`       | number  | no       | Sampling.                                                                  |
| `generationConfig.maxOutputTokens`                     | integer | no       | Output ceiling.                                                            |
| `generationConfig.stopSequences`                       | array   | no       | Stop strings.                                                              |
| `generationConfig.responseSchema`                      | object  | no       | Structured output.                                                         |
| `generationConfig.thinkingConfig`                      | object  | no       | `thinkingBudget` sets reasoning depth; `0` disables it.                    |
| `tools[].functionDeclarations`                         | array   | no       | Tool schemas.                                                              |
| `toolConfig.functionCallingConfig`                     | object  | no       | `mode` is `AUTO`, `NONE`, or `ANY`.                                        |


On a Gemini model the body rides through untouched. On another vendor's model the gateway rebuilds
it from the fields above, and Gemini fields outside that set — `safetySettings`, `cachedContent`,
`generationConfig.candidateCount`, `generationConfig.seed`, `generationConfig.responseMimeType` —
are not carried.

**Returns** — a `generateContent` response.

```bash
curl -sS "$NEXUS_URL/v1beta/models/moonshot-v1-128k:generateContent" \
  -H "x-goog-api-key: $NEXUS_KEY" \
  -H 'content-type: application/json' \
  -d '{
    "contents": [{"role": "user", "parts": [{"text": "Reply with only: ok"}]}],
    "generationConfig": {"maxOutputTokens": 8}
  }'
```

```json
{
  "candidates": [
    {
      "content": { "parts": [{ "text": "ok" }], "role": "model" },
      "finishReason": "STOP",
      "index": 0
    }
  ],
  "modelVersion": "moonshot-v1-128k",
  "responseId": "chatcmpl-6a8904e8e75ac1ced6997605",
  "usageMetadata": {
    "promptTokenCount": 20,
    "candidatesTokenCount": 2,
    "totalTokenCount": 22
  }
}
```

**Streaming** frames are unnamed `data: {…}` events carrying the same `candidates` shape, with
`finishReason` and `usageMetadata` on the last one. There is no `[DONE]` sentinel.

**Errors** use the Google envelope, where `code` is the numeric HTTP status and `status` is the
gRPC status name:

```json
{
  "error": {
    "code": 401,
    "message": "vkauth: virtual key missing (Include a virtual key via X-Nexus-Virtual-Key header or Authorization: Bearer)",
    "status": "UNAUTHENTICATED"
  }
}
```

---

## 7. Gateway endpoints

These have no vendor standard, so they exist once and are the same whichever SDK you use.

### 7.1 Rerank documents

`POST /v1/rerank`

Scores a list of documents against a query and returns them ranked. The request and response follow
the Cohere rerank shape, which is the de-facto standard.

**Requires a rerank model.** Only two provider wires serve this endpoint — Cohere and Voyage — so
the deployment must have a `type: rerank` model enabled with credentials for one of them. Check
`GET /v1/models` for entries whose `type` is `rerank`; with none, every call returns
`404 ROUTING_NO_MATCH`.

**Request body**


| Parameter          | Type    | Required | Description                                                                                                                   |
| ------------------ | ------- | -------- | ----------------------------------------------------------------------------------------------------------------------------- |
| `model`            | string  | **yes**  | Rerank model id.                                                                                                              |
| `query`            | string  | **yes**  | The query to rank against.                                                                                                    |
| `documents` | array | **yes** | 1 to 1000 entries. Plain strings work on every provider; one that accepts object-shaped documents still does. A request routed onto a different provider's wire needs strings, because translating a document means reading its text. |
| `top_n`            | integer | no       | Return only the top N results. Must be a positive integer. A value larger than the document count is clamped by the provider. |
| `return_documents` | boolean | no       | Echo each document's text alongside its score.                                                                                |


**Returns** — ranked results, most relevant first. `results[].index` points back into the
`documents` array you sent.

```bash
curl -sS $NEXUS_URL/v1/rerank \
  -H "Authorization: Bearer $NEXUS_KEY" \
  -H 'content-type: application/json' \
  -d '{
    "model": "rerank-v3.5",
    "query": "capital of France",
    "documents": ["Paris is the capital of France.", "Bananas are yellow."],
    "top_n": 2
  }'
```

```json
{
  "id": "843b335e-7434-4ebb-98fc-f94572aa698d",
  "results": [
    { "index": 0, "relevance_score": 0.8627572 },
    { "index": 1, "relevance_score": 0.022373645 }
  ],
  "meta": {
    "api_version": { "version": "2" },
    "billed_units": { "search_units": 1 }
  }
}
```

Rerank responses are never cached. Billing depends on the provider that serves you: the
Cohere-family wire bills in search units — one per 100 documents, rounded up — while Voyage bills
per token and reports `meta.billed_units.total_tokens` instead.

The `documents` ceiling is enforced on every request, whichever provider serves it — it bounds what
one call can cost, since rerank bills per search unit. The remaining shape rules are checked when
the request has to be translated onto a different provider's wire, because translating a document
means reading its text.

### 7.2 Evaluate content policy

`POST /v1/guardrail`

Runs the deployment's compliance pipeline over text you supply and returns a verdict. No model is
called and nothing is relayed upstream — this is the policy engine on its own, so you can screen
content before you decide what to do with it.

**Requires compliance hooks bound to the stage you ask for.** The endpoint has no policy of its
own; it runs whatever the deployment has configured. With nothing bound for that stage it still
answers `200`, with `action: "allow"`, `decision: "approve"` and — the part that matters —
`coverage: "none"`.

> **Read `coverage` before you trust `action`.** `coverage: "none"` means nothing was scanned, not
> that the content is clean. An `allow` verdict is only as strong as the coverage that produced it:
> `full` means every bound policy ran, `degraded` means at least one could not complete. Treating a
> `none`-coverage `allow` as a pass is the one way to use this endpoint and get no protection from
> it.

Unknown fields are rejected rather than ignored.

**Request body**


| Parameter                  | Type    | Required | Description                                                                                                        |
| -------------------------- | ------- | -------- | ------------------------------------------------------------------------------------------------------------------ |
| `content`                  | string  | one of   | Flat text to evaluate. Mutually exclusive with `messages`.                                                         |
| `messages`                 | array   | one of   | `{"role":…,"content":…}` entries; contents are joined in turn order. Mutually exclusive with `content`.            |
| `stage`                    | string  | no       | `"input"` (default) or `"output"` — which side of a conversation this text represents. Any other value is a `400`. |
| `include_redacted_content` | boolean | no       | Defaults to `true`. When the verdict is `redact`, echoes the sanitized text.                                       |


Exactly one of `content` and `messages` must be non-empty.

**Returns** — a verdict. A blocked verdict is still `HTTP 200`: a block is data, not a transport
error.


| Field                    | Type    | Description                                                                                                          |
| ------------------------ | ------- | -------------------------------------------------------------------------------------------------------------------- |
| `action`                 | string  | `allow`, `block`, or `redact` — the caller-facing verdict                                                            |
| `decision`               | string  | the pipeline decision: `approve`, `reject_hard`, `block_soft`, or `modify`                                           |
| `coverage`               | string  | `full`, `degraded`, or `none` — how completely the text was scanned. `none` means no policy was bound for this stage |
| `reason` / `reason_code` | string  | aggregate human and machine reasons                                                                                  |
| `labels`                 | array   | aggregate tags such as `category:pii.contact`, `severity:restricted`                                                 |
| `assessments`            | array   | one entry per policy that flagged the content: `check`, `decision`, `action`, `labels`, `reason_code`                |
| `blocking`               | object  | present only when `action` is `block`: `category`, `severity`, `labels`                                              |
| `redactions`             | array   | spans as byte offsets into the evaluated text: `start`, `end`, `replacement`, `action`, `reason`                     |
| `redacted_content`       | string  | the sanitized text, when `action` is `redact`                                                                        |
| `metadata.latency_ms`    | integer | how long the evaluation took                                                                                         |


`labels` carries the identity of what matched, including `rulepack:<name>` and `rule:<id>` tags.
The `blocking` object itself carries only `category`, `severity` and `labels`; the rule pack's
version appears nowhere in the response.

```bash
curl -sS $NEXUS_URL/v1/guardrail \
  -H "Authorization: Bearer $NEXUS_KEY" \
  -H 'content-type: application/json' \
  -d '{
    "content": "My email is alice@example.com and my card is 4111 1111 1111 1111",
    "stage": "input",
    "include_redacted_content": true
  }'
```

```json
{
  "action": "redact",
  "decision": "modify",
  "coverage": "full",
  "reason": "rule-pack match: content redacted",
  "reason_code": "RULEPACK_REDACTED",
  "labels": [
    "category:pii.contact", "category:pii.financial", "contact:email", "detector:pii",
    "finance:card-number", "rule:pii-con-001", "rule:pii-fin-001", "rulepack:nexus/pii",
    "severity:confidential", "severity:restricted"
  ],
  "assessments": [
    {
      "check": "pii-scanner",
      "decision": "modify",
      "action": "redact",
      "labels": [
        "rulepack:nexus/pii", "rule:pii-con-001", "category:pii.contact", "detector:pii",
        "contact:email", "severity:confidential", "rule:pii-fin-001", "category:pii.financial",
        "finance:card-number", "severity:restricted"
      ],
      "reason_code": "RULEPACK_REDACTED"
    }
  ],
  "redactions": [
    { "start": 12, "end": 29, "replacement": "[REDACTED_PII-CON-001]", "action": "redact" },
    { "start": 45, "end": 64, "replacement": "[REDACTED_PII-FIN-001]", "action": "redact" }
  ],
  "redacted_content": "My email is [REDACTED_PII-CON-001] and my card is [REDACTED_PII-FIN-001]",
  "metadata": { "latency_ms": 3 }
}
```

### 7.3 Get usage

`GET /v1/usage`

This key's spend for the current period. Takes no parameters.

```bash
curl -sS $NEXUS_URL/v1/usage -H "Authorization: Bearer $NEXUS_KEY"
```

```json
{
  "virtualKeyId": "cc1ec7ee-8e02-47f3-ade4-9fa92b477976",
  "period": "2026-08",
  "periodType": "monthly",
  "usage": {
    "totalRequests": 21,
    "promptTokens": 212,
    "completionTokens": 1441,
    "totalTokens": 1653,
    "estimatedCostUsd": 0.04
  },
  "quota": {
    "limitUsd": 500000,
    "usedUsd": 0,
    "remainingUsd": 500000,
    "enforcementMode": "reject",
    "rateLimitRpm": null
  }
}
```

`quota.enforcementMode` on this endpoint reports `reject` when a quota is enforced and `none` when
none is configured. The four enforcement behaviours themselves are described in §9.

### 7.4 Get daily usage

`GET /v1/usage/daily`

Per-day spend broken out by model.

**Query parameters**


| Parameter   | Type   | Description                                                              |
| ----------- | ------ | ------------------------------------------------------------------------ |
| `startDate` | string | `YYYY-MM-DD`. Defaults to 30 days ago, giving a 31-day inclusive window. |
| `endDate`   | string | `YYYY-MM-DD`. Defaults to today.                                         |


A malformed date, or an `endDate` earlier than `startDate`, returns `400 USAGE_INVALID_DATE`; a
span wider than 90 days returns `400 USAGE_RANGE_TOO_LARGE`.

```bash
curl -sS $NEXUS_URL/v1/usage/daily -H "Authorization: Bearer $NEXUS_KEY"
```

```json
{
  "virtualKeyId": "cc1ec7ee-8e02-47f3-ade4-9fa92b477976",
  "startDate": "2026-07-23",
  "endDate": "2026-08-22",
  "daily": [
    {
      "date": "2026-08-22",
      "requests": 9,
      "promptTokens": 172,
      "completionTokens": 18,
      "totalTokens": 190,
      "costUsd": 0.000434,
      "models": [
        {
          "model": "moonshot-v1-128k",
          "provider": "moonshot",
          "requests": 9,
          "promptTokens": 172,
          "completionTokens": 18,
          "totalTokens": 190,
          "costUsd": 0.000434
        }
      ]
    }
  ],
  "totals": {
    "requests": 9,
    "promptTokens": 172,
    "completionTokens": 18,
    "totalTokens": 190,
    "costUsd": 0.000434
  }
}
```

### 7.5 Estimate and compare cost

`POST /v1/estimate`

Prices a request before you send it, across up to 10 targets at once. Nothing is dispatched and
nothing is billed.

**Request**


| Field                              | Type    | Required | Description                                                                                                               |
| ---------------------------------- | ------- | -------- | ------------------------------------------------------------------------------------------------------------------------- |
| `request`                          | object  | yes      | The body you would have posted to the data plane, verbatim.                                                               |
| `compareTargets`                   | array   | yes      | 1–10 candidates. Each is priced independently against the same `request`.                                                 |
| `compareTargets[].modelId`         | string  | yes      | A model UUID or its human-friendly code.                                                                                  |
| `compareTargets[].providerId`      | string  | no       | Pins the provider when a model is served by more than one.                                                                |
| `compareTargets[].reasoningEffort` | string  | no       | Overrides the effort the body carries, for this target only — so one call can price the same prompt at two efforts. One of `minimal`, `low`, `medium`, `high`, or a positive integer token budget. |
| `options.ingressFormat`            | string  | no       | Labels the telemetry. Validated against the wire formats this gateway speaks; an unrecognised value is refused with `400 ESTIMATE_INVALID_INGRESS_FORMAT` rather than reaching a metric label. It cannot change a number — the estimate is dialect-agnostic. |


```bash
curl -sS $NEXUS_URL/v1/estimate \
  -H "Authorization: Bearer $NEXUS_KEY" -H 'content-type: application/json' \
  -d '{
    "request": {
      "model": "deepseek-v4-flash",
      "messages": [{"role": "user", "content": "Summarise the Treaty of Westphalia."}]
    },
    "compareTargets": [
      {"modelId": "deepseek-v4-flash", "reasoningEffort": "low"},
      {"modelId": "deepseek-v4-flash", "reasoningEffort": "high"}
    ]
  }'
```

**Returns** — one entry per target plus a summary naming the cheapest. The two efforts above price
the same prompt at `0.0000854` and `0.0011214`, a thirteenfold spread, which is the point of asking.

```json
{
  "targets": [ … ],
  "summary": {
    "cheapestExpectedTarget": "deepseek-v4-flash",
    "cheapestExpectedTotalUsd": 0.0000854,
    "mostExpensiveExpectedTotalUsd": 0.0011214,
    "errorsCount": 0,
    "successCount": 2
  }
}
```

This endpoint has a per-key rate limit of its own, separate from the data plane's — see §9. Its
errors use the same envelope as every other route.

### 7.6 Public model catalog

`GET /api/v1/open/models`
`GET /api/v1/open/models/{model_id}`

The full enabled catalog, with no key. Model ids here are provider-prefixed — `openai/gpt-4o`,
`deepseek/deepseek-v4-flash` — which is why the detail route accepts a two-segment id.

**Query parameters**


| Parameter | Type    | Description                               |
| --------- | ------- | ----------------------------------------- |
| `limit`   | integer | Page size. Defaults to 50, capped at 200. |
| `offset`  | integer | Page offset. Defaults to 0.               |


```bash
curl -sS "$NEXUS_URL/api/v1/open/models?limit=1"
```

```json
{
  "data": [
    {
      "id": "deepseek/deepseek-v4-flash",
      "provider": "deepseek",
      "name": "DeepSeek V4 Flash",
      "context_length": 1048565,
      "modalities": ["text"],
      "pricing": { "input": 0.14, "output": 0.28, "currency": "USD", "unit": "per_million_tokens" },
      "capabilities": { "features": ["function_calling", "streaming", "json_mode", "reasoning"] },
      "status": "ga"
    }
  ],
  "limit": 1,
  "offset": 0,
  "total_count": 80
}
```

---

## 8. Errors

### Two sources, told apart by one prefix

**Gateway errors** are requests the gateway declined to send. Their message begins with `nexus:`
and names the reason and the fix:

```json
{
  "error": {
    "code": "INVALID_REQUEST",
    "message": "nexus: Cohere chat carries documents as text, and application/pdf is not text. Extracting it would change what the model is given, so send the document's text instead",
    "type": "invalid_request_error"
  }
}
```

**Provider errors** are the upstream's own words, reshaped into your dialect's envelope. Anything
without the `nexus:` prefix came from the model provider.

### Envelope by dialect


| Dialect                     | Shape                                                                                                                     |
| --------------------------- | ------------------------------------------------------------------------------------------------------------------------- |
| OpenAI                      | `{"error":{"message":…,"type":…,"code":…[,"param":…][,"hint":…]}}`                                                        |
| Responses (`/v1/responses`) | `{"error":{"message":…,"type":…[,"code":…]}}` — no `param`, no `hint` key; a hint is appended to `message` in parentheses |
| Anthropic                   | `{"type":"error","error":{"type":…,"message":…}}`                                                                         |
| Gemini                      | `{"error":{"code":<status>,"message":…,"status":"<GRPC_NAME>"}}`                                                          |


`error.type` on the OpenAI envelope is derived from the HTTP status: 400 →
`invalid_request_error`, 401 → `authentication_error`, 403 → `permission_error`, 404 →
`not_found_error`, 429 → `rate_limit_error`, 5xx → `api_error`.

`error.param` is set to `"model"` on the errors that are about the model you named.

On a **gateway** error, `error.code` is always a string — an UPPER_SNAKE machine code — or absent,
and it is the field to branch on. Every route uses the envelope above, including `/v1/models`,
`/v1/usage`, `/v1/estimate`, the public catalog, and any path this gateway does not serve.

On a **provider** error the field is the upstream's own: a lower_snake string such as
`provider_quota_exhausted`, or on the Gemini dialect the numeric status, which is Google's
convention. Reading the HTTP status first and `error.code` second works against both.

Every gateway error takes the shape of the dialect you called, cross-format rejections included.
On a dialect whose envelope has no `code` field — Anthropic's does not — the machine code is absent
from the body; branch on the HTTP status and `error.type` there, and read the code from the traffic
row if you are an operator. Handing an Anthropic SDK the OpenAI shape would cost it more than the
field: the parse fails and the message never reaches the caller at all.

### Status codes and what they mean


| Status | `code`                                                                          | Meaning                                                                                               |
| ------ | ------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------- |
| 400    | `MODEL_REQUIRED`                                                                | the body has no `model`                                                                               |
| 400    | `INVALID_REQUEST`                                                               | malformed JSON, an unknown field, or a failed validation                                              |
| 400    | `MODEL_CAPABILITY_MISMATCH`                                                     | the named model cannot serve these parameters                                                         |
| 400    | `SPEND_LIMIT_EXCEEDED`                                                          | the request asks for more billable units than one call may multiply — image `n` above 4, or more than 1000 rerank documents |
| 400    | `NO_COMPATIBLE_CAPABILITY`                                                      | no candidate model supports the requested parameters; the body lists what each candidate does support |
| 400    | `NO_COMPATIBLE_PROVIDER`                                                        | your dialect cannot be translated onto any candidate provider for this request kind                   |
| 400    | `CROSS_FORMAT_STREAM_UNSUPPORTED`                                               | streaming is not available across this dialect/provider pair                                          |
| 401    | `AUTH_KEY_MISSING`, `AUTH_INVALID_KEY`, `AUTH_KEY_DISABLED`, `AUTH_KEY_EXPIRED` | see §2                                                                                                |
| 403    | `MODEL_NOT_ALLOWED`                                                             | the key is not entitled to that model                                                                 |
| 403    | `HOOK_BLOCKED`                                                                  | the compliance pipeline blocked the request                                                           |
| 403    | `GENERATIVE_PROMPT_BLOCKED`                                                     | an image or video prompt tripped a content rule                                                       |
| 404    | `ENDPOINT_NOT_SUPPORTED`                                                        | this gateway does not serve that path                                                                 |
| 413    | `PAYLOAD_TOO_LARGE`                                                             | the body exceeded the ceiling on a JSON proxy route                                                   |
| 413    | `STT_UPLOAD_TOO_LARGE`                                                          | the upload exceeded the transcription ceiling                                                         |
| 413    | `GUARDRAIL_BODY_TOO_LARGE`                                                      | the body exceeded the guardrail ceiling                                                               |
| 429    | `RATE_LIMITED`                                                                  | the key's requests-per-minute bucket is empty                                                         |
| 429    | `GENERATIVE_CONCURRENCY_LIMIT`                                                  | too many concurrent image, speech, transcription or guardrail requests on this key                    |
| 429    | `QUOTA_EXCEEDED`                                                                | the spend quota is exhausted and enforcement is `reject`                                              |
| 429    | `PROVIDER_RATE_LIMITED`                                                         | the upstream provider rate-limited us                                                                 |
| upstream's own status | `provider_quota_exhausted` | the provider account serving you is out of budget. The gateway moves the request to another provider when one can serve it; when none can, the upstream's own message reaches you, and it is the one that names when access returns |
| 429    | `gateway_overloaded`                                                            | the gateway shed the request before admitting it                                                      |
| 499    | `CLIENT_CLOSED`                                                                 | you disconnected before the upstream answered                                                         |
| 404    | `ROUTING_NO_MATCH` (`param: "model"`)                                           | the model name does not resolve to any enabled provider — the response names the model you sent       |
| 500    | `ROUTING_NO_MATCH`                                                              | routing failed for a reason other than an unknown model                                               |
| 502    | `PROVIDER_UNAVAILABLE`                                                          | every upstream attempt failed                                                                         |
| 503    | `ROUTING_RULES_RESOLVED_NOTHING`                                                | a routing rule matched but produced no usable target                                                  |
| 503    | `QUOTA_MODEL_UNPRICED`                                                          | the routed model has no price under an active cost quota                                              |
| 503    | `AUTH_BACKEND_UNAVAILABLE`                                                      | the key could not be looked up                                                                        |


Errors carry a `hint` on the endpoints whose envelope has room for one. It says what to change.

---

## 9. Rate limits and quota

**Requests per minute.** Each virtual key has an RPM bucket. Exhausting it returns `429
RATE_LIMITED` with `Retry-After` — on every route, including `/v1/models` and `/v1/usage`.

The gateway sets `X-RateLimit-Limit` when a limit is configured, on the 429 as well as on accepted
requests. It does not emit `X-RateLimit-Remaining` or `X-RateLimit-Reset`.

A 429 whose code is `GATEWAY_OVERLOADED` is different: it is the gateway shedding load, not your
key's bucket, and it carries `Retry-After: 1`.

**Concurrency.** Image generation, text to speech, transcription and guardrail cap concurrent
in-flight requests per key — see §3.

**Spend quota.** A key can carry a dollar quota. `GET /v1/usage` reports `limitUsd`, `usedUsd` and
`remainingUsd`, and its `enforcementMode` tells you only whether the quota is enforced (`reject`)
or absent (`none`). The mode configured on the key is one of four, and decides what happens when
the quota runs out:


| `enforcementMode`    | Behaviour when the quota is exhausted                                                                         |
| -------------------- | ------------------------------------------------------------------------------------------------------------- |
| `track-only`         | nothing; spend is recorded                                                                                    |
| `notify-and-proceed` | the request proceeds and carries `X-Nexus-Quota-Warning`                                                      |
| `downgrade`          | the request is served by a cheaper model; `X-Nexus-Quota-Downgrade` and `X-Nexus-Quota-Original-Model` say so |
| `reject`             | `429 QUOTA_EXCEEDED`                                                                                          |


Every response carries `X-Nexus-Quota-Used` and `X-Nexus-Quota-Limit` when a quota is configured,
so you can watch the budget without polling `/v1/usage`.

---

## 10. Postman collection

Two files ship alongside this document:


| File                                     | What it is                               |
| ---------------------------------------- | ---------------------------------------- |
| `nexus-gateway.postman_collection.json`  | every endpoint above, grouped by dialect |
| `nexus-gateway.postman_environment.json` | the variables the collection reads       |


Import both, select the environment, set `baseUrl` to your gateway, and fill in `virtualKey`.
Nothing in the collection is hard-coded — the base URL, the key and the model names are all
environment variables, so pointing the same collection at a different deployment means editing the
environment, not the requests.

The model variables ship with working defaults, but a catalog is per-deployment: if a request comes
back `404` or `403 MODEL_NOT_ALLOWED`, call `GET /v1/models` and set the variable to an id your key
can actually reach.


| Variable         | Purpose                                                         |
| ---------------- | --------------------------------------------------------------- |
| `baseUrl`        | gateway base URL                                                |
| `virtualKey`     | your virtual key — the only value you must supply               |
| `chatModel`      | model used by the chat, messages, responses and Gemini requests |
| `embeddingModel` | model used by the embeddings request                            |
| `rerankModel`    | model used by the rerank request                                |
| `imageModel`     | model used by the image-generation request                      |
| `ttsModel`       | model used by the speech request                                |
| `sttModel`       | model used by the transcription request                         |


Authentication is set once on the collection and inherited by every request, except the two
requests that deliberately demonstrate a dialect-native header (`x-api-key` on Messages,
`x-goog-api-key` on Gemini) and the public catalog requests, which send no key at all.

The transcription request needs a local audio file: open it, and re-select the file in the body
tab. Postman does not carry file contents inside a collection.

---

## References

- `packages/ai-gateway/cmd/ai-gateway/wiring/` — route registration for every path in this document
- `packages/ai-gateway/internal/ingress/proxy/` — the request pipeline, response headers, error writers
- `packages/ai-gateway/internal/ingress/envelope/` — usage endpoints and the not-supported envelope
- `packages/ai-gateway/internal/ingress/models/` — model catalog endpoints
- `packages/ai-gateway/internal/ingress/sttproxy/` — multipart handling for transcriptions
- `packages/ai-gateway/internal/auth/vkauth/` — virtual-key extraction and validation
- `packages/ai-gateway/internal/policy/guardrail/` — guardrail request, verdict, and response shapes
- `packages/ai-gateway/internal/policy/generativecaps/` — per-endpoint body ceilings and concurrency caps
- `packages/ai-gateway/internal/policy/quota/` — quota enforcement modes
- `packages/ai-gateway/internal/routing/` — model selection, capability filtering
- `packages/ai-gateway/internal/execution/canonicalbridge/` — cross-dialect request and stream translation
- `packages/ai-gateway/internal/execution/estimator/` — cost estimation
- `packages/ai-gateway/internal/providers/specs/` — per-provider codecs
- `packages/shared/transport/typology/` — wire shapes and endpoint kinds


# AI Gateway API

The gateway speaks the API you already use. Point your existing OpenAI, Anthropic or Gemini SDK at
it, swap the base URL and the key, and your code works — including against models from a different
vendor than the SDK was written for.

Everything below is a request you can paste and run.

```bash
export NEXUS_URL="https://api.<your-domain>"
export NEXUS_KEY="<your virtual key>"
```

---

## 1. The surface

Four **ingress dialects**. Each accepts its vendor's request shape and returns that vendor's
response shape, whatever model actually served it.

| Dialect | Endpoints |
|---|---|
| **OpenAI** | `POST /v1/chat/completions` · `POST /v1/responses` · `POST /v1/embeddings` · `POST /v1/images/generations` · `POST /v1/audio/speech` · `POST /v1/audio/transcriptions` · `GET /v1/models` · `GET /v1/models/{model}` |
| **Anthropic** | `POST /v1/messages` |
| **Gemini** | `POST /v1beta/models/{model}` (`:generateContent`, `:streamGenerateContent`, `:embedContent`) |
| **Azure / GLM compatibility** | `POST /openai/deployments/{deployment}/chat/completions` · `.../embeddings` · `POST /api/paas/v4/chat/completions` · `.../embeddings` |

Plus surfaces with no single vendor standard:

| Endpoint | What it does |
|---|---|
| `POST /v1/rerank` | Rerank documents against a query (Cohere-shaped — the de-facto standard) |
| `POST /v1/videos`, `GET /v1/videos/{id}`, `GET /v1/videos/{id}/content`, `DELETE /v1/videos/{id}` | Video generation, async: submit, poll, fetch, delete |
| `GET /v1/realtime` | Realtime bidirectional audio (WebSocket) |
| `POST /v1/guardrail` | Run the content policy over text without calling a model |
| `POST /v1/estimate` | Price a request before sending it |
| `GET /v1/usage`, `GET /v1/usage/daily` | Your own spend |

**`model` is the only routing input you need.** Send a model name and the gateway picks the
provider, translates your request onto that provider's wire, and translates the answer back.
Send `"model": "auto"` and it picks the model too.

---

## 2. Chat, and the same request in three dialects

The same question, three SDKs, one gateway. All three work against any chat model.

```bash
# OpenAI dialect
curl -sS $NEXUS_URL/v1/chat/completions \
  -H "Authorization: Bearer $NEXUS_KEY" -H 'content-type: application/json' \
  -d '{"model":"gpt-4.1","max_tokens":32,
       "messages":[{"role":"user","content":"Reply with only: ok"}]}'

# Anthropic dialect — note the model is an OpenAI one; the gateway translates
curl -sS $NEXUS_URL/v1/messages \
  -H "Authorization: Bearer $NEXUS_KEY" -H 'content-type: application/json' \
  -H 'anthropic-version: 2023-06-01' \
  -d '{"model":"gpt-4.1","max_tokens":32,
       "messages":[{"role":"user","content":"Reply with only: ok"}]}'

# Gemini dialect
curl -sS "$NEXUS_URL/v1beta/models/gemini-2.5-flash:generateContent" \
  -H "Authorization: Bearer $NEXUS_KEY" -H 'content-type: application/json' \
  -d '{"contents":[{"role":"user","parts":[{"text":"Reply with only: ok"}]}]}'
```

Add `"stream": true` (or `:streamGenerateContent` for Gemini) for SSE. The stream you get back is
your dialect's event sequence, not the upstream's.

---

## 3. Images

An image is a content part. Two forms, and **which one to use is not a preference**:

```bash
# INLINE — works on every vision model, on every provider.
B64=$(base64 -i photo.png | tr -d '\n')
curl -sS $NEXUS_URL/v1/chat/completions \
  -H "Authorization: Bearer $NEXUS_KEY" -H 'content-type: application/json' \
  -d "{\"model\":\"gpt-4.1\",\"max_tokens\":64,\"messages\":[{\"role\":\"user\",\"content\":[
        {\"type\":\"image_url\",\"image_url\":{\"url\":\"data:image/png;base64,$B64\"}},
        {\"type\":\"text\",\"text\":\"What is in this image?\"}]}]}"

# BY URL — works on OpenAI and Anthropic models. Cohere and Moonshot models do
# NOT fetch images, and the gateway tells you so rather than passing you the
# vendor's message about your URL being invalid.
curl -sS $NEXUS_URL/v1/chat/completions \
  -H "Authorization: Bearer $NEXUS_KEY" -H 'content-type: application/json' \
  -d '{"model":"gpt-4.1","max_tokens":64,"messages":[{"role":"user","content":[
        {"type":"image_url","image_url":{"url":"https://example.com/photo.png"}},
        {"type":"text","text":"What is in this image?"}]}]}'
```

**Use inline if you want one request that works everywhere.** "Supports images" and "fetches an
image URL" are separate capabilities and the second is not universal.

**Formats.** Anthropic models accept `image/jpeg`, `image/png`, `image/gif` and `image/webp` only —
a HEIC photo straight from an iPhone is refused, by the gateway, with that list. Your declared
media type is normalized first, so `IMAGE/PNG`, `image/png; charset=utf-8` and a line-wrapped
base64 payload all work even though the upstream would reject each of them verbatim.

---

## 4. Documents

A document is a `file` content part:

```bash
DOC=$(base64 -i runbook.md | tr -d '\n')
curl -sS $NEXUS_URL/v1/chat/completions \
  -H "Authorization: Bearer $NEXUS_KEY" -H 'content-type: application/json' \
  -d "{\"model\":\"command-a-03-2025\",\"max_tokens\":64,\"messages\":[{\"role\":\"user\",\"content\":[
        {\"type\":\"file\",\"file\":{\"filename\":\"runbook.md\",
          \"file_data\":\"data:text/markdown;base64,$DOC\"}},
        {\"type\":\"text\",\"text\":\"What is the reference number in the attached document?\"}]}]}"
```

The gateway puts the document wherever that provider carries one — an Anthropic `document` block,
a Gemini `inlineData` part, a Cohere top-level `documents` entry. You send the same shape either
way.

**What each provider can do with it differs, and you get told which:**

| | Text documents (md, txt, json, csv…) | PDFs and other binaries |
|---|---|---|
| Anthropic, Gemini, OpenAI | yes | yes |
| Cohere | yes | **refused** — this wire carries documents as text, and the gateway does not extract a PDF for you |
| DeepSeek | no | no |

A refusal names the media type and says what to send instead. It never silently drops the
attachment and answers anyway.

**Always send `file_data` as a `data:` URL.** A bare base64 string or an `https://` link is
refused — the gateway does not fetch documents by URL.

---

## 5. Audio

```bash
WAV=$(base64 -i speech.wav | tr -d '\n')
curl -sS $NEXUS_URL/v1/chat/completions \
  -H "Authorization: Bearer $NEXUS_KEY" -H 'content-type: application/json' \
  -d "{\"model\":\"gpt-audio-1.5\",\"max_tokens\":128,\"modalities\":[\"text\"],
       \"messages\":[{\"role\":\"user\",\"content\":[
        {\"type\":\"text\",\"text\":\"Transcribe this exactly.\"},
        {\"type\":\"input_audio\",\"input_audio\":{\"data\":\"$WAV\",\"format\":\"wav\"}}]}]}"
```

For transcription without a chat model, `POST /v1/audio/transcriptions` takes a multipart upload.
`POST /v1/audio/speech` goes the other way.

**On `/v1/responses`, audio is handled for you.** That API's input vocabulary is `input_text`,
`input_image` and `input_file` — it has no audio part. Rather than passing you a 400, the gateway
routes an audio-carrying `/v1/responses` request to the chat wire, which does carry it, and returns
the Responses shape you asked for.

**`model: auto` will not hand your audio to a model that cannot hear it.** Routing narrows the
candidate pool by every modality your request carries, and drops models that *require* a modality
you did not send (an audio-only model never serves a plain text prompt).

---

## 6. Prompt caching

Mark the prefix you want cached and the gateway carries the marker to whichever provider serves
you — including from the OpenAI dialect to an Anthropic model:

```bash
curl -sS $NEXUS_URL/v1/chat/completions \
  -H "Authorization: Bearer $NEXUS_KEY" -H 'content-type: application/json' \
  -d '{"model":"claude-sonnet-4-5-20250929","max_tokens":32,"messages":[
        {"role":"system","content":[{"type":"text","text":"<a long stable prefix…>",
          "cache_control":{"type":"ephemeral"}}]},
        {"role":"user","content":"Reply with only: ok"}]}'
```

An explicit marker is always respected; without one, the gateway may insert cache breakpoints for
you when the deployment enables it.

---

## 7. Reading usage — and the one place the numbers differ on purpose

Each dialect reports usage in its own convention, because that is what its SDK expects.

**OpenAI shape — `prompt_tokens` is the TOTAL, cached tokens are a subset already inside it:**

```json
"usage": {"prompt_tokens": 2717, "completion_tokens": 4,
          "prompt_tokens_details": {"cached_tokens": 2560}}
```
Your input cost is `prompt_tokens`. Adding `cached_tokens` to it double-counts.

**Anthropic shape — ADDITIVE. `input_tokens` counts only what was neither read from nor written
to the cache:**

```json
"usage": {"input_tokens": 157, "cache_read_input_tokens": 2560, "output_tokens": 4}
```
Your input total is the sum: `157 + 2560 = 2717` — the same request as above.

Both are correct for their dialect. Sum the Anthropic fields; do not sum the OpenAI ones.

Streaming reports the same numbers as non-streaming. On `/v1/messages` the authoritative counts
ride on `message_delta`, which is where a real Anthropic upstream puts them.

---

## 8. Errors

Two kinds, and they read differently on purpose.

**The gateway's own** — a request it will not send, with the reason and the fix:

```json
{"error":{"message":"nexus: Cohere chat carries documents as text, and application/pdf is not
 text. Extracting it would change what the model is given, so send the document's text instead",
 "type":"proxy_error","code":"invalid_request"}}
```

**The provider's** — reshaped into your dialect's error envelope but otherwise the upstream's own
words, so a message about a model's limits still reads like one.

Anything prefixed `nexus:` is us. Everything else came from the model provider.

`error.code` is always a string — an UPPER_SNAKE machine code — or absent, and it is the field to
branch on. Every path answers in JSON, including one this gateway does not serve.

---

## 9. Discovery

```bash
curl -sS $NEXUS_URL/v1/models -H "Authorization: Bearer $NEXUS_KEY"
curl -sS $NEXUS_URL/v1/models/gpt-4.1 -H "Authorization: Bearer $NEXUS_KEY"
```

`GET /v1/models` lists what your key can reach. Per-model entries carry the input modalities that
model accepts, which is the same data routing uses.

---

## References

- `docs/users/api/openapi/` — OpenAPI 3.1 specs for the endpoints that have one
- `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md` — how a request is translated onto each provider's wire
- `docs/developers/architecture/services/ai-gateway/smart-routing-architecture.md` — what `model: auto` does
- `docs/developers/architecture/services/ai-gateway/prompt-cache-architecture.md` — cache markers and hit classification
- `docs/developers/architecture/cross-cutting/safety/error-taxonomy-architecture.md` — the error envelopes per ingress

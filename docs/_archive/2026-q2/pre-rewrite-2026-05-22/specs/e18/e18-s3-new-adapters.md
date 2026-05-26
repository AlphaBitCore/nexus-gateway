# E18 — Story 3: Detection for Enterprise Providers (Bedrock, Vertex, GLM, DeepSeek)

## Context

Enterprise customers commonly route AI traffic through AWS Bedrock (SigV4-signed) and Google Vertex AI (OAuth bearer), alongside Chinese providers Zhipu GLM (JWT-signed) and DeepSeek (OpenAI wire-compatible). None of these have traffic detect adapters today. This story adds four new adapters. Depends on s1 and s4 (streaming for the ones that have streaming).

## User Story

**As an** enterprise security engineer,
**I want** Bedrock, Vertex, GLM, and DeepSeek traffic to produce the same LLM signal columns as OpenAI and Anthropic traffic,
**so that** I can enforce unified cost attribution and key governance across all providers my organization uses, not just a subset.

## Tasks

### 3.1 AWS Bedrock — `adapters/bedrock/bedrock.go`

- **Host pattern:** `bedrock-runtime.*.amazonaws.com` and `bedrock.*.amazonaws.com`.
- **`DetectRequestMeta`:** `Provider = "bedrock"`. Auth is `Authorization: AWS4-HMAC-SHA256 Credential=<AKID>/.../...` — the signature header contains the AWS Access Key ID in the `Credential=` substring. Class is `aws-sigv4`. `ApiKeyFingerprint` is `SHA256(AKID)[:8]` — extracting the AKID from the `Credential=` field (between `Credential=` and the first `/`). We record the AKID fingerprint so cost attribution can map to an IAM user; we never record the signature itself. Model is in the URL path: `/model/{model-id}/invoke` or `/model/{model-id}/invoke-with-response-stream` → extract the `{model-id}` segment. `stream = strings.HasSuffix(path, "/invoke-with-response-stream")`.
- **`DetectResponseUsage`:** Bedrock wraps provider-native formats. For Anthropic-on-Bedrock (model id `anthropic.claude-*`): parse `$.usage.input_tokens` / `$.usage.output_tokens`. For Amazon Titan / AI21 / Cohere models: parse `$.inputTextTokenCount` / `$.results[0].tokenCount` (Titan), `$.meta.billed_units.input_tokens` / `$.meta.billed_units.output_tokens` (Cohere). Model-id prefix dispatches to the right parser.
- **Streaming:** Bedrock streams as base64-encoded Smithy event stream frames with `:content-type: application/json` and a `bytes` field containing the underlying provider's SSE-equivalent JSON. The accumulator must decode the Smithy envelope first, then dispatch by model-id prefix.

### 3.2 Vertex AI — `adapters/vertex/vertex.go`

- **Host pattern:** `*-aiplatform.googleapis.com` and `aiplatform.googleapis.com`.
- **`DetectRequestMeta`:** `Provider = "vertex"`. Auth is `Authorization: Bearer <short-lived OAuth token>` — class `gcp-oauth`. Fingerprint is `SHA256(token)[:8]`; the token is short-lived (~1 hour), so fingerprint uniqueness is per-session, which is acceptable for attribution. Model is in URL path: `/v1/projects/{p}/locations/{l}/publishers/google/models/{model}:{method}` — extract `{model}`. `stream = strings.HasSuffix(methodPart, "streamGenerateContent")`.
- **`DetectResponseUsage`:** identical shape to Gemini (`$.usageMetadata.promptTokenCount` / `$.usageMetadata.candidatesTokenCount`). For third-party models on Vertex (Anthropic via Vertex Partner), the path is `/publishers/anthropic/models/...` and the usage shape follows Anthropic.
- **Streaming:** same envelope as Gemini — can reuse gemini accumulator dispatch.

### 3.3 Zhipu GLM — `adapters/glm/glm.go`

- **Host pattern:** `open.bigmodel.cn`.
- **`DetectRequestMeta`:** `Provider = "glm"`. Auth is `Authorization: Bearer <JWT>` where the JWT is signed from a `{api-key-id}.{secret}` credential pair. Class is `glm-jwt`. Fingerprint is `SHA256(jwt)[:8]`. Model and stream parsing mirrors OpenAI (body-level `$.model` / `$.stream`).
- **`DetectResponseUsage`:** GLM's chat/completions API is OpenAI-compatible; reuse the OpenAI parser via an internal helper. Streaming mirrors OpenAI SSE.

### 3.4 DeepSeek — `adapters/deepseek/deepseek.go`

- **Host pattern:** `api.deepseek.com`.
- **DeepSeek is OpenAI wire-compat.** Implementation is a ~30-line wrapper that delegates to the OpenAI adapter's extraction logic but sets `Provider = "deepseek"` and class `"deepseek-bearer"` (DeepSeek uses `Authorization: Bearer sk-...` but the `sk-` prefix collides with OpenAI — host match is the disambiguator). Model string comes through unchanged from body `$.model` (e.g. `deepseek-chat`, `deepseek-reasoner`).

### 3.5 Registry wiring — `adapters/builtins.go`

- Register the four new adapters in the built-in list. Order for host-pattern matching: Bedrock, Vertex, GLM, DeepSeek, then the s2 adapters, Generic last.

### 3.6 Golden fixtures

Per-adapter `testdata/` directories with representative request/response/headers fixtures. Bedrock needs Titan + Anthropic-on-Bedrock + Cohere-on-Bedrock response fixtures to cover the dispatch logic.

## Acceptance Criteria

- Four new adapters compile, register, and pass `go test -race -count=1 ./packages/shared/traffic/adapters/...`.
- Bedrock adapter correctly extracts AKID from SigV4 `Credential=` field on a golden fixture and produces a stable fingerprint.
- Vertex adapter handles both Google-native model path and Anthropic partner model path.
- GLM and DeepSeek adapters produce non-empty `Provider` and `Model` on their host patterns and do not match on non-matching hosts.
- `generic` adapter's host-pattern deferral (s2.6) routes matching hosts to these new adapters rather than returning `unknown`.

## Non-Goals

- AWS SigV4 signature **verification** (we detect only, not enforce auth).
- Vertex OAuth token refresh or IAM service account mapping.
- GLM JWT signature verification.
- Bedrock response format coverage for every niche provider (initial coverage: Anthropic, Titan, Cohere; others fall to `parse_failed` with the right provider/model labels still recorded).

# E46 S2 — AI Gateway + OpenAI Chat normalizer end-to-end

**Epic:** E46
**Requirements:** [e46-traffic-normalization.md](../../../../docs/developers/specs/e46/e46-traffic-normalization.md)
**OpenAPI:** [e46-s2-aigw-openai.yaml](../../../../docs/users/api/openapi/ai-gateway/e46-s2-aigw-openai.yaml)
**Status:** Approved
**Date:** 2026-05-13

---

## Architecture summary

Phase 1 wires the first concrete normalizer (OpenAI Chat Completions) end-to-end through ai-gateway and verifies the storage and metrics paths work. Hooks are unchanged in this phase (they still see today's `HookInput.Content`); the Phase 1 PR only adds the persistence side. The hook input swap happens in S3.

The OpenAI normalizer handles both non-stream and streaming response bodies. For streaming, normalization runs at stream finalize — the buffered assembled body that the existing payload-capture pipeline already maintains is fed into the normalizer at the same `OnStreamEnd` hook point used today. No additional buffering.

---

## Story

### S2 — OpenAI Chat normalizer in ai-gateway

**User story:** As a compliance officer, I can open any OpenAI chat-completion traffic event and see a fully assembled `NormalizedPayload` row stored alongside the raw bytes.

**Tasks:**

- **T2.1** — Implement `packages/shared/transport/normalize/openai_chat.go`:
  - `OpenAIChatNormalizer` struct implementing `Normalizer`.
  - Request normalize: parse the `/v1/chat/completions` body; map `messages[]` → `NormalizedPayload.messages[]`, multimodal `content[]` → `ContentBlock[]` (text → text; image_url → image_ref placeholder pointing at the spill artifact when available); tools → `tools[]`; sampling params → `params{}`.
  - Response normalize (non-stream): the API's `choices[]` array becomes `output_messages[]`. Function/tool calls map to `tool_use` content blocks. `usage` maps directly.
  - Response normalize (stream): walk the assembled SSE byte buffer, concatenate `delta.content` per choice into the final assistant message, accumulate `tool_calls` arguments, capture `finish_reason` from the last chunk; ignore heartbeat `data: [DONE]` markers.
  - kind: `ai-chat` (or `ai-embedding` / `ai-image` when endpoint paths differ — handled by registry routing).
  - On unrecoverable parse error: return `(zero NormalizedPayload, fmt.Errorf("…: %w", normalize.ErrUnsupported))` so callers know the storage row should be marked `failed`.
- **T2.2** — Implement `packages/shared/transport/normalize/registry.go` lookup logic:
  - Route by `(provider, content_type, endpoint_path)` to a `Normalizer`. ai-gateway's adapter knows the provider — passes that as a hint.
  - Fallback chain: exact match → provider-only match → content-type match → `GenericHTTPNormalizer` (Phase 8, returns ErrUnsupported until then) → record as `unsupported`.
- **T2.3** — Wire ai-gateway adapter:
  - In `packages/ai-gateway/internal/adapter/` (or the equivalent layer where today the audit record is finalized), invoke the normalizer registry with the captured request body and the assembled response body at the same lifecycle points where `traffic_event` is written.
  - Write the resulting `NormalizedPayload` JSON into `traffic_event_normalized.{request_normalized, response_normalized}` along with `status` and any `error_reason`.
  - Emit metrics: `normalize_total{adapter="openai-chat", kind, status}`, `normalize_latency_ms`, `normalize_payload_bytes`.
- **T2.4** — Admin read endpoint `GET /api/admin/traffic/{id}/normalized` returns `{ request_normalized, response_normalized, request_status, response_status, request_error_reason, response_error_reason, request_redaction_spans, response_redaction_spans, normalize_version }`. Gated by existing `iam.ResourceTraffic.Action(VerbRead)`. OpenAPI spec under `docs/users/api/openapi/ai-gateway/e46-s2-aigw-openai.yaml`.
- **T2.5** — Hook input unchanged in this phase. Confirm by running existing hook tests; they must not require code changes.
- **T2.6** — Unit tests for `OpenAIChatNormalizer`: representative request/response fixtures (with and without tools, with and without streaming, with and without multimodal content), including pathological cases (truncated stream, malformed SSE event, missing finish_reason).
- **T2.7** — Integration test: send a real chat-completion request through a locally running ai-gateway against a mock provider; assert the new `traffic_event_normalized` row appears with the expected `messages[]` and `usage`.

**Acceptance:**

- **AC-S2.1** — A non-stream OpenAI chat call produces a `traffic_event_normalized` row with `request_status="ok"`, `response_status="ok"`, request `messages[]` exactly matching the request body's messages, and response `output_messages[0].content[0].text` equal to the provider's returned `choices[0].message.content`.
- **AC-S2.2** — A streamed OpenAI chat call produces the same shape; `output_messages[0].content[0].text` is the concatenation of every `delta.content` chunk; `finish_reason` is set from the last `delta.finish_reason`.
- **AC-S2.3** — A request with tools produces `tools[]` populated and (when the response includes tool calls) `output_messages[].content[].type == "tool_use"` blocks with `name` + `input` populated.
- **AC-S2.4** — A malformed response body yields `response_status="failed"`, `response_error_reason` populated, and `response_normalized=NULL`. The parent `traffic_event` row is unaffected.
- **AC-S2.5** — Metrics `normalize_total{adapter="openai-chat", kind="ai-chat", status="ok"}` and `normalize_latency_ms` are visible at `/metrics`.
- **AC-S2.6** — `GET /api/admin/traffic/{id}/normalized` returns the expected JSON for a known traffic event id; returns 404 for a non-existent id; returns 403 when the caller lacks the resource read action.
